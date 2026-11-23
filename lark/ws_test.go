package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func (s *testWS) helloV2(t *testing.T, id, cat string) wsFrame {
	t.Helper()
	return s.helloHardware(t, id, cat, capBattery|capDrag)
}

var (
	testTermMu    sync.Mutex
	testTermAddrs = map[string]string{}
)

type testWS struct {
	conn    net.Conn
	in      *bufio.Reader
	timeout time.Duration
}

func apiObjects(t *testing.T, url, field string) []any {
	t.Helper()
	return testObject(t, testRequest(t, http.MethodGet, url, "unit", nil, 200))[field].([]any)
}

func testConnectWS(t *testing.T, base, _ string, _ bool) *testWS {
	t.Helper()
	testTermMu.Lock()
	addr := testTermAddrs[base]
	testTermMu.Unlock()
	if addr == "" {
		t.Fatalf("no terminal server for %s", base)
	}
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal("cannot connect to terminal test address:", err)
	}
	t.Cleanup(func() { conn.Close() })
	return &testWS{conn: conn, in: bufio.NewReader(conn)}
}

func (s *testWS) write(t *testing.T, data []byte) {
	t.Helper()
	_ = s.conn.SetWriteDeadline(time.Now().Add(4 * time.Second))
	if _, err := s.conn.Write(data); err != nil {
		t.Fatal("cannot send terminal packet:", err)
	}
}

func (s *testWS) sendProto(t *testing.T, msgType uint8, payload []byte) {
	t.Helper()
	s.write(t, packProtoPacket(msgType, 0, payload))
}

func (s *testWS) sendRTP(t *testing.T, pt uint8, seq uint16, ts, ssrc uint32, pcm []byte) {
	t.Helper()
	pkt := make([]byte, rtpHeaderSize+len(pcm))
	n := encodeRTPPacket(pkt, pt, seq, ts, ssrc, pcm)
	s.sendProto(t, msgMedia, pkt[:n])
}

func (s *testWS) tap(t *testing.T, x, y int, rev uint16) {
	t.Helper()
	s.sendProto(t, msgPointer, encodePointer(ptrTap, uint16(x), uint16(y), rev))
}

func (s *testWS) drag(t *testing.T, x, y int, rev uint16) {
	t.Helper()
	s.sendProto(t, msgPointer, encodePointer(ptrDrag, uint16(x), uint16(y), rev))
}

func (s *testWS) up(t *testing.T, x, y int, rev uint16) {
	t.Helper()
	s.sendProto(t, msgPointer, encodePointer(ptrUp, uint16(x), uint16(y), rev))
}

func (s *testWS) tapMenuSpec(t *testing.T, spec pixelDisplaySpec) {
	t.Helper()
	btn := physicalRect(pixelHomeMenuButtonRect(spec), spec.artScale)
	s.tap(t, (btn.Min.X+btn.Max.X)/2, (btn.Min.Y+btn.Max.Y)/2, 0)
}

func (s *testWS) tapMenu(t *testing.T) {
	t.Helper()
	s.tapMenuSpec(t, pixelSpecVoCat)
}

func (s *testWS) recStart(t *testing.T, target string) {
	t.Helper()
	b := binary.LittleEndian.AppendUint16(nil, 1)
	b = appendStr8(b, target)
	s.sendProto(t, msgRecStart, b)
}

func (s *testWS) recEnd(t *testing.T, target string, cancelled bool) {
	t.Helper()
	b := binary.LittleEndian.AppendUint16(nil, 1)
	var c byte
	if cancelled {
		c = 1
	}
	b = append(b, c)
	b = appendStr8(b, target)
	s.sendProto(t, msgRecEnd, b)
}

func (s *testWS) barrier(t *testing.T) {
	t.Helper()
	s.sendProto(t, msgPing, []byte{1, 2, 3, 4})
	s.until(t, hasProto(msgPong, ""))
}

func (s *testWS) read(t *testing.T) wsFrame {
	t.Helper()
	_ = s.conn.SetReadDeadline(time.Now().Add(max(5*time.Second, s.timeout)))
	var hdr [protoHeaderSize]byte
	if _, err := io.ReadFull(s.in, hdr[:]); err != nil {
		t.Fatal("cannot read terminal packet:", err)
	}
	if hdr[0] != protoMagic {
		t.Fatalf("invalid terminal header: %x", hdr)
	}
	payloadLen := binary.LittleEndian.Uint32(hdr[4:8])
	if payloadLen > protoMaxPayload {
		t.Fatalf("invalid terminal header: %x", hdr)
	}
	pkt := make([]byte, protoHeaderSize+int(payloadLen))
	copy(pkt[:protoHeaderSize], hdr[:])
	if _, err := io.ReadFull(s.in, pkt[protoHeaderSize:]); err != nil {
		t.Fatal("cannot read terminal payload:", err)
	}
	return wsFrame{wsBinary, pkt}
}

