package main

import (
	"bytes"
	"context"
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
	for _, text := range []string{"", "Once upon a time there was a frog, and then.", "Could you set a timer for?",
		"Draw a purple frog, um.", "Try doing it with the...", "This is something I want to", "First I want a castle."} {
		var p endpointPolicy
		p.observe(time.Second, true)
		if got := p.decision(time.Second, tunedEndpoint.silence(p, text)); got != endpointWait {
			t.Fatal(got)
		}
		p.observe(5*time.Second, false)
		if got := p.decision(5*time.Second, tunedEndpoint.silence(p, text)); got != endpointWait {
			t.Fatalf("%q prematurely committed at 4-second pause", text)
		}
		p.observe(5200*time.Millisecond, true)
		p.observe(6*time.Second, false)
		if got := p.decision(6*time.Second, tunedEndpoint.silence(p, text)); got != endpointWait {
			t.Fatalf("%q kept the old speech deadline", text)
		}
		if got := p.decision(9600*time.Millisecond, tunedEndpoint.silence(p, text)); got != endpointCommit {
			t.Fatalf("%q did not commit after the new end", text)
		}
	}
	var p, empty endpointPolicy
	p.observe(time.Second, true)
	p.observe(2*time.Second, false)
	for _, tc := range []struct {
		text string
		wait time.Duration
	}{
		{"Start a timer for ninety seconds.", tunedEndpoint.quick},
		{"Where do the ducks go?", tunedEndpoint.quick},
		{"What's eight?", tunedEndpoint.quick},
		{"How are you?", tunedEndpoint.quick},
		{"Can you draw a dragon?", tunedEndpoint.complete},
		{"Tell me a knock knock joke!", tunedEndpoint.complete},
		{"Start a timer for 90 seconds", tunedEndpoint.uncertain},
		{"I found a crystal.", tunedEndpoint.patient},
		{"When I was nine?", tunedEndpoint.patient},
	} {
		if got := tunedEndpoint.silence(p, tc.text); got != tc.wait {
			t.Errorf("%q scored %v, want %v", tc.text, got, tc.wait)
		}
	}
	p.last = p.first + tunedEndpoint.storySpan
	if got := tunedEndpoint.silence(p, "Where do the ducks go?"); got != tunedEndpoint.patient {
		t.Fatalf("long story scored %v, want patient", got)
	}
	if got := empty.decision(endpointIdle, tunedEndpoint.patient); got != endpointDiscard {
		t.Fatalf("initial silence did not discard: %v", got)
	}
	p.inVoice = true
	if got := p.decision(endpointLimit, tunedEndpoint.patient); got != endpointCommit {
		t.Fatalf("maximum duration did not commit: %v", got)
	}
}

