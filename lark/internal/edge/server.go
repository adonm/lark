package edge

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"lark/internal/proto"
)

type outMsg struct {
	msgType uint8
	payload []byte
}

// nativeConn is one TCP session. Its write loop owns the socket; all other
// goroutines talk to it through the three priority queues.
type nativeConn struct {
	t        *Terminal
	conn     net.Conn
	remoteIP netip.Addr
	ssrc     uint32
	started  time.Time

	ctrlOut chan outMsg
	pcmOut  chan outMsg

	frameMu      sync.Mutex
	pendingFrame outMsg
	framePending bool
	frameNotify  chan struct{}
	txSeq        uint16

	rtp   atomic.Uint32 // high 16 bits: seen flag; low 16: last sequence
	drops atomic.Uint64

	lastMsg atomic.Uint32 // last received message type + 1

	reasonMu sync.Mutex
	reason   string

	closed  atomic.Bool
	writeWG sync.WaitGroup
}

// setReason records the first cause that ends this session so close logs it.
func (c *nativeConn) setReason(reason string) {
	c.reasonMu.Lock()
	if c.reason == "" {
		c.reason = reason
	}
	c.reasonMu.Unlock()
}

func (c *nativeConn) noteMsg(msgType uint8) { c.lastMsg.Store(uint32(msgType) + 1) }

func (c *nativeConn) sessionReason() string {
	c.reasonMu.Lock()
	defer c.reasonMu.Unlock()
	if c.reason == "" {
		return "closed"
	}
	return c.reason
}

func (c *nativeConn) enqueueCtrl(msgType uint8, payload []byte) bool {
	if c.closed.Load() {
		return false
	}
	select {
	case c.ctrlOut <- outMsg{msgType, payload}:
		return true
	default:
		c.drops.Add(1)
		return false
	}
}

func (c *nativeConn) enqueuePCM(payload []byte) bool {
	if c.closed.Load() {
		return false
	}
	select {
	case c.pcmOut <- outMsg{proto.MsgTTSPCM, payload}:
		return true
	default:
		c.drops.Add(1)
		return false
	}
}

func (c *nativeConn) enqueueFrame(payload []byte) bool {
	if c.closed.Load() {
		return false
	}
	c.frameMu.Lock()
	// Only the newest frame matters; reuse the slot instead of allocating a
	// fresh outMsg for every render. The replaced payload returns to the
	// encoder's free list once it can no longer be written.
	old := c.pendingFrame.payload
	c.pendingFrame = outMsg{proto.MsgFrame, payload}
	c.framePending = true
	c.frameMu.Unlock()
	if old != nil {
		c.releaseFrame(old)
	}
	select {
	case c.frameNotify <- struct{}{}:
	default:
	}
	return true
}

func (c *nativeConn) takeFrame() (outMsg, bool) {
	c.frameMu.Lock()
	defer c.frameMu.Unlock()
	if !c.framePending {
		return outMsg{}, false
	}
	msg := c.pendingFrame
	c.pendingFrame = outMsg{}
	c.framePending = false
	return msg, true
}

// releaseFrame returns a frame payload to the terminal's encoder after write.
func (c *nativeConn) releaseFrame(payload []byte) {
	if c.t != nil {
		c.t.releaseFrame(payload)
	}
}

func (c *nativeConn) nextMsg() (outMsg, bool, bool) {
	select {
	case msg := <-c.ctrlOut:
		return msg, false, true
	default:
	}
	select {
	case msg := <-c.pcmOut:
		return msg, true, true
	default:
	}
	if msg, ok := c.takeFrame(); ok {
		return msg, false, true
	}
	return outMsg{}, false, false
}

func (c *nativeConn) writePacket(msgType uint8, payload []byte) error {
	var hdr [proto.ProtoHeaderSize]byte
	hdr[0] = proto.ProtoMagic
	hdr[1] = msgType
	binary.LittleEndian.PutUint16(hdr[2:4], c.txSeq)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(payload)))
	c.txSeq++
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	bufs := net.Buffers{hdr[:], payload}
	_, err := bufs.WriteTo(c.conn)
	return err
}

// writeMsg writes one queued message and returns frame buffers to the encoder.
// The payload is not touched after WriteTo, whether the write succeeded or not.
func (c *nativeConn) writeMsg(msg outMsg) error {
	err := c.writePacket(msg.msgType, msg.payload)
	if msg.msgType == proto.MsgFrame {
		c.releaseFrame(msg.payload)
	}
	return err
}

