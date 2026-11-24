package main

import (
	"context"
	"log"
	"time"
)

// decision is what one endpoint evaluation wants to do with the open capture.
// It is returned by the policy and consumed by the run loop; the run owns the
// side effects (diagnostics, stopping the mic, committing the decode).
type decision uint8

const (
	decisionContinue        decision = iota
	decisionCommitQuiet              // closing pause reached and Smart Turn agreed
	decisionCommitCap                // silence cap after speech
	decisionCommitMonologue          // speech without a pause
	decisionCommitNoWords            // speech heard, but no usable transcript
	decisionDiscardIdle              // tap-to-talk window never heard speech
	decisionDiscardNoise             // window ended on noise that never crossed the gate
)

func (d decision) stops() bool {
	return d != decisionContinue
}

func (d decision) discards() bool {
	return d == decisionDiscardIdle || d == decisionDiscardNoise
}

func (d decision) String() string {
	switch d {
	case decisionContinue:
		return "continue"
	case decisionCommitQuiet:
		return "commit_quiet"
	case decisionCommitCap:
		return "commit_cap"
	case decisionCommitMonologue:
		return "commit_monologue"
	case decisionCommitNoWords:
		return "commit_no_words"
	case decisionDiscardIdle:
		return "discard_idle"
	case decisionDiscardNoise:
		return "discard_noise"
	}
	return "unknown"
}

// endpointRun owns one capture's policy state. The hub starts it on rec_start
// and it exits after delivering exactly one captureResult.
type endpointRun struct {
	hub *terminalHub
	d   *terminalSession
	c   *endpointCapture

	u    utterance
	vad  endpointVAD
	spec *specTurn

	// Transcript progress from the pause probes.
	cached            pauseTranscript
	probeCh           <-chan pauseTranscript
	probeCancel       context.CancelFunc
	lastProbeAt       time.Duration
	lastProbeAudioLen int
	lastGateOpenLen   int
	specTriedText     string

	// Smart Turn.
	smart             func([]byte) (float32, error)
	smartTurnCh       <-chan smartTurnResult
	smartCheckedAt    time.Duration
	smartCheckedQuiet time.Duration
	smartProb         float32

	// Wall clocks for the result and the diagnostics.
	speechAt    time.Time
	end         time.Time
	maxVADQueue time.Duration
	wasInVoice  bool

	// Named capture configuration instead of closure captures.
	listen      bool
	limitSpan   time.Duration
	debug       bool
	nameGate    bool
	companionID string
	decode      func(context.Context, []byte) (string, error)
}

func (h *terminalHub) newEndpointRun(d *terminalSession, c *endpointCapture) *endpointRun {
	vad := h.b.endpoint.newVAD()
	if debugEndpoint {
		log.Printf("capture worker start term=%s ctxErr=%v vad=%v", d.id, c.ctx.Err(), vad != nil)
	}
	if c.debug {
		vad = nil
	}
	r := &endpointRun{
		hub:         h,
		d:           d,
		c:           c,
		vad:         vad,
		smart:       h.b.endpoint.smart(),
		listen:      c.listen,
		limitSpan:   c.limitSpan,
		debug:       c.debug,
		nameGate:    c.nameGate,
		companionID: c.companionID,
		decode:      h.b.decodeClip,
		// The first probe and Smart Turn check may run as soon as speech is
		// heard; only later checks need a quiet offset.
		lastProbeAt:    -endpointProbeEvery,
		smartCheckedAt: -1,
		smartProb:      -1,
	}
	r.u.pcm = make([]byte, 0, sampleRate*2*15)
	r.u.limit = c.limitBytes
	return r
}

func (r *endpointRun) close() {
	if r.probeCancel != nil {
		r.probeCancel()
	}
	if r.spec != nil {
		r.specStats().abortOther.Add(1)
		r.abortSpec()
	}
	r.releaseRing()
	if r.vad != nil {
		r.vad.close()
	}
}

func (r *endpointRun) releaseRing() {
	r.c.slotsMu.Lock()
	ring := r.c.slots
	r.c.slots = nil
	r.c.slotsMu.Unlock()
	if ring != nil {
		endpointRings.Put(ring)
	}
}

