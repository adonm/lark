package main

import (
	"context"
	"log"
	"os"
	"sync"
	"time"
)

var debugEndpoint = os.Getenv("LARK_DEBUG_ENDPOINT") != ""

type endpointSettings struct {
	newVAD    func() endpointVAD
	smartTurn func([]byte) (float32, error)
}

const (
	endpointRingSlots = 256
	endpointSlotBytes = rtpPCMBytes // one firmware record chunk (20 ms)
)

type endpointRing [endpointRingSlots][endpointSlotBytes]byte

var endpointRings = sync.Pool{New: func() any { return new(endpointRing) }}

type endpointCapture struct {
	ctx         context.Context
	cancel      context.CancelFunc
	owner       *terminalClient
	packets     chan micPacket
	free        chan int
	slotsMu     sync.Mutex // Guards the pool hand-off between push and captureEndpoint.
	slots       *endpointRing
	stop        chan struct{}
	result      chan captureResult
	listening   chan struct{}
	deadline    *time.Timer // Hub owns and stops the wall-clock watchdog.
	commitAt    time.Time   // Hub writes before closing stop.
	listen      bool
	debug       bool // Raw capture for lark capture; no VAD decisions or turn.
	limitBytes  int  // PCM cap for this capture's full window
	limitSpan   time.Duration
	started     time.Time // Capture start; a read-out after this is not its answer.
	reply       *turnReply
	companionID string
}

type micPacket struct {
	pcm  []byte
	at   time.Time
	slot int
}

func (c *endpointCapture) push(pcm []byte, at time.Time) bool {
	c.slotsMu.Lock()
	defer c.slotsMu.Unlock()
	if c.slots == nil {
		return false
	}
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
		if len(pcm) <= endpointSlotBytes {
			n := copy(c.slots[idx][:], pcm)
			c.packets <- micPacket{pcm: c.slots[idx][:n], at: at, slot: idx}
			return true
		}
		// Rare oversized chunk: keep the slot and copy out of band.
		c.free <- idx
		select {
		case c.packets <- micPacket{pcm: append([]byte(nil), pcm...), at: at, slot: -1}:
			return true
		default:
			return false
		}
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
	reply            *turnReply
	spec             *specTurn
	speech, audio    time.Duration
}

type pauseTranscript struct {
	at               time.Duration
	from             time.Duration // start of the decoded audio within the clip
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

func (h *terminalHub) startEndpoint(d *terminalSession) {
	ctx, cancel := context.WithCancel(d.client.ctx)
	limit := endpointLimit
	var reply *turnReply
	if d.recording.listen && d.recording.listenSpan > 0 {
		limit = d.recording.listenSpan
	}
	// A read-out arms the reply for the next spoken turn, whether the child taps
	// to talk or answers inside the listening window.
	reply = d.pendingReply(time.Now())
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
		slots:       endpointRings.Get().(*endpointRing),
		stop:        make(chan struct{}),
		result:      make(chan captureResult, 1),
		listening:   make(chan struct{}),
		listen:      d.recording.listen,
		debug:       d.recording.debug,
		limitSpan:   limit,
		limitBytes:  utteranceBytes(limit),
		started:     time.Now(),
		reply:       reply,
		companionID: d.companionID,
	}
	d.recording.endpoint = c
	d.client.activeCapture.Store(c)
	c.deadline = time.AfterFunc(limit, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.active(d) && d.recording.endpoint == c {
			if c.listen {
				h.b.diag.listen.closeCap.Add(1)
			}
			if c.debug {
				h.finishEndpoint(d, time.Now())
				return
			}
			h.recordingControl(d, time.Now(), c.listen)
		}
	})
	go h.captureEndpoint(d, c)
}

