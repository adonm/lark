package edge

import (
	"bufio"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lark/internal/audio"
	proto "lark/internal/proto"
)

type fakeSTT struct {
	text  string
	delay time.Duration
	calls atomic.Int32
}

func (f *fakeSTT) Transcribe(ctx context.Context, _ []byte) (string, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return f.text, nil
}
func (f *fakeSTT) Close() {}

type fakeTTS struct{}

func (f *fakeTTS) Generate(string, audio.Voice, func() bool) ([]float32, int) {
	raw := make([]float32, 1600)
	for i := range raw {
		raw[i] = 0.2
	}
	return raw, audio.SampleRate
}
func (f *fakeTTS) Close() {}

type fakeTurn struct{}

func (f *fakeTurn) PredictPCM16(pcm []byte) (float32, error) {
	if len(pcm) >= audio.SampleRate {
		return 0.99, nil
	}
	return 0.1, nil
}
func (f *fakeTurn) Close() {}

type fakeVAD struct{}

func (f *fakeVAD) Detect(pcm []byte) bool { return frameRMS(pcm) > 500 }
func (f *fakeVAD) Close()                 {}

func startTestEdge(t *testing.T) *Edge {
	t.Helper()
	sockDir := t.TempDir()
	e, err := New(Config{
		Addr: "127.0.0.1:0", Sock: filepath.Join(sockDir, "edge.sock"),
		Token: "test-token-123", DataDir: t.TempDir(),
		DisableModels: true,
		Transcriber:   &fakeSTT{text: "hello there"},
		Synthesizer:   &fakeTTS{},
		TurnDetector:  &fakeTurn{},
		VADFactory:    func() VAD { return &fakeVAD{} },
		Logf:          t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.Start()
	t.Cleanup(e.Close)
	return e
}

type brain struct {
	t    *testing.T
	conn net.Conn
	dec  *json.Decoder
	mu   sync.Mutex
	seen []map[string]any
}

func dialBrain(t *testing.T, e *Edge) *brain {
	t.Helper()
	conn, err := net.Dial("unix", e.SockPath())
	if err != nil {
		t.Fatal(err)
	}
	b := &brain{t: t, conn: conn, dec: json.NewDecoder(bufio.NewReader(conn))}
	t.Cleanup(func() { conn.Close() })
	return b
}

func (b *brain) send(cmd map[string]any) {
	b.t.Helper()
	raw, err := json.Marshal(cmd)
	if err != nil {
		b.t.Fatal(err)
	}
	raw = append(raw, '\n')
	if _, err := b.conn.Write(raw); err != nil {
		b.t.Fatal(err)
	}
}

func (b *brain) next() map[string]any {
	b.t.Helper()
	_ = b.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var msg map[string]any
	if err := b.dec.Decode(&msg); err != nil {
		b.t.Fatalf("brain read: %v", err)
	}
	b.mu.Lock()
	b.seen = append(b.seen, msg)
	b.mu.Unlock()
	return msg
}

func (b *brain) waitFor(kind string) map[string]any {
	b.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msg := b.next()
		if msg["type"] == kind {
			return msg
		}
	}
	b.t.Fatalf("timed out waiting for %s", kind)
	return nil
}

func (b *brain) rpc(id string, fields map[string]any) map[string]any {
	b.t.Helper()
	fields["id"] = id
	b.send(fields)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msg := b.next()
		if msg["id"] == id {
			return msg
		}
	}
	b.t.Fatalf("timed out waiting for rpc %s", id)
	return nil
}

type hardware struct {
	t     *testing.T
	conn  net.Conn
	br    *bufio.Reader
	ssrc  uint32
	seq   uint16
	port  uint16
	addr  *net.UDPAddr
	token [proto.ResumeTokenLen]byte
}

