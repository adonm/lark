package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type testEndpointVAD struct{ tail int }

func (v *testEndpointVAD) detect(frame []byte) bool {
	if frame[0] != 0 {
		v.tail = int(vadTail / endpointFrame)
		return true
	}
	if v.tail > 0 {
		v.tail--
		return true
	}
	return false
}
func (*testEndpointVAD) close() {}

func testEndpointSettings() *endpointSettings {
	return &endpointSettings{newVAD: func() endpointVAD { return &testEndpointVAD{} }}
}

func TestEndpointWaitsForContinuation(t *testing.T) {
	t.Parallel()
	var p, empty endpointPolicy
	p.observeEnergy(20*time.Millisecond, true, 2000)
	p.observeEnergy(230*time.Millisecond, false, 0)
	if !p.observeProgress(250*time.Millisecond, "Hello", false) {
		t.Fatal("first word should advance progress")
	}
	if p.observeProgress(500*time.Millisecond, "um", false) {
		t.Fatal("filler-only output should not advance progress")
	}
	if p.observeProgress(750*time.Millisecond, "Hello", false) {
		t.Fatal("unchanged word count should not advance progress")
	}
	if got := p.decision(250*time.Millisecond+endpointQuiet, endpointCap, endpointLimit, false, false); got != endpointWait {
		t.Fatalf("without smartTurnReady at quiet, got %v, want wait", got)
	}
	if got := p.decision(250*time.Millisecond+endpointQuiet, endpointCap, endpointLimit, true, false); got != endpointCommit {
		t.Fatalf("with smartTurnReady at quiet, got %v, want commit", got)
	}
	if got := p.decision(250*time.Millisecond+endpointCap, endpointCap, endpointLimit, false, false); got != endpointCommit {
		t.Fatalf("at cap without smartTurnReady, got %v, want commit", got)
	}
	// Trailing comma or conjunction/filler blocks early smartTurn commit until cap.
	for _, incomplete := range []string{
		"Hello,", "apples and", "I want to, but", "Wait or", "It was so", "I ran because",
		"Well um", "Wait uh", "Can you put his", "Where is the", "It is a", "A story about an",
		"Tell me of", "Put it in", "Give it to", "The door is", "We were", "Please could",
	} {
		if !p.observeProgress(time.Second, incomplete, false) {
			// Ensure word count is > previous for test or reset
		}
		var q endpointPolicy
		base := 1250 * time.Millisecond
		q.observeEnergy(base, true, 2000)
		q.observeEnergy(base+endpointFrame, false, 0)
		q.observeProgress(base, incomplete, false)
		if !q.incomplete {
			t.Fatalf("%q should be marked incomplete", incomplete)
		}
		if got := q.decision(base+endpointQuiet, endpointCap, endpointLimit, true, false); got != endpointWait {
			t.Fatalf("%q committed early despite incomplete trailing", incomplete)
		}
		if got := q.decision(base+endpointCap, endpointCap, endpointLimit, true, false); got != endpointCommit {
			t.Fatalf("%q did not commit at cap", incomplete)
		}
	}
	if got := empty.decision(endpointIdle, endpointCap, endpointLimit, false, false); got != endpointDiscard {
		t.Fatalf("initial silence did not discard: %v", got)
	}
	if got := empty.decision(endpointIdle, endpointCap, endpointLimit, false, true); got != endpointWait {
		t.Fatalf("listen mode should not discard at endpointIdle: %v", got)
	}
	p.inVoice = true
	if got := p.decision(endpointLimit, endpointCap, endpointLimit, false, false); got != endpointCommit {
		t.Fatalf("maximum duration did not commit: %v", got)
	}
}