func (h *terminalHub) captureEndpoint(d *terminalSession, c *endpointCapture) {
	defer close(c.result)
	defer func() {
		c.slotsMu.Lock()
		ring := c.slots
		c.slots = nil
		c.slotsMu.Unlock()
		if ring != nil {
			endpointRings.Put(ring)
		}
	}()
	vad := h.b.endpoint.newVAD()
	if vad != nil {
		defer vad.close()
	}
	if c.debug {
		vad = nil
	}
	var u utterance
	u.pcm = make([]byte, 0, sampleRate*2*15)
	u.limit = c.limitBytes
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
			// Only audio that actually contains gated speech may move the
			// transcript forward. Parakeet happily invents words on room
			// noise, and those guesses must never extend a window.
			wordCount, fillerOnly, incomplete := inspectTranscript(result.text)
			spoken := u.policy.lastVoicedAt > result.from
			if spoken {
				u.policy.incomplete = incomplete
			}
			newTokens := wordCount - u.policy.wordCount
			grew := spoken && !fillerOnly && wordCount > 0 &&
				(u.policy.wordCount == 0 || newTokens >= 2 || (result.windowed && result.text != u.policy.lastText))
			if grew && u.policy.observeProgress(result.at, result.text, result.windowed) {
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
			if debugEndpoint && !spoken && !fillerOnly && wordCount > 0 {
				log.Printf("endpoint probe ignored_unvoiced term=%s at=%v text=%q from=%v lastVoicedAt=%v",
					d.id, result.at, result.text, result.from, u.policy.lastVoicedAt)
			}
			cached = result
		}
	}
	wasInVoice := false
	processPacket := func(packet micPacket) {
		if wait := time.Since(packet.at); wait > maxVADQueue {
			maxVADQueue = wait
		}
		if c.debug {
			u.add(packet.pcm, nil)
		} else {
			u.addFor(packet.pcm, vad, c.listen)
		}
		if c.free != nil && packet.slot >= 0 {
			c.free <- packet.slot
		}
		if debugEndpoint && u.policy.inVoice != wasInVoice {
			wasInVoice = u.policy.inVoice
			log.Printf("endpoint gate term=%s open=%v rms=%.1f floor=%.1f enter=%.1f at=%v",
				d.id, wasInVoice, frameRMS(packet.pcm), u.policy.gate.floor, u.policy.gate.enter(), pcmDuration(len(u.pcm)))
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
				result := captureResult{
					pcm: u.clip(), vadQueue: maxVADQueue,
					hasEnd: !end.IsZero() && !u.policy.inVoice && !end.After(c.commitAt),
					listen: c.listen, reply: c.reply,
					speech: u.policy.speechSpan(), audio: pcmDuration(len(u.pcm)),
				}
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
			if turn.wordAt != u.policy.lastVoicedAt {
				continue // New speech arrived while this check ran.
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
			// Early commit needs a confident end and a punctuated transcript
			// that already covers all speech; a newer probe may still be running.
			early := lastSmartTurnProb >= smartTurnEarlyThreshold && !u.policy.incomplete &&
				cached.err == nil && cached.text != "" && lastGateOpenLen <= cached.audioLen &&
				terminalPunctuation(cached.text)
			decision = u.policy.decide(at, endpointCap, c.limitSpan, lastSmartTurnProb, early, c.listen)
		}
		if c.debug {
			continue // Record the whole window; no endpoint decisions.
		}
		if decision != endpointWait {
			if probeCancel != nil {
				probeCancel()
				probe, probeCancel = nil, nil
			}
			if decision == endpointDiscard && u.policy.vadFrames > 0 && u.policy.speechFrames == 0 {
				h.b.diag.listen.droppedGate.Add(1)
			}
			if c.listen && at < c.limitSpan {
				h.b.diag.listen.closeSilence.Add(1)
			}
			if debugEndpoint {
				silence := at - u.policy.lastVoicedAt
				mode := "cap"
				switch {
				case decision == endpointDiscard:
					mode = "discard"
				case at >= c.limitSpan:
					mode = "limit"
				case u.policy.inVoice && u.policy.lastVoicedAt-u.policy.firstVoicedAt >= endpointMonologue:
					mode = "monologue"
				case smartTurnCommit:
					mode = "smart_turn"
				case silence >= endpointQuiet && u.policy.hasWords && !u.policy.incomplete:
					mode = "quiet"
				case silence >= endpointEarlyQuiet && u.policy.hasWords && !u.policy.incomplete:
					mode = "early"
				case !u.policy.hasWords:
					mode = "no_words"
				}
				log.Printf("endpoint decide term=%s decision=%d mode=%s listen=%v at=%v lastVoicedAt=%v silence=%v words=%d speech=%v floor=%.1f rms=%.1f",
					d.id, decision, mode, c.listen, at, u.policy.lastVoicedAt, silence,
					u.policy.wordCount, u.policy.speechSpan(), u.policy.gate.floor, u.policy.speechRMS)
			}
			h.mu.Lock()
			// Don't stop on an old decision while more speech is queued.
			if h.active(d) && d.recording.endpoint == c && d.recording.phase == captureOpen && len(c.packets) == 0 {
				h.recordingControl(d, time.Now(), decision == endpointDiscard)
			}
			h.mu.Unlock()
		} else {
			// No transcript can change the decision once the quiet window has
			// passed or the cap is imminent; skip the extra STT work.
			if vad != nil && probe == nil && u.policy.voiced &&
				len(u.pcm) > lastProbeAudioLen &&
				at-lastProbeAt >= endpointProbeEvery &&
				(!u.policy.inVoice || at-u.policy.lastVoicedAt < endpointQuiet) &&
				at-u.policy.lastVoicedAt < endpointCap-endpointProbeEvery &&
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
				go func(pcm []byte, probeAt, from time.Duration, audioLen int, windowed bool) {
					start := time.Now()
					sttCtx, timing := withSTTTiming(withSTTProbe(ctx))
					text, err := h.b.decodeClip(sttCtx, pcm)
					result <- pauseTranscript{
						at: probeAt, from: from, audioLen: audioLen, windowed: windowed,
						text: text, err: err, stt: time.Since(start), sttQueue: timing.queue, sttRun: timing.run,
					}
				}(clip, pcmDuration(lastGateOpenLen), pcmDuration(startOff), len(u.pcm), windowed)
			}
			if h.b.endpoint.smartTurn != nil && vad != nil && u.policy.hasWords &&
				!u.policy.incomplete && smartTurnCh == nil &&
				(smartTurnCheckedAt != u.policy.lastVoicedAt && at-u.policy.lastVoicedAt >= endpointSpecQuiet ||
					smartTurnCheckedQuiet < endpointQuiet && lastSmartTurnProb < smartTurnThreshold &&
						at-u.policy.lastVoicedAt >= endpointQuiet) {
				smartTurnCheckedQuiet = at - u.policy.lastVoicedAt
				smartTurnCheckedAt = u.policy.lastVoicedAt
				ch := make(chan smartTurnResult, 1)
				smartTurnCh = ch
				wordAt := u.policy.lastVoicedAt
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
				at-u.policy.lastVoicedAt >= endpointSpecQuiet &&
				cached.err == nil && cached.text != "" && !cached.windowed &&
				lastGateOpenLen <= cached.audioLen && cached.text != specTriedText &&
				smartTurnCheckedAt == u.policy.lastVoicedAt && smartTurnCh == nil && lastSmartTurnProb >= smartTurnSpeculateThreshold {
				specTriedText = cached.text
				spec = h.b.startSpeculate(c.companionID, cached.text, len(u.pcm), "voice", turnChannel{reply: c.reply})
				if spec != nil {
					h.b.diag.spec.started.Add(1)
				}
				if debugEndpoint && spec != nil {
					log.Printf("endpoint speculate start term=%s prob=%.3f quiet=%v text=%q", d.id, lastSmartTurnProb, at-u.policy.lastVoicedAt, cached.text)
				}
			}
		}
	}
}