func (s *testWS) until(t *testing.T, match func(wsFrame) bool) wsFrame {
	t.Helper()
	deadline := time.Now().Add(max(6*time.Second, s.timeout))
	for range 256 {
		if time.Now().After(deadline) {
			break
		}
		f := s.read(t)
		if match(f) {
			return f
		}
		mt, _, payload, ok := unpackProtoPacket(f.data)
		if ok && mt == msgPing && len(payload) == 4 {
			s.sendProto(t, msgPong, payload)
		}
	}
	t.Fatal("wanted operation not found in terminal frames")
	return wsFrame{}
}

func hasProto(msgType uint8, sample string) func(wsFrame) bool {
	return func(f wsFrame) bool {
		if f.op != wsBinary {
			return false
		}
		mt, _, payload, ok := unpackProtoPacket(f.data)
		return ok && mt == msgType && (sample == "" || bytes.Contains(payload, []byte(sample)))
	}
}

func waitSVG(t *testing.T, h *wsHub, termID, sample string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		d := h.terminals[strings.ToLower(termID)]
		if d != nil && strings.Contains(d.svg.source, sample) {
			got := d.svg.source
			h.mu.Unlock()
			return got
		}
		h.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q on %s", sample, termID)
	return ""
}

func isPlayback(f wsFrame) bool {
	if f.op != wsBinary {
		return false
	}
	mt, _, payload, ok := unpackProtoPacket(f.data)
	return ok && mt == msgTTSPCM && len(payload) > 0
}

func isDisplay(f wsFrame) bool {
	if f.op != wsBinary {
		return false
	}
	mt, _, payload, ok := unpackProtoPacket(f.data)
	return ok && mt == msgFrame && len(payload) > 0
}

func framePayload(f wsFrame) []byte {
	_, _, payload, _ := unpackProtoPacket(f.data)
	return payload
}

func (s *testWS) helloHardware(t *testing.T, id, compID string, caps uint32) wsFrame {
	t.Helper()
	s.sendProto(t, msgHello, encodeTerminalHello(terminalHello{
		protoVersion:   protoVersion1,
		kind:           termKindHardware,
		capsMask:       caps,
		displayFormats: dispRGB565,
		frameCodecs:    codecBitAll,
		width:          360,
		height:         360,
		shape:          shapeRound,
		terminalID:     id,
		companionID:    compID,
		board:          "esp-vocat",
		firmware:       "0.12.0",
		token:          "unit",
	}))
	s.until(t, hasProto(msgWelcome, ""))
	return s.until(t, isDisplay)
}

func (s *testWS) hello(t *testing.T, id, compID string) wsFrame {
	t.Helper()
	return s.helloHardware(t, id, compID, capBattery|capDrag)
}

func wsHarness(t *testing.T, setup func(*larkServer, string)) (*larkServer, string) {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	db := testDB(t, filepath.Join(dir, "lark.db"))
	b := &larkServer{root: ctx, token: "unit", db: db, terminals: terminalStore(db), history: &chatHistory{db: db},
		library: filepath.Join(dir, "library")}
	if setup != nil {
		setup(b, dir)
	}
	b.ws = newWSHub(b, false)
	termSrv, err := newTerminalServer(b.ws, "127.0.0.1", "0")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(b)
	testTermMu.Lock()
	testTermAddrs[srv.URL] = termSrv.tcpLn.Addr().String()
	testTermMu.Unlock()
	t.Cleanup(func() {
		testTermMu.Lock()
		delete(testTermAddrs, srv.URL)
		testTermMu.Unlock()
		termSrv.shutdown()
		b.ws.shutdown()
		cancel()
		srv.Close()
	})
	return b, srv.URL
}