func TestEndpointAccumulatorPreservesOriginalPCM(t *testing.T) {
	t.Parallel()
	var u utterance
	vad := &testEndpointVAD{}
	frame := bytes.Repeat([]byte{46, 0}, endpointBytes/2)
	quiet := make([]byte, endpointBytes)
	u.add(frame[:50], vad)
	u.add(frame[50:], vad)
	for range 200 {
		u.add(quiet, vad)
	}
	u.add(frame, vad)
	want := slices.Concat(frame, make([]byte, 200*endpointBytes), frame)
	if !bytes.Equal(u.clip(), want) {
		t.Fatal("accumulated PCM changed")
	}
	if u = (utterance{}); u.clip() != nil {
		t.Fatal("empty capture would enter STT")
	}
}

func TestEndpointWebSendCancelAndAuto(t *testing.T) {
	t.Parallel()
	clips := make(chan []byte, 8)
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.endpoint = testEndpointSettings()
		b.setSTT(func(_ context.Context, pcm []byte) (string, error) {
			clips <- slices.Clone(pcm)
			if bytes.Count(pcm, bytes.Repeat([]byte{17, 0}, endpointBytes/2)) >= 2 {
				return "hello world", nil
			}
			return "hello", nil
		})
	})
	const id = "vocat-e021"
	if _, err := b.createCompanion(t.Context(), object{"id": "echo", "name": "Echo", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	d := testConnectWS(t, base, "unit", false)
	d.helloHardware(t, id, "echo", capBridgeEndpoint|capDrag)
	start := func() {
		b.hub.mu.Lock()
		b.hub.startRecording(b.hub.terminals[id], "", time.Now(), true)
		b.hub.mu.Unlock()
		d.recStart(t, "")
		waitSVG(t, b.hub, id, "rec-send")
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
	idleOrDone := func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgStatus, "")(f) && len(p) >= 5 && (p[4] == statusIdle || p[4] == statusDone || p[4] == statusError)
	}
	frame := bytes.Repeat([]byte{17, 0}, endpointBytes/2)
	start()
	appendPCM(frame)
	d.recEnd(t, "", true)
	// The cancel chime is emitted exactly when the cancelled capture ends, so
	// it is the completion signal. An idle status can also arrive earlier from
	// rec_start, which made the next recording race a late rec_end.
	d.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgChime, "")(f) && len(p) >= 1 && p[0] == chimeCancel
	})
	d.until(t, idleOrDone)
	start()
	appendPCM(frame)
	d.recEnd(t, "", false) // Browser Send now flushes then sends rec_end.
	d.until(t, idleOrDone)
	select {
	case got := <-clips:
		if !bytes.Equal(got, frame) {
			t.Fatal("Send did not commit the original single frame exactly once")
		}
	default:
		t.Fatal("Send did not transcribe")
	}
	start()
	appendPCM(frame)
	quiet := make([]byte, endpointBytes*4)
	for range 8 { // 640ms < cap (1200ms)
		appendPCM(quiet)
	}
	appendPCM(frame)
	d.barrier(t)
	b.hub.mu.Lock()
	stillListening := b.hub.terminals[id].recording.phase == captureOpen
	b.hub.mu.Unlock()
	if !stillListening {
		t.Fatal("bridge stopped before the continuation")
	}
	for range 20 { // 1600ms > cap (1200ms)
		appendPCM(quiet)
	}
	d.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpStop
	})
	d.recEnd(t, "", false)
	d.until(t, idleOrDone)
	var last []byte
	for len(clips) != 0 {
		last = <-clips
	}
	if last == nil {
		t.Fatal("automatic endpoint did not transcribe")
	}
	if voices := bytes.Count(last, frame); voices != 2 {
		t.Fatalf("final clip did not contain both clauses (got %d)", voices)
	}
	b.diag.sig.wait(t, func() bool {
		b.diag.mu.Lock()
		defer b.diag.mu.Unlock()
		return len(b.diag.metrics[diagEndpoint].items) >= 1
	})
	b.diag.mu.Lock()
	n := len(b.diag.metrics[diagEndpoint].items)
	b.diag.mu.Unlock()
	if n != 1 {
		t.Fatalf("got %d end-to-commit samples, want 1", n)
	}
}