func (c *nativeConn) writeLoop() {
	defer c.writeWG.Done()
	defer c.close()
	ping := time.NewTimer(c.t.pingInterval())
	defer ping.Stop()
	for {
		if c.t.isClosed() {
			return
		}
		if msg, isPCM, ok := c.nextMsg(); ok {
			if err := c.writeMsg(msg); err != nil {
				c.setReason("write: " + err.Error())
				return
			}
			if isPCM {
				if frame, ok := c.takeFrame(); ok {
					if err := c.writeMsg(frame); err != nil {
						c.setReason("write: " + err.Error())
						return
					}
				}
			}
			continue
		}
		select {
		case <-c.t.life.done:
			return
		case msg := <-c.ctrlOut:
			if err := c.writeMsg(msg); err != nil {
				c.setReason("write: " + err.Error())
				return
			}
		case msg := <-c.pcmOut:
			if err := c.writeMsg(msg); err != nil {
				c.setReason("write: " + err.Error())
				return
			}
		case <-c.frameNotify:
		case <-ping.C:
			var ts [4]byte
			binary.LittleEndian.PutUint32(ts[:], uint32(time.Now().UnixMilli()))
			if err := c.writePacket(proto.MsgPing, ts[:]); err != nil {
				c.setReason("ping: " + err.Error())
				return
			}
			ping.Reset(c.t.pingInterval())
		}
	}
}

func (c *nativeConn) close() {
	if c.closed.Swap(true) {
		return
	}
	_ = c.conn.Close()
	if frame, ok := c.takeFrame(); ok {
		c.releaseFrame(frame.payload)
	}
	c.t.detach(c)
	last := "-"
	if v := c.lastMsg.Load(); v != 0 {
		last = msgName(uint8(v - 1))
	}
	c.t.logf("terminal.disconnected id=%s ip=%s reason=%s last_msg=%s duration=%s reconnects=%d",
		c.t.info.id, c.remoteIP, c.sessionReason(), last, time.Since(c.started).Round(time.Millisecond),
		c.t.reconnects())
}

func msgName(msgType uint8) string {
	switch msgType {
	case proto.MsgHello:
		return "hello"
	case proto.MsgResume:
		return "resume"
	case proto.MsgTelemetry:
		return "telemetry"
	case proto.MsgPointer:
		return "pointer"
	case proto.MsgRecStart:
		return "rec_start"
	case proto.MsgRecEnd:
		return "rec_end"
	case proto.MsgTTSAck:
		return "tts_ack"
	case proto.MsgPing:
		return "ping"
	case proto.MsgPong:
		return "pong"
	}
	return fmt.Sprintf("0x%02x", msgType)
}

type terminalServer struct {
	e   *Edge
	tcp net.Listener
	udp *net.UDPConn

	peers sync.Map // uint32 SSRC -> *Terminal; lock-free RTP lookup

	// Reconnect counts outlive Terminal objects: a board that re-HELLOs inside
	// or after the resume window still belongs to the same physical device.
	statMu     sync.Mutex
	known      map[string]bool
	reconnects map[string]uint64

	closed  atomic.Bool
	wg      sync.WaitGroup
	udpPort uint16
}

func (s *terminalServer) trackConnect(id string, resume bool) uint64 {
	s.statMu.Lock()
	defer s.statMu.Unlock()
	if s.reconnects == nil {
		s.reconnects = make(map[string]uint64)
	}
	if resume || s.known[id] {
		s.reconnects[id]++
	}
	s.known[id] = true
	return s.reconnects[id]
}

func (s *terminalServer) reconnectsFor(id string) uint64 {
	s.statMu.Lock()
	defer s.statMu.Unlock()
	return s.reconnects[id]
}

func newTerminalServer(e *Edge, addr string) (*terminalServer, error) {
	tcp, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	tcpAddr := tcp.Addr().(*net.TCPAddr)
	port := tcpAddr.Port
	// With an ephemeral TCP port the kernel's UDP pick is independent; port 0
	// lets the OS choose a second ephemeral number.
	if tcpAddr.Port == 0 || tcpAddr.Port != 8766 {
		port = 0
	}
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: tcpAddr.IP, Zone: tcpAddr.Zone, Port: port})
	if err != nil {
		_ = tcp.Close()
		return nil, err
	}
	_ = udp.SetReadBuffer(256 << 10)
	_ = udp.SetWriteBuffer(256 << 10)
	s := &terminalServer{
		e: e, tcp: tcp, udp: udp,
		known:      make(map[string]bool),
		reconnects: make(map[string]uint64),
		udpPort:    uint16(udp.LocalAddr().(*net.UDPAddr).Port),
	}
	return s, nil
}

func (s *terminalServer) start() {
	s.wg.Add(2)
	go s.acceptLoop()
	go s.udpLoop()
}

