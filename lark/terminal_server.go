package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

type nativeOutMsg struct {
	msgType uint8
	payload []byte
}

type nativeClient struct {
	server   *terminalServer
	hub      *terminalHub
	conn     net.Conn
	br       *bufio.Reader
	remoteIP netip.Addr
	ssrc     uint32
	client   *terminalClient

	ctrlOut      chan nativeOutMsg
	out          chan nativeOutMsg
	outMu        sync.Mutex
	drained      chan struct{}
	pendingFrame *nativeOutMsg
	frameNotify  chan struct{}
	txSeq        uint16
	droppedCtrl  atomic.Uint64

	udpMu   sync.Mutex
	udpAddr *net.UDPAddr
	rtpSeq  uint16
	rtpTS   uint32

	rtpMu      sync.Mutex
	rtpSeen    bool
	rtpLastSeq uint16
	rtpDrops   atomic.Uint64
}

type terminalServer struct {
	hub     *terminalHub
	tcpLn   net.Listener
	udpConn *net.UDPConn
	udpPort uint16

	peersMu sync.RWMutex
	peers   map[uint32]*nativeClient
	onUDP   func()

	closed atomic.Bool
	wg     sync.WaitGroup
}

func newTerminalServer(h *terminalHub, host, port string) (*terminalServer, error) {
	tcpLn, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, err
	}
	tcpAddr := tcpLn.Addr().(*net.TCPAddr)
	udpPort := tcpAddr.Port
	if port == "0" {
		// An ephemeral listener must not reuse the kernel's TCP pick: TCP and
		// UDP port spaces are independent, so the number may already belong to
		// another process. Production passes the firmware's fixed port, where
		// the shared number is the protocol.
		udpPort = 0
	}
	udpAddr := &net.UDPAddr{IP: tcpAddr.IP, Port: udpPort, Zone: tcpAddr.Zone}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		_ = tcpLn.Close()
		return nil, err
	}
	_ = udpConn.SetReadBuffer(256 << 10)
	_ = udpConn.SetWriteBuffer(256 << 10)

	s := &terminalServer{
		hub:     h,
		tcpLn:   tcpLn,
		udpConn: udpConn,
		udpPort: uint16(udpConn.LocalAddr().(*net.UDPAddr).Port),
		peers:   make(map[uint32]*nativeClient),
	}
	s.wg.Add(2)
	go s.acceptLoop()
	go s.udpReadLoop()
	return s, nil
}

func (s *terminalServer) shutdown() {
	if s == nil || s.closed.Swap(true) {
		return
	}
	_ = s.tcpLn.Close()
	_ = s.udpConn.Close()
	s.peersMu.RLock()
	for _, nc := range s.peers {
		nc.client.cancel()
		_ = nc.conn.Close()
	}
	s.peersMu.RUnlock()
	s.wg.Wait()
}

func (s *terminalServer) nextSSRC() uint32 {
	var b [4]byte
	for {
		_, _ = rand.Read(b[:])
		v := binary.BigEndian.Uint32(b[:])
		if v == 0 {
			continue
		}
		s.peersMu.RLock()
		_, exists := s.peers[v]
		s.peersMu.RUnlock()
		if !exists {
			return v
		}
	}
}

func (s *terminalServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.tcpLn.Accept()
		if err != nil {
			if s.closed.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			s.serveConn(c)
		}(conn)
	}
}

func writeNativeReject(conn net.Conn, reason uint8, msg string) {
	pkt := packProtoPacket(msgReject, 0, encodeReject(reason, msg))
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Write(pkt)
}

