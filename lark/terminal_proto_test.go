package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
	"time"
)

type testNativeClient struct {
	t       *testing.T
	conn    net.Conn
	udpConn *net.UDPConn
	udpPort uint16
	srvAddr *net.UDPAddr
	ssrc    uint32
	txSeq   uint16
	rtpSeq  uint16
	rtpTS   uint32
}

func dialNativeDevice(t *testing.T, srv *terminalServer, id, token string, caps uint32) (*testNativeClient, []byte) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", srv.tcpLn.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { udpConn.Close() })
	localUDPPort := uint16(udpConn.LocalAddr().(*net.UDPAddr).Port)

	nc := &testNativeClient{
		t:       t,
		conn:    conn,
		udpConn: udpConn,
		udpPort: localUDPPort,
	}
	nc.send(msgHello, encodeTerminalHello(terminalHello{
		protoVersion:   protoVersion1,
		kind:           termKindHardware,
		capsMask:       caps,
		displayFormats: dispRGB565,
		frameCodecs:    codecBitAll,
		width:          360,
		height:         360,
		shape:          shapeRound,
		udpPort:        localUDPPort,
		terminalID:     id,
		board:          "esp-vocat",
		firmware:       "0.12.0-test",
		token:          token,
	}))

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	mType, _, payload, err := readProtoFrame(conn)
	if err != nil {
		return nil, nil
	}
	if mType == msgReject {
		return nil, payload
	}
	if mType != msgWelcome || len(payload) < 38 {
		t.Fatalf("expected WELCOME, got type=0x%02x len=%d", mType, len(payload))
	}
	srvPort := binary.LittleEndian.Uint16(payload[2:4])
	nc.ssrc = binary.LittleEndian.Uint32(payload[4:8])
	nc.srvAddr = &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: int(srvPort)}
	return nc, payload
}

func (nc *testNativeClient) send(msgType uint8, payload []byte) {
	nc.t.Helper()
	pkt := packProtoPacket(msgType, nc.txSeq, payload)
	nc.txSeq++
	if _, err := nc.conn.Write(pkt); err != nil {
		nc.t.Fatal(err)
	}
}

func (nc *testNativeClient) readUntil(wantType uint8) []byte {
	nc.t.Helper()
	_ = nc.conn.SetReadDeadline(time.Now().Add(4 * time.Second))
	for {
		mType, _, payload, err := readProtoFrame(nc.conn)
		if err != nil {
			nc.t.Fatalf("readUntil(0x%02x): %v", wantType, err)
		}
		if mType == msgPing && len(payload) == 4 {
			nc.send(msgPong, payload)
			continue
		}
		if mType == wantType {
			return payload
		}
	}
}

func (nc *testNativeClient) sendRTP(pt uint8, pcm []byte) {
	nc.t.Helper()
	pkt := make([]byte, rtpHeaderSize+len(pcm))
	n := encodeRTPPacket(pkt, pt, nc.rtpSeq, nc.rtpTS, nc.ssrc, pcm)
	nc.rtpSeq++
	nc.rtpTS += uint32(len(pcm) / 2)
	if _, err := nc.udpConn.WriteToUDP(pkt[:n], nc.srvAddr); err != nil {
		nc.t.Fatal(err)
	}
}

func (nc *testNativeClient) readRTP(wantPT uint8) []byte {
	nc.t.Helper()
	_ = nc.udpConn.SetReadDeadline(time.Now().Add(4 * time.Second))
	buf := make([]byte, 1500)
	for {
		n, _, err := nc.udpConn.ReadFromUDP(buf)
		if err != nil {
			nc.t.Fatalf("readRTP(pt=%d): %v", wantPT, err)
		}
		pt, _, _, ssrc, ok := decodeRTPHeader(buf[:n])
		if ok && pt == wantPT && ssrc == nc.ssrc {
			out := make([]byte, n-rtpHeaderSize)
			copy(out, buf[rtpHeaderSize:n])
			return out
		}
	}
}