func TestWSHelloRejectsBadVersionAndPersistsHardware(t *testing.T) {
	b, base := wsHarness(t, nil)
	dev := testConnectWS(t, base, "unit", false)
	const id = "box3-hello"
	dev.sendProto(t, msgHello, encodeTerminalHello(terminalHello{
		protoVersion: 0, kind: termKindHardware, displayFormats: dispRGB565,
		width: 320, height: 240, shape: shapeRect, terminalID: id, board: "esp-box-3", token: "unit",
	}))
	rej := dev.until(t, hasProto(msgReject, ""))
	if framePayload(rej)[0] != rejectBadVersion {
		t.Fatalf("expected rejectBadVersion, got %x", framePayload(rej))
	}
	if list := apiObjects(t, base+"/api/terminals", "terminals"); len(list) != 0 {
		t.Fatal("bad version registered a terminal", list)
	}
	dev = testConnectWS(t, base, "unit", false)
	dev.sendProto(t, msgHello, encodeTerminalHello(terminalHello{
		protoVersion: protoVersion1, kind: termKindHardware, displayFormats: dispRGB565, frameCodecs: codecBitAll,
		width: 320, height: 240, shape: shapeRect, terminalID: id, board: "esp-box-3", firmware: "0.12.0", token: "unit",
	}))
	dev.until(t, hasProto(msgWelcome, ""))
	frame, _, _ := decodeDisplay(t, dev.until(t, isDisplay))
	if frame.Dx() != 320 || frame.Dy() != 240 {
		t.Fatal("unexpected initial framebuffer", frame)
	}
	dev.conn.Close()
	b.ws.sig.wait(t, func() bool {
		list := apiObjects(t, base+"/api/terminals", "terminals")
		return len(list) == 1 && list[0].(object)["ws_connected"] == false
	})
	p, ok, err := terminalStore(b.db).get(id)
	if err != nil || !ok || p.Kind != "hardware" || p.IP != "127.0.0.1" || p.Port != 80 || p.LastSeen <= 0 {
		t.Fatalf("incorrect remembered identity: %+v (%v)", p, err)
	}
}

func TestWSRecordingLimits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pcm     []byte
		timeout bool
	}{
		{"timeout with PCM", make([]byte, 640), true},
		{"timeout without PCM", nil, true},
		{"exact PCM cap", bytes.Repeat([]byte{12, 0}, maxPCM/2), false},
		{"cross PCM cap", append(bytes.Repeat([]byte{12, 0}, (maxPCM-2)/2), 15, 0, 16, 0), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pcm := tc.pcm[:min(len(tc.pcm), maxPCM)]
			sttCalled := make(chan struct{}, 1)
			b, base := wsHarness(t, func(b *larkServer, dir string) {
				b.stt = func(_ context.Context, got []byte) (string, error) {
					if !bytes.Equal(got, pcm) {
						t.Errorf("STT received %d bytes, want %d with the captured PCM intact", len(got), len(pcm))
					}
					select {
					case sttCalled <- struct{}{}:
					default:
						t.Error("STT ran more than once for one recording")
					}
					return "", nil
				}
			})
			const id = "vocat-e001"
			if _, err := b.createCompanion(t.Context(), object{"id": "cake", "name": "Cake", "form": "a cat"}); err != nil {
				t.Fatal(err)
			}
			dev := testConnectWS(t, base, "unit", false)
			dev.hello(t, id, "cake")
			dev.recStart(t, "")
			dev.until(t, func(f wsFrame) bool {
				p := framePayload(f)
				return hasProto(msgStatus, "")(f) && len(p) >= 5 && p[4] == statusListening
			})
			var seq uint16
			for chunk := range slices.Chunk(tc.pcm, pcmPacket) {
				seq++
				dev.sendRTP(t, rtpPTRecPCM, seq, uint32(seq)*320, 1, chunk)
			}
			if tc.timeout {
				// Wait for the server to consume the PCM before aging the recording.
				dev.barrier(t)
				b.ws.mu.Lock()
				b.ws.terminals[id].recording.started = time.Now().Add(-91 * time.Second)
				b.ws.mu.Unlock()
			}
			dev.until(t, func(f wsFrame) bool {
				p := framePayload(f)
				return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpStop
			})
			dev.until(t, func(f wsFrame) bool {
				p := framePayload(f)
				if hasProto(msgChime, "")(f) && len(p) >= 1 && p[0] == chimeCancel {
					t.Fatal("recording limits must not play the explicit cancel chime")
				}
				return hasProto(msgStatus, "")(f) && len(p) >= 5 && (p[4] == statusDone || p[4] == statusIdle)
			})
			select {
			case <-sttCalled:
				if len(tc.pcm) == 0 {
					t.Fatal("transcribed an empty recording")
				}
			default:
				if len(tc.pcm) != 0 {
					t.Fatal("captured PCM was not transcribed")
				}
			}
			b.ws.mu.Lock()
			d := b.ws.terminals[id]
			cleared := !d.recording.active() && len(d.recording.pcm) == 0 && !d.busy && d.view == nil
			b.ws.mu.Unlock()
			if !cleared {
				t.Fatal("an empty transcript must leave the device idle with no error card")
			}
		})
	}
}