func TestEndpointSpeculatesWithoutClosingMicAndReusesLatestPause(t *testing.T) {
	t.Parallel()
	frame := bytes.Repeat([]byte{17, 0}, endpointBytes/2)
	first := make(chan struct{}, 1)
	second := make(chan int, 1)
	var decodes atomic.Int32
	b, base := wsHarness(t, func(b *larkServer, _ string) {
		b.endpoint = testEndpointSettings()
		b.setSTT(func(ctx context.Context, pcm []byte) (string, error) {
			switch decodes.Add(1) {
			case 1:
				first <- struct{}{}
				return "Where is that dinosaur?", nil
			case 2:
				second <- bytes.Count(pcm, frame)
				return "Where is that dinosaur hiding?", nil
			default:
				t.Error("already transcribed the silence; do not decode it again")
				return "", nil
			}
		})
	})
	const id = "vocat-e025"
	if _, err := b.createCompanion(t.Context(), object{"id": "echo", "name": "Echo", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	d := testConnectWS(t, base, "unit", false)
	d.helloHardware(t, id, "echo", capBridgeEndpoint|capDrag)
	b.hub.mu.Lock()
	b.hub.startRecording(b.hub.terminals[id], "", time.Now(), true)
	b.hub.mu.Unlock()
	d.recStart(t, "")
	waitSVG(t, b.hub, id, "rec-send")
	var seq uint16
	appendPCM := func(p []byte) {
		for len(p) > 0 {
			n := min(2*pcmPacket, len(p))
			seq++
			d.sendRTP(t, rtpPTRecPCM, seq, uint32(seq)*320, p[:n])
			p = p[n:]
		}
	}
	quiet := make([]byte, 4*endpointBytes)
	appendPCM(frame)
	for range 4 { // 320ms
		appendPCM(quiet)
	}
	expectValue(t, first, struct{}{})
	b.hub.mu.Lock()
	stillListening := b.hub.terminals[id].recording.phase == captureOpen
	b.hub.mu.Unlock()
	if !stillListening {
		t.Fatal("speculation changed the mic state")
	}
	appendPCM(frame)
	for range 4 { // 320ms (> 250ms probe interval)
		appendPCM(quiet)
	}
	expectValue(t, second, 2)
	for range 15 { // 1200ms -> cap
		appendPCM(quiet)
	}
	d.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpStop
	})
	d.recEnd(t, "", false)
	b.diag.sig.wait(t, func() bool {
		b.diag.mu.Lock()
		defer b.diag.mu.Unlock()
		return len(b.diag.metrics[diagEndpoint].items) >= 1
	})
	b.diag.mu.Lock()
	n := len(b.diag.metrics[diagEndpoint].items)
	b.diag.mu.Unlock()
	if n != 1 {
		t.Fatalf("got %d end-to-commit samples, want 1", n)
	}
	if decodes.Load() != 2 {
		t.Fatal("finalization did not reuse the latest valid pause")
	}
}

func TestEndpointHardwareNegotiation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ model, cap, want bool }{
		{false, true, false}, {true, false, false}, {true, true, true},
	} {
		t.Run(fmt.Sprintf("model=%t cap=%t", tc.model, tc.cap), func(t *testing.T) {
			b, base := wsHarness(t, func(b *larkServer, _ string) {
				if tc.model {
					b.endpoint = testEndpointSettings()
				}
			})
			d := testConnectWS(t, base, "unit", false)
			var caps uint32 = capDrag
			if tc.cap {
				caps |= capBridgeEndpoint
			}
			d.helloHardware(t, "vocat-ep00", "", caps)
			b.hub.mu.Lock()
			b.hub.startRecording(b.hub.terminals["vocat-ep00"], "", time.Now(), true)
			b.hub.mu.Unlock()
			f := d.until(t, func(f protocolFrame) bool {
				p := framePayload(f)
				return hasProto(msgRecCtrl, "")(f) && len(p) >= 2 && p[0] == recOpStart
			})
			p := framePayload(f)
			if got := p[1]&recTargetBridgeE != 0; got != tc.want {
				t.Fatalf("negotiation: flags=%x want=%t", p[1], tc.want)
			}
		})
	}
}

