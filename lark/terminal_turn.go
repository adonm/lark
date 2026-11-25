package main

import (
	"context"
	"errors"

	"log"
	"strings"
	"time"

	"lark/internal/stories"
)

const (
	micPrefix = 0x01
	pcmPrefix = 0x02
	pcmPacket = 4 << 10
	// Start playback with at least one second buffered. Sixteen 60 ms gaps
	// (the cake/mint underruns) begin at a sentence boundary where only the
	// short first sentence was available. Kokoro fills 1 s in roughly 110 ms,
	// so the cap only delays a reply whose stream stays short; it bounds the
	// first-audio delay at half a second.
	ttsPrefill    = time.Second
	ttsPrefillMax = 250 * time.Millisecond
	maxAudioLead  = 8 * time.Second
	recStopWait   = 450 * time.Millisecond
	captionWait   = 30 * time.Second
	// How long after a read-out finishes playing the child may answer it.
	deviceReplyTTL = time.Minute
)

// listenWindow bounds one listening window. Listening is a single on/off
// setting; a window re-arms after each reply and closes on this much silence
// (or a tap, or the cap). It is a variable only so tests can shorten it.
var listenWindow = 30 * time.Second

func recordTarget(s string) (string, bool) {
	s = strings.TrimSpace(s)
	return s, s == "" || targetRE.MatchString(s)
}

func (d *terminalSession) noticeBusy() bool {
	return d.busy || d.recording.active() || d.queuedVoice != nil || d.stopAudio != nil || d.awaitAck
}

func (h *terminalHub) bridgeEndpoint(d *terminalSession) bool {
	return h.b.endpoint != nil && d.caps.BridgeEndpoint
}

func (h *terminalHub) startRecording(d *terminalSession, target string, now time.Time, request bool) {
	bridgeEndpoint := request && target == "" && h.bridgeEndpoint(d)
	if bridgeEndpoint && d.queuedVoice != nil {
		// Recording remains explicitly initiated; no automatic reopening.
		return
	}
	h.wake(d, now)
	if !bridgeEndpoint {
		h.cancelTurn(d, "recording")
	}
	d.drawer, d.card, d.pending, d.view = "", nil, nil, nil
	if !bridgeEndpoint || d.ttsStarted || d.awaitAck {
		h.stopPlayback(d, true)
	}
	d.recording.start(now, target, request)
	if request {
		d.recSession++
	}
	d.recording.session = d.recSession
	if w := h.b.worker; target != "" && w != nil && w.ready.Load() {
		// A voice note can use the cached introduction without waiting on
		// synthesis. Warm it while its sender is recording, not on every hello.
		intro, voice := "From "+d.companion.Name+"!", d.voice
		go func() {
			ctx, cancel := context.WithTimeout(d.client.ctx, ttsWait)
			defer cancel()
			_, _ = h.b.clips.getWith(ctx, w.warm, voice, intro)
		}()
	}
	if request {
		if bridgeEndpoint {
			h.startEndpoint(d)
		}
		h.pushRecStart(d, target, bridgeEndpoint, false)
	}
	h.takeVoiceOwnership(d)
	h.setCompanionStatus(d.companionID.get(), statusListening)
	h.render(d, now, false)
}

func (h *terminalHub) startListeningWindow(d *terminalSession, now time.Time) bool {
	if !h.active(d) || !d.companion.Listening || !h.voiceOwnerIs(d) || !h.bridgeEndpoint(d) {
		return false
	}
	if d.busy || d.recording.active() || d.queuedVoice != nil || len(d.queuedNotices) > 0 || d.ttsStarted || d.awaitAck || d.drawer != "" {
		return false
	}
	h.wake(d, now)
	d.drawer, d.card, d.pending, d.view = "", nil, nil, nil
	span := listenWindow
	d.recording.start(now, "", true)
	d.recording.listen = true
	d.recording.listenSpan = span
	d.recording.listenUntil = now.Add(span)
	d.recording.nameGate = true
	d.recSession++
	d.recording.session = d.recSession
	h.b.diag.listen.windows.Add(1)
	h.startEndpoint(d)
	h.pushRecStart(d, "", true, true)
	h.setCompanionStatus(d.companionID.get(), statusListening)
	h.render(d, now, false)
	return true
}

// startCaptureWindow opens a debug-only raw capture on the companion's voice
// owner. It uses the normal listening REC_CTRL path, records for the full
// span regardless of VAD, and never sends a turn.
func (h *terminalHub) startCaptureWindow(d *terminalSession, now time.Time, span time.Duration) bool {
	if !h.active(d) || !h.voiceOwnerIs(d) || !h.bridgeEndpoint(d) {
		return false
	}
	if d.busy || d.recording.active() || d.queuedVoice != nil || d.ttsStarted || d.awaitAck {
		return false
	}
	h.wake(d, now)
	d.drawer, d.card, d.pending, d.view = "", nil, nil, nil
	d.recording.start(now, "", true)
	d.recording.listen = true
	d.recording.listenSpan = span
	d.recording.listenUntil = now.Add(span)
	d.recording.debug = true
	d.recSession++
	d.recording.session = d.recSession
	h.startEndpoint(d)
	h.pushRecStart(d, "", true, true)
	h.setCompanionStatus(d.companionID.get(), statusListening)
	h.render(d, now, false)
	return true
}

func (h *terminalHub) startStoryChoiceListen(d *terminalSession, now time.Time) bool {
	if !h.active(d) || d.drawer != drawerStoryReader || d.story == nil || len(d.story.choices) == 0 ||
		!h.voiceOwnerIs(d) || !h.bridgeEndpoint(d) {
		return false
	}
	if d.busy || d.recording.active() || d.queuedVoice != nil || len(d.queuedNotices) > 0 || d.ttsStarted || d.awaitAck {
		return false
	}
	h.wake(d, now)
	span := listenWindow
	d.recording.start(now, "", true)
	d.recording.listen = true
	d.recording.listenSpan = span
	d.recording.listenUntil = now.Add(span)
	d.recording.nameGate = true
	d.recSession++
	d.recording.session = d.recSession
	h.b.diag.listen.windows.Add(1)
	h.startEndpoint(d)
	h.pushRecStart(d, "", true, true)
	h.setCompanionStatus(d.companionID.get(), statusListening)
	h.render(d, now, false)
	return true
}

