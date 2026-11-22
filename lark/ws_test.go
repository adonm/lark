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
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type testWS struct {
	conn    net.Conn
	in      *bufio.Reader
	timeout time.Duration
}

const testKey = "dGhlIHNhbXBsZSBub25jZQ=="
const testAccept = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="

func dialTestWS(t *testing.T, base, token string, query bool, headers object) (*testWS, *http.Response) {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal("invalid WebSocket test address")
	}
	u.Path = "/ws"
	if query {
		u.RawQuery = url.Values{"token": {token}}.Encode()
	}
	conn, err := net.DialTimeout("tcp", u.Host, 3*time.Second)
	if err != nil {
		t.Fatal("cannot connect to WebSocket test address")
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
	req := &http.Request{Method: "GET", URL: u, Host: u.Host, Header: http.Header{}}
	req.Header.Set("Connection", "keep-alive, Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", testKey)
	if !query && token != "" {
		req.Header.Set("X-Lark-Token", token)
	}
	for k, v := range headers {
		req.Header.Set(k, str(v))
	}
	if req.Write(conn) != nil {
		t.Fatal("cannot write upgrade request")
	}
	in := bufio.NewReader(conn)
	res, err := http.ReadResponse(in, req)
	if err != nil {
		t.Fatal("cannot read upgrade response")
	}
	return &testWS{conn: conn, in: in}, res
}

func testConnectWS(t *testing.T, base, token string, query bool) *testWS {
	t.Helper()
	ws, res := dialTestWS(t, base, token, query, nil)
	if res.StatusCode != 101 || res.Header.Get("Sec-WebSocket-Accept") != testAccept {
		t.Fatalf("upgrade failed: HTTP %d", res.StatusCode)
	}
	return ws
}

func maskedFrame(op byte, final bool, data []byte) []byte {
	if final {
		op |= 128
	}
	f := []byte{op}
	switch n := len(data); {
	case n < 126:
		f = append(f, 128|byte(n))
	case n <= 65535:
		f = binary.BigEndian.AppendUint16(append(f, 128|126), uint16(n))
	default:
		f = binary.BigEndian.AppendUint64(append(f, 128|127), uint64(n))
	}
	mask := []byte{0xb1, 0xf1, 0x27, 0xe6}
	f = append(f, mask...)
	for i, c := range data {
		f = append(f, c^mask[i%4])
	}
	return f
}

func (s *testWS) write(t *testing.T, data []byte) {
	t.Helper()
	_ = s.conn.SetWriteDeadline(time.Now().Add(4 * time.Second))
	if _, err := s.conn.Write(data); err != nil {
		t.Fatal("cannot send WebSocket frame:", err)
	}
}

func (s *testWS) sendProto(t *testing.T, msgType uint8, payload []byte) {
	t.Helper()
	s.write(t, maskedFrame(wsBinary, true, packProtoPacket(msgType, 0, payload)))
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

func (s *testWS) text(t *testing.T, msg string) {
	t.Helper()
	s.sendProto(t, msgTextInput, appendStr16(nil, msg))
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
	s.write(t, maskedFrame(wsPing, true, nil))
	s.until(t, func(f wsFrame) bool { return f.op == wsPong })
}

func (s *testWS) read(t *testing.T) wsFrame {
	t.Helper()
	_ = s.conn.SetReadDeadline(time.Now().Add(max(5*time.Second, s.timeout)))
	var h [8]byte
	if _, err := io.ReadFull(s.in, h[:2]); err != nil {
		t.Fatal("cannot read WebSocket frame:", err)
	}
	if h[0]&0xf0 != 0x80 || h[1]&128 != 0 {
		t.Fatal("invalid server frame header")
	}
	op, n := h[0]&15, uint64(h[1])
	if n == 126 {
		if _, err := io.ReadFull(s.in, h[:2]); err != nil {
			t.Fatal(err)
		}
		n = uint64(binary.BigEndian.Uint16(h[:2]))
	} else if n == 127 {
		if _, err := io.ReadFull(s.in, h[:]); err != nil {
			t.Fatal(err)
		}
		n = binary.BigEndian.Uint64(h[:])
	}
	if n > maxPCM+1 {
		t.Fatal("oversized server frame")
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(s.in, data); err != nil {
		t.Fatal(err)
	}
	return wsFrame{op, data}
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
		if f.op == wsPing {
			s.write(t, maskedFrame(wsPong, true, f.data))
		} else if f.op == wsClose {
			t.Fatal("unexpected close frame")
		}
	}
	t.Fatal("wanted operation not found in WebSocket frames")
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

func hasSVG(sample string) func(wsFrame) bool { return hasProto(msgFrameSVG, sample) }

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
	}))
	s.until(t, hasProto(msgWelcome, ""))
	return s.until(t, isDisplay)
}