func TestTargetedRecordingKeepsLegacyEndpoint(t *testing.T) {
	t.Parallel()
	b, base := wsHarness(t, func(b *larkServer, _ string) { b.endpoint = testEndpointSettings() })
	const id = "vocat-ep01"
	d := testConnectWS(t, base, "unit", false)
	d.helloHardware(t, id, "", capBridgeEndpoint|capDrag)
	b.hub.mu.Lock()
	dev := b.hub.terminals[id]
	b.hub.startRecording(dev, "all", time.Now(), true)
	b.hub.mu.Unlock()
	start := d.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "all")(f) && len(p) >= 2 && p[0] == recOpStart
	})
	if framePayload(start)[1]&recTargetBridgeE != 0 {
		t.Fatal("intercom should still use the client VAD")
	}
}

func TestEndpointHoldsUnpublishedPCMUntilMicCloses(t *testing.T) {
	t.Parallel()
	b, base := wsHarness(t, func(b *larkServer, _ string) { b.endpoint = testEndpointSettings() })
	const id = "vocat-e023"
	if _, err := b.createCompanion(t.Context(), object{"id": "echo", "name": "Echo", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	d := testConnectWS(t, base, "unit", false)
	d.helloHardware(t, id, "echo", capBridgeEndpoint|capDrag)
	b.hub.mu.Lock()
	b.hub.startRecording(b.hub.terminals[id], "", time.Now(), true)
	b.hub.mu.Unlock()
	d.recStart(t, "")
	waitSVG(t, b.hub, id, "rec-send")
	d.barrier(t)
	stream := newAudioStream()
	stream.append([]byte{8, 0, 9, 0})
	stream.finish(nil)
	b.hub.mu.Lock()
	dev := b.hub.terminals[id]
	b.hub.play(dev, stream)
	b.hub.mu.Unlock()
	d.recEnd(t, "", true)
	cleared := false
	d.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		if hasProto(msgChime, "")(f) && len(p) >= 1 && p[0] == chimeCancel {
			cleared = true
		}
		if isPlayback(f) && !cleared {
			t.Fatal("PCM escaped while recording remained open")
		}
		return isPlayback(f)
	})
}

func TestStopDiscardsQueuedEndpoint(t *testing.T) {
	t.Parallel()
	b, base := wsHarness(t, func(b *larkServer, _ string) { b.endpoint = testEndpointSettings() })
	const id = "vocat-e024"
	d := testConnectWS(t, base, "unit", false)
	d.hello(t, id, "")
	b.hub.mu.Lock()
	dev := b.hub.terminals[id]
	ctx, cancel := context.WithCancel(dev.client.ctx)
	dev.busy = true
	dev.queuedVoice = &endpointCapture{ctx: ctx, cancel: cancel}
	for range 3 {
		b.hub.tapStop(dev, time.Now())
	}
	busy, queued := dev.busy, dev.queuedVoice
	b.hub.mu.Unlock()
	if ctx.Err() == nil || busy || queued != nil {
		t.Fatal("explicit Stop did not discard both the active and queued turns")
	}
}

