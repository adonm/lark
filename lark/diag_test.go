package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDiagnosticsRecordsSilentAndToolErrors(t *testing.T) {
	t.Parallel()
	b := &larkServer{}
	now := time.Now()
	traceFrom(b.traceTurn(t.Context(), "cake", "Cake", "voice", now)).silent()
	b.diag.toolError("cake", "patch: terminal_id required: no online terminal for companion cake")
	b.diag.toolError("mint", "story: unknown story")

	report := b.diagnosticReport()
	if len(report.Turns) != 1 {
		t.Fatalf("turns = %+v", report.Turns)
	}
	turn := report.Turns[0]
	if !turn.Silent || len(turn.ToolErrors) != 1 ||
		turn.ToolErrors[0] != "patch: terminal_id required: no online terminal for companion cake" {
		t.Fatalf("silent/error diagnostics missing: %+v", turn)
	}
}

func TestDiagnosticsListeningCountersAndSegments(t *testing.T) {
	t.Parallel()
	b := &larkServer{}
	b.diag.listen.windows.Add(3)
	b.diag.listen.speech.Add(2)
	b.diag.listen.droppedGate.Add(1)
	b.diag.listen.emptyTranscript.Add(1)
	b.diag.listen.fillerOnly.Add(1)
	b.diag.listen.committed.Add(2)
	b.diag.listen.silentReplies.Add(1)
	b.diag.listen.didntCatch.Add(1)
	b.diag.listen.nameRequired.Add(1)
	b.diag.listen.closeSilence.Add(1)
	b.diag.listen.closeTap.Add(1)
	b.diag.listen.closeCap.Add(1)
	b.diag.addSegment(diagSegment{TS: 1, Terminal: "box3", DurationMS: 2000, SpeechMS: 1200, TextLen: 21, Words: 4})
	got := b.diagnosticReport().Listening
	if got.Windows != 3 || got.Speech != 2 || got.DroppedGate != 1 || got.EmptyTranscript != 1 ||
		got.FillerOnly != 1 || got.Committed != 2 || got.SilentReplies != 1 || got.DidntCatch != 1 ||
		got.NameRequired != 1 || got.CloseSilence != 1 || got.CloseTap != 1 || got.CloseCap != 1 {
		t.Fatalf("listening counters: %+v", got)
	}
	if len(got.RecentSegments) != 1 || got.RecentSegments[0].TextLen != 21 || got.RecentSegments[0].SpeechMS != 1200 {
		t.Fatalf("recent listening segments: %+v", got.RecentSegments)
	}
}

func TestDiagnosticsMetricWindowAndTurns(t *testing.T) {
	t.Parallel()
	b := &larkServer{}
	t0 := time.Now()
	for i := 1; i <= 40; i++ {
		ctx := b.traceTurn(t.Context(), "board", "Cake", "voice", t0.Add(time.Duration(i)*time.Second))
		trace := traceFrom(ctx)
		trace.measure(diagSTT, time.Duration(i)*time.Millisecond)
		trace.measure(diagSTT, 999*time.Millisecond)
		trace.audioEnqueued(trace.start.Add(50 * time.Millisecond))
	}
	report := b.diagnosticReport()
	stt := report.Latency["stt_ms"]
	if stt.Count != 32 || *stt.Last != 40 || *stt.P50 != 24 || *stt.P95 != 39 || stt.Samples[0] != 9 {
		t.Fatalf("unexpected STT window: %+v", stt)
	}
	if report.Latency["llm_first_ms"].Last != nil || len(report.Turns) != 20 ||
		report.Turns[0].TS != t0.Add(40*time.Second).UnixMilli() || *report.Turns[19].STT != 21 ||
		*report.Turns[0].E2E != 50 || report.Turns[0].Terminal != "board" || report.Turns[0].Character != "Cake" {
		t.Fatalf("unexpected turn log: %+v", report.Turns)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			trace := traceFrom(b.traceTurn(t.Context(), "web", "Mint", "typed", time.Now()))
			trace.measure(diagLLMFirst, 3*time.Millisecond)
			trace.measure(diagLLMTotal, 7*time.Millisecond)
			_ = b.diagnosticReport()
		})
	}
	wg.Wait()
	if m := b.diagnosticReport().Latency["llm_total_ms"]; m.Count != 16 || *m.P95 != 7 {
		t.Fatalf("concurrent metric writes: %+v", m)
	}
}