func (h *terminalHub) tryStoryVoiceChoice(d *terminalSession, turnID uint64, text string) bool {
	handled := false
	d.update(func(d *terminalSession) {
		if !h.active(d) || d.turn != turnID || d.drawer != drawerStoryReader || d.story == nil || len(d.story.choices) == 0 {
			return
		}
		idx, ok := stories.MatchChoice(text, d.story.choices)
		if !ok {
			return
		}
		target := d.story.choices[idx].Target
		d.busy = false
		d.syncMode()
		d.turn = 0
		h.setCompanionStatus(d.companionID.get(), statusIdle)
		h.advanceStoryPassage(d, target, true)
		handled = true
	})
	return handled
}

func (h *terminalHub) stopListeningWindow(d *terminalSession, now time.Time) {
	if d.recording.active() && d.recording.listen {
		h.b.diag.listen.closeTap.Add(1)
		h.recordingControl(d, now, true)
	}
}

func (h *terminalHub) recordingControl(d *terminalSession, now time.Time, cancelled bool) {
	c := &d.recording
	if !c.active() {
		return
	}
	if cancelled {
		h.pushRecStop(d, c.target, true)
		c.suppressLateStart()
		h.endRecording(d, "", true, now)
		return
	}
	if !c.requestStop() {
		return
	}
	if c.endpoint != nil {
		c.endpoint.commitAt = now
	}
	h.pushRecStop(d, "", false)
	// Keep accepting DMA packets until the board's rec_end. The timer is
	// tied to this stop request, so it cannot end a subsequent recording.
	d.recStop++
	stopID := d.recStop
	timer := time.AfterFunc(recStopWait, func() {
		now := time.Now()
		if d.actor == nil {
			// Unit tests drive bare sessions without an actor.
			h.captureStopTimer(d, stopID, now)
			return
		}
		d.post(terminalEvent{kind: evRecTimer, now: now, recTimer: terminalRecTimer{id: stopID}})
	})
	c.stopTimer = timer
}

func (h *terminalHub) captureStopTimer(d *terminalSession, stopID uint64, now time.Time) {
	c := &d.recording
	if h.active(d) && c.stopTimer != nil && d.recStop == stopID {
		c.suppressLateStart()
		h.endRecording(d, "", false, now)
	}
}

func (h *terminalHub) dispatchRTPPCM(c *terminalClient, pt uint8, pcm []byte) {
	if pt == rtpPTRecPCM {
		h.micChunkPCM(c, micPrefix, pcm)
	}
}

func (h *terminalHub) micChunkPCM(c *terminalClient, prefix byte, pcm []byte) {
	if len(pcm) < 2 || len(pcm)%2 != 0 || prefix != micPrefix {
		return
	}
	if ep := c.activeCapture.Load(); ep != nil {
		if len(pcm) <= 2*pcmPacket && ep.push(pcm, time.Now()) {
			return
		}
		d := c.terminal.Load()
		if d != nil {
			d.update(func(d *terminalSession) {
				if h.active(d) && d.recording.endpoint == ep {
					h.recordingControl(d, time.Now(), true)
				}
			})
		}
		return
	}
	d := c.terminal.Load()
	if d == nil {
		return
	}
	// Legacy in-band capture: append into the session's preallocated buffer on
	// the read loop (allocation-free), then let the actor react at the cap.
	rec := &d.recording
	mu := rec.lock()
	mu.Lock()
	if !rec.accepting.load() {
		mu.Unlock()
		return
	}
	room := (maxPCM - len(rec.pcm)) &^ 1
	n := min(len(pcm), room)
	rec.pcm = append(rec.pcm, pcm[:n]...)
	full := n == room
	mu.Unlock()
	if full {
		d.post(terminalEvent{kind: evCapturePCM, now: time.Now()})
	}
}

// captureLimit runs when the raw capture buffer reached its cap.
func (h *terminalHub) captureLimit(d *terminalSession, now time.Time) {
	rec := &d.recording
	if !h.active(d) || !rec.open() || rec.endpoint != nil {
		return
	}
	mu := rec.lock()
	mu.Lock()
	full := len(rec.pcm) >= maxPCM
	mu.Unlock()
	if full {
		h.stopRecordingAtLimit(d, now)
	}
}

func (h *terminalHub) stopRecordingAtLimit(d *terminalSession, now time.Time) {
	h.pushRecStop(d, "", false)
	h.endRecording(d, "", false, now)
}

func (h *terminalHub) endRecording(d *terminalSession, reqTarget string, cancelled bool, now time.Time) {
	if d.recording.endpoint != nil && !cancelled {
		h.finishEndpoint(d, now)
		return
	}
	// Stop accepting audio before taking the buffer so a late DMA packet
	// cannot land in the turn that is about to start.
	d.recording.accepting.store(false)
	mu := d.recording.lock()
	mu.Lock()
	pcm := d.recording.pcm
	d.recording.pcm = nil
	mu.Unlock()
	target := d.recording.target
	if requested, ok := recordTarget(reqTarget); ok && requested != "" {
		target = requested
	}
	d.recording.clear()
	if cancelled || len(pcm) == 0 {
		if cancelled {
			h.pushChime(d, "cancel")
		}
		h.setCompanionStatus(d.companionID.get(), statusIdle)
		h.wake(d, now)
		if !h.maybeStartNotice(d, now) {
			h.render(d, now, false)
		}
		return
	}
	h.startTurn(d, now, false, "voice", func(ctx context.Context, id uint64) {
		h.runVoice(ctx, d, id, target, pcm)
	})
}