func TestWSHardwareSendWaitsForFinalPCM(t *testing.T) {
	for _, recEnd := range []bool{true, false} {
		name := "rec_end"
		if !recEnd {
			name = "fail-safe"
		}
		t.Run(name, func(t *testing.T) {
			const id = "lark-dma"
			const samples = sampleRate * 80 / 1000
			first := bytes.Repeat([]byte{12, 0}, samples)
			tail := bytes.Repeat([]byte{23, 0}, samples)
			pcm := first
			if recEnd {
				pcm = slices.Concat(first, tail)
			}
			clips := make(chan []byte, 2)
			b, base := wsHarness(t, func(b *larkServer, dir string) {
				b.stt = func(_ context.Context, got []byte) (string, error) {
					clips <- slices.Clone(got)
					return "", nil
				}
			})
			dev := testConnectWS(t, base, "unit", false)
			dev.hello(t, id, "Cake")
			dev.recStart(t, "")
			waitSVG(t, b.ws, id, "rec-send")
			dev.sendRTP(t, rtpPTRecPCM, 1, 320, 1, first)
			dev.tap(t, 247, 290, 0)
			dev.until(t, func(f wsFrame) bool {
				p := framePayload(f)
				return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpStop
			})
			b.ws.mu.Lock()
			d := b.ws.terminals[id]
			stopping := d.recording.phase == captureStoppingOpen && d.recording.stopTimer != nil && bytes.Equal(d.recording.pcm, first) && !d.busy
			b.ws.mu.Unlock()
			if !stopping {
				t.Fatal("Send finalized before it could receive the final DMA packet")
			}
			if recEnd {
				dev.sendRTP(t, rtpPTRecPCM, 2, 640, 1, tail)
				dev.recEnd(t, "", false)
			}
			select {
			case got := <-clips:
				if !bytes.Equal(got, pcm) {
					t.Fatalf("STT got %d bytes, want %d with intact PCM", len(got), len(pcm))
				}
			case <-time.After(3 * time.Second):
				t.Fatal("recording was not finalized")
			}
			b.ws.mu.Lock()
			cleared := !d.recording.active() && d.recording.stopTimer == nil && len(d.recording.pcm) == 0
			b.ws.mu.Unlock()
			if !cleared {
				t.Fatal("recording did not clear its stop timer and PCM")
			}
		})
	}
}

func TestWSHardwareSendFailsafeDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := &larkServer{root: t.Context()}
		c := &wsClient{kind: "hardware", ctx: t.Context(), out: make(chan wsFrame, 8)}
		d := &wsTerminal{client: c, key: "vocat-noac", id: "vocat-noac", kind: "hardware", screen: roundScreen, companion: blankCompanion()}
		d.recording.start(time.Now(), "", false)
		h := &wsHub{b: b, terminals: map[string]*wsTerminal{d.key: d}}
		h.mu.Lock()
		h.recordingControl(d, time.Now(), false)
		h.mu.Unlock()
		time.Sleep(449 * time.Millisecond)
		synctest.Wait()
		h.mu.Lock()
		waiting := d.recording.phase == captureStoppingOpen
		h.mu.Unlock()
		if !waiting {
			t.Fatal("fail-safe fired before 450 ms")
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		h.mu.Lock()
		done := !d.recording.active() && d.recording.stopTimer == nil
		h.mu.Unlock()
		if !done {
			t.Fatal("fail-safe did not clear recording at 450 ms")
		}
	})
}