func dialHardware(t *testing.T, e *Edge, id string) *hardware {
	t.Helper()
	conn, err := net.Dial("tcp", e.TCPAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	hello := proto.TerminalHello{
		ProtoVersion: proto.ProtoVersion1, Kind: proto.TermKindHardware,
		CapsMask:       proto.CapBattery | proto.CapPointerRev | proto.CapEdgeEndpoint,
		DisplayFormats: proto.DispRGB565, FrameCodecs: proto.CodecBitAll,
		Width: 360, Height: 360, Shape: proto.ShapeRound, UdpPort: 40000,
		TerminalID: id, CompanionID: "cake", Board: "esp-vocat",
		Firmware: "0.21.0", Token: "test-token-123",
	}
	if _, err := conn.Write(proto.Pack(proto.MsgHello, 0, hello.Encode())); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msgType, _, payload, err := proto.ReadFrame(br)
	if err != nil {
		t.Fatal(err)
	}
	if msgType != proto.MsgWelcome {
		t.Fatalf("welcome type = 0x%02x", msgType)
	}
	w, ok := proto.DecodeProtoWelcome(payload)
	if !ok {
		t.Fatal("decode welcome")
	}
	ua, _ := net.ResolveUDPAddr("udp", e.UDPAddr().String())
	return &hardware{t: t, conn: conn, br: br, ssrc: w.Ssrc, port: w.UdpPort, addr: ua, token: w.ResumeToken}
}

func (h *hardware) close() { h.conn.Close() }

func (h *hardware) read(typ uint8) (uint8, []byte) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_ = h.conn.SetReadDeadline(deadline)
		msgType, _, payload, err := proto.ReadFrame(h.br)
		if err != nil {
			h.t.Fatalf("hardware read: %v", err)
		}
		if msgType == proto.MsgPing {
			// Echo pings so the server's liveness view stays clean.
			_, _ = h.conn.Write(proto.Pack(proto.MsgPong, 0, payload))
			continue
		}
		if typ == 0 || msgType == typ {
			return msgType, payload
		}
	}
	h.t.Fatalf("timed out waiting for message 0x%02x", typ)
	return 0, nil
}

func (h *hardware) tap(x, y int) {
	h.t.Helper()
	m := proto.ProtoPointer{Kind: proto.PtrTap, X: uint16(x), Y: uint16(y)}
	if _, err := h.conn.Write(proto.Pack(proto.MsgPointer, 0, m.Encode())); err != nil {
		h.t.Fatal(err)
	}
}

func (h *hardware) rtp(pcm []byte) {
	h.t.Helper()
	conn, err := net.DialUDP("udp", nil, h.addr)
	if err != nil {
		h.t.Fatal(err)
	}
	defer conn.Close()
	for len(pcm) > 0 {
		n := min(proto.RtpPCMBytes, len(pcm))
		pkt := make([]byte, proto.RtpHeaderSize+n)
		pkt[0] = proto.RtpVersionByte
		pkt[1] = proto.RtpPTRecPCM
		h.seq++
		pkt[2] = byte(h.seq >> 8)
		pkt[3] = byte(h.seq)
		pkt[8] = byte(h.ssrc >> 24)
		pkt[9] = byte(h.ssrc >> 16)
		pkt[10] = byte(h.ssrc >> 8)
		pkt[11] = byte(h.ssrc)
		copy(pkt[proto.RtpHeaderSize:], pcm[:n])
		if _, err := conn.Write(pkt); err != nil {
			h.t.Fatal(err)
		}
		pcm = pcm[n:]
	}
}

func tone(samples int) []byte {
	pcm := make([]byte, samples*2)
	for i := range samples {
		v := int16(4000)
		if i%2 == 0 {
			v = -4000
		}
		pcm[2*i] = byte(uint16(v))
		pcm[2*i+1] = byte(uint16(v) >> 8)
	}
	return pcm
}