func (h *terminalHub) interruptTurn(d *terminalSession, reason string) {
	if d.busy && h.b.oc != nil && d.companionID.get() != "" {
		companionID := d.companionID.get()
		// Interrupting OpenCode is a network call; the actor keeps running.
		go func() {
			log.Printf("terminal interrupt companion=%s reason=%s", companionID, reason)
			h.b.oc.interruptDevice(companionID)
		}()
	}
}

// cancelTurn runs in the session actor.
func (h *terminalHub) cancelTurn(d *terminalSession, reason string) {
	h.interruptTurn(d, reason)
	h.cancelLocalTurn(d)
}

func (h *terminalHub) cancelLocalTurn(d *terminalSession) {
	defer d.syncMode()
	if c := d.queuedVoice; c != nil {
		d.queuedVoice = nil
		c.cancel()
	}
	d.turn++
	d.trace = nil
	d.busy = false
	d.syncMode()
	d.resetStopTaps()
	d.agentSetScene = false
	if d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
	h.sig.notify()
}

func (h *terminalHub) startTurn(d *terminalSession, now time.Time, keepView bool, source string, run func(context.Context, uint64)) bool {
	if d.companionID.get() == "" {
		return false
	}
	h.cancelTurn(d, "superseded")
	h.beginTurn(d, now, keepView, source, run)
	return true
}

func (h *terminalHub) beginTurn(d *terminalSession, now time.Time, keepView bool, source string, run func(context.Context, uint64)) {
	h.wake(d, now)
	h.stopPlayback(d, true)
	if d.recording.active() {
		h.pushRecStop(d, d.recording.target, false)
		d.recording.clear()
	}
	d.drawer, d.card, d.pending = "", nil, nil
	if !keepView {
		d.view = nil
	}
	ctx, cancel := context.WithTimeout(d.client.ctx, turnTimeout)
	ctx = h.b.traceTurn(ctx, d.companionID.get(), d.companion.Name, source, now)
	d.trace = traceFrom(ctx)
	id := d.turn
	d.cancel, d.busy = cancel, true
	d.syncMode()
	if !keepView {
		h.pushChime(d, "think")
	}
	h.setCompanionStatus(d.companionID.get(), statusThinking)
	h.render(d, now, false)
	go run(ctx, id)
}

// Protected by the hub lock. Eager playback can fail before the final reply is
// known, or after the turn has handed it off to playback.
type turnAudio struct {
	stream *AudioStream
	turn   uint64
	answer *sceneView
	failed bool
}

func (h *terminalHub) finishTurn(d *terminalSession, id uint64, view *sceneView, audio *AudioStream) {
	defer d.syncMode()
	defer h.sig.notify()
	if !h.active(d) || d.turn != id {
		return
	}
	d.busy = false
	d.finished++
	now := time.Now()
	d.last = now
	if audio != nil {
		switch a := d.replyAudio; {
		case a == nil || a.stream != audio:
			view = nil // Playback completed, was interrupted, or was replaced.
		case !a.failed:
			a.answer, view = view, nil
		default:
			d.replyAudio = nil
		}
	}
	if view != nil || audio != nil {
		// A visible reply grants follow-ups now; spoken replies refresh the
		// grace again when their playback completes.
		h.freeFollowUpsNow(d, now)
	}
	if view != nil && !d.agentSetScene {
		if d.stopAudio != nil || d.awaitAck {
			d.pending = view
		} else {
			d.view = view
		}
	}
	if d.stopAudio == nil && d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
	if d.queuedVoice != nil {
		d.trace = nil
		if h.maybeStartVoice(d, now) {
			return
		}
	}
	if !d.ttsStarted && !d.awaitAck && d.stopAudio == nil && d.replyAudio == nil {
		if d.queuedVoice != nil {
			h.setCompanionStatus(d.companionID.get(), statusThinking)
		} else if st, ok := h.statusState(d.companionID.get()); !ok || st.state != statusError {
			h.setCompanionStatus(d.companionID.get(), statusDone)
		}
	}
	if h.maybeStartNotice(d, now) {
		d.trace = nil
		return
	}
	if !d.ttsStarted && !d.awaitAck && d.stopAudio == nil && d.replyAudio == nil {
		if st, ok := h.statusState(d.companionID.get()); !ok || st.state != statusError {
			if h.startListeningWindow(d, now) {
				d.trace = nil
				return
			}
		}
	}
	h.render(d, now, false)
	d.trace = nil
}

func (h *terminalHub) turnError(d *terminalSession, id uint64, message string) {
	d.post(terminalEvent{kind: evTurnError, now: time.Now(), turnErr: terminalTurnError{id: id, message: message}})
}

func (h *terminalHub) turnFailed(d *terminalSession, id uint64, message string) {
	if d.turn == id && d.trace != nil {
		d.trace.fail()
	}
	if h.active(d) && d.turn == id {
		h.setCompanionStatus(d.companionID.get(), statusError)
	}
	view := noticeScene("Try again", message)
	h.finishTurn(d, id, &view, nil)
}

// stopCompanionTurn asks the session actor to stop a turn in flight.
func (h *terminalHub) stopCompanionTurn(d *terminalSession) {
	d.post(terminalEvent{kind: evStopTurn, now: time.Now()})
}

func (h *terminalHub) stopCompanionTurns(ids ...string) {
	for _, id := range ids {
		for _, d := range h.liveFor(id) {
			h.stopCompanionTurn(d)
		}
	}
}

func (h *terminalHub) stopTurnNow(d *terminalSession, now time.Time) {
	h.cancelLocalTurn(d)
	h.stopPlayback(d, true)
	if d.recording.active() {
		h.pushRecStop(d, d.recording.target, false)
		d.recording.clear()
	}
	h.render(d, now, false)
}

// finishTurnAsync posts a completion from a turn goroutine.
func (h *terminalHub) finishTurnAsync(d *terminalSession, id uint64, view *sceneView, audio *AudioStream) {
	d.update(func(d *terminalSession) { h.finishTurn(d, id, view, audio) })
}