func TestEndpointAccumulatorPreservesOriginalPCM(t *testing.T) {
	var u utterance
	vad := &testEndpointVAD{}
	frame := bytes.Repeat([]byte{46, 0}, endpointBytes/2)
	quiet := make([]byte, endpointBytes)
	u.add(frame[:50], vad)
	u.add(frame[50:], vad)
	for range 200 {
		u.add(quiet, vad)
		if got := u.policy.decision(pcmDuration(len(u.pcm)), tunedEndpoint.patient); got != endpointWait {
			t.Fatalf("unexpected decision %v", got)
		}
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
	clips := make(chan []byte, 8)
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.endpoint = testEndpointSettings()
		b.stt = func(_ context.Context, pcm []byte) (string, error) {
			clips <- slices.Clone(pcm)
			return "", nil
		}
	})
	const id = "vocat-e021"
	if _, err := b.createCompanion(t.Context(), object{"id": "echo", "name": "Echo", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	d := testConnectWS(t, base, "unit", false)
	d.helloHardware(t, id, "echo", capBridgeEndpoint|capDrag)
	start := func() {
		b.ws.mu.Lock()
		b.ws.startRecording(b.ws.terminals[id], "", time.Now(), true)
		b.ws.mu.Unlock()
		d.recStart(t, "")
		waitSVG(t, b.ws, id, "rec-send")
	}
	var seq uint16
	appendPCM := func(p []byte) {
		for len(p) > 0 {
			n := min(2*pcmPacket, len(p))
			seq++
			d.sendRTP(t, rtpPTRecPCM, seq, uint32(seq)*320, 1, p[:n])
			p = p[n:]
		}
	}
	idleOrDone := func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgStatus, "")(f) && len(p) >= 5 && (p[4] == statusIdle || p[4] == statusDone)
	}
	frame := bytes.Repeat([]byte{17, 0}, endpointBytes/2)
	start()
	appendPCM(frame)
	d.recEnd(t, "", true)
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
	case <-time.After(2 * time.Second):
		t.Fatal("Send did not transcribe")
	}
	start()
	appendPCM(frame)
	quiet := make([]byte, endpointBytes*4)
	for range 55 {
		appendPCM(quiet)
	}
	appendPCM(frame) // 4.4 seconds of silence is not an endpoint in unknown speech.
	d.barrier(t)
	b.ws.mu.Lock()
	stillListening := b.ws.terminals[id].recording.phase == captureOpen
	b.ws.mu.Unlock()
	if !stillListening {
		t.Fatal("bridge stopped before the continuation")
	}
	for range 60 {
		appendPCM(quiet)
	}
	d.until(t, func(f wsFrame) bool {
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
	b.diag.mu.Lock()
	n := len(b.diag.metrics[diagEndpoint].items)
	b.diag.mu.Unlock()
	if n != 1 {
		t.Fatalf("got %d end-to-commit samples, want 1 for the observed endpoint", n)
	}
}

func TestEndpointSpeculatesWithoutClosingMicAndReusesLatestPause(t *testing.T) {
	frame := bytes.Repeat([]byte{17, 0}, endpointBytes/2)
	first := make(chan struct{}, 1)
	firstCanceled := make(chan struct{}, 1)
	second := make(chan int, 1)
	var decodes atomic.Int32
	b, base := wsHarness(t, func(b *larkServer, _ string) {
		b.endpoint = testEndpointSettings()
		b.stt = func(ctx context.Context, pcm []byte) (string, error) {
			switch decodes.Add(1) {
			case 1:
				first <- struct{}{}
				<-ctx.Done()
				firstCanceled <- struct{}{}
				// A stale completion must not shorten the next pause.
				return "Where is that dinosaur?", nil
			case 2:
				second <- bytes.Count(pcm, frame)
				return "Where is that dinosaur hiding?", nil
			default:
				t.Error("already transcribed the silence; do not decode it again")
				return "", nil
			}
		}
	})
	const id = "vocat-e025"
	if _, err := b.createCompanion(t.Context(), object{"id": "echo", "name": "Echo", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	d := testConnectWS(t, base, "unit", false)
	d.helloHardware(t, id, "echo", capBridgeEndpoint|capDrag)
	b.ws.mu.Lock()
	b.ws.startRecording(b.ws.terminals[id], "", time.Now(), true)
	b.ws.mu.Unlock()
	d.recStart(t, "")
	waitSVG(t, b.ws, id, "rec-send")
	var seq uint16
	appendPCM := func(p []byte) {
		for len(p) > 0 {
			n := min(2*pcmPacket, len(p))
			seq++
			d.sendRTP(t, rtpPTRecPCM, seq, uint32(seq)*320, 1, p[:n])
			p = p[n:]
		}
	}
	quiet := make([]byte, 4*endpointBytes)
	appendPCM(frame)
	for range 8 {
		appendPCM(quiet)
	}
	expectValue(t, first, struct{}{})
	b.ws.mu.Lock()
	stillListening := b.ws.terminals[id].recording.phase == captureOpen
	b.ws.mu.Unlock()
	if !stillListening {
		t.Fatal("speculation changed the mic state")
	}
	appendPCM(frame)
	expectValue(t, firstCanceled, struct{}{})
	for range 8 {
		appendPCM(quiet)
	}
	expectValue(t, second, 2)
	for range 5 {
		appendPCM(quiet)
	}
	d.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpStop
	})
	d.recEnd(t, "", false)
	b.diag.sig.wait(t, func() bool {
		b.diag.mu.Lock()
		defer b.diag.mu.Unlock()
		return len(b.diag.metrics[diagEndpoint].items) == 1
	})
	if decodes.Load() != 2 {
		t.Fatal("finalization did not reuse the latest valid pause")
	}
}

func TestEndpointHardwareNegotiation(t *testing.T) {
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
			b.ws.mu.Lock()
			b.ws.startRecording(b.ws.terminals["vocat-ep00"], "", time.Now(), true)
			b.ws.mu.Unlock()
			f := d.until(t, func(f wsFrame) bool {
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
	b, base := wsHarness(t, func(b *larkServer, _ string) { b.endpoint = testEndpointSettings() })
	const id = "vocat-ep01"
	d := testConnectWS(t, base, "unit", false)
	d.helloHardware(t, id, "", capBridgeEndpoint|capDrag)
	b.ws.mu.Lock()
	dev := b.ws.terminals[id]
	b.ws.startRecording(dev, "all", time.Now(), true)
	b.ws.mu.Unlock()
	start := d.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "all")(f) && len(p) >= 2 && p[0] == recOpStart
	})
	if framePayload(start)[1]&recTargetBridgeE != 0 {
		t.Fatal("intercom should still use the client VAD")
	}
}