func TestDiagnosticsProcAndShutdown(t *testing.T) {
	t.Parallel()
	if got := parseProcStat("42 (cat ) server) S 0 0 0 0 0 0 0 0 0 0 12 30 0 0 0 0 0 0 567", 42); got != (procReading{42, 42, 567}) {
		t.Fatal(got)
	}
	if got := parseHostCPU("cpu 1 2 3 4 5 6 7 8 9 10\ncpu0 15"); got != (cpuReading{27, 36}) {
		t.Fatal(got)
	}
	path := filepath.Join(t.TempDir(), "lark.db")
	for _, f := range []string{path, path + "-wal"} {
		if err := os.WriteFile(f, []byte("telemetry"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	root, cancel := context.WithCancel(t.Context())
	defer cancel()
	b := &larkServer{root: root}
	stop := b.sampleDiagnostics(time.Now(), path)
	defer stop()
	deadline := time.NewTimer(testWait(t))
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		r := b.diagnosticReport()
		if r.Host.CPU != nil && r.Bridge.CPU != nil {
			if r.Host.Total == nil || r.Host.Used == nil || r.Bridge.RSS == nil || *r.Bridge.RSS == 0 ||
				r.Bridge.Goroutine == 0 || r.Bridge.DB != 18 {
				t.Fatalf("incomplete sample: %+v", r.diagSystem)
			}
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("timed out waiting for Linux CPU deltas")
		case <-tick.C:
		}
	}
	cancel()
	stop()
}

func TestDiagnosticsEndpointAndBoardHUD(t *testing.T) {
	t.Parallel()
	b, base := wsHarness(t, nil)
	board := testConnectWS(t, base, "unit", false)
	const id = "vocat-diag"
	board.sendProto(t, msgHello, encodeTerminalHello(terminalHello{
		protoVersion: protoVersion1, kind: termKindHardware, capsMask: capDrag, displayFormats: dispRGB565, frameCodecs: codecBitAll,
		width: 320, height: 240, shape: shapeRect, terminalID: id, board: "esp-box-3", firmware: "0.12.0", token: "unit",
	}))
	board.until(t, hasProto(msgWelcome, ""))
	board.until(t, isDisplay)
	var st [16]byte
	rssi, batt := int16(-60), int16(-1)
	binary.LittleEndian.PutUint16(st[0:2], uint16(rssi))
	binary.LittleEndian.PutUint16(st[2:4], uint16(batt))
	st[5] = 27
	binary.LittleEndian.PutUint32(st[8:12], 65536)
	binary.LittleEndian.PutUint32(st[12:16], 320)
	board.sendProto(t, msgTelemetry, st[:])
	device := b.hub.live(id)
	sentAt := time.Now().Add(-12 * time.Millisecond)
	device.client.pingMu.Lock()
	device.client.pingSentAt = sentAt
	device.client.pingMu.Unlock()
	board.sendProto(t, msgPong, []byte{1, 2, 3, 4})
	board.barrier(t)
	if device.client.pingRTT() != nil {
		t.Fatal("unsolicited pong was accepted")
	}
	var pongPayload [4]byte
	binary.LittleEndian.PutUint32(pongPayload[:], uint32(sentAt.UnixMilli()))
	board.sendProto(t, msgPong, pongPayload[:])
	board.barrier(t)
	testRequest(t, "GET", base+"/api/diagnostics", "", nil, 401)
	testRequest(t, "GET", base+"/api/diagnostics?token=unit", "", nil, 401)
	raw := testRequest(t, "GET", base+"/api/diagnostics", "unit", nil, 200)
	var report diagReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.Generated.IsZero() || len(report.Terminals) != 1 || report.Turns == nil || report.Latency["render_ms"].Count == 0 {
		t.Fatalf("invalid diagnostics response: %s", raw)
	}
	d := report.Terminals[0]
	if d.Board != "esp-box-3" || d.Firmware != "0.12.0" || d.RTT == nil || *d.RTT < 12 ||
		d.CPU == nil || *d.CPU != 27 || *d.WiFi != -60 || *d.FreeHeap != 65536 || *d.Uptime != 320 ||
		*d.HasBattery || d.Frame.Bytes >= d.Frame.RawBytes || (d.Frame.Codec != "RLE565" && d.Frame.Codec != "LZ4") {
		t.Fatalf("invalid device telemetry: %+v", d)
	}
	tap := func(x, y int) { board.tap(t, x, y, 0) }
	board.tapMenuSpec(t, pixelSpecBox3)
	svg := waitSVG(t, b.hub, id, "menu-more")
	mx, my, ok := hitCenter(svg, "menu-more")
	if !ok {
		t.Fatal("missing menu-more")
	}
	tap(mx, my)
	svg = waitSVG(t, b.hub, id, "menu-stats")
	sx, sy, ok := hitCenter(svg, "menu-stats")
	if !ok {
		t.Fatal("missing menu-stats")
	}
	tap(sx, sy)
	hud := waitSVG(t, b.hub, id, "Companion")
	if !strings.Contains(hud, "Voice") {
		t.Fatal("missing Voice in stats")
	}
	rssi2 := int16(-54)
	binary.LittleEndian.PutUint16(st[0:2], uint16(rssi2))
	st[5] = 39
	board.sendProto(t, msgTelemetry, st[:])
	bx, by, ok := hitCenter(hud, "menu-back")
	if !ok {
		t.Fatal("missing menu-back")
	}
	tap(bx, by)
	svg = waitSVG(t, b.hub, id, "menu-stats")
	bx, by, _ = hitCenter(svg, "menu-back")
	tap(bx, by)
	svg = waitSVG(t, b.hub, id, "menu-more")
	bx, by, _ = hitCenter(svg, "menu-back")
	tap(bx, by)
	waitSVG(t, b.hub, id, `data-id="listen"`)
}

func TestDiagnosticsFirstSpokenDeltaAndAudio(t *testing.T) {
	t.Parallel()
	b := &larkServer{}
	ctx := b.traceTurn(t.Context(), "box", "Mint", "voice", time.Now().Add(-10*time.Millisecond))
	tr := traceFrom(ctx)
	text := streamedText{onFirst: func() { tr.measure(diagLLMFirst, time.Millisecond) }}
	if next := text.add(" \nHey"); len(next) != 0 || tr.record.First == nil {
		t.Fatal("time-to-first-text waited for a complete sentence")
	}
	audio := newAudioStream()
	start := time.Now()
	audio.append([]byte{1, 0})
	audio.traceFirst(tr, start)
	tr.audioEnqueued(time.Now())
	row := b.diagnosticReport().Turns[0]
	if row.TTS == nil || row.E2E == nil || *row.E2E < 10 || row.FirstPCM == nil || *row.FirstPCM != *row.E2E {
		t.Fatal(row)
	}
}

func TestDiagnosticsTTSQueueDropsCanceledSpeechWithoutRestart(t *testing.T) {
	t.Parallel()
	root, stop := context.WithCancel(t.Context())
	var calls []string
	loads := 0
	stale, cancel := context.WithCancel(root)
	defer cancel()
	worker := newSynthWorker(root, func() (speechEngine, error) {
		loads++
		return fakeSpeechEngine(func(text string, _ voiceChoice, keepGoing func() bool) ([]float32, int) {
			calls = append(calls, text)
			if text == "Interrupt me" {
				cancel()
				if keepGoing() {
					t.Error("native callback did not observe request cancellation")
				}
			}
			return []float32{0.25}, sampleRate
		}), nil
	})
	defer func() { stop(); <-worker.done }()
	b := &larkServer{}
	ctx := b.traceTurn(root, "vocat", "Cake", "typed", time.Now())
	if pcm, err := worker.synthesizeQueued(ctx, heartVoice, "First", time.Now().Add(-10*time.Millisecond)); err != nil || len(pcm) != 2 {
		t.Fatalf("worker did not return PCM: %v, %v", pcm, err)
	}
	if _, err := worker.synthesize(stale, heartVoice, "Interrupt me"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := worker.synthesize(stale, heartVoice, "Discard me"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := worker.synthesize(ctx, heartVoice, "Second"); err != nil || !worker.ready.Load() || loads != 1 {
		t.Fatalf("worker did not stay warm: %v", err)
	}
	if got := strings.Join(calls, ","); got != "First,Interrupt me,Second" {
		t.Fatalf("unexpected synthesis calls: %s", got)
	}
	report := b.diagnosticReport()
	if row := report.Turns[0]; row.TTSQueue == nil || *row.TTSQueue < 10 || row.TTSInfer == nil ||
		report.Latency["tts_queue_ms"].Count != 1 || report.Latency["tts_infer_ms"].Count != 1 {
		t.Fatalf("missing split timings: %+v", row)
	}
}

func TestAccessibleRecordingControls(t *testing.T) {
	t.Parallel()
	transcribed := make(chan struct{}, 2)
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.setSTT(func(context.Context, []byte) (string, error) {
			transcribed <- struct{}{}
			return "", nil
		})
	})
	dev := testConnectWS(t, base, "unit", false)
	const id = "vocat-mic0"
	dev.hello(t, id, "Cake")
	tap := func(x, y int) { dev.tap(t, x, y, 0) }
	record := func() {
		dev.recStart(t, "")
		waitSVG(t, b.hub, id, "rec-send")
		dev.sendRTP(t, rtpPTRecPCM, 1, 320, []byte{12, 0})
	}
	record()
	tap(110, 290)
	dev.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpCancel
	})
	dev.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgChime, "")(f) && len(p) >= 1 && p[0] == chimeCancel
	})
	dev.recEnd(t, "", false)
	dev.barrier(t)
	waitSVG(t, b.hub, id, `data-id="listen"`)
	select {
	case <-transcribed:
		t.Fatal("cancelled PCM reached STT")
	default:
	}
	record()
	tap(250, 290)
	dev.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpStop
	})
	select {
	case <-transcribed:
		// The target did not send rec_end; the fail-safe used buffered hardware PCM.
	case <-time.After(testWait(t)):
		t.Fatal("Send did not finalize after the hardware rec_end timeout")
	}
	dev.recEnd(t, "", false)
	dev.barrier(t)
	raw := apiObjects(t, base+"/api/diagnostics", "recent_turns")
	if len(raw) != 1 || raw[0].(object)["source"] != "voice" {
		t.Fatal("recording did not produce exactly one voice turn", raw)
	}
}

