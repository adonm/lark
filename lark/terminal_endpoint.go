package main

import (
	"context"
	"time"
)

type endpointSettings struct {
	newVAD    func() endpointVAD
	smartTurn func([]byte) (float32, error)
}

const endpointRingSlots = 256

type endpointCapture struct {
	ctx       context.Context
	cancel    context.CancelFunc
	owner     *wsClient
	packets   chan micPacket
	free      chan int
	slots     *[endpointRingSlots][2 * pcmPacket]byte
	stop      chan struct{}
	result    chan captureResult
	listening chan struct{}
	deadline  *time.Timer // Hub owns and stops the wall-clock watchdog.
	commitAt  time.Time   // Hub writes before closing stop.
	listen    bool
	replyTo   string
}

type micPacket struct {
	pcm  []byte
	at   time.Time
	slot int
}

func (c *endpointCapture) push(pcm []byte, at time.Time) bool {
	if c.free == nil {
		select {
		case c.packets <- micPacket{pcm: append([]byte(nil), pcm...), at: at, slot: -1}:
			return true
		default:
			return false
		}
	}
	select {
	case idx := <-c.free:
		n := copy(c.slots[idx][:], pcm)
		c.packets <- micPacket{pcm: c.slots[idx][:n], at: at, slot: idx}
		return true
	default:
		return false
	}
}

type captureResult struct {
	pcm              []byte
	text             string
	err              error
	stt, endToCommit time.Duration
	sttQueue, sttRun time.Duration
	vadQueue         time.Duration
	hasEnd           bool
	listen           bool
	replyTo          string
}

type pauseTranscript struct {
	last             time.Duration
	text             string
	err              error
	stt              time.Duration
	sttQueue, sttRun time.Duration
}

