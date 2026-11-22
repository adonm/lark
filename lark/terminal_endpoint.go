package main

import (
	"context"
	"log"
	"os"
	"time"
)

var debugEndpoint = os.Getenv("LARK_DEBUG_ENDPOINT") != ""

type endpointSettings struct {
	newVAD    func() endpointVAD
	smartTurn func([]byte) (float32, error)
}

const endpointRingSlots = 256

type endpointCapture struct {
	ctx         context.Context
	cancel      context.CancelFunc
	owner       *wsClient
	packets     chan micPacket
	free        chan int
	slots       *[endpointRingSlots][2 * pcmPacket]byte
	stop        chan struct{}
	result      chan captureResult
	listening   chan struct{}
	deadline    *time.Timer // Hub owns and stops the wall-clock watchdog.
	commitAt    time.Time   // Hub writes before closing stop.
	listen      bool
	replyTo     string
	companionID string
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
	spec             *specTurn
}

type pauseTranscript struct {
	at               time.Duration
	audioLen         int
	windowed         bool
	text             string
	err              error
	stt              time.Duration
	sttQueue, sttRun time.Duration
}

type smartTurnResult struct {
	prob   float32
	wordAt time.Duration
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
		ctx:         ctx,
		cancel:      cancel,
		owner:       d.client,
		packets:     make(chan micPacket, endpointRingSlots),
		free:        free,
		slots:       new([endpointRingSlots][2 * pcmPacket]byte),
		stop:        make(chan struct{}),
		result:      make(chan captureResult, 1),
		listening:   make(chan struct{}),
		listen:      d.recording.listen,
		replyTo:     replyTo,
		companionID: d.companionID,
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
	lastProbeAt := -endpointProbeEvery
	var lastProbeAudioLen int
	var lastGateOpenLen int
	var smartTurnCh <-chan smartTurnResult
	smartTurnCheckedAt := time.Duration(-1)
	smartTurnCheckedQuiet := time.Duration(0)
	var smartTurnCommit bool
	var lastSmartTurnProb float32 = -1
	var spec *specTurn
	specTriedText := ""
	abortSpec := func() {
		if spec == nil {
			return
		}
		spec.abort()
		spec = nil
	}
	defer func() {
		if probeCancel != nil {
			probeCancel()
		}
		if spec != nil {
			h.b.diag.spec.abortOther.Add(1)
			spec.abort()
		}
	}()
	accept := func(result pauseTranscript) {
		probeCancel()
		probe, probeCancel = nil, nil
		if result.err == nil {
			if u.policy.observeProgress(result.at, result.text, result.windowed) {
				smartTurnCommit = false
				smartTurnCheckedAt = -1
				smartTurnCheckedQuiet = 0
				lastSmartTurnProb = -1
				if spec != nil {
					h.b.diag.spec.abortWords.Add(1)
					if debugEndpoint {
						log.Printf("endpoint speculate abort_words term=%s text=%q", d.id, result.text)
					}
					abortSpec()
				}
			}
			cached = result
		}
	}
	processPacket := func(packet micPacket) {
		if wait := time.Since(packet.at); wait > maxVADQueue {
			maxVADQueue = wait
		}
		u.addFor(packet.pcm, vad, c.listen)
		if c.free != nil && packet.slot >= 0 {
			c.free <- packet.slot
		}
		if u.policy.inVoice {
			lastGateOpenLen = len(u.pcm)
			end = packet.at
			if spec != nil && len(u.pcm) > spec.audioLen {
				h.b.diag.spec.abortSpeech.Add(1)
				if debugEndpoint {
					log.Printf("endpoint speculate abort_speech term=%s", d.id)
				}
				abortSpec()
			}
		}
	}
	commit := func() {
		for {
			select {
			case packet := <-c.packets:
				processPacket(packet)
			default:
				// Send may close the mic during a probe. Reuse a completed
				// result only when no new speech arrived after it.
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
					switch {
					case cached.err == nil && cached.text != "" && !cached.windowed && lastGateOpenLen <= cached.audioLen:
						result.text, result.stt, result.sttQueue, result.sttRun = cached.text, cached.stt, cached.sttQueue, cached.sttRun
					case c.listen && !u.policy.hasWords && cached.err == nil && cached.audioLen > 0 && lastGateOpenLen <= cached.audioLen:
						result.text = cached.text
					default:
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
				if spec != nil {
					if result.err != nil || len(result.pcm) == 0 || normalizeSpecText(result.text) != normalizeSpecText(spec.text) {
						h.b.diag.spec.abortMismatch.Add(1)
						if debugEndpoint {
							log.Printf("endpoint speculate abort_mismatch term=%s spec=%q final=%q", d.id, spec.text, result.text)
						}
						abortSpec()
					} else {
						h.b.diag.spec.hit.Add(1)
						if lead := time.Since(spec.started).Milliseconds(); lead > 0 {
							h.b.diag.spec.overlapMS.Add(lead)
						}
						if debugEndpoint {
							log.Printf("endpoint speculate hit term=%s text=%q", d.id, result.text)
						}
						result.spec = spec
						spec = nil
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
			if debugEndpoint {
				log.Printf("endpoint probe term=%s at=%v text=%q err=%v", d.id, result.at, result.text, result.err)
			}
			accept(result)
		case turn := <-smartTurnCh:
			smartTurnCh = nil
			if turn.wordAt != u.policy.lastWordAt {
				continue // The transcript advanced while this check ran.
			}
			lastSmartTurnProb = turn.prob
			if debugEndpoint {
				log.Printf("endpoint smart_turn term=%s prob=%.3f incomplete=%v at=%v lastWordAt=%v",
					d.id, turn.prob, u.policy.incomplete, pcmDuration(len(u.pcm)), u.policy.lastWordAt)
			}
			if turn.prob >= smartTurnThreshold && !u.policy.incomplete {
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
		if vad != nil {
			decision = u.policy.decision(at, endpointCap, smartTurnCommit, c.listen)
		}
		if decision != endpointWait {
			if debugEndpoint {
				mode := "cap"
				if smartTurnCommit {
					mode = "smart_turn"
				} else if decision == endpointDiscard {
					mode = "discard"
				}
				log.Printf("endpoint decide term=%s decision=%d mode=%s listen=%v at=%v lastWordAt=%v words=%d rms=%.1f",
					d.id, decision, mode, c.listen, at, u.policy.lastWordAt, u.policy.wordCount, u.policy.speechRMS)
			}
			h.mu.Lock()
			// Don't stop on an old decision while more speech is queued.
			if h.active(d) && d.recording.endpoint == c && d.recording.phase == captureOpen && len(c.packets) == 0 {
				h.recordingControl(d, time.Now(), decision == endpointDiscard)
			}
			h.mu.Unlock()
		} else {
			if vad != nil && probe == nil && u.policy.voiced &&
				len(u.pcm) > lastProbeAudioLen &&
				at-lastProbeAt >= endpointProbeEvery &&
				(u.policy.inVoice || lastGateOpenLen > lastProbeAudioLen) {
				lastProbeAt = at
				lastProbeAudioLen = len(u.pcm)
				windowed := len(u.pcm) > endpointProbeWindow
				startOff := max(0, len(u.pcm)-endpointProbeWindow)
				clip := u.pcm[startOff:len(u.pcm):len(u.pcm)]
				ctx, cancel := context.WithCancel(c.ctx)
				result := make(chan pauseTranscript, 1)
				probe, probeCancel = result, cancel
				// The prefix is immutable; the capture worker only appends.
				go func(pcm []byte, probeAt time.Duration, audioLen int, windowed bool) {
					start := time.Now()
					sttCtx, timing := withSTTTiming(withSTTProbe(ctx))
					text, err := h.b.decodeClip(sttCtx, pcm)
					result <- pauseTranscript{
						at: probeAt, audioLen: audioLen, windowed: windowed,
						text: text, err: err, stt: time.Since(start), sttQueue: timing.queue, sttRun: timing.run,
					}
				}(clip, pcmDuration(lastGateOpenLen), len(u.pcm), windowed)
			}
			if h.b.endpoint.smartTurn != nil && vad != nil && u.policy.hasWords &&
				!u.policy.incomplete && smartTurnCh == nil &&
				(smartTurnCheckedAt != u.policy.lastWordAt && at-u.policy.lastWordAt >= endpointSpecQuiet ||
					smartTurnCheckedQuiet < endpointQuiet && lastSmartTurnProb < smartTurnThreshold &&
						at-u.policy.lastWordAt >= endpointQuiet) {
				smartTurnCheckedQuiet = at - u.policy.lastWordAt
				smartTurnCheckedAt = u.policy.lastWordAt
				ch := make(chan smartTurnResult, 1)
				smartTurnCh = ch
				wordAt := u.policy.lastWordAt
				startOff := max(0, len(u.pcm)-endpointProbeWindow)
				pcm := u.pcm[startOff:len(u.pcm):len(u.pcm)]
				fn := h.b.endpoint.smartTurn
				go func() {
					prob, err := fn(pcm)
					if err != nil {
						prob = 0
					}
					ch <- smartTurnResult{prob: prob, wordAt: wordAt}
				}()
			}
			if spec == nil && !c.listen && c.companionID != "" && h.b != nil && h.b.oc != nil &&
				h.b.endpoint.smartTurn != nil && vad != nil && u.policy.hasWords && !u.policy.incomplete &&
				at-u.policy.lastWordAt >= endpointSpecQuiet &&
				cached.err == nil && cached.text != "" && !cached.windowed &&
				lastGateOpenLen <= cached.audioLen && cached.text != specTriedText &&
				smartTurnCheckedAt == u.policy.lastWordAt && smartTurnCh == nil && lastSmartTurnProb >= smartTurnSpeculateThreshold {
				specTriedText = cached.text
				spec = h.b.startSpeculate(c.companionID, cached.text, len(u.pcm), "voice", turnChannel{replyTo: c.replyTo})
				if spec != nil {
					h.b.diag.spec.started.Add(1)
				}
				if debugEndpoint && spec != nil {
					log.Printf("endpoint speculate start term=%s prob=%.3f quiet=%v text=%q", d.id, lastSmartTurnProb, at-u.policy.lastWordAt, cached.text)
				}
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
		h.runTurnChannel(ctx, d, id, result.text, "voice", result.pcm, turnChannel{replyTo: result.replyTo, spec: result.spec})
	})
	return true
}