func TestEndpointQueuesWhileOpenCodeOwnsTurn(t *testing.T) {
	t.Parallel()
	const sid, id = "ses-followup", "vocat-e022"
	prompts := make(chan string, 2)
	finishFirst := make(chan struct{})
	var count atomic.Int32
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.endpoint = testEndpointSettings()
		b.setSTT(func(context.Context, []byte) (string, error) { return "and add stars", nil })
		b.oc = testOpenCode(t, b.root, dir, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/session", "/api/session/" + sid:
				sendJSON(w, 200, object{"data": object{"id": sid}})
			case "/api/experimental/session/" + sid + "/instructions/entries/lark.companion":
				w.WriteHeader(http.StatusNoContent)
			case "/api/session/" + sid + "/agent", "/api/session/" + sid + "/model":
				w.WriteHeader(http.StatusNoContent)
			case "/api/session/" + sid + "/prompt":
				var input struct{ Text, Delivery string }
				_ = json.NewDecoder(r.Body).Decode(&input)
				if input.Delivery == "steer" {
					t.Error("unverified steering must stay off")
				}
				i := count.Add(1)
				prompts <- strings.TrimSpace(stripLarkHeader(input.Text))
				fmt.Fprintf(w, `{"data":{"id":"prompt%d"}}`, i)
			case "/api/experimental/session/" + sid + "/wait":
				select {
				case <-finishFirst:
				case <-r.Context().Done():
				}
				w.WriteHeader(http.StatusNoContent)
			case "/api/session/" + sid + "/message":
				fmt.Fprintf(w, `{"data":[{"id":"answer","type":"assistant","finish":"stop","content":[{"type":"text","text":"Ready."}]},{"id":"prompt%d","type":"user"}]}`, count.Load())
			case "/api/session/" + sid + "/interrupt":
				t.Error("a voice follow-up must not interrupt the prompt gate owner")
				w.WriteHeader(http.StatusNoContent)
			default:
				http.NotFound(w, r)
			}
		})
	})
	if _, err := b.createCompanion(t.Context(), object{"id": "echo", "name": "Echo", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	d := testConnectWS(t, base, "unit", false)
	d.helloHardware(t, id, "echo", capBridgeEndpoint|capDrag)
	b.hub.mu.Lock()
	b.hub.startTurn(b.hub.terminals[id], time.Now(), false, "voice", func(ctx context.Context, turn uint64) {
		b.hub.runTurn(ctx, b.hub.terminals[id], turn, "Draw a dragon", "voice", nil)
	})
	b.hub.mu.Unlock()
	expectValue(t, prompts, "Draw a dragon")
	b.hub.mu.Lock()
	b.hub.startRecording(b.hub.terminals[id], "", time.Now(), true)
	b.hub.mu.Unlock()
	d.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 2 && p[0] == recOpStart && p[1]&recTargetBridgeE != 0
	})
	d.recStart(t, "")
	pcm := make([]byte, endpointBytes)
	pcm[0] = 18
	d.sendRTP(t, rtpPTRecPCM, 1, 320, pcm)
	d.recEnd(t, "", false)
	d.barrier(t)
	select {
	case <-prompts:
		t.Fatal("follow-up bypassed gate")
	default:
	}
	close(finishFirst)
	expectValue(t, prompts, "and add stars")
	d.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgStatus, "")(f) && len(p) >= 5 && (p[4] == statusIdle || p[4] == statusDone)
	})
	b.hub.sig.wait(t, func() bool {
		b.hub.mu.Lock()
		defer b.hub.mu.Unlock()
		return !b.hub.terminals[id].busy
	})
	if count.Load() != 2 {
		t.Fatal("prompt was replayed")
	}
}

func TestListenCaptureCapCoversWholeWindow(t *testing.T) {
	t.Parallel()
	var u utterance
	span := 305 * time.Second
	u.limit = utteranceBytes(span)
	vad := &testEndpointVAD{}
	second := bytes.Repeat([]byte{12}, sampleRate*2)
	for range 70 {
		u.add(second, vad)
	}
	if got := len(u.pcm); got != 70*sampleRate*2 {
		t.Fatalf("listen capture dropped speech after %.0fs: %d bytes", pcmDuration(got).Seconds(), got)
	}
	for range 300 {
		u.add(second, vad)
	}
	if got := len(u.pcm); got != utteranceBytes(span) {
		t.Fatalf("listen capture not bounded by the window: %d bytes", got)
	}
}

func TestEndpointRingPushDoesNotAllocate(t *testing.T) {
	serial(t)
	free := make(chan int, 1)
	free <- 0
	c := &endpointCapture{
		free:    free,
		packets: make(chan micPacket, 1),
		slots:   new(endpointRing),
	}
	pcm := make([]byte, endpointSlotBytes)
	allocs := testing.AllocsPerRun(100, func() {
		if !c.push(pcm, time.Now()) {
			t.Fatal("push rejected")
		}
		pkt := <-c.packets
		if len(pkt.pcm) != len(pcm) {
			t.Fatal("short packet")
		}
		c.free <- pkt.slot
	})
	if allocs != 0 {
		t.Fatalf("ring push allocated %v times/op, want 0", allocs)
	}
}