func (s *terminalServer) close() {
	if s.closed.Swap(true) {
		return
	}
	_ = s.tcp.Close()
	_ = s.udp.Close()
	s.peers.Range(func(_, v any) bool {
		v.(*Terminal).close()
		return true
	})
	s.wg.Wait()
}

func (s *terminalServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.tcp.Accept()
		if err != nil {
			if s.closed.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serve(conn)
		}()
	}
}

func tuneTCP(conn net.Conn) {
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(15 * time.Second)
		_ = tc.SetReadBuffer(128 << 10)
		_ = tc.SetWriteBuffer(128 << 10)
	}
}

func (s *terminalServer) serve(conn net.Conn) {
	defer conn.Close()
	tuneTCP(conn)
	br := bufio.NewReaderSize(conn, 16<<10)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msgType, _, payload, err := proto.ReadFrame(br)
	if err != nil {
		return
	}
	host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	remoteIP, err := netip.ParseAddr(host)
	if err != nil {
		return
	}
	remoteIP = remoteIP.Unmap()

	var t *Terminal
	var isResume bool
	var reconnects uint64
	switch msgType {
	case proto.MsgHello:
		hello, ok := proto.DecodeTerminalHello(payload)
		if !ok {
			writeReject(conn, proto.RejectBadCaps, "invalid hello")
			return
		}
		if hello.ProtoVersion != proto.ProtoVersion1 {
			writeReject(conn, proto.RejectBadVersion, "protocol version mismatch")
			return
		}
		if s.e.cfg.Token == "" || subtle.ConstantTimeCompare([]byte(hello.Token), []byte(s.e.cfg.Token)) != 1 {
			writeReject(conn, proto.RejectBadToken, "invalid token")
			return
		}
		reconnects = s.trackConnect(hello.TerminalID, false)
		t = s.newTerminal(hello, remoteIP)
	case proto.MsgResume:
		resume, ok := proto.DecodeTerminalResume(payload)
		if !ok {
			writeReject(conn, proto.RejectUnknownTerminal, "invalid resume")
			return
		}
		if resume.ProtoVersion != proto.ProtoVersion1 {
			writeReject(conn, proto.RejectBadVersion, "protocol version mismatch")
			return
		}
		t = s.e.Terminal(resume.TerminalID)
		if t == nil {
			writeReject(conn, proto.RejectUnknownTerminal, "unknown terminal")
			return
		}
		reconnects = s.trackConnect(resume.TerminalID, true)
		isResume = true
	default:
		writeReject(conn, proto.RejectBadVersion, "expected hello or resume")
		return
	}

	ssrc := randomSSRC()
	nc := &nativeConn{
		t: t, conn: conn, remoteIP: remoteIP, ssrc: ssrc, started: time.Now(),
		ctrlOut:     make(chan outMsg, 64),
		pcmOut:      make(chan outMsg, 192),
		frameNotify: make(chan struct{}, 1),
	}
	if !isResume {
		t.conn.mu.Lock()
		t.conn.ip = remoteIP.String()
		t.conn.ssrc = ssrc
		t.conn.mu.Unlock()
		// A fresh HELLO is authoritative: the board rebooted (or its old TCP
		// session is half-open), so replace any warm or live session at once
		// instead of making it wait out the resume window.
		if old := s.e.install(t); old != nil {
			if onc := old.native(); onc != nil {
				onc.setReason("replaced")
			}
			old.close()
		}
		t.start(nc, s)
		s.addPeer(ssrc, t)
		_ = nc.writePacket(proto.MsgWelcome, t.welcomePayload(s.udpPort))
		nc.t.touch()
		s.e.logf("terminal.connected id=%s ip=%s board=%s fw=%s reconnects=%d",
			t.info.id, remoteIP, t.info.board, t.info.firmware, reconnects)
		s.e.emit("terminal.connected", t.connectedEvent())
	} else {
		t.conn.mu.Lock()
		old := t.conn.ssrc
		t.conn.ip = remoteIP.String()
		t.conn.ssrc = ssrc
		t.conn.mu.Unlock()
		if old != 0 {
			s.dropPeer(old)
		}
		s.addPeer(ssrc, t)
		t.attach(nc)
		_ = nc.writePacket(proto.MsgWelcome, t.welcomePayload(s.udpPort))
		s.e.logf("terminal.connected id=%s ip=%s board=%s fw=%s reconnects=%d resume=true",
			t.info.id, remoteIP, t.info.board, t.info.firmware, reconnects)
	}

	nc.writeWG.Add(1)
	go nc.writeLoop()

	for {
		_ = conn.SetReadDeadline(time.Now().Add(75 * time.Second))
		msgType, _, payload, err := proto.ReadFrame(br)
		if err != nil {
			nc.setReason(disconnectReason(err, t))
			nc.close()
			return
		}
		if t.isClosed() {
			nc.setReason("replaced")
			nc.close()
			return
		}
		nc.noteMsg(msgType)
		t.touch()
		s.handleTCP(t, nc, msgType, payload)
	}
}