func stopTestHarness(t *testing.T, id string) (*larkServer, string, <-chan string) {
	t.Helper()
	const path = "/api/session/ses-stop/interrupt"
	interrupts := make(chan string, 4)
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.oc = testOpenCode(t, b.root, dir, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != path {
				http.NotFound(w, r)
				return
			}
			interrupts <- r.URL.Path
			_, _ = io.WriteString(w, `{"data":{"interrupted":true}}`)
		})
		if err := b.oc.save(id, "ses-stop"); err != nil {
			t.Fatal(err)
		}
	})
	return b, base, interrupts
}

func TestThinkingStopRequiresThreeTaps(t *testing.T) {
	t.Parallel()
	const id = "vocat-stop"
	b, base, interrupts := stopTestHarness(t, "cake")
	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, id, "Cake")
	cancelled := make(chan bool, 1)
	d := b.hub.live(id)
	now := time.Now()
	updateTerm(t, d, func(d *terminalSession) {
		b.hub.startTurn(d, now, false, "typed", func(ctx context.Context, _ uint64) {
			<-ctx.Done()
			cancelled <- true
		})
		b.hub.pointer(d, "tap", 180, 180, 0, now)
	})
	waitSVG(t, b.hub, id, "stop")

	stillThinking := func(taps int) {
		t.Helper()
		st := d.state()
		busy, count := st.busy, st.stopTaps
		if !busy || count != taps {
			t.Fatalf("want ongoing turn with %d Stop taps, busy=%t taps=%d", taps, busy, count)
		}
		select {
		case <-cancelled:
			t.Fatal("turn canceled before confirmation")
		case got := <-interrupts:
			t.Fatalf("early upstream interrupt: %s", got)
		default:
		}
	}

	dev.tap(t, 180, 180, 0)
	dev.barrier(t)
	stillThinking(0)

	tap := func(_ string, count int) {
		t.Helper()
		dev.tap(t, 180, 290, 0)
		dev.until(t, func(f protocolFrame) bool {
			p := framePayload(f)
			return hasProto(msgChime, "")(f) && len(p) >= 1 && p[0] == chimeTap
		})
		stillThinking(count)
	}
	tap("Tap 2×", 1)
	tap("Tap 1×", 2)
	// Age the tap window instead of sleeping through it; the next tap must
	// start a fresh sequence. Rendering applies the expiry synchronously.
	updateTerm(t, d, func(d *terminalSession) {
		d.stopTapAt = now.Add(-stopTapWindow - time.Second)
		b.hub.render(d, time.Now(), false)
	})
	stillThinking(0)
	tap("Tap 2×", 1)
	tap("Tap 1×", 2)
	dev.tap(t, 180, 290, 0)
	dev.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgChime, "")(f) && len(p) >= 1 && p[0] == chimeCancel
	})
	waitSVG(t, b.hub, id, `data-id="listen"`)
	expectValue(t, cancelled, true)
	expectValue(t, interrupts, "/api/session/ses-stop/interrupt")
	st := d.state()
	if st.busy || st.stopTaps != 0 || !st.stopTapAt.IsZero() {
		t.Fatal("Stop did not clear the turn and confirmation state")
	}
}

