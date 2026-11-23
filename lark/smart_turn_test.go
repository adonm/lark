package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestWhisperMelZeroAlloc(t *testing.T) {
	m := newWhisperMel()
	pcm := make([]byte, sampleRate*2*2) // 2s
	out := make([]float32, smartTurnMels*smartTurnFrames)
	allocs := testing.AllocsPerRun(5, func() {
		m.extractPCM16(pcm, out)
	})
	if allocs != 0 {
		t.Fatalf("extractPCM16 allocated %.0f times, want 0", allocs)
	}
}

func TestSmartTurnModelSmoke(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	data := env("XDG_DATA_HOME", filepath.Join(home, ".local/share"))
	modelPath := filepath.Join(data, "lark", "stt-models", smartTurnModel)
	if _, err := os.Stat(modelPath); err != nil {
		t.Skip("smart-turn model not installed")
	}
	eng, err := openSmartTurn(modelPath, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.close()
	t.Logf("in=%s out=%s", eng.inName, eng.outName)

	pcm := make([]byte, sampleRate*2*3)
	start := time.Now()
	prob, err := eng.predictPCM16(pcm)
	dur := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("silence prob=%.4f dur=%s", prob, dur)

	if wav, err := readWAVPCM16k("/tmp/opencode/what2plus2.wav"); err == nil {
		for _, padMS := range []int{0, 200, 520} {
			p := append(append([]byte(nil), wav...), make([]byte, sampleRate*2*padMS/1000)...)
			pr, _ := eng.predictPCM16(p)
			t.Logf("what2plus2 pad=%dms prob=%.4f", padMS, pr)
		}
		sileroPath := filepath.Join(data, "lark", "stt-models", sileroModel)
		if vad := newSileroVAD(sileroPath); vad != nil {
			defer vad.close()
			var u utterance
			full := append(append([]byte(nil), wav...), make([]byte, sampleRate*2*1200/1000)...)
			for off := 0; off+endpointBytes <= len(full); off += endpointBytes {
				u.add(full[off:off+endpointBytes], vad)
				at := pcmDuration(len(u.pcm))
				if u.policy.voiced && !u.policy.inVoice && at >= endpointQuiet {
					pr, err := eng.predictPCM16(u.pcm)
					t.Logf("first smartTurn trigger at=%s dur=%s prob=%.4f err=%v", at, at, pr, err)
					break
				}
			}
		}
	}
}

func TestSmartTurnEndpointPolicy(t *testing.T) {
	var calls atomic.Int32
	var prob atomic.Uint32
	prob.Store(0x3f733333) // 0.95 (math.Float32bits(0.95))
	b, base := wsHarness(t, func(b *larkServer, _ string) {
		ep := testEndpointSettings()
		ep.smartTurn = func([]byte) (float32, error) {
			calls.Add(1)
			bits := prob.Load()
			switch {
			case bits == 1:
				return 0.95, nil
			case bits == 2:
				return 0.10, nil
			default:
				return 0.95, nil
			}
		}
		b.endpoint = ep
		b.setSTT(func(_ context.Context, pcm []byte) (string, error) {
			if bytes.Count(pcm, bytes.Repeat([]byte{17, 0}, endpointBytes/2)) >= 15 {
				return "what is two plus two?", nil
			}
			return "what is", nil
		})
	})
	const id = "vocat-st01"
	if _, err := b.createCompanion(t.Context(), object{"id": "echo", "name": "Echo", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	d := testConnectWS(t, base, "unit", false)
	d.helloHardware(t, id, "echo", capBridgeEndpoint|capDrag)

	var seq uint16
	appendPCM := func(p []byte) {
		for len(p) > 0 {
			n := min(2*pcmPacket, len(p))
			seq++
			d.sendRTP(t, rtpPTRecPCM, seq, uint32(seq)*320, p[:n])
			p = p[n:]
		}
	}
	voiced := bytes.Repeat([]byte{17, 0}, endpointBytes/2)
	silent := make([]byte, endpointBytes)

	recStop := func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpStop
	}
	idleOrDone := func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgStatus, "")(f) && len(p) >= 5 && (p[4] == statusIdle || p[4] == statusDone || p[4] == statusError)
	}

	// 1. Complete (>= threshold): commits after smartTurnPause (~200ms) + vadTail.
	prob.Store(1)
	calls.Store(0)
	b.ws.mu.Lock()
	b.ws.startRecording(b.ws.terminals[id], "", time.Now(), true)
	b.ws.mu.Unlock()
	d.recStart(t, "")
	d.barrier(t)
	waitSVG(t, b.ws, id, "rec-send")
	appendPCM(bytes.Repeat(voiced, 10))
	appendPCM(bytes.Repeat(silent, 28)) // 560ms silence (> endpointQuiet 400ms)
	d.until(t, recStop)
	d.recEnd(t, "", false)
	d.until(t, idleOrDone)
	if calls.Load() != 1 {
		t.Fatalf("smartTurn calls=%d, want 1", calls.Load())
	}

	// 2. Incomplete (< threshold) keeps listening, then re-checks on next pause and commits when >= threshold.
	// Let any inference from the first recording drain before counting again.
	time.Sleep(150 * time.Millisecond)
	prob.Store(2)
	calls.Store(0)
	b.ws.mu.Lock()
	b.ws.startRecording(b.ws.terminals[id], "", time.Now(), true)
	b.ws.mu.Unlock()
	d.recStart(t, "")
	d.barrier(t)
	waitSVG(t, b.ws, id, "rec-send")
	appendPCM(bytes.Repeat(voiced, 10))
	appendPCM(bytes.Repeat(silent, 22))
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	firstPause := calls.Load()
	if firstPause < 1 {
		t.Fatalf("expected a smartTurn call on first pause, got %d", firstPause)
	}
	// Now user speaks again and pauses with complete prosody.
	prob.Store(1)
	appendPCM(bytes.Repeat(voiced, 10))
	appendPCM(bytes.Repeat(silent, 22))
	d.until(t, recStop)
	d.recEnd(t, "", false)
	d.until(t, idleOrDone)
	if calls.Load() <= firstPause {
		t.Fatalf("expected another smartTurn call after second pause, got %d before and %d after", firstPause, calls.Load())
	}
}