func TestWSRawIntercomDoesNotWaitForSTT(t *testing.T) {
	allowSTT := make(chan struct{})
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.stt = func(ctx context.Context, _ []byte) (string, error) {
			select {
			case <-allowSTT:
				return "Voice caption.", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
	})
	closed := false
	defer func() {
		if !closed {
			close(allowSTT)
		}
	}()
	if _, err := b.createCompanion(t.Context(), object{"id": "mint", "name": "Mint", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	from, to := testConnectWS(t, base, "unit", false), testConnectWS(t, base, "unit", false)
	from.hello(t, "vocat-000a", "Cake")
	to.hello(t, "vocat-000b", "mint")
	pcm := bytes.Repeat([]byte{12, 0}, 120)
	from.recStart(t, "mint")
	from.sendRTP(t, rtpPTRecPCM, 1, 320, 1, pcm[:100])
	from.sendRTP(t, 99, 1, 320, 1, []byte{1, 2}) // unknown RTP payload type is ignored
	from.sendRTP(t, rtpPTRecPCM, 2, 640, 1, pcm[100:])
	from.recEnd(t, "", false)
	to.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgChime, "")(f) && len(p) >= 1 && p[0] == chimeIntercom
	})
	to.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgTTSCtrl, "")(f) && len(p) >= 1 && p[0] == ttsOpStart
	})
	got := to.until(t, isPlayback)
	if !bytes.Equal(framePayload(got), pcm) {
		t.Fatal("raw intercom did not preserve concatenated PCM frames")
	}
	end := to.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == ttsOpEnd
	})
	to.sendProto(t, msgTTSAck, encodeTTSAck(false, binary.LittleEndian.Uint64(framePayload(end)[1:9])))
	close(allowSTT)
	closed = true
	capSVG := waitSVG(t, b.ws, "vocat-000b", "Voice caption.")
	rx, ry, ok := hitCenter(capSVG, "reply")
	if !ok {
		t.Fatal("missing reply hit")
	}
	to.tap(t, rx, ry, 0)
	to.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "cake")(f) && len(p) >= 1 && p[0] == recOpStart
	})
	to.recEnd(t, "", true)
}

func TestWSLongPlaybackWithBackpressure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const duration = 12 * time.Second
		pcm := make([]byte, 12*sampleRate*2)
		for i := 0; i < len(pcm); i += 2 {
			binary.LittleEndian.PutUint16(pcm[i:], uint16(i/2))
		}
		stream := newAudioStream()
		for chunk := range slices.Chunk(pcm, 3*sampleRate*2) {
			if !stream.append(chunk) {
				t.Fatal("could not build playback stream")
			}
		}
		stream.finish(nil)

		server, client := net.Pipe()
		defer client.Close()
		defer server.Close()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		nc := &nativeClient{conn: server, out: make(chan nativeOutMsg, 12)}
		c := &wsClient{native: nc, ctx: ctx, cancel: cancel}
		nc.client = c
		dev := &testWS{conn: client, in: bufio.NewReader(client)}
		d := &wsTerminal{client: c, key: "lark-slow", id: "lark-slow", screen: roundScreen, companion: blankCompanion()}
		h := &wsHub{terminals: map[string]*wsTerminal{d.key: d}}
		go nc.writeLoop()
		start := time.Now()
		h.mu.Lock()
		h.play(d, stream)
		h.mu.Unlock()
		// Pause reads until the playback goroutine is waiting for queue space.
		synctest.Wait()
		if nc.drained == nil || c.ctx.Err() != nil {
			t.Fatal("expected live playback to wait on a full audio queue")
		}
		if !h.mu.TryLock() {
			t.Fatal("playback blocked the hub while waiting for the client")
		}
		d.vol = 42
		h.pushVolume(d, false, false)
		h.mu.Unlock()

		var got []byte
		var audio uint64
		hasAudio := false
		volume := false
		for {
			f := dev.read(t)
			mt, _, p, ok := unpackProtoPacket(f.data)
			if !ok {
				continue
			}
			switch {
			case mt == msgTTSCtrl && len(p) >= 9 && p[0] == ttsOpStart:
				if hasAudio {
					t.Fatal("unexpected second TTS start")
				}
				hasAudio = true
				audio = binary.LittleEndian.Uint64(p[1:9])
			case mt == msgTTSPCM:
				if !hasAudio || len(p) == 0 || len(p) > pcmPacket {
					t.Fatal("invalid PCM frame or PCM arrived before TTS start")
				}
				got = append(got, p...)
				time.Sleep(5 * time.Millisecond)
			case mt == msgVolume && len(p) >= 1 && p[0] == 42:
				volume = true
			case mt == msgTTSCtrl && len(p) >= 9 && p[0] == ttsOpEnd:
				if !volume || binary.LittleEndian.Uint64(p[1:9]) != audio || !bytes.Equal(got, pcm) {
					t.Fatalf("invalid TTS end: volume=%v, received %d of %d PCM bytes (must match in order)", volume, len(got), len(pcm))
				}
				if time.Since(start) < duration-maxAudioLead {
					t.Fatal("playback was sent more than eight seconds ahead")
				}
				return
			}
		}
	})
}