func (r *endpointRun) listenStats() *diagListen {
	if r.hub == nil || r.hub.b == nil {
		return nil
	}
	return &r.hub.b.diag.listen
}

func (r *endpointRun) specStats() *diagSpec {
	if r.hub == nil || r.hub.b == nil {
		return nil
	}
	return &r.hub.b.diag.spec
}

func (r *endpointRun) count(listen func(*diagListen)) {
	if st := r.listenStats(); st != nil {
		listen(st)
	}
}

func (r *endpointRun) loop() {
	for r.c.ctx.Err() == nil {
		select {
		case <-r.c.ctx.Done():
			r.finishCancelled()
			return
		case result := <-r.probeCh:
			if debugEndpoint {
				log.Printf("endpoint probe term=%s at=%v text=%s err=%v", r.d.id, result.at, redactTranscript(result.text), result.err)
			}
			r.acceptProbe(result)
		case turn := <-r.smartTurnCh:
			r.acceptSmartTurn(turn)
		case <-r.c.stop:
			r.commit()
			return
		case pkt, more := <-r.c.packets:
			if !more {
				r.commit()
				return
			}
			r.packet(pkt)
		}
		if len(r.c.packets) != 0 || r.c.ctx.Err() != nil {
			continue
		}
		if r.debug {
			continue // Record the whole window; no endpoint decisions.
		}
		if d := r.decision(); d.stops() {
			r.stop(d)
			continue
		}
		r.tick()
	}
}

func (r *endpointRun) finishCancelled() {
	if r.listen && r.u.policy.vadFrames > 0 && r.u.policy.speechFrames == 0 {
		r.count(func(st *diagListen) { st.droppedGate.Add(1) })
	}
	if debugEndpoint {
		log.Printf("capture worker ctx_cancelled term=%s", r.d.id)
	}
}

// packet folds one RTP chunk into the clip and advances the speech clocks.
func (r *endpointRun) packet(pkt micPacket) {
	if wait := time.Since(pkt.at); wait > r.maxVADQueue {
		r.maxVADQueue = wait
	}
	if r.debug {
		r.u.add(pkt.pcm, nil)
	} else {
		r.u.addFor(pkt.pcm, r.vad, r.listen)
	}
	if r.c.free != nil && pkt.slot >= 0 {
		r.c.free <- pkt.slot
	}
	if debugEndpoint && r.u.policy.inVoice != r.wasInVoice {
		r.wasInVoice = r.u.policy.inVoice
		log.Printf("endpoint gate term=%s open=%v rms=%.1f floor=%.1f enter=%.1f at=%v",
			r.d.id, r.wasInVoice, frameRMS(pkt.pcm), r.u.policy.gate.floor, r.u.policy.gate.enter(), r.u.at(len(r.u.pcm)))
	}
	if !r.u.policy.inVoice {
		return
	}
	if r.speechAt.IsZero() {
		r.speechAt = pkt.at
	}
	r.lastGateOpenLen = len(r.u.pcm)
	r.end = pkt.at
	if r.spec != nil && len(r.u.pcm) > r.spec.audioLen {
		r.specStats().abortSpeech.Add(1)
		if debugEndpoint {
			log.Printf("endpoint speculate abort_speech term=%s", r.d.id)
		}
		r.abortSpec()
	}
}

// decision classifies the clip end with the latest Smart Turn probability.
func (r *endpointRun) decision() decision {
	if r.vad == nil {
		return decisionContinue
	}
	return r.u.policy.classify(r.u.at(len(r.u.pcm)), endpointCap, r.limitSpan, r.smartProb, r.listen)
}