func (s *terminalServer) serveConn(conn net.Conn) {
	defer conn.Close()
	tuneTCP(conn)
	br := bufio.NewReaderSize(conn, 16<<10)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	hs, ok := readNativeHandshake(conn, br, s.hub.b.token)
	if !ok {
		return
	}
	host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	remoteIP, err := netip.ParseAddr(host)
	if err != nil {
		return
	}
	remoteIP = remoteIP.Unmap()

	ctx, cancel := context.WithCancel(s.hub.b.root)
	ssrc := s.nextSSRC()
	nc := newNativeClient(s, conn, br, remoteIP, ssrc, hs)
	c := &terminalClient{native: nc, ctx: ctx, cancel: cancel}
	nc.client = c

	if !s.admitClient(c) {
		cancel()
		writeNativeReject(conn, rejectFull, "server full")
		return
	}
	s.peersMu.Lock()
	s.peers[ssrc] = nc
	s.peersMu.Unlock()

	defer s.hub.conns.Done()
	defer func() {
		s.peersMu.Lock()
		delete(s.peers, ssrc)
		s.peersMu.Unlock()
		cancel()
		conn.Close()
		s.hub.detach(c)
	}()

	if hs.isResume {
		ok = s.hub.handleResume(c, hs.resume, time.Now())
	} else {
		ok = s.hub.handleHello(c, hs.hello, time.Now())
	}
	if !ok {
		s.flushQueuedReject(nc)
		return
	}
	go nc.writeLoop()
	for {
		_ = conn.SetReadDeadline(time.Now().Add(75 * time.Second))
		msgType, _, payload, err := readProtoFrame(nc.br)
		if err != nil || c.ctx.Err() != nil {
			return
		}
		s.handleTCPMessage(nc, msgType, payload)
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

// nativeHandshake is the parsed first frame: hello or resume.
type nativeHandshake struct {
	hello    terminalHello
	resume   terminalResume
	isResume bool
}

func readNativeHandshake(conn net.Conn, br *bufio.Reader, token string) (nativeHandshake, bool) {
	var hs nativeHandshake
	msgType, _, payload, err := readProtoFrame(br)
	if err != nil {
		return hs, false
	}
	switch msgType {
	case msgHello:
		hs.hello, err = parseTerminalHello(payload)
		if err != nil {
			writeNativeReject(conn, rejectBadCaps, "invalid hello")
			return hs, false
		}
		if hs.hello.protoVersion != protoVersion1 {
			writeNativeReject(conn, rejectBadVersion, "protocol version mismatch")
			return hs, false
		}
		if token == "" || subtle.ConstantTimeCompare([]byte(hs.hello.token), []byte(token)) != 1 {
			writeNativeReject(conn, rejectBadToken, "invalid token")
			return hs, false
		}
	case msgResume:
		hs.isResume = true
		hs.resume, err = parseTerminalResume(payload)
		if err != nil {
			writeNativeReject(conn, rejectUnknownTerminal, "invalid resume")
			return hs, false
		}
		if hs.resume.protoVersion != protoVersion1 {
			writeNativeReject(conn, rejectBadVersion, "protocol version mismatch")
			return hs, false
		}
	default:
		writeNativeReject(conn, rejectBadVersion, "expected hello or resume")
		return hs, false
	}
	return hs, true
}

func newNativeClient(s *terminalServer, conn net.Conn, br *bufio.Reader, remoteIP netip.Addr, ssrc uint32, hs nativeHandshake) *nativeClient {
	var initialUDP *net.UDPAddr
	if !hs.isResume && hs.hello.udpPort > 0 {
		initialUDP = &net.UDPAddr{IP: remoteIP.AsSlice(), Port: int(hs.hello.udpPort)}
	}
	return &nativeClient{
		server:      s,
		hub:         s.hub,
		conn:        conn,
		br:          br,
		remoteIP:    remoteIP,
		ssrc:        ssrc,
		ctrlOut:     make(chan nativeOutMsg, 64),
		out:         make(chan nativeOutMsg, 192),
		frameNotify: make(chan struct{}, 1),
		udpAddr:     initialUDP,
	}
}

// admitClient takes the connected-client slot before any hub registration.
func (s *terminalServer) admitClient(c *terminalClient) bool {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	if s.hub.closed || len(s.hub.clients) >= 64 {
		return false
	}
	s.hub.clients[c] = struct{}{}
	s.hub.conns.Add(1)
	return true
}

// flushQueuedReject writes a REJECT frame queued before the write loop started.
func (s *terminalServer) flushQueuedReject(nc *nativeClient) {
	select {
	case msg := <-nc.ctrlOut:
		pkt := packProtoPacket(msg.msgType, nc.txSeq, msg.payload)
		_ = nc.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_, _ = nc.conn.Write(pkt)
	default:
	}
}

func (s *terminalServer) handleTCPMessage(nc *nativeClient, msgType uint8, payload []byte) {
	c := nc.client
	switch msgType {
	case msgPing:
		if len(payload) == 4 {
			nc.enqueue(msgPong, payload)
		}
	case msgPong:
		if len(payload) == 4 {
			c.pingMu.Lock()
			if !c.pingSentAt.IsZero() && binary.LittleEndian.Uint32(payload) == uint32(c.pingSentAt.UnixMilli()) {
				c.rtt = new(milliseconds(time.Since(c.pingSentAt)))
				c.pingSentAt = time.Time{}
			}
			c.pingMu.Unlock()
		}
	default:
		s.hub.handleProto(c, msgType, payload)
	}
}

func (s *terminalServer) udpReadLoop() {
	defer s.wg.Done()
	buf := make([]byte, 1500)
	for {
		n, remote, err := s.udpConn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if s.closed.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		s.handleUDP(buf[:n], remote)
	}
}

func (s *terminalServer) handleUDP(pkt []byte, remote netip.AddrPort) {
	if len(pkt) < rtpHeaderSize+2 || (len(pkt)-rtpHeaderSize)%2 != 0 {
		return
	}
	pt, seq, _, ssrc, ok := decodeRTPHeader(pkt)
	if !ok || ssrc == 0 {
		return
	}
	s.peersMu.RLock()
	nc := s.peers[ssrc]
	s.peersMu.RUnlock()
	if nc == nil || remote.Addr().Unmap() != nc.remoteIP {
		return
	}

	if !nc.acceptRTP(seq) {
		return
	}
	nc.udpMu.Lock()
	if nc.udpAddr == nil || nc.udpAddr.Port != int(remote.Port()) {
		nc.udpAddr = net.UDPAddrFromAddrPort(remote)
	}
	nc.udpMu.Unlock()

	s.hub.dispatchRTPPCM(nc.client, pt, pkt[rtpHeaderSize:])
	if s.onUDP != nil {
		s.onUDP()
	}
}

// acceptRTP drops duplicate and reordered datagrams. Both are harmless for
// captured audio and replaying them would duplicate PCM into the turn.
func (nc *nativeClient) acceptRTP(seq uint16) bool {
	nc.rtpMu.Lock()
	defer nc.rtpMu.Unlock()
	if nc.rtpSeen && int16(seq-nc.rtpLastSeq) <= 0 {
		nc.rtpDrops.Add(1)
		return false
	}
	nc.rtpSeen, nc.rtpLastSeq = true, seq
	return true
}

func (nc *nativeClient) writePacket(msgType uint8, payload []byte) bool {
	var hdr [protoHeaderSize]byte
	hdr[0] = protoMagic
	hdr[1] = msgType
	binary.LittleEndian.PutUint16(hdr[2:4], nc.txSeq)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(payload)))
	nc.txSeq++
	_ = nc.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	bufs := net.Buffers{hdr[:], payload}
	_, err := bufs.WriteTo(nc.conn)
	return err == nil
}