func (s *testWS) helloWeb(t *testing.T, id, compID string, caps uint32, ifFree bool) wsFrame {
	t.Helper()
	var flags uint8
	if ifFree {
		flags |= helloFlagIfFree
	}
	s.sendProto(t, msgHello, encodeTerminalHello(terminalHello{
		protoVersion:   protoVersion1,
		kind:           termKindWeb,
		capsMask:       caps,
		displayFormats: dispSVG,
		width:          360,
		height:         360,
		shape:          shapeRound,
		flags:          flags,
		terminalID:     id,
		companionID:    compID,
		firmware:       "web",
	}))
	s.until(t, hasProto(msgWelcome, ""))
	return s.until(t, hasSVG(""))
}

func (s *testWS) hello(t *testing.T, id, compID string) wsFrame {
	t.Helper()
	if webTerminalID.MatchString(strings.ToLower(id)) {
		return s.helloWeb(t, id, compID, capDrag, false)
	}
	return s.helloHardware(t, id, compID, capBattery|capDrag)
}

func wsHarness(t *testing.T, setup func(*larkServer, string)) (*larkServer, string) {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	db := testDB(t, filepath.Join(dir, "lark.db"))
	b := &larkServer{root: ctx, token: "unit", db: db, terminals: terminalStore(db), history: &chatHistory{db}}
	if setup != nil {
		setup(b, dir)
	}
	b.ws = newWSHub(b, false)
	srv := httptest.NewServer(b)
	t.Cleanup(func() {
		b.ws.shutdown()
		cancel()
		srv.Close()
	})
	return b, srv.URL
}

func TestWSHandshakeAndRFC6455(t *testing.T) {
	b, base := wsHarness(t, nil)
	for _, c := range []struct {
		token string
		query bool
		h     object
		want  int
	}{
		{"", false, nil, 401},
		{"wrong", true, nil, 401},
		{"unit", true, object{"Origin": "http://example.test"}, 403},
		{"unit", true, object{"Sec-WebSocket-Version": "12"}, 426},
		{"unit", false, object{"Sec-WebSocket-Key": "dGhpcw=="}, 400},
	} {
		_, res := dialTestWS(t, base, c.token, c.query, c.h)
		if res.StatusCode != c.want || c.want == 426 && res.Header.Get("Sec-WebSocket-Version") != "13" {
			t.Fatalf("handshake HTTP %d, wanted %d", res.StatusCode, c.want)
		}
		res.Body.Close()
	}
	c := testConnectWS(t, base, "unit", false)
	helloPkt := packProtoPacket(msgHello, 0, encodeTerminalHello(terminalHello{protoVersion: protoVersion1, kind: termKindWeb, displayFormats: dispSVG}))
	c.write(t, slices.Concat(maskedFrame(wsBinary, false, helloPkt[:6]),
		maskedFrame(wsPing, true, []byte("hi")), maskedFrame(0, true, helloPkt[6:])))
	pong := c.read(t)
	if pong.op != wsPong || string(pong.data) != "hi" {
		t.Fatal("ping was not echoed during fragmentation")
	}
	c.until(t, hasProto(msgWelcome, ""))
	c.until(t, hasProto(msgPeers, ""))
	reply := wsCloseFrame(1000)
	c.write(t, maskedFrame(wsClose, true, reply.data))
	for {
		got := c.read(t)
		if got.op == wsClose {
			if !bytes.Equal(got.data, reply.data) {
				t.Fatal("close not echoed")
			}
			break
		}
	}
	for _, tt := range []struct {
		name string
		raw  []byte
		code uint16
	}{
		{"unmasked", []byte{0x81, 0}, 1002},
		{"reserved bit", []byte{0xc1, 0x80}, 1002},
		{"orphan continuation", maskedFrame(0, true, nil), 1002},
		{"fragmented ping", maskedFrame(wsPing, false, []byte("hi")), 1002},
		{"nonminimal length", []byte{0x81, 0xfe, 0, 1}, 1002},
		{"invalid UTF-8", maskedFrame(wsText, true, []byte{0xff}), 1007},
		{"invalid close code", maskedFrame(wsClose, true, []byte{3, 237}), 1002},
		{"oversize", maskedFrame(wsBinary, true, make([]byte, wsLimit+1)), 1009},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConnectWS(t, base, "unit", true)
			c.write(t, tt.raw)
			got := c.read(t)
			if got.op != wsClose || len(got.data) != 2 || binary.BigEndian.Uint16(got.data) != tt.code {
				t.Fatalf("bad close frame: %+v", got)
			}
		})
	}
	shutdown := testConnectWS(t, base, "unit", true)
	b.ws.shutdown()
	if got := shutdown.read(t); got.op != wsClose || !bytes.Equal(got.data, wsCloseFrame(1001).data) {
		t.Fatalf("hub did not send a going-away close: %+v", got)
	}
}