// stop applies a stopping decision. It never commits: recordingControl ends the
// capture and the commit path delivers the result.
func (r *endpointRun) stop(d decision) {
	r.cancelProbe()
	if d == decisionDiscardNoise && r.u.policy.vadFrames > 0 && r.u.policy.speechFrames == 0 {
		r.count(func(st *diagListen) { st.droppedGate.Add(1) })
	}
	at := r.u.at(len(r.u.pcm))
	if r.listen && at < r.limitSpan {
		r.count(func(st *diagListen) { st.closeSilence.Add(1) })
	}
	if debugEndpoint {
		log.Printf("endpoint decide term=%s decision=%s listen=%v at=%v lastVoicedAt=%v silence=%v words=%d speech=%v floor=%.1f rms=%.1f",
			r.d.id, d, r.listen, at, r.u.policy.lastVoicedAt, at-r.u.policy.lastVoicedAt,
			r.u.policy.wordCount, r.u.policy.speechSpan(), r.u.policy.gate.floor, r.u.policy.speechRMS)
	}
	r.hub.mu.Lock()
	// Don't stop on an old decision while more speech is queued.
	if r.hub.active(r.d) && r.d.recording.endpoint == r.c && r.d.recording.phase == captureOpen && len(r.c.packets) == 0 {
		r.hub.recordingControl(r.d, time.Now(), d.discards())
	}
	r.hub.mu.Unlock()
}

// tick starts the background checks that can make the next decision better.
func (r *endpointRun) tick() {
	at := r.u.at(len(r.u.pcm))
	if r.probeDue(at) {
		r.startProbe(at)
	}
	if r.smartTurnDue(at) {
		r.startSmartTurn(at)
	}
	if r.speculateDue(at) {
		r.startSpeculation(at)
	}
}

func (r *endpointRun) probeDue(at time.Duration) bool {
	// No transcript can change the decision once the quiet window has passed
	// or the cap is imminent; skip the extra STT work.
	return r.vad != nil && r.probeCh == nil && r.u.policy.voiced &&
		len(r.u.pcm) > r.lastProbeAudioLen &&
		at-r.lastProbeAt >= endpointProbeEvery &&
		(!r.u.policy.inVoice || at-r.u.policy.lastVoicedAt < endpointQuiet) &&
		at-r.u.policy.lastVoicedAt < endpointCap-endpointProbeEvery &&
		(r.u.policy.inVoice || r.lastGateOpenLen > r.lastProbeAudioLen)
}

func (r *endpointRun) startProbe(at time.Duration) {
	r.lastProbeAt = at
	r.lastProbeAudioLen = len(r.u.pcm)
	windowed := len(r.u.pcm) > endpointProbeWindow
	startOff := max(0, len(r.u.pcm)-endpointProbeWindow)
	// The prefix is immutable; the capture worker only appends.
	clip := r.u.pcm[startOff:len(r.u.pcm):len(r.u.pcm)]
	ctx, cancel := context.WithCancel(r.c.ctx)
	result := make(chan pauseTranscript, 1)
	r.probeCh, r.probeCancel = result, cancel
	go func(pcm []byte, probeAt, from time.Duration, audioLen int, windowed bool) {
		start := time.Now()
		sttCtx, timing := withSTTTiming(withSTTProbe(ctx))
		text, err := r.decode(sttCtx, pcm)
		result <- pauseTranscript{
			at: probeAt, from: from, audioLen: audioLen, windowed: windowed,
			text: text, err: err, stt: time.Since(start), sttQueue: timing.queue, sttRun: timing.run,
		}
	}(clip, r.u.at(r.lastGateOpenLen), r.u.at(startOff), len(r.u.pcm), windowed)
}

func (r *endpointRun) acceptProbe(result pauseTranscript) {
	r.cancelProbe()
	if result.err != nil {
		return
	}
	// Only audio that actually contains gated speech may move the transcript
	// forward. Parakeet happily invents words on room noise, and those guesses
	// must never extend a window.
	wordCount, fillerOnly, incomplete := inspectTranscript(result.text)
	spoken := r.u.policy.lastVoicedAt > result.from
	if spoken {
		r.u.policy.incomplete = incomplete
	}
	newTokens := wordCount - r.u.policy.wordCount
	grew := spoken && !fillerOnly && wordCount > 0 &&
		(r.u.policy.wordCount == 0 || newTokens >= 2 || (result.windowed && result.text != r.u.policy.lastText))
	if grew && r.u.policy.observeProgress(result.at, result.text, result.windowed) {
		r.smartCheckedAt = -1
		r.smartCheckedQuiet = 0
		r.smartProb = -1
		if r.spec != nil {
			r.specStats().abortWords.Add(1)
			if debugEndpoint {
				log.Printf("endpoint speculate abort_words term=%s text=%s", r.d.id, redactTranscript(result.text))
			}
			r.abortSpec()
		}
	}
	if debugEndpoint && !spoken && !fillerOnly && wordCount > 0 {
		log.Printf("endpoint probe ignored_unvoiced term=%s at=%v text=%s from=%v lastVoicedAt=%v",
			r.d.id, result.at, redactTranscript(result.text), result.from, r.u.policy.lastVoicedAt)
	}
	r.cached = result
}