func TestNativeProtoHandshakeAndBadToken(t *testing.T) {
	b, _ := wsHarness(t, nil)
	srv, err := newTerminalServer(b.hub, "127.0.0.1", "0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.shutdown()

	// Bad token must be rejected and connection closed.
	bad, _ := dialNativeDevice(t, srv, "lark-bad", "wrong-token", capDrag)
	if bad != nil {
		t.Fatal("expected bad token to be rejected")
	}

	// Valid handshake returns MSG_HELLO_OK and initial MSG_FRAME.
	caps := uint32(capBattery | capDrag | capPointerRev | capBridgeEndpoint | capPanelSleep)
	nc, okPayload := dialNativeDevice(t, srv, "lark-nat1", "unit", caps)
	if nc == nil || nc.ssrc == 0 || binary.LittleEndian.Uint16(okPayload[2:4]) != srv.udpPort {
		t.Fatal("invalid WELCOME")
	}
	frame := nc.readUntil(msgFrame)
	if len(frame) < 12 || frame[0] != frameTag {
		t.Fatalf("expected valid MSG_FRAME, got len=%d", len(frame))
	}
	codec := frame[9]
	if codec != frameCodecRaw && codec != frameCodecRLE565 && codec != frameCodecJPEG && codec != frameCodecLZ4 {
		t.Fatalf("unexpected frame codec %d", codec)
	}
}

func TestNativeProtoControlAndMessages(t *testing.T) {
	b, _ := wsHarness(t, nil)
	srv, err := newTerminalServer(b.hub, "127.0.0.1", "0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.shutdown()

	nc, _ := dialNativeDevice(t, srv, "lark-nat2", "unit", capDrag|capPointerRev|capBattery)
	frame := nc.readUntil(msgFrame)
	_ = binary.LittleEndian.Uint16(frame[10:12])

	// Send MSG_STATUS
	var st [16]byte
	rssi := int16(-52)
	binary.LittleEndian.PutUint16(st[0:2], uint16(rssi))
	binary.LittleEndian.PutUint16(st[2:4], uint16(88))
	st[4] = 1
	st[5] = 23
	binary.LittleEndian.PutUint32(st[8:12], 512000)
	binary.LittleEndian.PutUint32(st[12:16], 42)
	nc.send(msgTelemetry, st[:])

	// Cancel recording cleanly via MSG_REC_START + MSG_REC_END
	nc.send(msgRecStart, []byte{1, 0, 0})
	nc.send(msgRecEnd, []byte{1, 0, 1, 0})
	chime := nc.readUntil(msgChime)
	if len(chime) != 1 || chime[0] != chimeCancel {
		t.Fatalf("expected MSG_CHIME cancel, got %v", chime)
	}

	// Test TTS control + PCM + ACK, power, and volume
	b.hub.mu.Lock()
	dev := b.hub.terminals["lark-nat2"]
	if dev == nil || dev.telemetry.Battery == nil || *dev.telemetry.Battery != 88 || *dev.telemetry.WiFi != -52 {
		b.hub.mu.Unlock()
		t.Fatal("MSG_TELEMETRY was not applied")
	}
	b.hub.setSleeping(dev, true)
	b.hub.pushVolume(dev, true, false)
	stream := newAudioStream()
	pcm := bytes.Repeat([]byte{0x12, 0x34}, 320)
	stream.append(pcm)
	stream.finish(nil)
	b.hub.play(dev, stream)
	b.hub.mu.Unlock()

	pwr := nc.readUntil(msgPower)
	if len(pwr) != 1 || pwr[0] != 12 {
		t.Fatalf("expected MSG_POWER brightness=12, got %v", pwr)
	}
	vol := nc.readUntil(msgVolume)
	if len(vol) < 3 || vol[1] != 1 {
		t.Fatalf("expected MSG_VOLUME with persist=1, got %v", vol)
	}
	ttsStart := nc.readUntil(msgTTSCtrl)
	if len(ttsStart) < 9 || ttsStart[0] != ttsOpStart {
		t.Fatalf("expected MSG_TTS_CTRL start, got %v", ttsStart)
	}
	ttsPCM := nc.readUntil(msgTTSPCM)
	if !bytes.Equal(ttsPCM, pcm) {
		t.Fatal("MSG_TTS_PCM payload mismatch")
	}
	ttsEnd := nc.readUntil(msgTTSCtrl)
	if len(ttsEnd) < 9 || ttsEnd[0] != ttsOpEnd {
		t.Fatalf("expected MSG_TTS_CTRL end, got %v", ttsEnd)
	}
	audioID := binary.LittleEndian.Uint64(ttsStart[1:9])
	nc.send(msgTTSAck, encodeTTSAck(false, audioID))
}

func TestNativeUDPRecordingAndCrossTransportCall(t *testing.T) {
	gotPCM := make(chan []byte, 1)
	b, _ := wsHarness(t, func(b *larkServer, _ string) {
		b.setSTT(func(_ context.Context, pcm []byte) (string, error) {
			cp := make([]byte, len(pcm))
			copy(cp, pcm)
			select {
			case gotPCM <- cp:
			default:
			}
			return "", nil
		})
	})
	srv, err := newTerminalServer(b.hub, "127.0.0.1", "0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.shutdown()

	// 1. Native device UDP 20 ms REC_PCM
	nat, _ := dialNativeDevice(t, srv, "lark-natcall", "unit", capDrag|capPointerRev)
	frame := nat.readUntil(msgFrame)
	_ = binary.LittleEndian.Uint16(frame[10:12])

	udpDone := make(chan struct{}, 1)
	srv.onUDP = func() {
		select {
		case udpDone <- struct{}{}:
		default:
		}
	}

	nat.send(msgRecStart, []byte{1, 0, 0})
	nat.send(msgPing, []byte{1, 2, 3, 4})
	nat.readUntil(msgPong)

	recChunk := bytes.Repeat([]byte{0x2A, 0x00}, rtpSamples) // 640 B = 20 ms
	nat.sendRTP(rtpPTRecPCM, recChunk)
	select {
	case <-udpDone:
	case <-time.After(testWait(t)):
		t.Fatal("timed out waiting for UDP REC_PCM")
	}
	nat.send(msgRecEnd, []byte{1, 0, 0, 0})

	select {
	case pcm := <-gotPCM:
		if !bytes.Equal(pcm, recChunk) {
			t.Fatalf("UDP REC_PCM mismatch: got %d bytes", len(pcm))
		}
	case <-time.After(testWait(t)):
		t.Fatal("timed out waiting for UDP REC_PCM")
	}
}

func TestNativeUDPEndpointCopyRace(t *testing.T) {
	b, _ := wsHarness(t, nil)
	ds, err := newTerminalServer(b.hub, "127.0.0.1", "0")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.shutdown()
	_, _ = dialNativeDevice(t, ds, "lark-udprace", "unit", capBridgeEndpoint)

	b.hub.mu.Lock()
	d := b.hub.terminals["lark-udprace"]
	free := make(chan int, 16)
	for i := range 16 {
		free <- i
	}
	ep := &endpointCapture{
		cancel:    func() {},
		packets:   make(chan micPacket, 16),
		free:      free,
		slots:     new(endpointRing),
		result:    make(chan captureResult, 1),
		listening: make(chan struct{}),
		deadline:  time.NewTimer(time.Minute),
	}
	d.recording.phase = captureOpen
	d.recording.endpoint = ep
	ssrc := d.client.native.ssrc
	b.hub.mu.Unlock()

	remote := netip.MustParseAddrPort("127.0.0.1:40000")
	pkt := make([]byte, rtpPacketBytes)
	pcmPayload := make([]byte, rtpPCMBytes)

	const packets = 8
	for i := range packets {
		fill := byte(0x10 + i)
		for j := range pcmPayload {
			pcmPayload[j] = fill
		}
		encodeRTPPacket(pkt, rtpPTRecPCM, uint16(i+1), uint32(i*rtpSamples), ssrc, pcmPayload)
		ds.handleUDP(pkt, remote)
		// Immediately overwrite the shared buffer to catch any retained slice.
		clear(pkt[rtpHeaderSize:])
	}

	for i := range packets {
		p := <-ep.packets
		want := bytes.Repeat([]byte{byte(0x10 + i)}, rtpPCMBytes)
		if !bytes.Equal(p.pcm, want) {
			t.Fatalf("packet %d corrupted: got first byte 0x%02x, want 0x%02x", i, p.pcm[0], want[0])
		}
	}
}

func TestNativeUDPZeroAlloc(t *testing.T) {
	b, _ := wsHarness(t, nil)
	ds, err := newTerminalServer(b.hub, "127.0.0.1", "0")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.shutdown()
	nc, _ := dialNativeDevice(t, ds, "lark-udpalloc", "unit", 0)
	nc.readUntil(msgFrame)

	b.hub.mu.Lock()
	d := b.hub.terminals["lark-udpalloc"]
	d.recording.phase = captureOpen
	d.recording.pcm = make([]byte, 0, 128*rtpPCMBytes)
	ssrc := d.client.native.ssrc
	b.hub.mu.Unlock()

	remote := netip.MustParseAddrPort("127.0.0.1:40000")
	pkt := make([]byte, rtpPacketBytes)
	encodeRTPPacket(pkt, rtpPTRecPCM, 1, 0, ssrc, make([]byte, rtpPCMBytes))

	allocs := testing.AllocsPerRun(50, func() {
		ds.handleUDP(pkt, remote)
	})
	if allocs > 0 {
		t.Fatalf("handleUDP allocated %.2f times per packet, want 0", allocs)
	}
}

func TestEndpointCaptureZeroAlloc(t *testing.T) {
	b, _ := wsHarness(t, nil)
	ds, err := newTerminalServer(b.hub, "127.0.0.1", "0")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.shutdown()
	nc, _ := dialNativeDevice(t, ds, "lark-epalloc", "unit", 0)
	nc.readUntil(msgFrame)

	free := make(chan int, endpointRingSlots)
	for i := range endpointRingSlots {
		free <- i
	}
	b.hub.mu.Lock()
	d := b.hub.terminals["lark-epalloc"]
	ep := &endpointCapture{
		cancel:    func() {},
		owner:     d.client,
		packets:   make(chan micPacket, endpointRingSlots),
		free:      free,
		slots:     new(endpointRing),
		listening: make(chan struct{}),
		deadline:  time.NewTimer(time.Minute),
	}
	d.recording.phase = captureOpen
	d.recording.endpoint = ep
	d.client.activeCapture.Store(ep)
	ssrc := d.client.native.ssrc
	b.hub.mu.Unlock()
	defer d.client.activeCapture.Store(nil)

	remote := netip.MustParseAddrPort("127.0.0.1:40000")
	pkt := make([]byte, rtpPacketBytes)
	encodeRTPPacket(pkt, rtpPTRecPCM, 1, 0, ssrc, make([]byte, rtpPCMBytes))

	allocs := testing.AllocsPerRun(50, func() {
		ds.handleUDP(pkt, remote)
		p := <-ep.packets
		ep.free <- p.slot
	})
	if allocs > 0 {
		t.Fatalf("endpoint handleUDP allocated %.2f times per packet, want 0", allocs)
	}
}

func BenchmarkNativeUDPHandle(b *testing.B) {
	h := newTerminalHub(&larkServer{root: b.Context()}, false)
	defer h.shutdown()
	ds := &terminalServer{hub: h, peers: make(map[uint32]*nativeClient)}
	remote := netip.MustParseAddrPort("127.0.0.1:40000")
	const ssrc = uint32(0x12345678)
	nc := &nativeClient{
		ssrc:     ssrc,
		remoteIP: remote.Addr(),
		udpAddr:  net.UDPAddrFromAddrPort(remote),
	}
	d := &terminalSession{
		key:       "lark-udpbench",
		recording: deviceCapture{phase: captureOpen, started: time.Now(), pcm: make([]byte, 0, maxPCM)},
		client:    &terminalClient{native: nc, ctx: b.Context()},
	}
	d.client.terminal = d
	nc.client = d.client
	h.terminals["lark-udpbench"] = d
	ds.peers[ssrc] = nc

	pkt := make([]byte, rtpPacketBytes)
	encodeRTPPacket(pkt, rtpPTRecPCM, 1, 0, ssrc, make([]byte, rtpPCMBytes))

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		d.recording.pcm = d.recording.pcm[:0]
		ds.handleUDP(pkt, remote)
	}
}