func (h *terminalHub) runVoice(ctx context.Context, d *terminalSession, id uint64, target string, pcm []byte) {
	if target != "" {
		sender := h.b.companionFor(d.companionID.get())
		others, err := h.peerSnapshot(sender.ID)
		if err != nil {
			h.turnError(d, id, "Could not find nearby companions.")
			return
		}
		recipients := h.b.intercomTargets(others, target)
		voice := h.b.voiceFor(sender.ID)
		receipts := h.deliverOnline(ctx, sender, recipients, voice, "", pcm)
		// A card reply to a Delta Chat contact has no online peer. Delta Chat
		// is text-only off-device, so transcribe the clip and send clean text.
		if len(receipts) == 0 && h.b.dc != nil && h.b.dc.ready.Load() {
			transcript, err := h.b.transcribe(ctx, pcm)
			words, fillerOnly, _ := inspectTranscript(transcript)
			if err != nil || words == 0 || fillerOnly {
				h.replyNotCaught(ctx, d, id)
				return
			}
			h.sendIntercom(ctx, d, id, others, target, strings.TrimSpace(transcript))
			return
		}
		// Connected peers can play the original clip before transcription.
		if len(receipts) != 0 {
			go func() {
				captionCtx, cancel := context.WithTimeout(context.WithValue(h.b.root, traceKey{}, traceFrom(ctx)), captionWait)
				defer cancel()
				transcript, _ := h.b.transcribe(captionCtx, pcm)
				h.caption(receipts, sender, transcript)
				ids := make([]string, 0, len(receipts))
				for _, r := range receipts {
					ids = append(ids, r.terminal.companionID.get())
				}
				h.b.recordIntercom(sender, ids, transcript)
			}()
		}
		h.confirm(ctx, d, id, target, recipients, len(receipts) != 0, len(receipts) < len(recipients))
		return
	}
	text, err := h.b.transcribe(ctx, pcm)
	if err != nil {
		h.turnError(d, id, "Could not hear that. Try again.")
		return
	}
	if text == "" {
		h.finishTurnAsync(d, id, nil, nil)
		return
	}
	if h.tryStoryVoiceChoice(d, id, text) {
		return
	}
	h.runTurn(ctx, d, id, text, "voice", pcm)
}

func (h *terminalHub) runTurn(ctx context.Context, d *terminalSession, id uint64, text, source string, pcm []byte) {
	h.runTurnChannel(ctx, d, id, text, source, pcm, turnChannel{})
}

func (h *terminalHub) runTurnChannel(ctx context.Context, d *terminalSession, id uint64, text, source string, pcm []byte, ch turnChannel) {
	if ch.spec != nil {
		defer func() {
			if !ch.spec.isPromoted() {
				ch.spec.abort()
			}
		}()
	}
	if d.companionID.get() == "" {
		h.turnError(d, id, "This terminal is not attached to a companion.")
		return
	}
	sender := h.b.companionFor(d.companionID.get())
	if traceFrom(ctx) == nil {
		ctx = h.b.traceTurn(ctx, sender.ID, sender.Name, source, time.Now())
	}
	if ctx.Err() != nil {
		h.finishTurnAsync(d, id, nil, nil)
		return
	}
	others, direct, err := h.b.intercomIntent(sender.ID, text)
	if err != nil || others != nil && ctx.Err() != nil {
		h.turnError(d, id, "Could not find nearby companions.")
		return
	}
	if direct != nil {
		h.consumeReply(d, ch.reply)
		h.b.chat(sender.ID, newChat("user", "You", text, source), true)
		h.sendIntercom(ctx, d, id, others, direct.target, direct.text)
		return
	}
	if h.b.oc == nil {
		h.turnError(d, id, "Chat is not ready yet.")
		return
	}
	// Consume the read-out context now: its reply is this turn, even if the
	// model fails or the child interrupts playback.
	h.consumeReply(d, ch.reply)
	gen, err := h.b.generateReply(ctx, ctx, sender, text, turnOptions{source: source, channel: ch, spec: ch.spec,
		publish: func(stream *AudioStream, start time.Time) {
			d.update(func(d *terminalSession) { h.publish(d, id, ctx, stream, start) })
		}})
	if err != nil {
		h.turnError(d, id, "Could not finish that turn.")
		return
	}
	h.deliverDeviceReply(ctx, ch.reply, gen)
	var view *sceneView
	if gen.reply != "" && ctx.Err() == nil {
		v := noticeScene("Answer", shortText(gen.reply, maxMessage, true))
		view = &v
	}
	h.finishTurnAsync(d, id, view, gen.audio)
}

// deliverDeviceReply sends a spoken reply back to the Delta Chat chat whose
// message the device read out. It is skipped when the agent routed the turn
// with emit, so an explicit target always wins.
func (h *terminalHub) deliverDeviceReply(ctx context.Context, reply *turnReply, gen generatedReply) {
	if reply == nil || gen.reply == "" || gen.routed || ctx.Err() != nil {
		return
	}
	if h.b.dc == nil || !h.b.dc.ready.Load() {
		return
	}
	if err := h.b.dc.sendDeviceReply(ctx, reply.companionID, reply.chatID, gen.reply); err != nil {
		log.Printf("Delta Chat reply error: %v", err)
	}
}

func (h *terminalHub) consumeReply(d *terminalSession, reply *turnReply) {
	if reply == nil {
		return
	}
	d.update(func(d *terminalSession) {
		if d.reply == reply {
			d.clearReply()
		}
	})
}

// dropStaleReply clears a read-out armed while this capture was already open:
// the notice interrupted that capture, so its turn cannot answer it. Without
// this a queued notice can leak its reply context into later turns.
func (h *terminalHub) dropStaleReply(d *terminalSession, capturedAt time.Time) {
	if capturedAt.IsZero() {
		return
	}
	d.update(func(d *terminalSession) {
		if d.reply != nil && capturedAt.Before(d.replySpoken) {
			d.clearReply()
		}
	})
}