func (s *testWS) watch(t *testing.T, id string) wsFrame {
	t.Helper()
	s.sendProto(t, msgHello, encodeTerminalHello(terminalHello{protoVersion: protoVersion1, kind: termKindWeb, displayFormats: dispSVG}))
	s.until(t, hasProto(msgPeers, id))
	s.sendProto(t, msgWatch, appendStr8(nil, id))
	return s.until(t, hasSVG("companion-ring"))
}

func TestWSHelloRejectsBadVersionAndPersistsHardware(t *testing.T) {
	b, base := wsHarness(t, nil)
	dev := testConnectWS(t, base, "unit", false)
	const id = "box3-hello"
	dev.sendProto(t, msgHello, encodeTerminalHello(terminalHello{
		protoVersion: 0, kind: termKindHardware, displayFormats: dispRGB565,
		width: 320, height: 240, shape: shapeRect, terminalID: id, board: "esp-box-3",
	}))
	rej := dev.until(t, hasProto(msgReject, ""))
	if framePayload(rej)[0] != rejectBadVersion {
		t.Fatalf("expected rejectBadVersion, got %x", framePayload(rej))
	}
	if list := apiObjects(t, base+"/api/terminals", "terminals"); len(list) != 0 {
		t.Fatal("bad version registered a terminal", list)
	}
	dev.sendProto(t, msgHello, encodeTerminalHello(terminalHello{
		protoVersion: protoVersion1, kind: termKindHardware, displayFormats: dispRGB565, frameCodecs: codecBitAll,
		width: 320, height: 240, shape: shapeRect, terminalID: id, board: "esp-box-3", firmware: "0.12.0",
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
	web := testConnectWS(t, base, "unit", true)
	web.hello(t, "web-12345678", "")
	if p2, exists, err := terminalStore(b.db).get("web-12345678"); !exists || err != nil || p2.Kind != "web" {
		t.Fatal("web hello did not register a web terminal", err)
	}
}

func TestWSMirrorAndControls(t *testing.T) {
	_, base := wsHarness(t, nil)
	const id = "vocat-mir0"
	dev := testConnectWS(t, base, "unit", false)
	_, rev, _ := decodeDisplay(t, dev.helloHardware(t, id, "", capBattery|capDrag))
	web := testConnectWS(t, base, "unit", true)
	first := web.watch(t, id)
	svgRev, _ := decodeFrameSVG(first)
	if svgRev != uint32(rev) || !hasSVG("120,292,120,48")(first) {
		t.Fatal("watch did not receive current frame with centered Menu control")
	}
	var st [16]byte
	rssi, batt := int16(-45), int16(25)
	binary.LittleEndian.PutUint16(st[0:2], uint16(rssi))
	binary.LittleEndian.PutUint16(st[2:4], uint16(batt))
	dev.sendProto(t, msgTelemetry, st[:])
	web.until(t, hasSVG(menuSuccess))
	web.tap(t, 180, 300, 0)
	web.until(t, hasSVG("menu-next"))
	web.tap(t, 248, 294, 0)
	if f := web.until(t, hasSVG("menu-stats")); !hasSVG("menu-voice")(f) {
		t.Fatal("menu did not mirror voice controls")
	}
	testRequest(t, "POST", base+"/api/terminals/"+id+"/volume", "unit", object{"auto": false, "volume": 62}, 200)
	for _, c := range []*testWS{dev, web} {
		c.until(t, func(f wsFrame) bool {
			p := framePayload(f)
			return hasProto(msgVolume, "")(f) && len(p) >= 1 && p[0] == 62
		})
	}
	web.tap(t, 180, 294, 0)
	web.until(t, hasSVG("companion-ring"))
	web.tap(t, 180, 180, 0)
	dev.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpStart
	})
	dev.recEnd(t, "", true)
	dev.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgChime, "")(f) && len(p) >= 1 && p[0] == chimeCancel
	})
}