func TestVirtualWSModeFollowsPlaybackAck(t *testing.T) {
	b, base := wsHarness(t, nil)
	const id = "vocat-a123"
	if _, err := b.createCompanion(t.Context(), object{"id": "cake", "name": "Cake", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	phone := testConnectWS(t, base, "unit", false)
	phone.hello(t, id, "cake")
	stream := newAudioStream()
	stream.append(bytes.Repeat([]byte{12, 0}, sampleRate))
	stream.finish(nil)
	b.ws.mu.Lock()
	d := b.ws.terminals[id]
	b.ws.play(d, stream)
	b.ws.mu.Unlock()
	phone.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgStatus, "")(f) && len(p) >= 5 && p[4] == statusSpeaking
	})
	frame := phone.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == ttsOpEnd
	})
	audio := binary.LittleEndian.Uint64(framePayload(frame)[1:9])
	if audio < 1 {
		t.Fatal("missing playback ID")
	}
	phone.sendProto(t, msgTTSAck, encodeTTSAck(false, audio+1))
	phone.barrier(t)
	b.ws.mu.Lock()
	awaiting := d.awaitAck
	b.ws.mu.Unlock()
	if !awaiting {
		t.Fatal("a stale acknowledgment changed the current playback mode")
	}
	phone.sendProto(t, msgTTSAck, encodeTTSAck(false, audio))
	phone.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgStatus, "")(f) && len(p) >= 5 && (p[4] == statusDone || p[4] == statusIdle)
	})
}

func TestWSPlaybackAbortIgnoresOldACKs(t *testing.T) {
	b, base := wsHarness(t, nil)
	const id = "vocat-abrt"
	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, id, "Cake")
	play := func() uint64 {
		stream := newAudioStream()
		stream.append(bytes.Repeat([]byte{12, 0}, 800))
		stream.finish(nil)
		b.ws.mu.Lock()
		d := b.ws.terminals[id]
		b.ws.play(d, stream)
		audio := d.audio
		b.ws.mu.Unlock()
		return audio
	}
	first := play()
	dev.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == ttsOpEnd
	})
	second := play()
	if first == second {
		t.Fatal("replacement reused the aborted generation")
	}
	abort := dev.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		if hasProto(msgChime, "")(f) || (hasProto(msgTTSCtrl, "")(f) && len(p) >= 1 && (p[0] == ttsOpEnd || p[0] == ttsOpStart)) {
			t.Fatalf("abort did not precede replacement: %x", p)
		}
		return hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == ttsOpStop
	})
	if got := binary.LittleEndian.Uint64(framePayload(abort)[1:9]); got != first {
		t.Fatalf("abort used audio ID %d, want %d", got, first)
	}
	dev.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == ttsOpEnd
	})
	dev.sendProto(t, msgTTSAck, encodeTTSAck(false, first))
	dev.sendProto(t, msgTTSAck, encodeTTSAck(true, first))
	dev.barrier(t)
	awaiting := func() bool {
		b.ws.mu.Lock()
		defer b.ws.mu.Unlock()
		return b.ws.terminals[id].awaitAck
	}
	if !awaiting() {
		t.Fatal("aborted stream ACK cleared replacement playback")
	}
	dev.sendProto(t, msgTTSAck, encodeTTSAck(false, 0))
	dev.barrier(t)
	if !awaiting() {
		t.Fatal("ACK without an audio ID cleared replacement playback")
	}
	dev.sendProto(t, msgTTSAck, encodeTTSAck(false, second))
	dev.barrier(t)
	if awaiting() {
		t.Fatal("matching audio ACK did not clear replacement playback")
	}
}

func TestWSVoicePlaysFirstSentenceBeforeFinal(t *testing.T) {
	t.Run("Complete", func(t *testing.T) { wsEagerVoice(t, false) })
	t.Run("StopWhileGenerating", func(t *testing.T) { wsEagerVoice(t, true) })
}