func (nc *nativeClient) takeFrame() (nativeOutMsg, bool) {
	nc.outMu.Lock()
	defer nc.outMu.Unlock()
	if nc.pendingFrame == nil {
		return nativeOutMsg{}, false
	}
	msg := *nc.pendingFrame
	nc.pendingFrame = nil
	return msg, true
}

func (nc *nativeClient) nextMsg() (nativeOutMsg, bool, bool) {
	// Priority 1: control messages (REC_CTRL, TTS_CTRL, chimes, power, pong).
	select {
	case msg := <-nc.ctrlOut:
		return msg, false, true
	default:
	}
	// Priority 2: TTS PCM audio.
	select {
	case msg := <-nc.out:
		return msg, true, true
	default:
	}
	// Priority 3: latest coalesced display frame.
	nc.outMu.Lock()
	if nc.pendingFrame != nil {
		msg := *nc.pendingFrame
		nc.pendingFrame = nil
		nc.outMu.Unlock()
		return msg, false, true
	}
	nc.outMu.Unlock()
	return nativeOutMsg{}, false, false
}

const (
	pingAwake  = 12 * time.Second
	pingAsleep = 30 * time.Second
)

// pingInterval backs off liveness pings while the panel is off, so the radio
// and CPU are not woken every 12 s. The 75 s read deadline still catches a
// dead TCP connection.
func (nc *nativeClient) pingInterval() time.Duration {
	if nc.hub == nil || nc.client == nil {
		return pingAwake
	}
	d := nc.client.terminal.Load()
	asleep := d != nil && d.state().asleep
	if asleep {
		return pingAsleep
	}
	return pingAwake
}