func (h *terminalHub) sendIntercom(ctx context.Context, d *terminalSession, id uint64, others []companionTarget, target, text string) {
	sender := h.b.companionFor(d.companionID.get())
	if traceFrom(ctx) == nil {
		ctx = h.b.traceTurn(ctx, sender.ID, sender.Name, "intercom", time.Now())
	}
	if h.b.dc != nil && h.b.dc.ready.Load() {
		sentTo, err := h.b.dc.sendIntercom(ctx, sender.ID, target, text)
		recipients := h.b.intercomTargets(others, target)
		if len(recipients) == 0 {
			// Delta Chat names human contacts, who are not LAN peers.
			for _, name := range sentTo {
				recipients = append(recipients, companionTarget{ID: name, Name: name})
			}
		}
		h.confirm(ctx, d, id, target, recipients, err == nil && len(sentTo) > 0, false)
		return
	}
	recipients := h.b.intercomTargets(others, target)
	res := h.b.deliver(ctx, sender, recipients, text, nil)
	h.confirm(ctx, d, id, target, recipients, len(res.sent) != 0, len(res.failed) != 0)
}

func (h *terminalHub) confirm(ctx context.Context, d *terminalSession, id uint64, target string, recipients []companionTarget, sent, partial bool) {
	title, reply := intercomConfirmation(target, recipients, sent, partial)
	view := noticeScene(title, reply)
	h.sayTurn(ctx, d, id, reply, &view)
}

// replyNotCaught speaks a pre-cached line for speech that produced no usable
// transcript. It never reaches the agent session, and its playback completion
// re-arms the listening window through the normal path.
// nameRequired reports whether a new listening turn must contain the
// companion's name. The 10 s grace starts when the previous reply finished.
func (h *terminalHub) nameRequired(d *terminalSession, now time.Time) bool {
	st := d.state()
	return !st.nameFreeUntil.IsZero() && now.After(st.nameFreeUntil)
}

func (h *terminalHub) replyNotCaught(ctx context.Context, d *terminalSession, id uint64) {
	h.b.diag.listen.didntCatch.Add(1)
	d.update(func(d *terminalSession) { h.freeFollowUpsNow(d, time.Now()) })
	w := h.b.worker
	if w == nil || !w.ready.Load() {
		d.update(func(d *terminalSession) { h.finishTurn(d, id, nil, nil) })
		return
	}
	st := d.state()
	text := notCaughtLines[int(st.finished)%len(notCaughtLines)]
	start := time.Now()
	if traceFrom(ctx) == nil {
		ctx = h.b.traceTurn(ctx, d.companionID.get(), h.b.companionFor(d.companionID.get()).Name, "voice", start)
	}
	voice := h.b.voiceFor(d.companionID.get())
	stream := newAudioStream()
	d.update(func(d *terminalSession) { h.publish(d, id, ctx, stream, start) })
	pcm := h.b.clips.peek(voice, text)
	if pcm == nil {
		pcm, _ = w.synthesize(ctx, voice, text)
	}
	if !stream.append(pcm) {
		stream.finish(errTTS)
	} else {
		stream.finish(nil)
	}
	d.update(func(d *terminalSession) { h.finishTurn(d, id, nil, nil) })
}

func (h *terminalHub) sayTurn(ctx context.Context, d *terminalSession, id uint64, text string, view *sceneView) {
	start := time.Now()
	if traceFrom(ctx) == nil {
		ctx = h.b.traceTurn(ctx, d.companionID.get(), h.b.companionFor(d.companionID.get()).Name, "tap", start)
	}
	w := h.b.worker
	if w != nil && w.ready.Load() {
		stream := newAudioStream()
		d.update(func(d *terminalSession) { h.publish(d, id, ctx, stream, start) })
		voice := h.b.voiceFor(d.companionID.get())
		pcm := h.b.clips.peek(voice, text)
		if pcm == nil {
			clean, err := spokenText(text)
			if err == nil {
				pcm, _ = w.synthesize(ctx, voice, clean)
			}
		}
		if !stream.append(pcm) {
			stream.finish(errTTS)
		} else {
			stream.finish(nil)
		}
	}
	d.update(func(d *terminalSession) { h.finishTurn(d, id, view, nil) })
}

// publish runs in the session actor.
func (h *terminalHub) publish(d *terminalSession, id uint64, ctx context.Context, stream *AudioStream, start time.Time) {
	if !h.active(d) || d.turn != id || ctx.Err() != nil {
		return
	}
	if stream == nil {
		h.stopPlayback(d, true)
		h.playbackDone(d, time.Now(), true)
		return
	}
	// The turn deadline bounds synthesis; it must not cut off paced playback
	// of PCM that is already available. An interrupt still calls stopPlayback.
	h.playTraced(d, stream, traceFrom(ctx), start)
	d.replyAudio = &turnAudio{stream: stream, turn: id}
}