func TestEdgeRoundTrip(t *testing.T) {
	e := startTestEdge(t)
	b := dialBrain(t, e)
	if got := b.waitFor("ready"); got["models"] == nil {
		t.Fatal("ready missing models")
	}
	hw := dialHardware(t, e, "vocat-test")
	defer hw.close()

	ev := b.waitFor("terminal.connected")
	if ev["terminal_id"] != "vocat-test" || ev["firmware"] != "0.21.0" {
		t.Fatalf("connected event = %v", ev)
	}

	// Render home and verify a real RGB565/LZ4 frame arrives.
	b.send(map[string]any{"cmd": "terminal.render", "terminal_id": "vocat-test", "screen": map[string]any{
		"kind": "home", "status": "idle", "caption": "Hello",
	}})
	_, payload := hw.read(proto.MsgFrame)
	if len(payload) < 12 || payload[0] != 3 {
		t.Fatalf("frame payload header = %x", payload[:min(12, len(payload))])
	}
	codec := payload[9]
	if codec != 0 && codec != 1 && codec != 2 && codec != 3 {
		t.Fatalf("unknown frame codec %d", codec)
	}

	// terminal.hits exposes the rendered hit boxes.
	r := b.rpc("hits-1", map[string]any{"cmd": "terminal.hits", "terminal_id": "vocat-test"})
	hits, ok := r["hits"].([]any)
	if !ok || len(hits) == 0 {
		t.Fatalf("hits reply = %v", r)
	}
	menu := findHit(t, hits, "menu")
	listen := findHit(t, hits, "listen")

	b.send(map[string]any{"cmd": "terminal.render", "terminal_id": "vocat-test", "screen": map[string]any{
		"kind": "menu", "page": 0,
	}})
	hw.read(proto.MsgFrame)
	b.send(map[string]any{"cmd": "terminal.render", "terminal_id": "vocat-test", "screen": map[string]any{
		"kind": "home", "status": "idle", "caption": "Hello",
	}})
	hw.read(proto.MsgFrame)

	hw.tap(menu[0], menu[1])
	if ev := b.waitFor("pointer.action"); ev["action"] != "menu" {
		t.Fatalf("menu action = %v", ev)
	}
	hw.tap(listen[0], listen[1])
	if ev := b.waitFor("pointer.action"); ev["action"] != "listen" {
		t.Fatalf("listen action = %v", ev)
	}

	// Speak for 1 s, then a pause. The edge probes, speculates, then commits.
	hw.rtp(tone(audio.SampleRate))
	time.Sleep(300 * time.Millisecond)
	// 180 ms of silence lets the quiet probe finish and emit a speculation.
	silence := make([]byte, audio.SampleRate/5*2)
	hw.rtp(silence[:audio.SampleRate*2*18/100])
	time.Sleep(300 * time.Millisecond)
	hw.rtp(make([]byte, audio.SampleRate*2*30/100))

	spec := b.waitFor("turn.speculate")
	if spec["transcript"] != "hello there" {
		t.Fatalf("speculate = %v", spec)
	}
	commit := b.waitFor("turn.commit")
	if commit["transcript"] != "hello there" || commit["seq"] == nil {
		t.Fatalf("commit = %v", commit)
	}

	// TTS stream: start, chunk, end -> ctrl/pcm/ctrl -> ack -> playback.done.
	b.send(map[string]any{"cmd": "terminal.speak_start", "terminal_id": "vocat-test", "stream_id": 42, "voice": "af_heart"})
	b.send(map[string]any{"cmd": "terminal.speak_chunk", "terminal_id": "vocat-test", "stream_id": 42, "text": "hello"})
	b.send(map[string]any{"cmd": "terminal.speak_end", "terminal_id": "vocat-test", "stream_id": 42})
	_, ctrl := hw.read(proto.MsgTTSCtrl)
	c, ok := proto.DecodeProtoTTSCtrl(ctrl)
	if !ok || c.Op != proto.TtsOpStart || c.AudioID != 42 {
		t.Fatalf("tts start = %+v ok=%v", c, ok)
	}
	_, pcm := hw.read(proto.MsgTTSPCM)
	if len(pcm) == 0 {
		t.Fatal("empty TTS PCM")
	}
	_, ctrl = hw.read(proto.MsgTTSCtrl)
	if c, _ = proto.DecodeProtoTTSCtrl(ctrl); c.Op != proto.TtsOpEnd || c.AudioID != 42 {
		t.Fatalf("tts end = %+v", c)
	}
	ack := proto.ProtoTTSAck{Aborted: 0, AudioID: 42}
	if _, err := hw.conn.Write(proto.Pack(proto.MsgTTSAck, 0, ack.Encode())); err != nil {
		t.Fatal(err)
	}
	if ev := b.waitFor("playback.done"); ev["stream_id"] != float64(42) || ev["aborted"] != false {
		t.Fatalf("playback.done = %v", ev)
	}

	// Brain reconnect: hardware TCP must not drop and the snapshot repeats.
	b.conn.Close()
	b2 := dialBrain(t, e)
	if got := b2.waitFor("ready"); got["models"] == nil {
		t.Fatal("ready missing models after reconnect")
	}
	if got := b2.waitFor("terminal.connected"); got["terminal_id"] != "vocat-test" {
		t.Fatalf("snapshot = %v", got)
	}
	if _, err := hw.conn.Write(proto.Pack(proto.MsgPing, 0, []byte{1, 2, 3, 4})); err != nil {
		t.Fatal(err)
	}
	hw.read(proto.MsgPong)

	// STT RPC over a temp WAV.
	wav := filepath.Join(t.TempDir(), "note.wav")
	hdr := audio.WavHeader(len(tone(audio.SampleRate / 2)))
	raw := append(hdr[:], tone(audio.SampleRate/2)...)
	if err := os.WriteFile(wav, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	r = b2.rpc("stt-1", map[string]any{"cmd": "stt.transcribe", "path": wav})
	if r["text"] != "hello there" {
		t.Fatalf("stt reply = %v", r)
	}

	// TTS WAV helper for brain test/sim tooling.
	ttsPath := filepath.Join(t.TempDir(), "tts.wav")
	r = b2.rpc("tts-1", map[string]any{"cmd": "tts.wav", "voice": "af_heart", "text": "hello", "out_path": ttsPath})
	if r["ok"] != true {
		t.Fatalf("tts.wav reply = %v", r)
	}
	info, err := os.Stat(ttsPath)
	if err != nil {
		t.Fatalf("tts.wav file: %v", err)
	}
	if info.Size() <= 44 {
		t.Fatalf("tts.wav size = %d", info.Size())
	}

	// Portrait normalize RPC on a synthetic 1024x1024 four-pose sheet.
	dir := t.TempDir()
	writePortraitSheet(t, filepath.Join(dir, "sheet.png"))
	r = b2.rpc("portrait-1", map[string]any{"cmd": "portrait.normalize", "dir": dir})
	if r["ok"] != true || r["colors"].(float64) == 0 {
		t.Fatalf("portrait reply = %v", r)
	}
}

// TestTrailingSilenceProbesOnce pins the silence-storm and premature-cancel
// regressions: a finished speculation must not be cancelled by silence, and
// trailing silence must not re-probe Parakeet at the packet rate.
func TestTrailingSilenceProbesOnce(t *testing.T) {
	e := startTestEdge(t)
	stt := e.cfg.Transcriber.(*fakeSTT)
	stt.delay = 120 * time.Millisecond
	b := dialBrain(t, e)
	b.waitFor("ready")
	hw := dialHardware(t, e, "vocat-silence")
	defer hw.close()
	b.waitFor("terminal.connected")
	b.rpc("rec-1", map[string]any{"cmd": "terminal.rec_start", "terminal_id": "vocat-silence"})

	// 950 ms of speech leaves the last probe trailing the final voiced frame,
	// so the quiet probe covers the utterance and speculates promptly.
	hw.rtp(tone(audio.SampleRate * 95 / 100))
	// 600 ms of paced silence: the edge may probe and speculate once, then
	// commits without cancelling the speculation.
	for i := 0; i < 30; i++ {
		hw.rtp(make([]byte, proto.RtpPCMBytes))
		time.Sleep(20 * time.Millisecond)
	}

	specs, cancels := 0, 0
	var commit map[string]any
	for commit == nil {
		msg := b.next()
		switch msg["type"] {
		case "turn.speculate":
			specs++
		case "turn.cancel":
			cancels++
		case "turn.commit":
			commit = msg
		}
	}
	if commit["transcript"] != "hello there" {
		t.Fatalf("commit = %v", commit)
	}
	if cancels != 0 {
		t.Fatalf("turn.cancel during trailing silence = %d, want 0", cancels)
	}
	if specs > 2 {
		t.Fatalf("turn.speculate = %d, want at most 2", specs)
	}
	// Four in-speech probes, one quiet probe and the commit transcription fit
	// comfortably; the storm launched one per 20 ms silence packet.
	if calls := stt.calls.Load(); calls > 8 {
		t.Fatalf("STT calls = %d, want <= 8 (silence probe storm)", calls)
	}
}

// TestTurnCommitsWhenRTPStops pins the frozen endpoint clock: when RTP goes
// away mid-turn, the poll path must advance the policy clock on wall time and
// commit instead of hanging until the window limit.
func TestTurnCommitsWhenRTPStops(t *testing.T) {
	e := startTestEdge(t)
	b := dialBrain(t, e)
	b.waitFor("ready")
	hw := dialHardware(t, e, "vocat-rtp-stop")
	defer hw.close()
	b.waitFor("terminal.connected")
	b.rpc("rec-1", map[string]any{"cmd": "terminal.rec_start", "terminal_id": "vocat-rtp-stop"})

	start := time.Now()
	hw.rtp(tone(audio.SampleRate))
	// No more RTP at all: only the poll timer can end the turn.
	commit := b.waitFor("turn.commit")
	if commit["transcript"] != "hello there" {
		t.Fatalf("commit = %v", commit)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("commit after RTP stopped took %s, want <= 2s", elapsed)
	}
}

// TestIdleCaptureDiscardsWithoutPackets pins the other half of the clock: a
// capture that never receives RTP still reaches the 6 s idle discard.
func TestIdleCaptureDiscardsWithoutPackets(t *testing.T) {
	e := startTestEdge(t)
	b := dialBrain(t, e)
	b.waitFor("ready")
	hw := dialHardware(t, e, "vocat-idle")
	defer hw.close()
	b.waitFor("terminal.connected")
	term := e.Terminal("vocat-idle")
	b.rpc("rec-1", map[string]any{"cmd": "terminal.rec_start", "terminal_id": "vocat-idle"})

	deadline := time.Now().Add(2 * time.Second)
	for term.capture.pending.Load() == nil {
		if time.Now().After(deadline) {
			t.Fatal("capture did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	deadline = start.Add(8 * time.Second)
	for term.capture.pending.Load() != nil {
		if time.Now().After(deadline) {
			t.Fatal("idle capture never discarded")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if elapsed := time.Since(start); elapsed < 5*time.Second {
		t.Fatalf("idle capture ended after %s, want ~6s", elapsed)
	}
	if calls := e.cfg.Transcriber.(*fakeSTT).calls.Load(); calls != 0 {
		t.Fatalf("idle capture transcribed %d clips", calls)
	}
}

func TestDetachedTerminalReconnectsImmediately(t *testing.T) {
	e := startTestEdge(t)
	b := dialBrain(t, e)
	b.waitFor("ready")
	hw := dialHardware(t, e, "vocat-reconnect")
	defer hw.close()
	b.waitFor("terminal.connected")

	// The board goes away but stays inside the 30 s resume window.
	hw.close()
	b.waitFor("terminal.disconnected")

	// A fresh HELLO (board rebooted, so it has no resume token) is accepted at
	// once instead of being rejected as occupied by the warm terminal.
	hw2 := dialHardware(t, e, "vocat-reconnect")
	defer hw2.close()
	ev := b.waitFor("terminal.connected")
	if ev["reconnects"] != float64(1) {
		t.Fatalf("reconnects = %v, want 1", ev["reconnects"])
	}
	term := e.Terminal("vocat-reconnect")
	if term == nil || term.native() == nil {
		t.Fatal("reconnected terminal is not attached")
	}
	if term.reconnects() != 1 {
		t.Fatalf("terminal reconnects = %d, want 1", term.reconnects())
	}
	if _, err := hw2.conn.Write(proto.Pack(proto.MsgPing, 0, []byte{1, 2, 3, 4})); err != nil {
		t.Fatal(err)
	}
	hw2.read(proto.MsgPong)
}

type resumeAttempt struct {
	conn    net.Conn
	br      *bufio.Reader
	msgType uint8
	payload []byte
}

func dialResume(t *testing.T, e *Edge, id string, token [proto.ResumeTokenLen]byte) resumeAttempt {
	t.Helper()
	conn, err := net.Dial("tcp", e.TCPAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	r := proto.TerminalResume{ProtoVersion: proto.ProtoVersion1, ResumeToken: token, TerminalID: id}
	if _, err := conn.Write(proto.Pack(proto.MsgResume, 0, r.Encode())); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msgType, _, payload, err := proto.ReadFrame(br)
	if err != nil {
		t.Fatalf("resume read: %v", err)
	}
	return resumeAttempt{conn: conn, br: br, msgType: msgType, payload: payload}
}

func TestResumeRequiresIssuedToken(t *testing.T) {
	e := startTestEdge(t)
	b := dialBrain(t, e)
	b.waitFor("ready")
	hw := dialHardware(t, e, "vocat-resume")
	b.waitFor("terminal.connected")
	token := hw.token

	// Warm session: a wrong token must not claim or disturb it.
	hw.close()
	b.waitFor("terminal.disconnected")
	bad := token
	bad[0] ^= 0xff
	rejected := dialResume(t, e, "vocat-resume", bad)
	defer rejected.conn.Close()
	if rejected.msgType != proto.MsgReject {
		t.Fatalf("wrong-token resume type = 0x%02x, want reject", rejected.msgType)
	}
	if len(rejected.payload) == 0 || rejected.payload[0] != proto.RejectUnknownTerminal {
		t.Fatalf("wrong-token reject = %v", rejected.payload)
	}
	if term := e.Terminal("vocat-resume"); term == nil || term.native() != nil || term.isClosed() {
		t.Fatal("wrong-token resume disturbed the warm terminal")
	}

	unknown := dialResume(t, e, "vocat-nobody", token)
	defer unknown.conn.Close()
	if unknown.msgType != proto.MsgReject {
		t.Fatalf("unknown-terminal resume type = 0x%02x, want reject", unknown.msgType)
	}
	if len(unknown.payload) == 0 || unknown.payload[0] != proto.RejectUnknownTerminal {
		t.Fatalf("unknown-terminal reject = %v", unknown.payload)
	}

	// The issued token resumes the warm session.
	hw2 := dialResume(t, e, "vocat-resume", token)
	if hw2.msgType != proto.MsgWelcome {
		t.Fatalf("issued-token resume type = 0x%02x, want welcome", hw2.msgType)
	}
	if ev := b.waitFor("terminal.connected"); ev["reconnects"] != float64(1) {
		t.Fatalf("reconnects = %v, want 1", ev["reconnects"])
	}
	term := e.Terminal("vocat-resume")
	if term == nil || term.native() == nil {
		t.Fatal("issued-token resume did not attach")
	}

	// Live session: a wrong token must not replace the attached connection.
	rejectedLive := dialResume(t, e, "vocat-resume", bad)
	defer rejectedLive.conn.Close()
	if rejectedLive.msgType != proto.MsgReject || rejectedLive.payload[0] != proto.RejectUnknownTerminal {
		t.Fatalf("live wrong-token resume = 0x%02x %v", rejectedLive.msgType, rejectedLive.payload)
	}
	if _, err := hw2.conn.Write(proto.Pack(proto.MsgPing, 0, []byte{9, 8, 7, 6})); err != nil {
		t.Fatal(err)
	}
	_ = hw2.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msgType, _, _, err := proto.ReadFrame(hw2.br)
	if err != nil || msgType != proto.MsgPong {
		t.Fatalf("live session ping = 0x%02x err=%v", msgType, err)
	}
}

func TestLiveTerminalReplacedByFreshHello(t *testing.T) {
	e := startTestEdge(t)
	b := dialBrain(t, e)
	b.waitFor("ready")
	hw := dialHardware(t, e, "vocat-halfopen")
	b.waitFor("terminal.connected")

	// The old session is still open from the server's point of view (a
	// half-open TCP after a board reboot). The fresh HELLO must take over.
	hw2 := dialHardware(t, e, "vocat-halfopen")
	defer hw2.close()
	ev := b.waitFor("terminal.connected")
	if ev["reconnects"] != float64(1) {
		t.Fatalf("reconnects = %v, want 1", ev["reconnects"])
	}

	// The old socket is closed cleanly by the server.
	deadline := time.Now().Add(3 * time.Second)
	for {
		_ = hw.conn.SetReadDeadline(deadline)
		if _, err := hw.br.ReadByte(); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("old terminal socket stayed open")
		}
	}

	// The replacement session is fully live.
	term := e.Terminal("vocat-halfopen")
	if term == nil || term.native() == nil {
		t.Fatal("replacement terminal is not attached")
	}
	if _, err := hw2.conn.Write(proto.Pack(proto.MsgPing, 0, []byte{5, 6, 7, 8})); err != nil {
		t.Fatal(err)
	}
	hw2.read(proto.MsgPong)
}

func TestAudioStreamsAndStop(t *testing.T) {
	e := startTestEdge(t)
	b := dialBrain(t, e)
	b.waitFor("ready")
	hw := dialHardware(t, e, "vocat-audio")
	defer hw.close()
	b.waitFor("terminal.connected")

	// A tune renders and streams at once, then ends; the firmware ack closes
	// the playback with aborted=false.
	r := b.rpc("tune-1", map[string]any{
		"cmd": "terminal.play_tune", "terminal_id": "vocat-audio",
		"tempo": 240, "style": "hum",
		"notes": []map[string]any{{"note": "C4", "beats": 0.25}},
	})
	if r["ok"] != true || r["stream_id"] == nil {
		t.Fatalf("play_tune reply = %v", r)
	}
	_, ctrl := hw.read(proto.MsgTTSCtrl)
	if c, _ := proto.DecodeProtoTTSCtrl(ctrl); c.Op != proto.TtsOpStart {
		t.Fatalf("tune start = %+v", c)
	}
	if _, pcm := hw.read(proto.MsgTTSPCM); len(pcm) == 0 {
		t.Fatal("empty tune PCM")
	}
	_, ctrl = hw.read(proto.MsgTTSCtrl)
	if c, _ := proto.DecodeProtoTTSCtrl(ctrl); c.Op != proto.TtsOpEnd {
		t.Fatalf("tune end = %+v", c)
	}
	ack := proto.ProtoTTSAck{Aborted: 0, AudioID: uint64(r["stream_id"].(float64))}
	if _, err := hw.conn.Write(proto.Pack(proto.MsgTTSAck, 0, ack.Encode())); err != nil {
		t.Fatal(err)
	}
	if ev := b.waitFor("playback.done"); ev["aborted"] != false {
		t.Fatalf("tune playback.done = %v", ev)
	}

	// A metronome is long-running; Stop must cancel it and report aborted.
	r = b.rpc("metro-1", map[string]any{
		"cmd": "terminal.metronome", "terminal_id": "vocat-audio",
		"bpm": 120, "beats": 4, "duration_s": 30,
	})
	if r["ok"] != true {
		t.Fatalf("metronome reply = %v", r)
	}
	_, ctrl = hw.read(proto.MsgTTSCtrl)
	if c, _ := proto.DecodeProtoTTSCtrl(ctrl); c.Op != proto.TtsOpStart {
		t.Fatalf("metronome start = %+v", c)
	}
	if _, pcm := hw.read(proto.MsgTTSPCM); len(pcm) == 0 {
		t.Fatal("empty metronome PCM")
	}
	b.rpc("metro-stop", map[string]any{"cmd": "terminal.stop_audio", "terminal_id": "vocat-audio"})
	_, ctrl = hw.read(proto.MsgTTSCtrl)
	if c, _ := proto.DecodeProtoTTSCtrl(ctrl); c.Op != proto.TtsOpStop {
		t.Fatalf("metronome stop = %+v", c)
	}
	if ev := b.waitFor("playback.done"); ev["aborted"] != true {
		t.Fatalf("metronome playback.done = %v", ev)
	}

	// Invalid bounds are rejected before anything reaches the board.
	r = b.rpc("metro-bad", map[string]any{
		"cmd": "terminal.metronome", "terminal_id": "vocat-audio",
		"bpm": 999, "beats": 4, "duration_s": 10,
	})
	if r["ok"] != false || r["error"] == nil {
		t.Fatalf("invalid metronome reply = %v", r)
	}
}

// TestUDPHotPathAllocs pins the zero-allocation RTP ingest path.
func TestUDPHotPathAllocs(t *testing.T) {
	e := startTestEdge(t)
	hw := dialHardware(t, e, "vocat-alloc")
	defer hw.close()
	if _, err := hw.conn.Write(proto.Pack(proto.MsgPing, 0, []byte{1, 2, 3, 4})); err != nil {
		t.Fatal(err)
	}
	hw.read(proto.MsgPong)
	term := e.Terminal("vocat-alloc")
	if term == nil {
		t.Fatal("terminal missing")
	}
	// Install a capture with preallocated slots but no consumer, then isolate
	// the last-packet copy into a slot.
	cap := newCapture(term, false, 20*time.Second, "")
	term.capture.pending.Store(cap)
	t.Cleanup(func() { term.capture.pending.Store(nil) })
	remote := netip.MustParseAddrPort("127.0.0.1:40000")
	pkt := make([]byte, proto.RtpPacketBytes)
	pkt[0] = proto.RtpVersionByte
	pkt[1] = proto.RtpPTRecPCM
	pkt[8] = byte(hw.ssrc >> 24)
	pkt[9] = byte(hw.ssrc >> 16)
	pkt[10] = byte(hw.ssrc >> 8)
	pkt[11] = byte(hw.ssrc)
	for i := 0; i < 4; i++ {
		e.server.handleUDP(pkt, remote)
	}
	seq := uint16(10)
	allocs := testing.AllocsPerRun(100, func() {
		pkt[2], pkt[3] = byte(seq>>8), byte(seq)
		seq++
		e.server.handleUDP(pkt, remote)
	})
	if allocs != 0 {
		t.Fatalf("UDP hot path allocs = %v, want 0", allocs)
	}
}

func findHit(t *testing.T, hits []any, id string) [2]int {
	t.Helper()
	for _, raw := range hits {
		h, ok := raw.(map[string]any)
		if !ok || h["ID"] != id {
			continue
		}
		x := int(h["X1"].(float64)+h["X2"].(float64)) / 2
		y := int(h["Y1"].(float64)+h["Y2"].(float64)) / 2
		return [2]int{x, y}
	}
	t.Fatalf("hit %q not found in %v", id, hits)
	return [2]int{}
}

func writePortraitSheet(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1024, 1024))
	colors := []color.RGBA{{20, 20, 20, 255}, {40, 20, 20, 255}, {20, 40, 20, 255}, {20, 20, 40, 255}}
	for q := 0; q < 4; q++ {
		bg := colors[q]
		x0, y0 := (q%2)*512, (q/2)*512
		for y := 0; y < 512; y++ {
			for x := 0; x < 512; x++ {
				img.SetRGBA(x0+x, y0+y, bg)
			}
		}
		fg := color.RGBA{220, 180 - uint8(q*20), 150, 255}
		for y := 150; y < 380; y++ {
			for x := 150; x < 380; x++ {
				img.SetRGBA(x0+x, y0+y, fg)
			}
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