func TestWSWebOnlyDisplayAndIdenticalAppRender(t *testing.T) {
	b, base := wsHarness(t, nil)
	const id = "web-ab120000"
	if _, err := b.createCompanion(t.Context(), object{"id": "cake", "name": "Cake"}); err != nil {
		t.Fatal(err)
	}
	web := testConnectWS(t, base, "unit", true)
	web.helloWeb(t, id, "cake", capDrag, false)
	b.ws.mu.Lock()
	d := b.ws.terminals[id]
	native := d.svg.fb != nil
	b.ws.mu.Unlock()
	if native {
		t.Fatal("native framebuffer allocated for a browser terminal")
	}
	const code = `function render() {
		return '<svg><circle data-id="app-dot" cx="180" cy="180" r="35" fill="#3579da"/></svg>';
	} function onTap(s,p,id) { if (id === "app-dot") chime("tap"); }`
	toolTest(t, base, "app", object{"companions": "cake", "code": code}, false)
	web.until(t, func(f wsFrame) bool {
		if isDisplay(f) {
			t.Fatal("web-only app received RGB565")
		}
		return hasSVG("app-dot")(f)
	})
	b.ws.mu.Lock()
	native = d.svg.fb != nil || d.appView.fb != nil
	hit := hitID(d.svg.hits, 180, 180)
	rev, gen := d.rev, d.renderGen
	b.ws.mu.Unlock()
	if native || hit != "app-dot" {
		t.Fatal("web-only app needs hitboxes but no native framebuffer", hit)
	}
	web.tap(t, 180, 180, 0)
	web.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgChime, "")(f) && len(p) >= 1 && p[0] == chimeTap
	})
	b.ws.mu.Lock()
	unchanged := d.rev == rev && d.renderGen == gen
	b.ws.mu.Unlock()
	if !unchanged {
		t.Fatal("identical app SVG scheduled a new frame")
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
			const id = "web-a1230000"
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
			if _, err := b.createCompanion(t.Context(), object{"id": "cake", "name": "Cake"}); err != nil {
				t.Fatal(err)
			}
			dev := testConnectWS(t, base, "unit", false)
			dev.helloWeb(t, id, "cake", capDrag, false)
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
			dev, web := testConnectWS(t, base, "unit", false), testConnectWS(t, base, "unit", true)
			dev.hello(t, id, "Cake")
			web.watch(t, id)
			dev.recStart(t, "")
			web.until(t, hasSVG("rec-send"))
			dev.sendRTP(t, rtpPTRecPCM, 1, 320, 1, first)
			dev.tap(t, 247, 310, 0)
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
	if _, err := b.createCompanion(t.Context(), object{"id": "mint", "name": "Mint"}); err != nil {
		t.Fatal(err)
	}
	from, to := testConnectWS(t, base, "unit", false), testConnectWS(t, base, "unit", true)
	from.hello(t, "vocat-000a", "Cake")
	to.helloWeb(t, "web-000b0000", "mint", capDrag, false)
	pcm := bytes.Repeat([]byte{12, 0}, 120)
	from.recStart(t, "mint")
	from.sendRTP(t, rtpPTRecPCM, 1, 320, 1, pcm[:100])
	from.sendRTP(t, rtpPTCallUp, 1, 320, 1, []byte{1, 2}) // wrong audio direction is ignored
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
	captionFrame := to.until(t, hasSVG("Voice caption."))
	_, capSVG := decodeFrameSVG(captionFrame)
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
		c := &wsClient{wire: &websocket{conn: server}, ctx: ctx, cancel: cancel, out: make(chan wsFrame, 12)}
		dev := &testWS{conn: client, in: bufio.NewReader(client)}
		d := &wsTerminal{client: c, key: "lark-slow", id: "lark-slow", screen: roundScreen, companion: blankCompanion()}
		h := &wsHub{terminals: map[string]*wsTerminal{d.key: d}}
		go c.write()
		start := time.Now()
		h.mu.Lock()
		h.play(d, stream)
		h.mu.Unlock()
		// Pause reads until the playback goroutine is waiting for queue space.
		synctest.Wait()
		if c.drained == nil || c.ctx.Err() != nil {
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
	const id = "web-a1230000"
	if _, err := b.createCompanion(t.Context(), object{"id": "cake", "name": "Cake"}); err != nil {
		t.Fatal(err)
	}
	phone := testConnectWS(t, base, "unit", true)
	phone.helloWeb(t, id, "cake", capDrag, false)
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
					prompt.Text != "[Cake (cake) · hardware · voice]\nWhat are stars?" {
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
	mirror := testConnectWS(t, base, "unit", true)
	mirror.watch(t, "vocat-a000")
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
		mirror.until(t, func(f wsFrame) bool {
			return hasSVG(`aria-label="Stop"`)(f) && !bytes.Contains(f.data, []byte("Thinking"))
		})
		dev.tap(t, 180, 315, 0)
		abort := dev.until(t, func(f wsFrame) bool {
			p := framePayload(f)
			return hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == ttsOpStop
		})
		if binary.LittleEndian.Uint64(framePayload(abort)[1:9]) != binary.LittleEndian.Uint64(framePayload(start)[1:9]) {
			t.Fatal("one Stop tap must abort the audio that is playing")
		}
		entries := apiObjects(t, base+"/api/companions/cake/messages", "messages")
		if len(entries) != 1 {
			t.Fatal("Stop during eager playback must not commit a reply")
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
	entries := apiObjects(t, base+"/api/companions/cake/messages", "messages")
	if len(entries) != 2 || !slices.ContainsFunc(entries, func(v any) bool {
		e := v.(object)
		return e["id"] == "msg2" && e["text"] == first+last
	}) {
		t.Fatal("assistant fence leaked to canonical history", entries)
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

func TestEmbeddedApp(t *testing.T) {
	b := &larkServer{}
	assets, err := webFiles.ReadDir("web")
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{"/", "/app", "/app/style.css"}
	for _, f := range assets {
		if strings.HasSuffix(f.Name(), ".js") && !strings.HasSuffix(f.Name(), ".test.js") {
			paths = append(paths, "/app/"+f.Name())
		}
	}
	for _, path := range paths {
		w := httptest.NewRecorder()
		b.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if path == "/" {
			if w.Code != 302 || w.Header().Get("Location") != "/app" {
				t.Fatal("missing app redirect")
			}
		} else if w.Code != 200 || w.Body.Len() == 0 || w.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("%s: missing app asset or security headers", path)
		} else if strings.HasSuffix(path, ".js") && (w.Header().Get("Content-Type") != "text/javascript; charset=utf-8" ||
			w.Header().Get("Cache-Control") != "no-store") {
			t.Fatalf("%s: wrong module headers: %v", path, w.Header())
		} else if path == "/app" && (!strings.Contains(w.Body.String(), `viewBox="0 0 360 360"`) || !strings.Contains(w.Body.String(), "New Chat") ||
			!strings.Contains(w.Body.String(), `id="web-controls"`)) {
			t.Fatal("app is missing its SVG or web controls")
		}
	}
	for _, path := range []string{"/app/mic.test.mjs", "/app/mic.test.js", "/app/missing.js", "/app/../client.js", "/app/%2e%2e/client.js", "/app/%2flive.js"} {
		w := httptest.NewRecorder()
		b.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Errorf("%s = HTTP %d, want 404", path, w.Code)
		}
	}
}