func TestEndpointHoldsUnpublishedPCMUntilMicCloses(t *testing.T) {
	b, base := wsHarness(t, func(b *larkServer, _ string) { b.endpoint = testEndpointSettings() })
	const id = "vocat-e023"
	if _, err := b.createCompanion(t.Context(), object{"id": "echo", "name": "Echo", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	d := testConnectWS(t, base, "unit", false)
	d.helloHardware(t, id, "echo", capBridgeEndpoint|capDrag)
	b.ws.mu.Lock()
	b.ws.startRecording(b.ws.terminals[id], "", time.Now(), true)
	b.ws.mu.Unlock()
	d.recStart(t, "")
	waitSVG(t, b.ws, id, "rec-send")
	d.barrier(t)
	stream := newAudioStream()
	stream.append([]byte{8, 0, 9, 0})
	stream.finish(nil)
	b.ws.mu.Lock()
	dev := b.ws.terminals[id]
	b.ws.play(dev, stream)
	b.ws.mu.Unlock()
	d.recEnd(t, "", true)
	cleared := false
	d.until(t, func(f wsFrame) bool {
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
	b, base := wsHarness(t, func(b *larkServer, _ string) { b.endpoint = testEndpointSettings() })
	const id = "vocat-e024"
	d := testConnectWS(t, base, "unit", false)
	d.hello(t, id, "")
	b.ws.mu.Lock()
	dev := b.ws.terminals[id]
	ctx, cancel := context.WithCancel(dev.client.ctx)
	dev.busy = true
	dev.queuedVoice = &endpointCapture{ctx: ctx, cancel: cancel}
	for range 3 {
		b.ws.tapStop(dev, time.Now())
	}
	busy, queued := dev.busy, dev.queuedVoice
	b.ws.mu.Unlock()
	if ctx.Err() == nil || busy || queued != nil {
		t.Fatal("explicit Stop did not discard both the active and queued turns")
	}
}

func TestEndpointQueuesWhileOpenCodeOwnsTurn(t *testing.T) {
	const sid, id = "ses-followup", "vocat-e022"
	prompts := make(chan string, 2)
	finishFirst := make(chan struct{})
	var count atomic.Int32
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.endpoint = testEndpointSettings()
		b.stt = func(context.Context, []byte) (string, error) { return "and add stars", nil }
		b.oc = testOpenCode(t, b.root, dir, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/session", "/api/session/" + sid:
				sendJSON(w, 200, object{"data": object{"id": sid}})
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
	b.ws.mu.Lock()
	b.ws.startTurn(b.ws.terminals[id], time.Now(), false, "voice", func(ctx context.Context, turn uint64) {
		b.ws.runTurn(ctx, b.ws.terminals[id], turn, "Draw a dragon", "voice", nil)
	})
	b.ws.mu.Unlock()
	expectValue(t, prompts, "Draw a dragon")
	b.ws.mu.Lock()
	b.ws.startRecording(b.ws.terminals[id], "", time.Now(), true)
	b.ws.mu.Unlock()
	d.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 2 && p[0] == recOpStart && p[1]&recTargetBridgeE != 0
	})
	d.recStart(t, "")
	pcm := make([]byte, endpointBytes)
	pcm[0] = 18
	d.sendRTP(t, rtpPTRecPCM, 1, 320, 1, pcm)
	d.recEnd(t, "", false)
	d.barrier(t)
	select {
	case <-prompts:
		t.Fatal("follow-up bypassed gate")
	default:
	}
	close(finishFirst)
	expectValue(t, prompts, "and add stars")
	d.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgStatus, "")(f) && len(p) >= 5 && (p[4] == statusIdle || p[4] == statusDone)
	})
	b.ws.sig.wait(t, func() bool {
		b.ws.mu.Lock()
		defer b.ws.mu.Unlock()
		return !b.ws.terminals[id].busy
	})
	if count.Load() != 2 {
		t.Fatal("prompt was replayed")
	}
}