func (r *endpointRun) cancelProbe() {
	if r.probeCancel != nil {
		r.probeCancel()
		r.probeCh, r.probeCancel = nil, nil
	}
}

// smartFn reads the engine on every check: Smart Turn finishes loading in the
// background, so a capture that starts before it is ready must still use it.
func (r *endpointRun) smartFn() func([]byte) (float32, error) {
	if r.hub != nil && r.hub.b != nil && r.hub.b.endpoint != nil {
		return r.hub.b.endpoint.smart()
	}
	return r.smart
}

func (r *endpointRun) smartTurnDue(at time.Duration) bool {
	return r.smartFn() != nil && r.vad != nil && r.u.policy.hasWords &&
		!r.u.policy.incomplete && r.smartTurnCh == nil &&
		(r.smartCheckedAt != r.u.policy.lastVoicedAt && at-r.u.policy.lastVoicedAt >= endpointSpecQuiet ||
			r.smartCheckedQuiet < endpointQuiet && r.smartProb < smartTurnThreshold &&
				at-r.u.policy.lastVoicedAt >= endpointQuiet)
}

func (r *endpointRun) startSmartTurn(at time.Duration) {
	r.smartCheckedQuiet = at - r.u.policy.lastVoicedAt
	r.smartCheckedAt = r.u.policy.lastVoicedAt
	ch := make(chan smartTurnResult, 1)
	r.smartTurnCh = ch
	wordAt := r.u.policy.lastVoicedAt
	startOff := max(0, len(r.u.pcm)-endpointProbeWindow)
	pcm := r.u.pcm[startOff:len(r.u.pcm):len(r.u.pcm)]
	fn := r.smartFn()
	go func() {
		prob, err := fn(pcm)
		if err != nil {
			prob = 0
		}
		ch <- smartTurnResult{prob: prob, wordAt: wordAt}
	}()
}

func (r *endpointRun) acceptSmartTurn(turn smartTurnResult) {
	r.smartTurnCh = nil
	if turn.wordAt != r.u.policy.lastVoicedAt {
		return // New speech arrived while this check ran.
	}
	r.smartProb = turn.prob
	if debugEndpoint {
		log.Printf("endpoint smart_turn term=%s prob=%.3f incomplete=%v at=%v lastWordAt=%v",
			r.d.id, turn.prob, r.u.policy.incomplete, r.u.at(len(r.u.pcm)), r.u.policy.lastWordAt)
	}
}

func (r *endpointRun) speculateDue(at time.Duration) bool {
	if r.listen || r.companionID == "" || r.hub == nil || r.hub.b == nil || r.hub.b.oc == nil ||
		r.spec != nil || r.smartFn() == nil || r.vad == nil || !r.u.policy.hasWords || r.u.policy.incomplete {
		return false
	}
	return at-r.u.policy.lastVoicedAt >= endpointSpecQuiet &&
		r.cached.err == nil && r.cached.text != "" && !r.cached.windowed &&
		r.lastGateOpenLen <= r.cached.audioLen && r.cached.text != r.specTriedText &&
		r.smartCheckedAt == r.u.policy.lastVoicedAt && r.smartTurnCh == nil &&
		r.smartProb >= smartTurnSpeculateThreshold
}

func (r *endpointRun) startSpeculation(at time.Duration) {
	r.specTriedText = r.cached.text
	r.spec = r.hub.b.startSpeculate(r.companionID, r.cached.text, len(r.u.pcm), "voice", turnChannel{reply: r.c.reply})
	if r.spec != nil {
		r.specStats().started.Add(1)
	}
	if debugEndpoint && r.spec != nil {
		log.Printf("endpoint speculate start term=%s prob=%.3f quiet=%v text=%s",
			r.d.id, r.smartProb, at-r.u.policy.lastVoicedAt, redactTranscript(r.cached.text))
	}
}