func TestEndpointQuietWindowNeedsSmartTurnOnly(t *testing.T) {
	t.Parallel()
	var p endpointPolicy
	p.observeEnergy(20*time.Millisecond, true, 2000)
	p.observeEnergy(230*time.Millisecond, false, 0)
	p.observeProgress(250*time.Millisecond, "Tell me everything about all the dinosaurs", false)
	quietAt := p.lastVoicedAt + endpointQuiet
	if got := p.decide(quietAt-endpointFrame, endpointCap, endpointLimit, 0.90, false); got != endpointWait {
		t.Fatalf("before the quiet window, got %v, want wait", got)
	}
	if got := p.decide(quietAt, endpointCap, endpointLimit, 0.90, false); got != endpointCommit {
		t.Fatalf("quiet window with Smart Turn, got %v, want commit", got)
	}
	if got := p.decide(quietAt, endpointCap, endpointLimit, 0.89, false); got != endpointWait {
		t.Fatalf("quiet window without Smart Turn, got %v, want wait", got)
	}
}

func TestCommittedEndpointKeepsItsResult(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	c := &endpointCapture{ctx: ctx, cancel: cancel, committing: true}
	d := &deviceCapture{endpoint: c}
	d.clear()
	if ctx.Err() != nil {
		t.Fatal("clear cancelled a capture that was already committing its result")
	}
	// A cancelled capture must still stop its worker.
	ctx2, cancel2 := context.WithCancel(context.Background())
	c2 := &endpointCapture{ctx: ctx2, cancel: cancel2}
	d2 := &deviceCapture{endpoint: c2}
	d2.clear()
	if ctx2.Err() == nil {
		t.Fatal("cancelled capture left its worker running")
	}
}

// A listening window can run for minutes before speech; the policy clock must
// keep counting past the audio that the 400 ms pre-roll discarded.
func TestListenPreRollKeepsPolicyClock(t *testing.T) {
	t.Parallel()
	var u utterance
	vad := &testEndpointVAD{}
	quiet := make([]byte, endpointBytes)
	for range 500 { // 10 s of room tone before speech
		u.addFor(quiet, vad, true)
	}
	voiced := make([]byte, endpointBytes)
	voiced[0], voiced[1] = 0xE8, 0x03 // 1000 RMS counts
	for i := 1; i <= 1000; i++ {      // 20 s of monologue
		u.addFor(voiced, vad, true)
		if i < 1000 {
			continue
		}
		at := u.at(len(u.pcm))
		if span := u.policy.lastVoicedAt - u.policy.firstVoicedAt; span < endpointMonologue {
			t.Fatalf("speech span = %v, want >= %v (first=%v last=%v at=%v)", span, endpointMonologue,
				u.policy.firstVoicedAt, u.policy.lastVoicedAt, at)
		}
		if got := u.policy.decide(at, endpointCap, endpointLimit, 0, true); got != endpointCommit {
			t.Fatalf("20 s monologue after 10 s pre-roll: decision=%v, want commit", got)
		}
	}
	if u.policy.firstVoicedAt < 9*time.Second {
		t.Fatalf("firstVoicedAt = %v, want the pre-roll clock (~10 s)", u.policy.firstVoicedAt)
	}
}

