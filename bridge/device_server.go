package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"log"
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
	server   *deviceServer
	hub      *wsHub
	conn     net.Conn
	remoteIP netip.Addr
	ssrc     uint32
	client   *wsClient

	out     chan nativeOutMsg
	outMu   sync.Mutex
	drained chan struct{}
	txSeq   uint16

	udpMu   sync.Mutex
	udpAddr *net.UDPAddr
	rtpSeq  uint16
	rtpTS   uint32
}

type deviceServer struct {
	hub     *wsHub
	tcpLn   net.Listener
	udpConn *net.UDPConn
	udpPort uint16

	peersMu sync.RWMutex
	peers   map[uint32]*nativeClient

	closed atomic.Bool
	wg     sync.WaitGroup
}

func newDeviceServer(h *wsHub, host, port string) (*deviceServer, error) {
	tcpLn, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, err
	}
	tcpAddr := tcpLn.Addr().(*net.TCPAddr)
	udpAddr := &net.UDPAddr{IP: tcpAddr.IP, Port: tcpAddr.Port, Zone: tcpAddr.Zone}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		_ = tcpLn.Close()
		return nil, err
	}
	_ = udpConn.SetReadBuffer(256 << 10)
	_ = udpConn.SetWriteBuffer(256 << 10)

	s := &deviceServer{
		hub:     h,
		tcpLn:   tcpLn,
		udpConn: udpConn,
		udpPort: uint16(tcpAddr.Port),
		peers:   make(map[uint32]*nativeClient),
	}
	s.wg.Add(2)
	go s.acceptLoop()
	go s.udpReadLoop()
	return s, nil
}