func (h *wsHub) startEndpoint(d *wsTerminal) {
	ctx, cancel := context.WithCancel(d.client.ctx)
	limit := endpointLimit
	var replyTo string
	if d.recording.listen {
		if d.recording.listenSpan > 0 {
			limit = d.recording.listenSpan
		}
		replyTo = d.replyTo
		d.replyTo = ""
	}
	free := make(chan int, endpointRingSlots)
	for i := range endpointRingSlots {
		free <- i
	}
	c := &endpointCapture{
		ctx:       ctx,
		cancel:    cancel,
		owner:     d.client,
		packets:   make(chan micPacket, endpointRingSlots),
		free:      free,
		slots:     new([endpointRingSlots][2 * pcmPacket]byte),
		stop:      make(chan struct{}),
		result:    make(chan captureResult, 1),
		listening: make(chan struct{}),
		listen:    d.recording.listen,
		replyTo:   replyTo,
	}
	d.recording.endpoint = c
	d.client.activeCapture.Store(c)
	c.deadline = time.AfterFunc(limit, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.active(d) && d.recording.endpoint == c {
			h.recordingControl(d, time.Now(), c.listen)
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
	u.pcm = make([]byte, 0, sampleRate*2*15)
	var end time.Time
	var maxVADQueue time.Duration
	var cached pauseTranscript
	var probe <-chan pauseTranscript
	var probeCancel context.CancelFunc
	var attempted bool
	var smartTurnCh <-chan float32
	var smartTurnChecked bool
	var smartTurnCommit bool
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
	processPacket := func(packet micPacket) {
		if wait := time.Since(packet.at); wait > maxVADQueue {
			maxVADQueue = wait
		}
		previous := u.policy.last
		u.addFor(packet.pcm, vad, c.listen)
		if c.free != nil && packet.slot >= 0 {
			c.free <- packet.slot
		}
		if u.policy.inVoice || previous != u.policy.last {
			attempted, cached, silence = false, pauseTranscript{}, tunedEndpoint.patient
			smartTurnChecked, smartTurnCommit, smartTurnCh = false, false, nil
			if probeCancel != nil {
				probeCancel()
			}
		}
		if u.policy.voiced && (previous != u.policy.last || end.IsZero()) {
			end = packet.at.Add(u.policy.last - pcmDuration(len(u.pcm)))
		}
	}
	commit := func() {
		for {
			select {
			case packet := <-c.packets:
				processPacket(packet)
			default:
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
				result := captureResult{pcm: u.clip(), vadQueue: maxVADQueue, hasEnd: !end.IsZero() && !u.policy.inVoice && !end.After(c.commitAt), listen: c.listen, replyTo: c.replyTo}
				if len(result.pcm) != 0 && (!c.listen || u.policy.voiced) && c.ctx.Err() == nil {
					if cached.text != "" {
						result.text, result.stt, result.sttQueue, result.sttRun = cached.text, cached.stt, cached.sttQueue, cached.sttRun
					} else {
						start := time.Now()
						sttCtx, timing := withSTTTiming(c.ctx)
						result.text, result.err = h.b.decodeClip(sttCtx, result.pcm)
						result.stt = time.Since(start)
						result.sttQueue, result.sttRun = timing.queue, timing.run
					}
					if result.hasEnd {
						result.endToCommit = c.commitAt.Sub(end)
					}
				}
				c.result <- result
				return
			}
		}
	}
	for c.ctx.Err() == nil {
		select {
		case <-c.ctx.Done():
			return
		case result := <-probe:
			accept(result)
		case prob := <-smartTurnCh:
			smartTurnCh = nil
			if !u.policy.inVoice && prob >= smartTurnThreshold {
				smartTurnCommit = true
			}
		case <-c.stop:
			commit()
			return
		case packet, more := <-c.packets:
			if !more {
				commit()
				return
			}
			processPacket(packet)
		}
		if len(c.packets) != 0 || c.ctx.Err() != nil {
			continue
		}
		at := pcmDuration(len(u.pcm))
		decision := endpointWait
		if vad != nil && (!c.listen || u.policy.voiced) {
			if smartTurnCommit {
				decision = endpointCommit
			} else {
				decision = u.policy.decision(at, silence)
			}
		}
		if decision != endpointWait {
			h.mu.Lock()
			// Don't stop on an old decision while more speech is queued.
			if h.active(d) && d.recording.endpoint == c && d.recording.phase == captureOpen && len(c.packets) == 0 {
				h.recordingControl(d, time.Now(), decision == endpointDiscard)
			}
			h.mu.Unlock()
		} else {
			if h.b.endpoint.smartTurn != nil && vad != nil && (!c.listen || u.policy.voiced) &&
				!smartTurnChecked && smartTurnCh == nil &&
				u.policy.last-u.policy.first < tunedEndpoint.storySpan &&
				u.policy.paused(at, smartTurnPause) {
				smartTurnChecked = true
				ch := make(chan float32, 1)
				smartTurnCh = ch
				pcm := u.pcm[:len(u.pcm):len(u.pcm)]
				fn := h.b.endpoint.smartTurn
				go func() {
					prob, err := fn(pcm)
					if err != nil {
						prob = 0
					}
					ch <- prob
				}()
			}
			if !attempted && probe == nil && u.policy.paused(at, tunedEndpoint.probe) {
				attempted = true
				ctx, cancel := context.WithCancel(c.ctx)
				result := make(chan pauseTranscript, 1)
				probe, probeCancel = result, cancel
				// The prefix is immutable; the capture worker only appends.
				go func(pcm []byte, last time.Duration) {
					start := time.Now()
					sttCtx, timing := withSTTTiming(withSTTProbe(ctx))
					text, err := h.b.decodeClip(sttCtx, pcm)
					result <- pauseTranscript{last: last, text: text, err: err, stt: time.Since(start), sttQueue: timing.queue, sttRun: timing.run}
				}(u.pcm[:len(u.pcm):len(u.pcm)], u.policy.last)
			}
		}
	}
}

func (h *wsHub) finishEndpoint(d *wsTerminal, now time.Time) {
	c := d.recording.endpoint
	d.recording.endpoint = nil
	if d.client != nil {
		d.client.activeCapture.CompareAndSwap(c, nil)
	}
	c.deadline.Stop()
	if c.commitAt.IsZero() {
		c.commitAt = now
	}
	if c.stop != nil {
		close(c.stop)
	} else {
		close(c.packets)
	}
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
	if c == nil || d.busy || d.stopAudio != nil || d.awaitAck || !h.active(d) {
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
		if result.sttRun > 0 || result.sttQueue > 0 {
			trace.measure(diagSTTQueue, result.sttQueue)
			trace.measure(diagSTTRun, result.sttRun)
		}
		trace.measure(diagVADQueue, result.vadQueue)
		if result.hasEnd {
			trace.measure(diagEndpoint, result.endToCommit)
		}
		if result.err != nil {
			h.turnError(d, id, "Could not hear that. Try again.")
			return
		}
		if result.text == "" || (result.listen && isFillerTranscript(result.text)) {
			h.finishTurn(d, id, nil, nil)
			return
		}
		h.runTurnChannel(ctx, d, id, result.text, "voice", result.pcm, turnChannel{replyTo: result.replyTo})
	})
	return true
}
