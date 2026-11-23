package main

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"
)

const (
	micPrefix      = 0x01
	pcmPrefix      = 0x02
	pcmPacket      = 4 << 10
	maxAudioLead   = 8 * time.Second
	recStopWait    = 450 * time.Millisecond
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
	h.setCompanionStatus(d.companionID, statusListening)
	h.render(d, now, false)
}

func (h *terminalHub) startListeningWindow(d *terminalSession, now time.Time) bool {
	if !h.active(d) || !d.companion.Listening || h.voiceOwner(d.companionID) != d || !h.bridgeEndpoint(d) {
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
	d.recSession++
	h.b.diag.listen.windows.Add(1)
	h.startEndpoint(d)
	h.pushRecStart(d, "", true, true)
	h.setCompanionStatus(d.companionID, statusListening)
	h.render(d, now, false)
	return true
}

// startCaptureWindow opens a debug-only raw capture on the companion's voice
// owner. It uses the normal listening REC_CTRL path, records for the full
// span regardless of VAD, and never sends a turn.
func (h *terminalHub) startCaptureWindow(d *terminalSession, now time.Time, span time.Duration) bool {
	if !h.active(d) || h.voiceOwner(d.companionID) != d || !h.bridgeEndpoint(d) {
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
	h.startEndpoint(d)
	h.pushRecStart(d, "", true, true)
	h.setCompanionStatus(d.companionID, statusListening)
	h.render(d, now, false)
	return true
}

func (h *terminalHub) startStoryChoiceListen(d *terminalSession, now time.Time) bool {
	if !h.active(d) || d.drawer != drawerStoryReader || d.story == nil || len(d.story.choices) == 0 ||
		h.voiceOwner(d.companionID) != d || !h.bridgeEndpoint(d) {
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
	d.recSession++
	h.b.diag.listen.windows.Add(1)
	h.startEndpoint(d)
	h.pushRecStart(d, "", true, true)
	h.setCompanionStatus(d.companionID, statusListening)
	h.render(d, now, false)
	return true
}

func (h *terminalHub) tryStoryVoiceChoice(d *terminalSession, turnID uint64, text string) bool {
	h.mu.Lock()
	if !h.active(d) || d.turn != turnID || d.drawer != drawerStoryReader || d.story == nil || len(d.story.choices) == 0 {
		h.mu.Unlock()
		return false
	}
	idx, ok := matchStoryChoice(text, d.story.choices)
	if !ok {
		h.mu.Unlock()
		return false
	}
	target := d.story.choices[idx].Target
	d.busy = false
	d.turn = 0
	h.setCompanionStatus(d.companionID, statusIdle)
	h.advanceStoryPassage(d, target, true)
	h.mu.Unlock()
	return true
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
	var timer *time.Timer
	timer = time.AfterFunc(recStopWait, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.active(d) && c.stopTimer == timer {
			c.suppressLateStart()
			h.endRecording(d, "", false, time.Now())
		}
	})
	c.stopTimer = timer
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
		h.mu.Lock()
		d := c.terminal
		if d != nil && h.active(d) && d.recording.endpoint == ep {
			h.recordingControl(d, time.Now(), true)
		}
		h.mu.Unlock()
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	d := c.terminal
	if d == nil || !h.active(d) {
		return
	}
	rec := &d.recording
	if !rec.open() {
		return
	}
	if ep := rec.endpoint; ep != nil {
		if len(pcm) > 2*pcmPacket || !ep.push(pcm, time.Now()) {
			h.recordingControl(d, time.Now(), true)
		}
		return
	}
	room := (maxPCM - len(rec.pcm)) &^ 1
	n := min(len(pcm), room)
	rec.pcm = append(rec.pcm, pcm[:n]...)
	if n == room {
		h.stopRecordingAtLimit(d, time.Now())
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
	pcm, target := d.recording.pcm, d.recording.target
	if requested, ok := recordTarget(reqTarget); ok && requested != "" {
		target = requested
	}
	d.recording.clear()
	if cancelled || len(pcm) == 0 {
		if cancelled {
			h.pushChime(d, "cancel")
		}
		h.setCompanionStatus(d.companionID, statusIdle)
		h.wake(d, now)
		if !h.maybeStartNoticeLocked(d, now) {
			h.render(d, now, false)
		}
		return
	}
	d.measureAmbient(pcm)
	h.startTurn(d, now, false, "voice", func(ctx context.Context, id uint64) {
		h.runVoice(ctx, d, id, target, pcm)
	})
}

func (h *terminalHub) interruptTurn(d *terminalSession, reason string) {
	if d.busy && h.b.oc != nil && d.companionID != "" {
		log.Printf("ws interrupt companion=%s reason=%s", d.companionID, reason)
		h.b.oc.interruptDevice(d.companionID)
	}
}

func (h *terminalHub) cancelTurn(d *terminalSession, reason string) {
	h.interruptTurn(d, reason)
	h.cancelLocalTurn(d)
}

func (h *terminalHub) cancelLocalTurn(d *terminalSession) {
	if c := d.queuedVoice; c != nil {
		d.queuedVoice = nil
		c.cancel()
	}
	d.turn++
	d.trace = nil
	d.busy = false
	d.resetStopTaps()
	d.agentSetScene = false
	if d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
	h.sig.notify()
}

func (h *terminalHub) startTurn(d *terminalSession, now time.Time, keepView bool, source string, run func(context.Context, uint64)) bool {
	if d.companionID == "" {
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
	ctx = h.b.traceTurn(ctx, d.companionID, d.companion.Name, source, now)
	d.trace = traceFrom(ctx)
	id := d.turn
	d.cancel, d.busy = cancel, true
	if !keepView {
		h.pushChime(d, "think")
	}
	h.setCompanionStatus(d.companionID, statusThinking)
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
	h.mu.Lock()
	defer func() {
		h.mu.Unlock()
		h.sig.notify()
	}()
	if !h.active(d) || d.turn != id {
		return
	}
	d.busy = false
	d.finished++
	now := time.Now()
	d.last = now
	if d.nameFreeUntil.IsZero() {
		d.nameFreeUntil = now.Add(followUpGrace)
	}
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
			h.setCompanionStatus(d.companionID, statusThinking)
		} else if st, ok := h.companionStatuses[d.companionID]; !ok || st.state != statusError {
			h.setCompanionStatus(d.companionID, statusDone)
		}
	}
	if h.maybeStartNoticeLocked(d, now) {
		d.trace = nil
		return
	}
	if !d.ttsStarted && !d.awaitAck && d.stopAudio == nil && d.replyAudio == nil {
		if st, ok := h.companionStatuses[d.companionID]; !ok || st.state != statusError {
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
	d.trace.fail()
	h.mu.Lock()
	if h.active(d) && d.turn == id {
		h.setCompanionStatus(d.companionID, statusError)
	}
	h.mu.Unlock()
	view := noticeScene("Try again", message)
	h.finishTurn(d, id, &view, nil)
}

func (h *terminalHub) stopCompanionTurn(d *terminalSession) {
	h.cancelLocalTurn(d)
	h.stopPlayback(d, true)
	if d.recording.active() {
		h.pushRecStop(d, d.recording.target, false)
		d.recording.clear()
	}
}

func (h *terminalHub) stopCompanionTurns(ids ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	for _, id := range ids {
		for _, d := range h.terminalsFor(id) {
			h.stopCompanionTurn(d)
			h.render(d, now, false)
		}
	}
}

func (h *terminalHub) runVoice(ctx context.Context, d *terminalSession, id uint64, target string, pcm []byte) {
	if target != "" {
		sender := h.b.companionFor(d.companionID)
		others, err := h.peerSnapshot(sender.ID)
		if err != nil {
			h.turnError(d, id, "Could not find nearby companions.")
			return
		}
		recipients := h.b.intercomTargets(others, target)
		voice := h.b.voiceFor(sender.ID)
		receipts := h.deliverOnline(ctx, sender, recipients, voice, "", pcm)
		// A card reply to a Delta Chat contact has no online peer; send the
		// voice note back over Delta Chat instead of reporting it unreachable.
		if len(receipts) == 0 && h.b.dc != nil && h.b.dc.ready.Load() {
			h.sendIntercom(ctx, d, id, others, target, "", pcm)
			return
		}
		// Connected peers can play the original clip before transcription.
		if len(receipts) != 0 {
			go func() {
				captionCtx, cancel := context.WithTimeout(context.WithValue(h.b.root, traceKey{}, traceFrom(ctx)), 30*time.Second)
				defer cancel()
				transcript, _ := h.b.transcribe(captionCtx, pcm)
				h.caption(receipts, sender, transcript)
				ids := make([]string, 0, len(receipts))
				for _, r := range receipts {
					ids = append(ids, r.terminal.companionID)
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
		h.finishTurn(d, id, nil, nil)
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
	if d.companionID == "" {
		h.turnError(d, id, "This terminal is not attached to a companion.")
		return
	}
	sender := h.b.companionFor(d.companionID)
	if traceFrom(ctx) == nil {
		ctx = h.b.traceTurn(ctx, sender.ID, sender.Name, source, time.Now())
	}
	if ctx.Err() != nil {
		h.finishTurn(d, id, nil, nil)
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
		h.sendIntercom(ctx, d, id, others, direct.target, direct.text, pcm)
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
		publish: func(stream *AudioStream, start time.Time) { h.publish(d, id, ctx, stream, start) }})
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
	h.finishTurn(d, id, view, gen.audio)
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
	h.mu.Lock()
	if d.reply == reply {
		d.clearReply()
	}
	h.mu.Unlock()
}

// dropStaleReply clears a read-out armed while this capture was already open:
// the notice interrupted that capture, so its turn cannot answer it. Without
// this a queued notice can leak its reply context into later turns.
func (h *terminalHub) dropStaleReply(d *terminalSession, capturedAt time.Time) {
	if capturedAt.IsZero() {
		return
	}
	h.mu.Lock()
	if d.reply != nil && capturedAt.Before(d.replySpoken) {
		d.clearReply()
	}
	h.mu.Unlock()
}

func (h *terminalHub) sendIntercom(ctx context.Context, d *terminalSession, id uint64, others []companionTarget, target, text string, pcm []byte) {
	sender := h.b.companionFor(d.companionID)
	if traceFrom(ctx) == nil {
		ctx = h.b.traceTurn(ctx, sender.ID, sender.Name, "intercom", time.Now())
	}
	if h.b.dc != nil && h.b.dc.ready.Load() {
		sentTo, err := h.b.dc.sendIntercom(ctx, sender.ID, target, text, pcm)
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
	res := h.b.deliver(ctx, sender, recipients, text, pcm)
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
	h.mu.Lock()
	defer h.mu.Unlock()
	return !d.nameFreeUntil.IsZero() && now.After(d.nameFreeUntil)
}

func (h *terminalHub) replyNotCaught(ctx context.Context, d *terminalSession, id uint64) {
	h.b.diag.listen.didntCatch.Add(1)
	h.freeFollowUps(d, time.Now())
	w := h.b.worker
	if w == nil || !w.ready.Load() {
		h.finishTurn(d, id, nil, nil)
		return
	}
	text := notCaughtLines[int(d.finished)%len(notCaughtLines)]
	start := time.Now()
	if traceFrom(ctx) == nil {
		ctx = h.b.traceTurn(ctx, d.companionID, d.companion.Name, "voice", start)
	}
	voice := h.b.voiceFor(d.companionID)
	stream := newAudioStream()
	h.publish(d, id, ctx, stream, start)
	pcm := h.b.clips.peek(voice, text)
	if pcm == nil {
		pcm, _ = w.synthesize(ctx, voice, text)
	}
	if !stream.append(pcm) {
		stream.finish(errTTS)
	} else {
		stream.finish(nil)
	}
	h.finishTurn(d, id, nil, nil)
}

func (h *terminalHub) sayTurn(ctx context.Context, d *terminalSession, id uint64, text string, view *sceneView) {
	start := time.Now()
	if traceFrom(ctx) == nil {
		ctx = h.b.traceTurn(ctx, d.companionID, d.companion.Name, "tap", start)
	}
	w := h.b.worker
	if w != nil && w.ready.Load() {
		stream := newAudioStream()
		h.publish(d, id, ctx, stream, start)
		voice := h.b.voiceFor(d.companionID)
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
	h.finishTurn(d, id, view, nil)
}

func (h *terminalHub) publish(d *terminalSession, id uint64, ctx context.Context, stream *AudioStream, start time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
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

func (h *terminalHub) play(d *terminalSession, stream *AudioStream) context.Context {
	return h.playTraced(d, stream, nil, time.Time{})
}

func (h *terminalHub) playTraced(d *terminalSession, stream *AudioStream, trace *turnTrace, ttsStart time.Time) context.Context {
	ctx, cancel := context.WithCancel(d.client.ctx)
	h.stopPlayback(d, true)
	d.stopAudio = cancel
	id := d.audio
	go func() {
		defer cancel()
		err := h.streamPlayback(ctx, d, id, stream, trace, ttsStart)
		h.mu.Lock()
		defer h.mu.Unlock()
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
		}
		if !d.busy && d.cancel != nil {
			d.cancel()
			d.cancel = nil
		}
		now := time.Now()
		if !h.maybeStartVoice(d, now) {
			h.maybeStartNoticeLocked(d, now)
		}
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
			stream.traceFirst(trace, ttsStart)
			h.mu.Lock()
			for h.active(d) && d.audio == id && ctx.Err() == nil && d.recording.endpoint != nil {
				listening := d.recording.endpoint.listening
				h.mu.Unlock()
				select {
				case <-listening:
				case <-ctx.Done():
					return ctx.Err()
				}
				h.mu.Lock()
			}
			if !h.active(d) || d.audio != id || ctx.Err() != nil {
				h.mu.Unlock()
				return context.Canceled
			}
			d.ttsStarted = true
			h.applyAmbientVolume(d)
			h.setCompanionStatus(d.companionID, statusSpeaking)
			h.mu.Unlock()
			if !control("start") {
				return context.Canceled
			}
			h.mu.Lock()
			if h.active(d) && d.audio == id {
				h.render(d, time.Now(), false)
			}
			h.mu.Unlock()
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
	h.mu.Lock()
	if !h.active(d) || d.audio != id || ctx.Err() != nil {
		h.mu.Unlock()
		return context.Canceled
	}
	if until.IsZero() {
		h.mu.Unlock()
		return errTTS
	}
	// A fast client can acknowledge as soon as the end frame is queued.
	d.audioUntil, d.awaitAck = until, true
	h.mu.Unlock()
	if !control("end") {
		return context.Canceled
	}
	return nil
}

func (d *terminalSession) measureAmbient(pcm []byte) {
	if rms, ok := ambientRMS(pcm); ok {
		d.updateAmbient(rms)
	}
}

func (d *terminalSession) updateAmbient(rms float64) {
	if d.hasAmbient {
		d.ambientEMA += 0.45 * (rms - d.ambientEMA)
	} else {
		d.ambientEMA, d.hasAmbient = rms, true
	}
}

func (h *terminalHub) applyAmbientVolume(d *terminalSession) {
	if !d.autoVol || !d.hasAmbient {
		return
	}
	level := ambientVolume(d.ambientEMA)
	if delta := level - d.vol; delta >= 3 || delta <= -3 {
		d.vol = level
		h.pushVolume(d, false, true)
	}
}

func (h *terminalHub) stopPlayback(d *terminalSession, notify bool) {
	if d.stopAudio != nil {
		d.stopAudio()
		d.stopAudio = nil
	}
	if notify && (d.ttsStarted || d.awaitAck) {
		h.pushTTSAbort(d, d.audio)
	}
	d.ttsStarted, d.awaitAck = false, false
	d.replyAudio = nil
	d.audio++
}

func (h *terminalHub) playbackDone(d *terminalSession, now time.Time, aborted bool) {
	if !aborted {
		h.freeFollowUps(d, now)
	}
	d.awaitAck = false
	if d.pending != nil {
		d.view, d.pending = d.pending, nil
	}
	d.last = now
	if !d.busy {
		if st, ok := h.companionStatuses[d.companionID]; !ok || st.state != statusError {
			h.setCompanionStatus(d.companionID, statusDone)
		}
	}
	if h.maybeStartVoice(d, now) || h.maybeStartNoticeLocked(d, now) {
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
func (h *terminalHub) freeFollowUps(d *terminalSession, now time.Time) {
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
	view   *sceneView
	chime  string
	voice  string
	intro  string
	text   string
	stream *AudioStream
	reply  *turnReply
	trace  *turnTrace
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
	var pcmStream *AudioStream
	if n.pcm != nil {
		pcmStream = newAudioStream()
		if n.intro != "" {
			if intro := h.b.clips.peek(n.voice, n.intro); intro != nil && len(intro)+len(n.pcm) <= maxPeerPCM {
				pcmStream.append(intro)
			}
		}
		pcmStream.append(NormalizeVoicePCM(n.pcm))
		pcmStream.finish(nil)
	}

	h.mu.Lock()
	if ctx.Err() != nil {
		h.mu.Unlock()
		return nil
	}
	now := time.Now()
	var targets []*terminalSession
	seen := make(map[*terminalSession]bool)
	for _, id := range n.recipientIDs {
		for _, d := range h.terminalsFor(id) {
			if h.active(d) && !seen[d] {
				seen[d] = true
				targets = append(targets, d)
			}
		}
	}
	if len(targets) == 0 {
		h.mu.Unlock()
		return nil
	}
	var immediateStream *AudioStream
	if pcmStream == nil && (speakText != "" || n.intro != "") {
		immediateStream = newAudioStream()
	}
	receipts := make([]noticeReceipt, 0, len(targets))
	startedImmediate := false
	for _, d := range targets {
		stream := pcmStream
		if stream == nil {
			stream = immediateStream
		}
		q := queuedNotice{
			view:   view,
			chime:  n.chime,
			voice:  n.voice,
			intro:  n.intro,
			text:   speakText,
			stream: stream,
			reply:  n.reply,
			trace:  n.trace,
		}
		if d.noticeBusy() {
			// Queued notices synthesize their own stream when drained so they don't race or expire.
			if pcmStream == nil {
				q.stream = nil
			}
			d.queuedNotices = append(d.queuedNotices, q)
		} else {
			h.deliverNoticeNowLocked(d, now, q)
			if stream == immediateStream && immediateStream != nil {
				startedImmediate = true
			}
		}
		receipts = append(receipts, noticeReceipt{d, view})
	}
	h.mu.Unlock()
	if startedImmediate {
		go h.speakNotice(immediateStream, n.voice, n.intro, speakText)
	}
	return receipts
}

func (h *terminalHub) deliverNoticeNowLocked(d *terminalSession, now time.Time, q queuedNotice) {
	d.drawer, d.pending = "", nil
	d.reply = q.reply
	if q.reply != nil {
		d.replySpoken = now
		d.replyUntil = now.Add(deviceReplyTTL)
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
		if q.trace != nil {
			h.playTraced(d, q.stream, q.trace, time.Time{})
		} else {
			h.play(d, q.stream)
		}
	}
}

func (h *terminalHub) maybeStartNoticeLocked(d *terminalSession, now time.Time) bool {
	if len(d.queuedNotices) == 0 || d.noticeBusy() || !h.active(d) {
		return false
	}
	q := d.queuedNotices[0]
	d.queuedNotices = d.queuedNotices[1:]
	if q.stream == nil && (q.text != "" || q.intro != "") {
		q.stream = newAudioStream()
		h.deliverNoticeNowLocked(d, now, q)
		go h.speakNotice(q.stream, q.voice, q.intro, q.text)
		return true
	}
	h.deliverNoticeNowLocked(d, now, q)
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
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range receipts {
		if d := r.terminal; h.active(d) {
			switch {
			case d.card == r.view:
				d.card = &view
			case d.view == r.view:
				d.view = &view
			default:
				continue
			}
			h.render(d, time.Now(), false)
		}
	}
}

func (b *larkServer) deliver(ctx context.Context, sender companionState, recipients []companionTarget, text string, raw []byte) delivery {
	sent := make(map[string]bool, len(recipients))
	if b.hub != nil {
		for _, r := range b.hub.deliverOnline(ctx, sender, recipients, b.voiceFor(sender.ID), text, raw) {
			sent[r.terminal.companionID] = true
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
			h.playPCM(d, prefix)
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
		trace = traceFrom(h.b.traceTurn(d.client.ctx, d.companionID, d.companion.Name, "tap", start))
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
	for i := range limit {
		candidate := strings.ToLower(strings.Trim(fields[i], ".,!?;:'\""))
		if len(candidate) < max(3, len(name)-nameMaxDistance) {
			continue
		}
		if editDistance(candidate, strings.ToLower(name)) > nameMaxDistance {
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