func TestSpeakingStopAbortsPlaybackWithoutUpstreamInterrupt(t *testing.T) {
	t.Parallel()
	const id = "vocat-spk0"
	b, base, interrupts := stopTestHarness(t, id)
	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, id, "Cake")

	stream := newAudioStream()
	stream.append([]byte{1, 0})
	d := b.hub.live(id)
	updateTerm(t, d, func(d *terminalSession) {
		b.hub.startTurn(d, time.Now(), false, "typed", func(ctx context.Context, _ uint64) { <-ctx.Done() })
	})
	turn := d.state().turn
	waitSVG(t, b.hub, id, "stop")
	dev.tap(t, 180, 290, 0)
	dev.barrier(t)
	var playback context.Context
	updateTerm(t, d, func(d *terminalSession) { playback = b.hub.playTraced(d, stream, nil, time.Time{}) })
	dev.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgTTSCtrl, "")(f) && len(p) >= 1 && p[0] == ttsOpStart
	})
	updateTerm(t, d, func(d *terminalSession) { b.hub.finishTurn(d, turn, nil, nil) })
	waitSVG(t, b.hub, id, "stop")
	dev.tap(t, 180, 180, 0)
	dev.barrier(t)
	if playback.Err() != nil {
		t.Fatal("face tap stopped playback")
	}
	dev.tap(t, 180, 290, 0)
	dev.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgTTSCtrl, "")(f) && len(p) >= 1 && p[0] == ttsOpStop
	})
	waitSVG(t, b.hub, id, `data-id="listen"`)
	if playback.Err() == nil {
		t.Fatal("one Stop tap did not stop playback")
	}

	dev.recStart(t, "")
	waitSVG(t, b.hub, id, "rec-send")
	b.oc.mu.Lock()
	pending := b.oc.interrupts[id]
	b.oc.mu.Unlock()
	if pending || len(interrupts) != 0 {
		t.Fatal("stopping speech or starting an idle recording interrupted OpenCode")
	}
}