func wsEagerVoice(t *testing.T, stopWhileGenerating bool) {
	release := make(chan struct{})
	closeRelease := syncOnceClose(release)
	defer closeRelease()
	pcm := bytes.Repeat([]byte{9, 0}, 350)
	speech := bytes.Repeat([]byte{2, 3}, 220)
	kick := make(chan struct{})
	const first = "Stars release their own light. "
	const last = "That lights nearby planets."
	const raw = first + last + "\n```lark-screen\n{\"title\":\"Night sky\",\"shapes\":[{\"t\":\"btn\",\"x\":121,\"y\":212,\"w\":118,\"h\":34,\"c\":\"#ffaa33\",\"s\":\"Fact\",\"tap\":\"Stars are bright.\"}]}\n```"
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.stt = func(_ context.Context, got []byte) (string, error) {
			if !bytes.Equal(got, pcm) {
				t.Error("STT did not receive the captured PCM")
			}
			return "What are stars?", nil
		}
		b.worker = &synthWorker{requests: make(chan ttsRequest), done: make(chan struct{})}
		b.worker.ready.Store(true)
		go func() {
			defer close(b.worker.done)
			for {
				select {
				case <-b.root.Done():
					return
				case req := <-b.worker.requests:
					req.done <- ttsResult{pcm: speech}
				}
			}
		}()
		oc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/event":
				w.Header().Set("Content-Type", "text/event-stream")
				f := w.(http.Flusher)
				_, _ = io.WriteString(w, "data: {\"type\":\"server.connected\"}\n\n")
				f.Flush()
				select {
				case <-kick:
				case <-r.Context().Done():
					return
				}
				sse := func(delta string) {
					j, _ := json.Marshal(object{"type": "session.text.delta", "data": object{"sessionID": "ses-ws", "assistantMessageID": "msg2", "ordinal": 0, "delta": delta}})
					fmt.Fprintf(w, "data: %s\n\n", j)
					f.Flush()
				}
				sse(first)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				sse(strings.TrimPrefix(raw, first))
				_, _ = io.WriteString(w, "data: {\"type\":\"session.execution.succeeded\",\"data\":{\"sessionID\":\"ses-ws\"}}\n\n")
				f.Flush()
				<-r.Context().Done()
			case "/api/session":
				_, _ = io.WriteString(w, `{"data":{"id":"ses-ws"}}`)
			case "/api/session/ses-ws/prompt":
				var prompt struct{ Text string }
				if err := json.NewDecoder(r.Body).Decode(&prompt); err != nil ||
					!strings.HasPrefix(prompt.Text, "[You are Cake (cake). Speaker: someone in person at your device (name unknown) · voice]\nWhat are stars?\n[state: now: ") {
					t.Errorf("unexpected voice prompt: %q (%v)", prompt.Text, err)
				}
				close(kick)
				_, _ = io.WriteString(w, `{"data":{"id":"msg1"}}`)
			case "/api/session/ses-ws/message":
				select {
				case <-release:
					_ = json.NewEncoder(w).Encode(object{"data": []object{
						{"id": "msg2", "type": "assistant", "finish": "stop", "content": []object{{"type": "text", "text": raw}}},
						{"id": "msg1", "type": "user"},
					}})
				case <-r.Context().Done():
				}
			default:
				w.WriteHeader(404)
			}
		}))
		t.Cleanup(oc.Close)
		path := filepath.Join(dir, "oc-api.json")
		if err := writeMap(path, object{"url": oc.URL, "password": "unit"}); err != nil {
			t.Fatal(err)
		}
		b.oc = newOpenCode(b.root, upstream{"OpenCode", pooledClient(), &fileCache[endpoint]{path: path, read: openCodeEndpoint}},
			b.db, dir, ocSettings{})
	})
	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, "vocat-a000", "Cake")
	dev.recStart(t, "")
	dev.sendRTP(t, rtpPTRecPCM, 1, 320, 1, pcm)
	dev.recEnd(t, "", false)
	start := dev.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == ttsOpStart
	})
	got := dev.until(t, isPlayback)
	if !bytes.Equal(framePayload(got), speech) {
		t.Fatal("did not receive eager Kokoro frame")
	}
	b.diag.sig.wait(t, func() bool {
		turns := b.diagnosticReport().Turns
		return len(turns) == 1 && turns[0].TTS != nil && turns[0].E2E != nil && turns[0].Total == nil
	})
	if stopWhileGenerating {
		b.ws.sig.wait(t, func() bool {
			b.ws.mu.Lock()
			defer b.ws.mu.Unlock()
			d := b.ws.terminals["vocat-a000"]
			return d != nil && strings.Contains(d.svg.source, `data-id="stop"`)
		})
		dev.tap(t, 180, 290, 0)
		abort := dev.until(t, func(f wsFrame) bool {
			p := framePayload(f)
			return hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == ttsOpStop
		})
		if binary.LittleEndian.Uint64(framePayload(abort)[1:9]) != binary.LittleEndian.Uint64(framePayload(start)[1:9]) {
			t.Fatal("one Stop tap must abort the audio that is playing")
		}
		entries, err := b.history.list(t.Context(), "cake")
		if err != nil || len(entries) != 1 {
			t.Fatal("Stop during eager playback must not commit a reply", err, entries)
		}
		return
	}
	closeRelease()
	got = dev.until(t, isPlayback)
	if len(framePayload(got)) != sampleRate/5+len(speech) || !bytes.HasSuffix(framePayload(got), speech) {
		t.Fatal("did not receive tail sentence")
	}
	end := dev.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == ttsOpEnd
	})
	dev.sendProto(t, msgTTSAck, encodeTTSAck(false, binary.LittleEndian.Uint64(framePayload(end)[1:9])))
	dev.barrier(t)
	b.ws.mu.Lock()
	d := b.ws.terminals["vocat-a000"]
	settled := !d.busy && !d.awaitAck && d.scene.title == ""
	b.ws.mu.Unlock()
	if !settled {
		t.Fatal("device did not settle after tts_ack")
	}
	entries, err := b.history.list(t.Context(), "cake")
	if err != nil || len(entries) != 2 || !slices.ContainsFunc(entries, func(e chatEntry) bool {
		return e.ID == "msg2" && e.Text == first+last
	}) {
		t.Fatal("assistant fence leaked to canonical history", entries, err)
	}
	var diag diagReport
	if err := json.Unmarshal(testRequest(t, "GET", base+"/api/diagnostics", "unit", nil, 200), &diag); err != nil {
		t.Fatal(err)
	}
	if len(diag.Turns) != 1 {
		t.Fatalf("one voice turn expected, got %+v", diag.Turns)
	}
	turn := diag.Turns[0]
	if turn.Terminal != "cake" || turn.Source != "voice" || turn.STT == nil || turn.First == nil ||
		turn.Total == nil || turn.TTS == nil || turn.E2E == nil || *turn.First > *turn.Total {
		t.Fatalf("voice milestones missing or out of order: %+v", turn)
	}
}