type alwaysSpeechVAD struct{}

func (*alwaysSpeechVAD) detect([]byte) bool { return true }
func (*alwaysSpeechVAD) close()             {}

func TestEndpointNoisySilenceAndIncompleteCap(t *testing.T) {
	// 1. Policy-level: VAD stuck in speech on low-level noise still commits at cap.
	var p endpointPolicy
	p.observeEnergy(20*time.Millisecond, true, 2000)
	p.observeEnergy(200*time.Millisecond, true, 2000)
	// Low-level room noise (< 20% of 2000) keeps vadSpeech=true, must not open gate or extend lastWordAt.
	p.observeProgress(200*time.Millisecond, "hello world", false)
	for at := 220 * time.Millisecond; at < 200*time.Millisecond+endpointCap; at += 20 * time.Millisecond {
		p.observeEnergy(at, true, 150)
		if p.inVoice {
			t.Fatalf("low-level noise at %v marked as inVoice", at)
		}
	}
	if got := p.decision(200*time.Millisecond+endpointCap, endpointCap, endpointLimit, false, false); got != endpointCommit {
		t.Fatalf("noisy silence did not commit at cap: %v", got)
	}

	// 2. End-to-end WS: VAD stuck in speech + Smart Turn incomplete forever still commits within cap.
	var clips atomic.Int32
	b, base := wsHarness(t, func(b *larkServer, _ string) {
		b.endpoint = &endpointSettings{
			newVAD:    func() endpointVAD { return &alwaysSpeechVAD{} },
			smartTurn: func([]byte) (float32, error) { return 0.10, nil },
		}
		b.setSTT(func(_ context.Context, _ []byte) (string, error) {
			clips.Add(1)
			return "unfinished thought, um", nil
		})
	})
	const id = "vocat-cap1"
	if _, err := b.createCompanion(t.Context(), object{"id": "echo", "name": "Echo", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	d := testConnectWS(t, base, "unit", false)
	d.helloHardware(t, id, "echo", capBridgeEndpoint|capDrag)

	// Speech at amplitude ~2000, followed by low-level noise at amplitude ~150.
	speechFrame := make([]byte, endpointBytes)
	noiseFrame := make([]byte, endpointBytes)
	for i := 0; i < endpointBytes; i += 2 {
		speechFrame[i], speechFrame[i+1] = 0xd0, 0x07 // 2000
		noiseFrame[i], noiseFrame[i+1] = 0x96, 0x00   // 150
	}
	var seq uint16
	appendPCM := func(p []byte) {
		for len(p) > 0 {
			n := min(2*pcmPacket, len(p))
			seq++
			d.sendRTP(t, rtpPTRecPCM, seq, uint32(seq)*320, p[:n])
			p = p[n:]
		}
	}
	recStop := func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpStop
	}
	idleOrDone := func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgStatus, "")(f) && len(p) >= 5 && (p[4] == statusIdle || p[4] == statusDone || p[4] == statusError)
	}

	b.ws.mu.Lock()
	b.ws.startRecording(b.ws.terminals[id], "", time.Now(), true)
	b.ws.mu.Unlock()
	d.recStart(t, "")
	d.barrier(t)
	waitSVG(t, b.ws, id, "rec-send")
	appendPCM(bytes.Repeat(speechFrame, 15))                              // 300ms speech
	appendPCM(bytes.Repeat(noiseFrame, int(endpointCap/endpointFrame)+5)) // > cap (1.2s) noisy silence
	d.until(t, recStop)
	d.recEnd(t, "", false)
	d.until(t, idleOrDone)
	if clips.Load() < 1 {
		t.Fatal("expected auto-commit under noisy silence + incomplete Smart Turn")
	}
}

