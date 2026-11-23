package main

import (
	"bytes"
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// feedFrames advances a policy by n 20 ms frames and returns the next time.
func feedFrames(p *endpointPolicy, at time.Duration, n int, vad bool, rms float64) time.Duration {
	for range n {
		at += endpointFrame
		p.observeEnergy(at, vad, rms)
	}
	return at
}

// Quiet, distant speech over a room floor must open the gate and end the
// segment on silence, not on transcript text.
func TestEndpointQuietSpeechOverNoiseCommitsOnSilence(t *testing.T) {
	t.Parallel()
	var p endpointPolicy
	at := feedFrames(&p, 0, 100, false, 3) // 2 s room noise, floor 3
	at = feedFrames(&p, at, 50, true, 12)  // 1 s quiet speech
	if !p.voiced || p.speechSpan() < time.Second {
		t.Fatalf("quiet speech did not start a segment: voiced=%v span=%v", p.voiced, p.speechSpan())
	}
	p.observeProgress(at, "turn on the light please", false)
	at = feedFrames(&p, at, 15, true, 3) // VAD tail/noise, below the gate
	if p.inVoice {
		t.Fatal("steady noise kept the gate open")
	}
	if got := p.decide(p.lastVoicedAt+200*time.Millisecond, endpointCap, endpointLimit, 0.99, false, true); got != endpointWait {
		t.Fatalf("committed inside the quiet span: %v", got)
	}
	if got := p.decide(p.lastVoicedAt+endpointQuiet, endpointCap, endpointLimit, 0.99, false, true); got != endpointCommit {
		t.Fatalf("quiet span did not commit: %v", got)
	}
}

// VAD false positives on steady room noise must not form a segment or hold a
// listen window open.
func TestEndpointNoiseOnlyNeverOpensSegment(t *testing.T) {
	t.Parallel()
	var p endpointPolicy
	at := feedFrames(&p, 0, 500, true, 3) // 10 s of VAD-positive noise
	if p.voiced {
		t.Fatalf("noise-only opened a segment (floor %.1f, lastVoicedAt %v)", p.gate.floor, p.lastVoicedAt)
	}
	if got := p.decide(at, endpointCap, at, 0.99, false, true); got != endpointDiscard {
		t.Fatalf("noise-only window did not discard: %v", got)
	}
}

// Parakeet can invent new words on non-speech audio. Those guesses must not
// move the silence deadline.
func TestEndpointTranscriptChurnCannotExtendSilence(t *testing.T) {
	t.Parallel()
	var p endpointPolicy
	at := feedFrames(&p, 0, 50, false, 2)
	at = feedFrames(&p, at, 25, true, 20)
	end := at
	at = feedFrames(&p, at, 10, true, 2) // gate closes 200 ms after the words
	for i := range 40 {
		p.observeProgress(end+time.Duration(i+1)*endpointProbeEvery, fmt.Sprintf("invented words keep arriving %d", i), false)
	}
	if !p.hasWords {
		t.Fatal("test setup lost the transcript")
	}
	if got := p.decide(end+endpointQuiet+endpointQuiet, endpointCap, endpointLimit, 0.95, false, true); got != endpointCommit {
		t.Fatalf("transcript churn extended silence past the quiet span: %v", got)
	}
}

// A monologue gets a turn at 20 s even if it never pauses.
func TestEndpointMonologueCommitsAtCap(t *testing.T) {
	t.Parallel()
	var p endpointPolicy
	at := feedFrames(&p, 0, 10, false, 2)
	at = feedFrames(&p, at, 1100, true, 40) // 22 s of continuous speech
	p.observeProgress(at, "a long story without any pauses at all", false)
	if got := p.decide(at, endpointCap, 60*time.Second, 0.1, false, true); got != endpointCommit {
		t.Fatalf("22 s monologue did not commit: %v", got)
	}
	if got := p.decide(endpointMonologue+endpointFrame, endpointCap, 60*time.Second, 0.1, false, true); got != endpointCommit {
		t.Fatalf("monologue cap did not commit: %v", got)
	}
}

// A window with no gated speech closes on its own span and never sends a
// hallucinated turn.
func TestListenWindowNoiseOnlyClosesWithoutTurn(t *testing.T) {
	serial(t)
	old := listenWindow
	listenWindow = 2 * time.Second
	t.Cleanup(func() { listenWindow = old })

	b, base, prompts := listenHarness(t, func(string) string { return "Okay." })
	hw, _ := setupListenTerminal(t, b, base, "box3-noise", "Greg", true)
	var sttCalls atomic.Int32
	b.setSTT(func(context.Context, []byte) (string, error) {
		n := sttCalls.Add(1)
		return fmt.Sprintf("hallucinated words number %d", n), nil
	})
	b.hub.mu.Lock()
	if !b.hub.startListeningWindow(b.hub.terminals["box3-noise"], time.Now()) {
		b.hub.mu.Unlock()
		t.Fatal("listening window did not open")
	}
	b.hub.mu.Unlock()
	hw.until(t, isListenRecStart)
	hw.recStart(t, "")

	noise := bytes.Repeat([]byte{3, 0}, endpointBytes/2) // VAD-positive, below the floor margin
	var seq uint16
	for range 300 { // 6 s, longer than the shortened window
		seq++
		hw.sendRTP(t, rtpPTRecPCM, seq, uint32(seq)*320, noise)
	}
	hw.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpCancel
	})
	hw.barrier(t)
	if len(*prompts) != 0 || sttCalls.Load() != 0 {
		t.Fatalf("noise-only window produced %d prompts and %d STT calls", len(*prompts), sttCalls.Load())
	}
	got := b.diag.listen.droppedGate.Load()
	for range 50 {
		if got = b.diag.listen.droppedGate.Load(); got > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got == 0 {
		t.Fatal("noise-only window was not counted as a gate drop")
	}
}

// Once speech is heard, hallucinated probe text on the following noise must
// not keep the turn open: it commits within the quiet span and sends once.
func TestListenSpeechThenNoiseCommitsOnce(t *testing.T) {
	t.Parallel()
	b, base, prompts := listenHarness(t, func(string) string { return "Okay." })
	hw, _ := setupListenTerminal(t, b, base, "box3-churn", "Greg", true)
	var calls atomic.Int32
	b.setSTT(func(context.Context, []byte) (string, error) {
		n := calls.Add(1)
		return fmt.Sprintf("invented words keep arriving number %d", n), nil
	})
	b.hub.mu.Lock()
	if !b.hub.startListeningWindow(b.hub.terminals["box3-churn"], time.Now()) {
		b.hub.mu.Unlock()
		t.Fatal("listening window did not open")
	}
	b.hub.mu.Unlock()
	hw.until(t, isListenRecStart)
	hw.recStart(t, "")

	speech := bytes.Repeat([]byte{0xd0, 0x07}, endpointBytes/2) // 2000 RMS
	noise := bytes.Repeat([]byte{3, 0}, endpointBytes/2)        // 3 RMS, VAD-positive
	var seq uint16
	send := func(p []byte) {
		seq++
		hw.sendRTP(t, rtpPTRecPCM, seq, uint32(seq)*320, p)
	}
	for range 20 { // 400 ms speech
		send(speech)
	}
	for range 100 { // 2 s noise; commit must arrive well before this
		send(noise)
	}
	hw.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpStop
	})
	hw.recEnd(t, "", false)
	hw.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == ttsOpEnd
	})
	hw.barrier(t)
	if len(*prompts) != 1 {
		t.Fatalf("got %d prompts from one segment, want 1", len(*prompts))
	}
}