func (r *endpointRun) abortSpec() {
	if r.spec == nil {
		return
	}
	r.spec.abort()
	r.spec = nil
}

// pauseCoversSpeech reports whether the cached probe clip already holds all
// gated speech, so the commit can reuse its transcript instead of decoding.
func (r *endpointRun) pauseCoversSpeech() bool {
	return r.cached.err == nil && r.cached.text != "" && !r.cached.windowed &&
		r.lastGateOpenLen <= r.cached.audioLen
}

// pauseCoversListenWindow is the listening-mode variant: a probe of a window
// with no words still describes the whole capture when no gated audio followed.
func (r *endpointRun) pauseCoversListenWindow() bool {
	return r.listen && !r.u.policy.hasWords && r.cached.err == nil &&
		r.cached.audioLen > 0 && r.lastGateOpenLen <= r.cached.audioLen
}

// commit drains queued packets, reuses or runs the final decode, and sends the
// one result the hub waits for.
func (r *endpointRun) commit() {
	for {
		select {
		case pkt := <-r.c.packets:
			r.packet(pkt)
		default:
			// Send may close the mic during a probe. Reuse a completed result
			// only when no new speech arrived after it.
			select {
			case ready := <-r.probeCh:
				r.acceptProbe(ready)
			default:
			}
			r.cancelProbe()
			result := r.buildResult()
			if debugEndpoint {
				log.Printf("capture worker commit term=%s bytes=%d ctxErr=%v", r.d.id, len(result.pcm), r.c.ctx.Err())
			}
			r.c.result <- result
			return
		}
	}
}

func (r *endpointRun) buildResult() captureResult {
	result := captureResult{
		pcm: r.u.clip(), vadQueue: r.maxVADQueue,
		hasEnd: !r.end.IsZero() && !r.u.policy.inVoice && !r.end.After(r.c.commitAt),
		listen: r.listen, reply: r.c.reply,
		speech: r.u.policy.speechSpan(), audio: pcmDuration(len(r.u.pcm)),
		speechAt: r.speechAt, nameGate: r.nameGate,
	}
	if len(result.pcm) != 0 && (!r.listen || r.u.policy.voiced) && r.c.ctx.Err() == nil {
		switch {
		case r.pauseCoversSpeech():
			result.text, result.stt, result.sttQueue, result.sttRun = r.cached.text, r.cached.stt, r.cached.sttQueue, r.cached.sttRun
		case r.pauseCoversListenWindow():
			result.text = r.cached.text
		default:
			start := time.Now()
			sttCtx, timing := withSTTTiming(r.c.ctx)
			result.text, result.err = r.decode(sttCtx, result.pcm)
			result.stt = time.Since(start)
			result.sttQueue, result.sttRun = timing.queue, timing.run
		}
		if result.hasEnd {
			result.endToCommit = r.c.commitAt.Sub(r.end)
		}
	}
	r.resolveSpeculation(&result)
	return result
}

// resolveSpeculation either attaches the speculative turn whose transcript
// matched the final decode, or aborts it as a mismatch.
func (r *endpointRun) resolveSpeculation(result *captureResult) {
	if r.spec == nil {
		return
	}
	if result.err != nil || len(result.pcm) == 0 || normalizeSpecText(result.text) != normalizeSpecText(r.spec.text) {
		r.specStats().abortMismatch.Add(1)
		if debugEndpoint {
			log.Printf("endpoint speculate abort_mismatch term=%s spec=%s final=%s",
				r.d.id, redactTranscript(r.spec.text), redactTranscript(result.text))
		}
		r.abortSpec()
		return
	}
	r.specStats().hit.Add(1)
	if lead := time.Since(r.spec.started).Milliseconds(); lead > 0 {
		r.specStats().overlapMS.Add(lead)
	}
	if debugEndpoint {
		log.Printf("endpoint speculate hit term=%s text=%s", r.d.id, redactTranscript(result.text))
	}
	result.spec = r.spec
	r.spec = nil
}