func TestEndpointListenWindowNoisySilenceAutoCommits(t *testing.T) {
	var clips atomic.Int32
	b, base := wsHarness(t, func(b *larkServer, _ string) {
		b.endpoint = &endpointSettings{
			newVAD:    func() endpointVAD { return &testEndpointVAD{} },
			smartTurn: func([]byte) (float32, error) { return 0.15, nil },
		}
		b.setSTT(func(_ context.Context, _ []byte) (string, error) {
			clips.Add(1)
			return "hello there", nil
		})
	})
	const id = "vocat-lst1"
	if _, err := b.createCompanion(t.Context(), object{"id": "echo", "name": "Echo", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	toolTest(t, base, "patch", object{"companion_id": "echo", "companion": object{"listen_seconds": 30}}, false)
	d := testConnectWS(t, base, "unit", false)
	d.helloHardware(t, id, "echo", capBridgeEndpoint|capDrag)

	speechFrame := make([]byte, endpointBytes)
	noiseFrame := make([]byte, endpointBytes)
	for i := 0; i < endpointBytes; i += 2 {
		speechFrame[i], speechFrame[i+1] = 0xd0, 0x07 // 2000
		noiseFrame[i], noiseFrame[i+1] = 0x96, 0x00   // 150 (note: noiseFrame[0] != 0 so testEndpointVAD stays in speech)
	}
	var seq uint16
	appendPCM := func(p []byte) {
		for len(p) > 0 {
			n := min(2*pcmPacket, len(p))
			seq++
			d.sendRTP(t, rtpPTRecPCM, seq, uint32(seq)*320, p[:n])
			p = p[n:]
		}
	}
	recStop := func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpStop
	}
	idleOrDone := func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgStatus, "")(f) && len(p) >= 5 && (p[4] == statusIdle || p[4] == statusDone || p[4] == statusError)
	}

	b.ws.mu.Lock()
	ok := b.ws.startListeningWindow(b.ws.terminals[id], time.Now())
	b.ws.mu.Unlock()
	if !ok {
		t.Fatal("startListeningWindow returned false")
	}
	d.recStart(t, "")
	d.barrier(t)
	appendPCM(bytes.Repeat(speechFrame, 15))
	appendPCM(bytes.Repeat(noiseFrame, int(endpointCap/endpointFrame)+10))
	d.until(t, recStop)
	d.recEnd(t, "", false)
	d.until(t, idleOrDone)
	if clips.Load() < 1 {
		t.Fatal("listen window did not auto-commit under noisy silence")
	}
}

