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
		b.stt = func(_ context.Context, _ []byte) (string, error) { return "", nil }
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
			d.sendRTP(t, rtpPTRecPCM, seq, uint32(seq)*320, 1, p[:n])
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
		return hasProto(msgStatus, "")(f) && len(p) >= 5 && (p[4] == statusIdle || p[4] == statusDone)
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
	appendPCM(bytes.Repeat(silent, 28)) // 560ms silence (> vadTail 320ms + smartTurnPause 200ms)
	d.until(t, recStop)
	d.recEnd(t, "", false)
	d.until(t, idleOrDone)
	if calls.Load() != 1 {
		t.Fatalf("smartTurn calls=%d, want 1", calls.Load())
	}

	// 2. Incomplete (< threshold) keeps listening, then re-checks on next pause and commits when >= threshold.
	prob.Store(2)
	calls.Store(0)
	b.ws.mu.Lock()
	b.ws.startRecording(b.ws.terminals[id], "", time.Now(), true)
	b.ws.mu.Unlock()
	d.recStart(t, "")
	d.barrier(t)
	waitSVG(t, b.ws, id, "rec-send")
	appendPCM(bytes.Repeat(voiced, 10))
	appendPCM(bytes.Repeat(silent, 28))
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 smartTurn call on first pause, got %d", calls.Load())
	}
	// Now user speaks again and pauses with complete prosody.
	prob.Store(1)
	appendPCM(bytes.Repeat(voiced, 10))
	appendPCM(bytes.Repeat(silent, 28))
	d.until(t, recStop)
	d.recEnd(t, "", false)
	d.until(t, idleOrDone)
	if calls.Load() < 2 {
		t.Fatalf("expected >= 2 smartTurn calls after second pause, got %d", calls.Load())
	}
}