// play starts playback from outside the actor.
func (h *terminalHub) play(d *terminalSession, stream *AudioStream) context.Context {
	var playback context.Context
	d.update(func(d *terminalSession) {
		playback = h.playTraced(d, stream, nil, time.Time{})
	})
	if playback == nil {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	return playback
}

// playTraced starts playback and returns its cancel context. The caller is the
// session actor.
func (h *terminalHub) playTraced(d *terminalSession, stream *AudioStream, trace *turnTrace, ttsStart time.Time) context.Context {
	ctx, cancel := context.WithCancel(d.client.ctx)
	h.stopPlayback(d, true)
	d.stopAudio = cancel
	id := d.audio
	go func() {
		defer cancel()
		err := h.streamPlayback(ctx, d, id, stream, trace, ttsStart)
		d.update(func(d *terminalSession) {
			if !h.active(d) || d.audio != id {
				return
			}
			d.stopAudio = nil
			if err != nil {
				a := d.replyAudio
				h.stopPlayback(d, true)
				if a != nil && d.turn == a.turn && ctx.Err() == nil && !errors.Is(err, context.Canceled) {
					if a.answer == nil && d.busy {
						a.failed = true
						d.replyAudio = a
					} else if a.answer != nil && !d.agentSetScene {
						d.pending = a.answer
					}
				}
				h.playbackDone(d, time.Now(), true)
			} else {
				d.replyAudio = nil
				d.ttsStarted = false
				d.syncMode()
			}
			if !d.busy && d.cancel != nil {
				d.cancel()
				d.cancel = nil
			}
			now := time.Now()
			if !h.maybeStartVoice(d, now) {
				h.maybeStartNotice(d, now)
			}
		})
	}()
	return ctx
}

func (h *terminalHub) streamPlayback(ctx context.Context, d *terminalSession, id uint64, stream *AudioStream, trace *turnTrace, ttsStart time.Time) error {
	control := func(state string) bool {
		var op uint8 = ttsOpStop
		switch state {
		case "start":
			op = ttsOpStart
		case "end":
			op = ttsOpEnd
		}
		return d.client.enqueueProtoWait(ctx, msgTTSCtrl, encodeTTSCtrl(op, id))
	}
	var until time.Time
	for i := 0; ; i++ {
		pcm, err := stream.chunk(ctx, i)
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if len(pcm) == 0 {
			break
		}
		if i == 0 {
			if err := stream.waitBytes(ctx, utteranceBytes(ttsPrefill), ttsPrefillMax); err != nil {
				return err
			}
			stream.traceFirst(trace, ttsStart)
			// A listening window still open must finish before the reply speaks
			// into it; the window closes its own capture when it ends.
			for {
				var listening <-chan struct{}
				d.update(func(d *terminalSession) {
					if h.active(d) && d.audio == id && ctx.Err() == nil && d.recording.endpoint != nil {
						listening = d.recording.endpoint.listening
					}
				})
				if listening == nil {
					break
				}
				select {
				case <-listening:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			started := false
			d.update(func(d *terminalSession) {
				if !h.active(d) || d.audio != id || ctx.Err() != nil {
					return
				}
				d.ttsStarted = true
				d.syncMode()
				h.setCompanionStatus(d.companionID.get(), statusSpeaking)
				started = true
			})
			if !started {
				return context.Canceled
			}
			if !control("start") {
				return context.Canceled
			}
			d.update(func(d *terminalSession) {
				if h.active(d) && d.audio == id {
					h.render(d, time.Now(), false)
				}
			})
		}
		for len(pcm) > 0 {
			n := min(pcmPacket, len(pcm))
			duration := time.Duration(n) * time.Second / (2 * sampleRate)
			if wait := time.Until(until.Add(duration).Add(-maxAudioLead)); !until.IsZero() && wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				}
			}
			if !d.client.enqueueProtoWait(ctx, msgTTSPCM, pcm[:n]) {
				return context.Canceled
			}
			now := time.Now()
			if until.IsZero() {
				trace.audioEnqueued(now)
			}
			if until.Before(now) {
				until = now
			}
			until = until.Add(duration)
			pcm = pcm[n:]
		}
	}
	status := 0 // 0 cancelled, 1 no audio, 2 awaiting ack
	d.update(func(d *terminalSession) {
		if !h.active(d) || d.audio != id || ctx.Err() != nil {
			return
		}
		if until.IsZero() {
			status = 1
			return
		}
		// A fast client can acknowledge as soon as the end frame is queued.
		d.audioUntil, d.awaitAck, d.ackClient = until, true, d.client
		status = 2
	})
	switch status {
	case 0:
		return context.Canceled
	case 1:
		return errTTS
	}
	if !control("end") {
		return context.Canceled
	}
	return nil
}

func (h *terminalHub) stopPlayback(d *terminalSession, notify bool) {
	if d.stopAudio != nil {
		d.stopAudio()
		d.stopAudio = nil
	}
	if notify && (d.ttsStarted || d.awaitAck) {
		h.pushTTSAbort(d, d.audio)
	}
	d.ttsStarted, d.awaitAck, d.ackClient = false, false, nil
	d.replyAudio = nil
	d.audio++
	d.syncMode()
}

func (h *terminalHub) playbackDone(d *terminalSession, now time.Time, aborted bool) {
	if !aborted {
		h.freeFollowUpsNow(d, now)
		d.armReplyTTL(now)
	}
	d.awaitAck, d.ackClient = false, nil
	d.syncMode()
	if d.pending != nil {
		d.view, d.pending = d.pending, nil
	}
	d.last = now
	if !d.busy {
		if st, ok := h.statusState(d.companionID.get()); !ok || st.state != statusError {
			h.setCompanionStatus(d.companionID.get(), statusDone)
		}
	}
	if h.maybeStartVoice(d, now) || h.maybeStartNotice(d, now) {
		return
	}
	if !aborted && h.startStoryChoiceListen(d, now) {
		return
	}
	if !aborted && h.startListeningWindow(d, now) {
		return
	}
	h.render(d, now, false)
}

// A spoken reply buys free follow-ups without the companion's name.
// freeFollowUps refreshes the no-name window from outside the actor.
func (h *terminalHub) freeFollowUps(d *terminalSession, now time.Time) {
	d.update(func(d *terminalSession) { h.freeFollowUpsNow(d, now) })
}

// freeFollowUpsNow runs in the session actor.
func (h *terminalHub) freeFollowUpsNow(d *terminalSession, now time.Time) {
	d.nameFreeUntil = now.Add(followUpGrace)
}

type noticeReceipt struct {
	terminal *terminalSession
	view     *sceneView
}

func (b *larkServer) intercomTargets(others []companionTarget, target string) []companionTarget {
	if !broadcast(target) {
		return targets(others, target)
	}
	if b.hub == nil {
		return nil
	}
	h := b.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	var connected []companionTarget
	for _, p := range others {
		if d := h.voiceOwner(p.ID); d != nil {
			connected = append(connected, p)
		}
	}
	return connected
}

type queuedNotice struct {
	ctx     context.Context
	fromPCM bool
	view    *sceneView
	chime   string
	voice   string
	intro   string
	text    string
	stream  *AudioStream
	reply   *turnReply
	trace   *turnTrace
}

type terminalNotice struct {
	recipientIDs []string
	sender       companionState
	text         string
	speakText    string
	intro        string
	voice        string
	pcm          []byte
	chime        string
	showCard     bool
	reply        *turnReply
	cardReply    string // reply-button target; defaults to the card sender
	trace        *turnTrace
}

// intercomNotice is the one message-card shape shared by online intercom,
// Delta Chat deliveries, /say and emit: the reply button targets the sender.
func intercomNotice(sender companionState, recipients []string, text string, pcm []byte, voice string) terminalNotice {
	intro := "Message from " + sender.Name + "!"
	if pcm != nil {
		intro = "From " + sender.Name + "!"
	}
	return terminalNotice{
		recipientIDs: recipients,
		sender:       sender,
		text:         text,
		speakText:    text,
		intro:        intro,
		voice:        voice,
		pcm:          pcm,
		chime:        "intercom",
		showCard:     true,
		cardReply:    sender.Name,
	}
}

func (h *terminalHub) deliverOnline(ctx context.Context, sender companionState, recipients []companionTarget, voice, text string, pcm []byte) []noticeReceipt {
	ids := make([]string, 0, len(recipients))
	for _, p := range recipients {
		ids = append(ids, p.ID)
	}
	n := intercomNotice(sender, ids, text, pcm, voice)
	n.trace = traceFrom(ctx)
	return h.deliverNotice(ctx, n)
}

func (h *terminalHub) deliverNotice(ctx context.Context, n terminalNotice) []noticeReceipt {
	if h == nil || h.b == nil || len(n.recipientIDs) == 0 || len(n.pcm) > maxPeerPCM {
		return nil
	}
	text := shortText(n.text, maxMessage, true)
	speakText := shortText(n.speakText, maxMessage, true)
	var view *sceneView
	if n.showCard {
		v := (intercomCard{companion: n.sender.Name, reply: n.cardReply, text: text}).scene()
		view = &v
	}
	var targets []*terminalSession
	seen := make(map[*terminalSession]bool)
	for _, id := range n.recipientIDs {
		for _, d := range h.liveFor(id) {
			if !seen[d] {
				seen[d] = true
				targets = append(targets, d)
			}
		}
	}
	if len(targets) == 0 || ctx.Err() != nil {
		return nil
	}
	receipts := make([]noticeReceipt, 0, len(targets))
	for _, d := range targets {
		q := queuedNotice{
			view: view, chime: n.chime, voice: n.voice, intro: n.intro,
			text: speakText, reply: n.reply, trace: n.trace, ctx: ctx, fromPCM: n.pcm != nil,
		}
		if n.pcm != nil {
			// Each terminal plays its own copy: two actors must not consume one
			// stream concurrently.
			stream := newAudioStream()
			if n.intro != "" {
				if intro := h.b.clips.peek(n.voice, n.intro); intro != nil && len(intro)+len(n.pcm) <= maxPeerPCM {
					stream.append(intro)
				}
			}
			stream.append(NormalizeVoicePCM(n.pcm))
			stream.finish(nil)
			q.stream = stream
		}
		ack := make(chan struct{})
		if d.post(terminalEvent{kind: evNotice, now: time.Now(), queued: q, done: ack}) {
			<-ack
		}
		receipts = append(receipts, noticeReceipt{d, view})
	}
	return receipts
}

// deliverNoticeEvents runs in the session actor.
func (h *terminalHub) deliverNoticeEvent(d *terminalSession, q queuedNotice, now time.Time) {
	if !h.active(d) || q.ctx != nil && q.ctx.Err() != nil {
		return
	}
	if d.noticeBusy() {
		// Queued notices synthesize their own stream when drained so they don't race or expire.
		if q.fromPCM == false {
			q.stream = nil
		}
		d.queuedNotices = append(d.queuedNotices, q)
		return
	}
	if q.stream == nil && (q.text != "" || q.intro != "") {
		q.stream = newAudioStream()
		h.deliverNoticeNow(d, now, q)
		go h.speakNotice(q.stream, q.voice, q.intro, q.text)
		return
	}
	h.deliverNoticeNow(d, now, q)
}

func (h *terminalHub) deliverNoticeNow(d *terminalSession, now time.Time, q queuedNotice) {
	d.drawer, d.pending = "", nil
	d.reply = q.reply
	if q.reply != nil {
		// Playing the read-out is not the same as having heard it; the TTL
		// starts when playback completes (see armReplyTTL).
		d.replySpoken = now
		d.replyUntil = time.Time{}
	}
	if q.view != nil {
		d.view, d.card = q.view, nil
	}
	h.wake(d, now)
	h.render(d, now, false)
	if q.chime != "" {
		h.pushChime(d, q.chime)
	}
	if q.stream != nil && d.client.ctx.Err() == nil {
		h.playTraced(d, q.stream, q.trace, time.Time{})
	}
}

func (h *terminalHub) maybeStartNotice(d *terminalSession, now time.Time) bool {
	if len(d.queuedNotices) == 0 || d.noticeBusy() || !h.active(d) {
		return false
	}
	q := d.queuedNotices[0]
	d.queuedNotices = d.queuedNotices[1:]
	if q.stream == nil && (q.text != "" || q.intro != "") {
		q.stream = newAudioStream()
		h.deliverNoticeNow(d, now, q)
		go h.speakNotice(q.stream, q.voice, q.intro, q.text)
		return true
	}
	h.deliverNoticeNow(d, now, q)
	return true
}

func (h *terminalHub) speakNotice(stream *AudioStream, voice, introText, text string) {
	w := h.b.worker
	if w == nil || !w.ready.Load() {
		stream.finish(nil) // The card is still delivered when TTS is unavailable.
		return
	}
	ctx, cancel := context.WithTimeout(h.b.root, ttsWait)
	defer cancel()
	if introText != "" {
		if intro, _ := h.b.clips.get(ctx, w, voice, introText); len(intro) > 0 {
			stream.append(intro)
		}
	}
	for _, s := range SplitSentences(text) {
		pcm, err := w.synthesize(ctx, voice, s)
		if err != nil || !stream.append(pcm) {
			stream.finish(errTTS)
			return
		}
	}
	stream.finish(nil)
}

func (h *terminalHub) caption(receipts []noticeReceipt, sender companionState, text string) {
	if len(receipts) == 0 || text == "" {
		return
	}
	view := (intercomCard{companion: sender.Name, text: shortText(text, maxMessage, true)}).scene()
	for _, r := range receipts {
		d := r.terminal
		d.post(terminalEvent{kind: evCaption, now: time.Now(),
			caption: terminalCaption{view: &view, oldView: r.view}})
	}
}

// captionEvent runs in the session actor.
func (h *terminalHub) captionEvent(d *terminalSession, c terminalCaption, now time.Time) {
	if !h.active(d) {
		return
	}
	switch {
	case d.card == c.oldView:
		d.card = c.view
	case d.view == c.oldView:
		d.view = c.view
	default:
		return
	}
	h.render(d, now, false)
}

func (b *larkServer) deliver(ctx context.Context, sender companionState, recipients []companionTarget, text string, raw []byte) delivery {
	sent := make(map[string]bool, len(recipients))
	if b.hub != nil {
		for _, r := range b.hub.deliverOnline(ctx, sender, recipients, b.voiceFor(sender.ID), text, raw) {
			sent[r.terminal.companionID.get()] = true
		}
	}
	out := delivery{sent: []string{}, delivered: []string{}, failed: []string{}}
	for _, p := range recipients {
		if sent[p.ID] {
			out.sent = append(out.sent, p.ID)
			out.delivered = append(out.delivered, p.Name)
		} else {
			out.failed = append(out.failed, p.ID)
		}
	}
	b.recordIntercom(sender, out.sent, text)
	return out
}

func (b *larkServer) recordIntercom(sender companionState, ids []string, text string) {
	if len(ids) == 0 {
		return
	}
	text = shortText(text, maxMessage, true)
	if text == "" {
		text = "Voice message"
	}
	b.chat(sender.ID, newChat("intercom", sender.Name, text, "intercom"), true)
	for _, id := range ids {
		b.chat(id, newChat("intercom", sender.Name, text, "intercom"), true)
	}
}

func (h *terminalHub) speakLocal(d *terminalSession, text string) {
	h.speakLocalAfter(d, text, nil)
}

func (h *terminalHub) speakLocalAfter(d *terminalSession, text string, prefix []byte) {
	worker := h.b.worker
	if worker == nil || !worker.ready.Load() {
		if len(prefix) != 0 {
			h.playTraced(d, audioStreamFrom(prefix), nil, time.Time{})
		}
		return
	}
	voice := d.voice
	if voice == "" {
		voice = h.b.voices.defaultFor(d.voiceTheme())
	}
	start := time.Now()
	var trace *turnTrace
	if h.b != nil {
		trace = traceFrom(h.b.traceTurn(d.client.ctx, d.companionID.get(), d.companion.Name, "tap", start))
	}
	stream := newAudioStream()
	if len(prefix) != 0 {
		stream.append(prefix)
	}
	playback := h.playTraced(d, stream, trace, start)
	go func() {
		ctx, cancel := context.WithTimeout(playback, ttsWait)
		defer cancel()
		if trace != nil {
			ctx = context.WithValue(ctx, traceKey{}, trace)
		}
		pcm := h.b.clips.peek(voice, text)
		if pcm == nil {
			pcm, _ = worker.synthesize(ctx, voice, text)
		}
		if !stream.append(pcm) {
			stream.finish(errTTS)
		} else {
			stream.finish(nil)
		}
	}()
}

// Follow-up policy after a reply. Inside the grace period the child can keep
// talking without saying the companion's name; after it, speech is treated as
// background chatter unless the name (or a near miss) appears early.
const (
	followUpGrace   = 10 * time.Second
	nameMaxDistance = 2
	namePrefixWords = 5
)

var nameLeadWords = map[string]bool{
	"hey": true, "hi": true, "hello": true, "okay": true, "ok": true,
	"so": true, "um": true, "uh": true, "oh": true, "and": true,
}

// splitName returns the words after a leading near-match of name. ok is false
// when the name is absent from the first few words.
func splitName(text, name string) (rest string, ok bool) {
	fields := strings.Fields(text)
	limit := min(len(fields), namePrefixWords)
	lowerName := strings.ToLower(name)
	nameLen := len([]rune(lowerName))
	minLen := max(2, nameLen-nameMaxDistance)
	for i := range limit {
		candidate := strings.ToLower(strings.Trim(fields[i], ".,!?;:'\""))
		if len([]rune(candidate)) < minLen {
			continue
		}
		if nameLen <= 3 {
			// Two- and three-letter names tolerate no edit: "Bo" is not "bo",
			// "no", or "go".
			if candidate != lowerName {
				continue
			}
		} else if editDistance(candidate, lowerName) > nameMaxDistance {
			continue
		}
		rest = strings.Join(fields[i+1:], " ")
		for {
			word, after, _ := strings.Cut(rest, " ")
			if !nameLeadWords[strings.ToLower(strings.Trim(word, ".,!?;:'\""))] {
				break
			}
			rest = after
		}
		return strings.TrimSpace(rest), true
	}
	return "", false
}

// editDistance is the Levenshtein distance over runes, used only for short
// names and transcripts.
func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	if len(ar) == 0 || len(br) == 0 {
		return max(len(ar), len(br))
	}
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(br)]
}