func TestEndpointNoiseNeverFormsWordsDiscards(t *testing.T) {
	var p endpointPolicy
	p.observeEnergy(20*time.Millisecond, true, 200)
	// Empty or filler-only transcripts from noise never start a turn.
	for _, noiseText := range []string{"", "   ", "um", "uh-huh", "hmm"} {
		if p.observeProgress(300*time.Millisecond, noiseText, false) {
			t.Fatalf("noise transcript %q should not count as progress", noiseText)
		}
	}
	if p.hasWords {
		t.Fatal("hasWords should remain false on empty/filler output")
	}
	if got := p.decision(endpointCap, endpointCap, endpointLimit, true, false); got != endpointWait {
		t.Fatalf("expected wait before endpointIdle, got %v", got)
	}
	if got := p.decision(endpointIdle, endpointCap, endpointLimit, true, false); got != endpointDiscard {
		t.Fatalf("expected discard at endpointIdle, got %v", got)
	}
}

func TestEndpointIncompleteTrailingWaitsForCap(t *testing.T) {
	var sttCalls atomic.Int32
	b, base := wsHarness(t, func(b *larkServer, _ string) {
		b.endpoint = &endpointSettings{
			newVAD:    func() endpointVAD { return &testEndpointVAD{} },
			smartTurn: func([]byte) (float32, error) { return 0.99, nil },
		}
		b.setSTT(func(_ context.Context, _ []byte) (string, error) {
			sttCalls.Add(1)
			return "I want apples and", nil
		})
	})
	const id = "vocat-inc1"
	if _, err := b.createCompanion(t.Context(), object{"id": "echo", "name": "Echo", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	d := testConnectWS(t, base, "unit", false)
	d.helloHardware(t, id, "echo", capBridgeEndpoint|capDrag)

	voiced := bytes.Repeat([]byte{17, 0}, endpointBytes/2)
	silent := make([]byte, endpointBytes)
	var seq uint16
	appendPCM := func(p []byte) {
		for len(p) > 0 {
			n := min(2*pcmPacket, len(p))
			seq++
			d.sendRTP(t, rtpPTRecPCM, seq, uint32(seq)*320, p[:n])
			p = p[n:]
		}
	}
	recStop := func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpStop
	}
	idleOrDone := func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgStatus, "")(f) && len(p) >= 5 && (p[4] == statusIdle || p[4] == statusDone || p[4] == statusError)
	}

	b.ws.mu.Lock()
	b.ws.startRecording(b.ws.terminals[id], "", time.Now(), true)
	b.ws.mu.Unlock()
	d.recStart(t, "")
	d.barrier(t)
	waitSVG(t, b.ws, id, "rec-send")
	appendPCM(bytes.Repeat(voiced, 15))
	appendPCM(bytes.Repeat(silent, 25)) // 500ms > endpointQuiet (400ms), < endpointCap (1200ms)
	d.barrier(t)
	b.ws.mu.Lock()
	stillOpen := b.ws.terminals[id].recording.phase == captureOpen
	b.ws.mu.Unlock()
	if !stillOpen {
		t.Fatal("committed early despite trailing 'and'")
	}
	appendPCM(bytes.Repeat(silent, 45)) // +900ms -> 1400ms > endpointCap (1200ms)
	d.until(t, recStop)
	d.recEnd(t, "", false)
	d.until(t, idleOrDone)
}

func TestEndpointProbeAllocs(t *testing.T) {
	var p endpointPolicy
	p.observeEnergy(20*time.Millisecond, true, 2000)
	p.observeProgress(250*time.Millisecond, "What is two", false)
	allocs := testing.AllocsPerRun(100, func() {
		p.observeEnergy(500*time.Millisecond, true, 1800)
		p.observeProgress(500*time.Millisecond, "What is two plus two?", false)
		_ = p.decision(900*time.Millisecond, endpointCap, endpointLimit, true, false)
	})
	if allocs > 2 {
		t.Fatalf("probe loop allocs=%.1f, want <= 2", allocs)
	}
}