func disconnectReason(err error, t *Terminal) string {
	if t.isClosed() || t.life.replaced.Load() {
		return "replaced"
	}
	switch {
	case errors.Is(err, io.EOF):
		return "EOF"
	case errors.Is(err, net.ErrClosed):
		return "closed"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "i/o timeout"
	}
	return err.Error()
}

func (s *terminalServer) addPeer(ssrc uint32, t *Terminal) { s.peers.Store(ssrc, t) }

func (s *terminalServer) dropPeer(ssrc uint32) { s.peers.Delete(ssrc) }

func (s *terminalServer) handleTCP(t *Terminal, nc *nativeConn, msgType uint8, payload []byte) {
	switch msgType {
	case proto.MsgPing:
		if len(payload) == 4 {
			nc.enqueueCtrl(proto.MsgPong, payload)
		}
	case proto.MsgPong:
		// Liveness only; the read deadline is the real keepalive.
	case proto.MsgTelemetry:
		if m, ok := proto.DecodeProtoTelemetry(payload); ok {
			t.telemetry(m)
		}
	case proto.MsgPointer:
		if m, ok := proto.DecodeProtoPointer(payload); ok {
			t.pointer(m)
		}
	case proto.MsgRecStart:
		if m, ok := proto.DecodeProtoRecStart(payload); ok {
			t.event(evRecStart, func(ev *termEvent) { ev.start = m })
		}
	case proto.MsgRecEnd:
		if m, ok := proto.DecodeProtoRecEnd(payload); ok {
			t.event(evRecEnd, func(ev *termEvent) { ev.end = m })
		}
	case proto.MsgTTSAck:
		if m, ok := proto.DecodeProtoTTSAck(payload); ok {
			t.event(evTTSAck, func(ev *termEvent) { ev.ack = m })
		}
	default:
		// Unknown device messages are ignored forward-compatibly.
	}
}

func (s *terminalServer) udpLoop() {
	defer s.wg.Done()
	buf := make([]byte, 1500)
	for {
		n, remote, err := s.udp.ReadFromUDPAddrPort(buf)
		if err != nil {
			if s.closed.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		s.handleUDP(buf[:n], remote)
	}
}

// handleUDP is the RTP hot path. It performs no allocation: the RTP header is
// parsed in place and the PCM is copied into a preallocated capture slot.
func (s *terminalServer) handleUDP(pkt []byte, remote netip.AddrPort) {
	if len(pkt) < proto.RtpHeaderSize+2 {
		return
	}
	if pkt[0] != proto.RtpVersionByte || pkt[1] != proto.RtpPTRecPCM {
		return
	}
	ssrc := binary.BigEndian.Uint32(pkt[8:12])
	seq := binary.BigEndian.Uint16(pkt[2:4])
	v, ok := s.peers.Load(ssrc)
	if !ok {
		return
	}
	t := v.(*Terminal)
	nc := t.native()
	if nc == nil || remote.Addr().Unmap() != nc.remoteIP {
		return
	}
	if !nc.acceptRTP(seq) {
		return
	}
	t.ingestRTP(pkt[proto.RtpHeaderSize:], time.Now())
}

// acceptRTP is lock-free: one atomic word holds the seen flag and the last
// accepted sequence. Reordering and duplicates are dropped without a mutex.
func (c *nativeConn) acceptRTP(seq uint16) bool {
	for {
		old := c.rtp.Load()
		if old>>16 != 0 && int16(seq-uint16(old)) <= 0 {
			c.drops.Add(1)
			return false
		}
		if c.rtp.CompareAndSwap(old, 1<<16|uint32(seq)) {
			return true
		}
	}
}

func writeReject(conn net.Conn, reason uint8, msg string) {
	p := proto.ProtoReject{Reason: reason, Message: msg}.Encode()
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Write(proto.Pack(proto.MsgReject, 0, p))
}

func randomSSRC() uint32 {
	var b [4]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return 1
		}
		if v := binary.BigEndian.Uint32(b[:]); v != 0 {
			return v
		}
	}
}

func randomToken() [proto.ResumeTokenLen]byte {
	var token [proto.ResumeTokenLen]byte
	_, _ = io.ReadFull(rand.Reader, token[:])
	return token
}