func (nc *nativeClient) writeLoop() {
	defer nc.conn.Close()
	ping := time.NewTimer(nc.pingInterval())
	defer ping.Stop()
	for nc.client.ctx.Err() == nil {
		if msg, isPCM, ok := nc.nextMsg(); ok {
			if isPCM {
				nc.outMu.Lock()
				if nc.drained != nil {
					close(nc.drained)
					nc.drained = nil
				}
				nc.outMu.Unlock()
			}
			if !nc.writePacket(msg.msgType, msg.payload) {
				return
			}
			// A long TTS reply must not starve display and control traffic.
			if isPCM {
				if frame, ok := nc.takeFrame(); ok && !nc.writePacket(frame.msgType, frame.payload) {
					return
				}
			}
			continue
		}
		select {
		case <-nc.client.ctx.Done():
			return
		case msg := <-nc.ctrlOut:
			if !nc.writePacket(msg.msgType, msg.payload) {
				return
			}
		case msg := <-nc.out:
			nc.outMu.Lock()
			if nc.drained != nil {
				close(nc.drained)
				nc.drained = nil
			}
			nc.outMu.Unlock()
			if !nc.writePacket(msg.msgType, msg.payload) {
				return
			}
		case <-nc.frameNotify:
		case <-ping.C:
			now := time.Now()
			nc.client.pingMu.Lock()
			nc.client.pingSentAt = now
			nc.client.pingMu.Unlock()
			var ts [4]byte
			binary.LittleEndian.PutUint32(ts[:], uint32(now.UnixMilli()))
			if !nc.writePacket(msgPing, ts[:]) {
				return
			}
			ping.Reset(nc.pingInterval())
		}
	}
}

func (nc *nativeClient) enqueue(msgType uint8, payload []byte) bool {
	nc.outMu.Lock()
	defer nc.outMu.Unlock()
	if nc.client.ctx.Err() != nil {
		return false
	}
	if msgType == msgFrame {
		nc.pendingFrame = &nativeOutMsg{msgType: msgFrame, payload: payload}
		select {
		case nc.frameNotify <- struct{}{}:
		default:
		}
		return true
	}
	dst := nc.ctrlOut
	if dst == nil || msgType == msgTTSPCM {
		dst = nc.out
	}
	select {
	case dst <- nativeOutMsg{msgType: msgType, payload: payload}:
		return true
	default:
		if dst == nc.out {
			// PCM overflow is fatal for this stream: the client cannot keep up.
			nc.client.cancel()
			if nc.conn != nil {
				_ = nc.conn.Close()
			}
			return false
		}
		// Control frames are coalescing by nature. Drop and count instead of
		// killing a healthy connection over one burst.
		nc.droppedCtrl.Add(1)
		return false
	}
}

func (nc *nativeClient) enqueueWait(ctx context.Context, msgType uint8, payload []byte) bool {
	return enqueueWaitChan(ctx, nc.client.ctx, &nc.outMu, nc.out, &nc.drained, nativeOutMsg{msgType: msgType, payload: payload})
}