func (h *terminalHub) finishEndpoint(d *terminalSession, now time.Time) {
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
	if c.debug {
		h.render(d, now, false)
		return
	}
	d.queuedVoice = c
	if !h.maybeStartVoice(d, now) {
		h.render(d, now, false)
	}
}

// Only one captured follow-up may wait for the current reply. It enters the
// same regular prompt gate as any other turn, without interrupt or retry.
func (h *terminalHub) maybeStartVoice(d *terminalSession, now time.Time) bool {
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
		if result.speech > 0 {
			words, _, _ := inspectTranscript(result.text)
			h.b.diag.listen.speech.Add(1)
			h.b.diag.addSegment(diagSegment{
				TS: time.Now().UnixMilli(), Terminal: d.id, Companion: d.companionID,
				DurationMS: result.audio.Milliseconds(), SpeechMS: result.speech.Milliseconds(),
				TextLen: len(result.text), Words: words,
			})
		}
		if result.listen && h.nameRequired(d, time.Now()) {
			rest, named := splitName(result.text, d.companion.Name)
			if !named {
				// Background chatter after the follow-up grace period is
				// ignored without a spoken reply.
				h.b.diag.listen.nameRequired.Add(1)
				h.finishTurn(d, id, nil, nil)
				return
			}
			result.text = rest
		}
		if result.text == "" || isFillerTranscript(result.text) {
			if result.text == "" {
				h.b.diag.listen.emptyTranscript.Add(1)
			} else {
				h.b.diag.listen.fillerOnly.Add(1)
			}
			// A cough or a door does not deserve a spoken apology; a segment
			// with real speech and no usable words does.
			if result.speech >= time.Second {
				h.replyNotCaught(ctx, d, id)
				return
			}
			h.finishTurn(d, id, nil, nil)
			return
		}
		h.b.diag.listen.committed.Add(1)
		if result.reply == nil {
			h.dropStaleReply(d, c.started)
		}
		h.freeFollowUps(d, time.Now())
		h.runTurnChannel(ctx, d, id, result.text, "voice", result.pcm, turnChannel{reply: result.reply, spec: result.spec})
	})
	return true
}