func syncOnceClose(ch chan struct{}) func() {
	closed := false
	return func() {
		if !closed {
			close(ch)
			closed = true
		}
	}
}

func TestNativeClientPrioritizesControlAndCoalescesFrames(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	nc := &nativeClient{
		conn:        server,
		ctrlOut:     make(chan nativeOutMsg, 8),
		out:         make(chan nativeOutMsg, 8),
		frameNotify: make(chan struct{}, 1),
	}
	c := &wsClient{native: nc, ctx: ctx, cancel: cancel}
	nc.client = c
	dev := &testWS{conn: client, in: bufio.NewReader(client)}

	// Enqueue frames, PCM, and control before writeLoop starts so priority order is deterministic.
	nc.enqueue(msgFrame, []byte("frame-old"))
	nc.enqueue(msgFrame, []byte("frame-new"))
	nc.enqueue(msgTTSPCM, []byte("pcm-1"))
	nc.enqueue(msgRecCtrl, []byte("rec-start"))

	go nc.writeLoop()

	var got []struct {
		mt uint8
		p  string
	}
	for range 3 {
		f := dev.read(t)
		mt, _, p, ok := unpackProtoPacket(f.data)
		if !ok {
			t.Fatal("invalid packet")
		}
		got = append(got, struct {
			mt uint8
			p  string
		}{mt, string(p)})
	}
	if got[0].mt != msgRecCtrl || got[0].p != "rec-start" {
		t.Fatalf("first packet = (%#x, %q), want msgRecCtrl", got[0].mt, got[0].p)
	}
	if got[1].mt != msgTTSPCM || got[1].p != "pcm-1" {
		t.Fatalf("second packet = (%#x, %q), want msgTTSPCM", got[1].mt, got[1].p)
	}
	if got[2].mt != msgFrame || got[2].p != "frame-new" {
		t.Fatalf("third packet = (%#x, %q), want coalesced frame-new", got[2].mt, got[2].p)
	}
}