func (s *deviceServer) shutdown() {
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

func (s *deviceServer) nextSSRC() uint32 {
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

func (s *deviceServer) acceptLoop() {
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

func (s *deviceServer) serveConn(conn net.Conn) {
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(15 * time.Second)
		_ = tc.SetReadBuffer(128 << 10)
		_ = tc.SetWriteBuffer(128 << 10)
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msgType, _, payload, err := readProtoFrame(conn)
	if err != nil || msgType != msgHello {
		return
	}
	hello, err := parseNativeHello(payload)
	if err != nil {
		return
	}
	if s.hub.b.token == "" || subtle.ConstantTimeCompare([]byte(hello.token), []byte(s.hub.b.token)) != 1 {
		return
	}

	host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	remoteIP, err := netip.ParseAddr(host)
	if err != nil {
		return
	}
	remoteIP = remoteIP.Unmap()

	s.hub.mu.Lock()
	full := s.hub.closed || len(s.hub.clients) >= wsConnections
	s.hub.mu.Unlock()
	if full {
		return
	}

	ctx, cancel := context.WithCancel(s.hub.b.root)
	ssrc := s.nextSSRC()
	var initialUDP *net.UDPAddr
	if hello.udpPort > 0 {
		initialUDP = &net.UDPAddr{IP: remoteIP.AsSlice(), Port: int(hello.udpPort)}
	}
	nc := &nativeClient{
		server:   s,
		hub:      s.hub,
		conn:     conn,
		remoteIP: remoteIP,
		ssrc:     ssrc,
		out:      make(chan nativeOutMsg, 192),
		udpAddr:  initialUDP,
	}
	c := &wsClient{
		native: nc,
		ctx:    ctx,
		cancel: cancel,
	}
	nc.client = c

	s.hub.mu.Lock()
	if s.hub.closed || len(s.hub.clients) >= wsConnections {
		s.hub.mu.Unlock()
		cancel()
		return
	}
	s.hub.clients[c] = struct{}{}
	s.hub.conns.Add(1)
	s.hub.mu.Unlock()

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

	if !s.hub.helloNative(nc, hello, time.Now()) {
		return
	}

	go nc.writeLoop()

	for {
		_ = conn.SetReadDeadline(time.Now().Add(75 * time.Second))
		mType, _, mPayload, err := readProtoFrame(conn)
		if err != nil || c.ctx.Err() != nil {
			return
		}
		s.handleTCPMessage(nc, mType, mPayload)
	}
}

func (s *deviceServer) handleTCPMessage(nc *nativeClient, msgType uint8, payload []byte) {
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
	case msgStatus:
		if st, ok := parseNativeStatus(payload); ok {
			s.hub.handle(c, wsEvent{Event: "status", wsStatus: st})
		}
	case msgPointer:
		if evName, x, y, rev, ok := parseNativePointer(payload); ok {
			s.hub.handle(c, wsEvent{Event: evName, X: x, Y: y, RevLo: &rev})
		}
	case msgWakeWord:
		s.hub.handle(c, wsEvent{Event: "wake_word"})
	case msgRecStart:
		if _, target, ok := parseNativeRecStart(payload); ok {
			t := ""
			if target&recTargetInter != 0 {
				t = "intercom"
			}
			s.hub.handle(c, wsEvent{Event: "rec_start", Target: t})
		}
	case msgRecEnd:
		if _, target, cancelled, ok := parseNativeRecEnd(payload); ok {
			t := ""
			if target&recTargetInter != 0 {
				t = "intercom"
			}
			s.hub.handle(c, wsEvent{Event: "rec_end", Target: t, Cancelled: cancelled})
		}
	case msgTTSAck:
		if aborted, id, ok := parseNativeTTSAck(payload); ok {
			state := "done"
			if aborted {
				state = "interrupted"
			}
			s.hub.handle(c, wsEvent{Event: "tts_ack", State: state, Audio: id})
		}
	case msgCallFailed:
		if id, ok := parseNativeCallFailed(payload); ok {
			s.hub.handle(c, wsEvent{Event: "call_end", Call: id})
		}
	}
}

func (s *deviceServer) udpReadLoop() {
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
		if n < rtpHeaderSize+2 || (n-rtpHeaderSize)%2 != 0 {
			continue
		}
		pt, _, _, ssrc, ok := decodeRTPHeader(buf[:n])
		if !ok || ssrc == 0 {
			continue
		}
		s.peersMu.RLock()
		nc := s.peers[ssrc]
		s.peersMu.RUnlock()
		if nc == nil || remote.Addr().Unmap() != nc.remoteIP {
			continue
		}

		nc.udpMu.Lock()
		if nc.udpAddr == nil || nc.udpAddr.Port != int(remote.Port()) {
			nc.udpAddr = net.UDPAddrFromAddrPort(remote)
		}
		nc.udpMu.Unlock()

		pcm := make([]byte, n-rtpHeaderSize)
		copy(pcm, buf[rtpHeaderSize:n])
		switch pt {
		case rtpPTRecPCM:
			s.hub.micChunkPCM(nc.client, micPrefix, pcm)
		case rtpPTCallUp:
			s.hub.micChunkPCM(nc.client, callMicPrefix, pcm)
		}
	}
}

func (nc *nativeClient) writeLoop() {
	defer nc.conn.Close()
	ping := time.NewTicker(12 * time.Second)
	defer ping.Stop()
	for nc.client.ctx.Err() == nil {
		select {
		case <-nc.client.ctx.Done():
			return
		case msg := <-nc.out:
			nc.outMu.Lock()
			if nc.drained != nil {
				close(nc.drained)
				nc.drained = nil
			}
			nc.outMu.Unlock()
			pkt := packProtoPacket(msg.msgType, nc.txSeq, msg.payload)
			nc.txSeq++
			_ = nc.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := nc.conn.Write(pkt); err != nil {
				return
			}
		case <-ping.C:
			now := time.Now()
			nc.client.pingMu.Lock()
			nc.client.pingSentAt = now
			nc.client.pingMu.Unlock()
			var ts [4]byte
			binary.LittleEndian.PutUint32(ts[:], uint32(now.UnixMilli()))
			pkt := packProtoPacket(msgPing, nc.txSeq, ts[:])
			nc.txSeq++
			_ = nc.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := nc.conn.Write(pkt); err != nil {
				return
			}
		}
	}
}

func (nc *nativeClient) enqueue(msgType uint8, payload []byte) bool {
	nc.outMu.Lock()
	defer nc.outMu.Unlock()
	if nc.client.ctx.Err() != nil {
		return false
	}
	select {
	case nc.out <- nativeOutMsg{msgType: msgType, payload: payload}:
		return true
	default:
		nc.client.cancel()
		_ = nc.conn.Close()
		return false
	}
}

func (nc *nativeClient) enqueueWait(ctx context.Context, msgType uint8, payload []byte) bool {
	for {
		nc.outMu.Lock()
		if nc.client.ctx.Err() != nil || ctx.Err() != nil {
			nc.outMu.Unlock()
			return false
		}
		if len(nc.out) < min(8, cap(nc.out)) {
			nc.out <- nativeOutMsg{msgType: msgType, payload: payload}
			nc.outMu.Unlock()
			return true
		}
		if nc.drained == nil {
			nc.drained = make(chan struct{})
		}
		drained := nc.drained
		nc.outMu.Unlock()
		select {
		case <-drained:
		case <-nc.client.ctx.Done():
			return false
		case <-ctx.Done():
			return false
		}
	}
}

func (nc *nativeClient) sendCallDownUDP(pcm []byte) bool {
	if len(pcm) == 0 || len(pcm)%2 != 0 {
		return false
	}
	nc.udpMu.Lock()
	addr := nc.udpAddr
	if addr == nil {
		nc.udpMu.Unlock()
		return false
	}
	var pkt [rtpPacketBytes]byte
	for len(pcm) > 0 {
		chunk := min(rtpPCMBytes, len(pcm))
		seq := nc.rtpSeq
		nc.rtpSeq++
		ts := nc.rtpTS
		nc.rtpTS += uint32(chunk / 2)
		n := encodeRTPPacket(pkt[:], rtpPTCallDown, seq, ts, nc.ssrc, pcm[:chunk])
		if _, err := nc.server.udpConn.WriteToUDP(pkt[:n], addr); err != nil {
			nc.udpMu.Unlock()
			log.Printf("native UDP call send error: %v", err)
			return false
		}
		pcm = pcm[chunk:]
	}
	nc.udpMu.Unlock()
	return true
}