// A packet can straddle the VAD frame where speech starts. Splicing the whole
// packet into the pre-roll would append the post-frame bytes twice.
func TestListenPreRollSpliceDoesNotDuplicate(t *testing.T) {
	t.Parallel()
	var u utterance
	vad := &testEndpointVAD{}
	quiet := make([]byte, 600*time.Millisecond.Milliseconds()*sampleRate*2/1000)
	voiced := make([]byte, 600*time.Millisecond.Milliseconds()*sampleRate*2/1000)
	for i := 0; i < len(voiced); i += 2 {
		voiced[i], voiced[i+1] = 0xE8, 0x03
	}
	signal := slices.Concat(quiet, voiced)
	const chunk = 8192 // not a multiple of endpointBytes
	for pos := 0; pos < len(signal); pos += chunk {
		u.addFor(signal[pos:min(pos+chunk, len(signal))], vad, true)
	}
	want := slices.Concat(quiet[len(quiet)-listenPreRollBytes:], voiced)
	if !bytes.Equal(u.clip(), want) {
		t.Fatalf("clip = %d bytes, want %d (duplicated splice)", len(u.clip()), len(want))
	}
}

// A terminal sentence marks the thought complete even when its last word is a
// function word ("that"), which would otherwise look like a dangling clause.
func TestInspectTranscriptTerminalPunctuation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		text       string
		incomplete bool
	}{
		{"I like that.", false},
		{"I like that?", false},
		{"I like that!", false},
		{"I like that", true},
		{"Can you put his", true},
		{"Is it blue,", true},
		{"Wait.", false},
		{"um", false},
		{"Show me a giraffe, um.", true},
	} {
		words, filler, incomplete := inspectTranscript(tc.text)
		if filler && words == 0 {
			continue
		}
		if incomplete != tc.incomplete {
			t.Fatalf("inspectTranscript(%q).incomplete = %v, want %v", tc.text, incomplete, tc.incomplete)
		}
	}
}

// A capture belongs to the session the bridge requested. A stale ack must not
// open the next capture, and a cancelled end for a start the board never
// acknowledged must still close it.
func TestRecordingSessionsGateAcksAndEnds(t *testing.T) {
	t.Parallel()
	b, base := wsHarness(t, func(*larkServer, string) {})
	if _, err := b.createCompanion(t.Context(), object{"id": "echo", "name": "Echo", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	const id = "vocat-e022"
	d := testConnectWS(t, base, "unit", false)
	d.helloHardware(t, id, "echo", capDrag)
	phase := func() capturePhase {
		b.hub.mu.Lock()
		defer b.hub.mu.Unlock()
		return b.hub.terminals[id].recording.phase
	}
	start := func() {
		b.hub.mu.Lock()
		b.hub.startRecording(b.hub.terminals[id], "", time.Now(), true)
		b.hub.mu.Unlock()
		if got := phase(); got != captureRequested {
			t.Fatalf("phase after request = %v, want captureRequested", got)
		}
	}
	ack := func(session uint16) {
		p := binary.LittleEndian.AppendUint16(nil, session)
		d.sendProto(t, msgRecStart, appendStr8(p, ""))
	}
	end := func(session uint16, cancelled bool) {
		p := binary.LittleEndian.AppendUint16(nil, session)
		var c byte
		if cancelled {
			c = 1
		}
		d.sendProto(t, msgRecEnd, append(append(p, c), appendStr8(nil, "")...))
	}

	start()
	ack(1)
	d.barrier(t)
	if got := phase(); got != captureOpen {
		t.Fatalf("phase after matching ack = %v, want captureOpen", got)
	}
	end(1, true)
	d.barrier(t)
	if got := phase(); got != captureIdle {
		t.Fatalf("phase after cancelled end = %v, want idle", got)
	}

	start() // session 2
	ack(1)  // stale ack from the previous capture
	d.barrier(t)
	if got := phase(); got != captureRequested {
		t.Fatalf("stale ack opened the capture: phase = %v", got)
	}
	ack(2)
	d.barrier(t)
	if got := phase(); got != captureOpen {
		t.Fatalf("matching ack did not open the capture: phase = %v", got)
	}
	end(2, true)
	d.barrier(t)

	// The board failed to open the mic: it reports a cancelled end while the
	// host is still waiting for the start ack.
	start() // session 3
	end(3, true)
	d.barrier(t)
	if got := phase(); got != captureIdle {
		t.Fatalf("failed start stayed active: phase = %v", got)
	}
}
