package main

import (
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

type terminalServer struct {
	hub     *wsHub
	tcpLn   net.Listener
	udpConn *net.UDPConn
	udpPort uint16

	peersMu sync.RWMutex
	peers   map[uint32]*nativeClient
	onUDP   func()

	closed atomic.Bool
	wg     sync.WaitGroup
}

func newTerminalServer(h *wsHub, host, port string) (*terminalServer, error) {
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

	s := &terminalServer{
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
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(15 * time.Second)
		_ = tc.SetReadBuffer(128 << 10)
		_ = tc.SetWriteBuffer(128 << 10)
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msgType, _, payload, err := readProtoFrame(conn)
	if err != nil {
		return
	}

	var hello terminalHello
	var resume terminalResume
	isResume := false
	switch msgType {
	case msgHello:
		hello, err = parseTerminalHello(payload)
		if err != nil {
			writeNativeReject(conn, rejectBadCaps, "invalid hello")
			return
		}
		if hello.protoVersion != protoVersion1 {
			writeNativeReject(conn, rejectBadVersion, "protocol version mismatch")
			return
		}
		if s.hub.b.token == "" || subtle.ConstantTimeCompare([]byte(hello.token), []byte(s.hub.b.token)) != 1 {
			writeNativeReject(conn, rejectBadToken, "invalid token")
			return
		}
	case msgResume:
		isResume = true
		resume, err = parseTerminalResume(payload)
		if err != nil {
			writeNativeReject(conn, rejectUnknownTerminal, "invalid resume")
			return
		}
		if resume.protoVersion != protoVersion1 {
			writeNativeReject(conn, rejectBadVersion, "protocol version mismatch")
			return
		}
	default:
		writeNativeReject(conn, rejectBadVersion, "expected hello or resume")
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
	var initialUDP *net.UDPAddr
	if !isResume && hello.udpPort > 0 {
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
		writeNativeReject(conn, rejectFull, "server full")
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

	var ok bool
	if isResume {
		ok = s.hub.handleResume(c, resume, time.Now())
	} else {
		ok = s.hub.handleHello(c, hello, time.Now())
	}
	if !ok {
		// Flush any REJECT frame queued before closing.
		select {
		case msg := <-nc.out:
			pkt := packProtoPacket(msg.msgType, nc.txSeq, msg.payload)
			_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			_, _ = conn.Write(pkt)
		default:
		}
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
	pt, _, _, ssrc, ok := decodeRTPHeader(pkt)
	if !ok || ssrc == 0 {
		return
	}
	s.peersMu.RLock()
	nc := s.peers[ssrc]
	s.peersMu.RUnlock()
	if nc == nil || remote.Addr().Unmap() != nc.remoteIP {
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
	return enqueueWaitChan(ctx, nc.client.ctx, &nc.outMu, nc.out, &nc.drained, nativeOutMsg{msgType: msgType, payload: payload})
}
