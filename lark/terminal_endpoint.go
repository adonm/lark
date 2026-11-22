package main

import (
	"context"
	"time"
)

type endpointSettings struct {
	newVAD func() endpointVAD
}

type endpointCapture struct {
	ctx       context.Context
	cancel    context.CancelFunc
	packets   chan micPacket
	result    chan captureResult
	listening chan struct{}
	deadline  *time.Timer // Hub owns and stops the wall-clock watchdog.
	commitAt  time.Time   // Hub writes before closing packets.
}

type micPacket struct {
	pcm []byte
	at  time.Time
}

type captureResult struct {
	pcm              []byte
	text             string
	err              error
	stt, endToCommit time.Duration
	hasEnd           bool
}

type pauseTranscript struct {
	last time.Duration
	text string
	err  error
	stt  time.Duration
}

func (h *wsHub) startEndpoint(d *wsTerminal) {
	ctx, cancel := context.WithCancel(d.client.ctx)
	c := &endpointCapture{ctx: ctx, cancel: cancel, packets: make(chan micPacket, 64), result: make(chan captureResult, 1), listening: make(chan struct{})}
	d.recording.endpoint = c
	c.deadline = time.AfterFunc(endpointLimit, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.active(d) && d.recording.endpoint == c {
			h.recordingControl(d, time.Now(), false)
		}
	})
	go h.captureEndpoint(d, c)
}

func (h *wsHub) captureEndpoint(d *wsTerminal, c *endpointCapture) {
	defer close(c.result)
	vad := h.b.endpoint.newVAD()
	if vad != nil {
		defer vad.close()
	}
	var u utterance
	var end time.Time
	var cached pauseTranscript
	var probe <-chan pauseTranscript
	var probeCancel context.CancelFunc
	var attempted bool
	silence := tunedEndpoint.patient
	defer func() {
		if probeCancel != nil {
			probeCancel()
		}
	}()
	accept := func(result pauseTranscript) {
		probeCancel()
		probe, probeCancel = nil, nil
		if !u.policy.inVoice && result.last == u.policy.last && result.err == nil && result.text != "" {
			cached = result
			silence = tunedEndpoint.silence(u.policy, result.text)
		}
	}
	for c.ctx.Err() == nil {
		select {
		case <-c.ctx.Done():
			return
		case result := <-probe:
			accept(result)
		case packet, more := <-c.packets:
			if !more {
				// Send may close the mic during a probe. Reuse a completed
				// result only for the same silence, including any DMA tail.
				select {
				case ready := <-probe:
					accept(ready)
				default:
				}
				if probeCancel != nil {
					probeCancel()
				}
				result := captureResult{pcm: u.clip(), hasEnd: !end.IsZero() && !u.policy.inVoice && !end.After(c.commitAt)}
				if len(result.pcm) != 0 && c.ctx.Err() == nil {
					if cached.text != "" {
						result.text, result.stt = cached.text, cached.stt
					} else {
						start := time.Now()
						result.text, result.err = h.b.decodeClip(c.ctx, result.pcm)
						result.stt = time.Since(start)
					}
					if result.hasEnd {
						result.endToCommit = c.commitAt.Sub(end)
					}
				}
				c.result <- result
				return
			}
			previous := u.policy.last
			u.add(packet.pcm, vad)
			if u.policy.inVoice || previous != u.policy.last {
				attempted, cached, silence = false, pauseTranscript{}, tunedEndpoint.patient
				if probeCancel != nil {
					probeCancel()
				}
			}
			if u.policy.voiced && (previous != u.policy.last || end.IsZero()) {
				end = packet.at.Add(u.policy.last - pcmDuration(len(u.pcm)))
			}
		}
		if len(c.packets) != 0 || c.ctx.Err() != nil {
			continue
		}
		at := pcmDuration(len(u.pcm))
		decision := u.policy.decision(at, silence)
		if vad == nil {
			decision = endpointWait // Leave Send and the watchdog available on native init failure.
		}
		if decision != endpointWait {
			h.mu.Lock()
			// Don't stop on an old decision while more speech is queued.
			if h.active(d) && d.recording.endpoint == c && d.recording.phase == captureOpen && len(c.packets) == 0 {
				h.recordingControl(d, time.Now(), decision == endpointDiscard)
			}
			h.mu.Unlock()
		} else if !attempted && probe == nil && u.policy.paused(at, tunedEndpoint.probe) {
			attempted = true
			ctx, cancel := context.WithCancel(c.ctx)
			result := make(chan pauseTranscript, 1)
			probe, probeCancel = result, cancel
			// The prefix is immutable; the capture worker only appends.
			go func(pcm []byte, last time.Duration) {
				start := time.Now()
				text, err := h.b.decodeClip(ctx, pcm)
				result <- pauseTranscript{last: last, text: text, err: err, stt: time.Since(start)}
			}(u.pcm, u.policy.last)
		}
	}
}

func (h *wsHub) finishEndpoint(d *wsTerminal, now time.Time) {
	c := d.recording.endpoint
	d.recording.endpoint = nil
	c.deadline.Stop()
	if c.commitAt.IsZero() {
		c.commitAt = now
	}
	close(c.packets)
	close(c.listening)
	d.recording.clear()
	d.queuedVoice = c
	if !h.maybeStartVoice(d, now) {
		h.render(d, now, false)
	}
}

// Only one captured follow-up may wait for the current reply. It enters the
// same regular prompt gate as any other turn, without interrupt or retry.
func (h *wsHub) maybeStartVoice(d *wsTerminal, now time.Time) bool {
	c := d.queuedVoice
	if c == nil || d.busy || d.stopAudio != nil || d.awaitAck || d.callState != callNone || !h.active(d) {
		return false
	}
	d.queuedVoice = nil
	h.startTurn(d, now, false, "voice", func(ctx context.Context, id uint64) {
		// Explicit Stop also cancels any outstanding work for this capture.
		stop := context.AfterFunc(ctx, c.cancel)
		defer stop()
		defer c.cancel()
		var result captureResult
		select {
		case result = <-c.result:
		case <-ctx.Done():
			return
		}
		if len(result.pcm) == 0 {
			h.finishTurn(d, id, nil, nil)
			return
		}
		if rms, ok := ambientRMS(result.pcm); ok {
			h.mu.Lock()
			if h.active(d) && d.turn == id {
				d.updateAmbient(rms)
			}
			h.mu.Unlock()
		}
		trace := traceFrom(ctx)
		trace.measure(diagSTT, result.stt)
		if result.hasEnd {
			trace.measure(diagEndpoint, result.endToCommit)
		}
		if result.err != nil {
			h.turnError(d, id, "Could not hear that. Try again.")
			return
		}
		if result.text == "" {
			h.finishTurn(d, id, nil, nil)
			return
		}
		h.runTurn(ctx, d, id, result.text, "voice", result.pcm)
	})
	return true
}
