package main

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"
)

const (
	micPrefix     = 0x01
	pcmPrefix     = 0x02
	callMicPrefix = 0x04
	pcmPacket     = 4 << 10
	maxAudioLead  = 8 * time.Second
	recStopWait   = 450 * time.Millisecond
)

func recordTarget(s string) (string, bool) {
	s = strings.TrimSpace(s)
	return s, s == "" || targetRE.MatchString(s)
}

func (h *wsHub) bridgeEndpoint(d *wsDevice) bool {
	return h.b.endpoint != nil && d.caps.BridgeEndpoint
}

func (h *wsHub) startRecording(d *wsDevice, target string, now time.Time, request bool) {
	if h.parentRefresh(d.key, now) != "" {
		return
	}
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
	if w := h.b.worker; target != "" && w != nil && w.ready.Load() {
		// A voice note can use the cached introduction without waiting on
		// synthesis. Warm it while its sender is recording, not on every hello.
		intro, voice := "From "+d.cat.Name+"!", d.voice
		go func() {
			ctx, cancel := context.WithTimeout(d.client.ctx, ttsWait)
			defer cancel()
			_, _ = h.b.clips.getWith(ctx, w.warm, voice, intro)
		}()
	}
	if request {
		start := object{"op": "rec", "action": "start", "target": target}
		if bridgeEndpoint {
			h.startEndpoint(d)
			start["endpoint"] = "bridge"
		}
		h.push(d, start, false)
	}
	h.render(d, now, false)
}

func (h *wsHub) recordingControl(d *wsDevice, now time.Time, cancelled bool) {
	c := &d.recording
	if !c.active() {
		return
	}
	if cancelled {
		h.push(d, object{"op": "rec", "action": "stop", "target": c.target, "cancelled": true}, false)
		c.suppressLateStart()
		h.endRecording(d, wsEvent{Cancelled: true}, now)
		return
	}
	if !c.requestStop() {
		return
	}
	if c.endpoint != nil {
		c.endpoint.commitAt = now
	}
	h.push(d, object{"op": "rec", "action": "stop", "target": ""}, false)
	if d.client.role == "web" && c.endpoint == nil {
		return
	}
	// Keep accepting DMA packets until the board's rec_end. The timer is
	// tied to this stop request, so it cannot end a subsequent recording.
	var timer *time.Timer
	timer = time.AfterFunc(recStopWait, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.active(d) && c.stopTimer == timer {
			c.suppressLateStart()
			h.endRecording(d, wsEvent{}, time.Now())
		}
	})
	c.stopTimer = timer
}

func (h *wsHub) micChunk(c *wsClient, data []byte) {
	if len(data) < 3 || len(data)%2 != 1 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	d := c.device
	if d == nil || !h.active(d) || h.parentRefresh(d.key, time.Now()) != "" {
		return
	}
	if d.callState != callNone {
		if d.callState == callActive && d.caps.CallPCM && data[0] == callMicPrefix {
			h.relayCallPCM(d, data)
		}
		return
	}
	rec := &d.recording
	if data[0] != micPrefix || !rec.open() {
		return
	}
	if c := rec.endpoint; c != nil {
		// Bound bytes as well as entries. The limit leaves headroom over DMA and
		// worklet frames while preventing oversized frames from filling the queue.
		if len(data)-1 > 2*pcmPacket {
			h.recordingControl(d, time.Now(), true)
			return
		}
		select {
		case c.packets <- micPacket{data[1:], time.Now()}:
		default:
			h.recordingControl(d, time.Now(), true)
		}
		return
	}
	room := (maxPCM - len(rec.pcm)) &^ 1
	n := min(len(data)-1, room)
	rec.pcm = append(rec.pcm, data[1:1+n]...)
	if n == room {
		h.stopRecordingAtLimit(d, time.Now())
	}
}

func (h *wsHub) stopRecordingAtLimit(d *wsDevice, now time.Time) {
	h.push(d, object{"op": "rec", "action": "stop", "target": ""}, false)
	h.endRecording(d, wsEvent{}, now)
}

func (h *wsHub) endRecording(d *wsDevice, ev wsEvent, now time.Time) {
	if h.parentRefresh(d.key, now) != "" {
		return
	}
	if d.recording.endpoint != nil && !ev.Cancelled {
		h.finishEndpoint(d, now)
		return
	}
	pcm, target := d.recording.pcm, d.recording.target
	if requested, ok := recordTarget(ev.Target); ok && requested != "" {
		target = requested
	}
	d.recording.clear()
	if ev.Cancelled || len(pcm) == 0 {
		if ev.Cancelled {
			h.push(d, object{"op": "chime", "kind": "cancel"}, false)
		}
		h.wake(d, now)
		h.render(d, now, false)
		return
	}
	d.measureAmbient(pcm)
	h.startTurn(d, now, false, "voice", func(ctx context.Context, id uint64) {
		h.runVoice(ctx, d, id, target, pcm)
	})
}

func (h *wsHub) interruptTurn(d *wsDevice, reason string) {
	if d.busy && h.b.oc != nil {
		log.Printf("ws interrupt device=%s reason=%s", d.who.DeviceID, reason)
		h.b.oc.interruptDevice(d.who.DeviceID)
	}
}

func (h *wsHub) cancelTurn(d *wsDevice, reason string) {
	h.interruptTurn(d, reason)
	h.cancelLocalTurn(d)
}

func (h *wsHub) cancelLocalTurn(d *wsDevice) {
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
}

func (h *wsHub) startTurn(d *wsDevice, now time.Time, keepView bool, source string, run func(context.Context, uint64)) bool {
	if h.parentRefresh(d.key, now) != "" {
		return false
	}
	h.cancelTurn(d, "superseded")
	h.beginTurn(d, now, keepView, source, run)
	return true
}

func (h *wsHub) beginTurn(d *wsDevice, now time.Time, keepView bool, source string, run func(context.Context, uint64)) {
	h.wake(d, now)
	h.stopPlayback(d, true)
	if d.recording.active() {
		h.push(d, object{"op": "rec", "action": "stop", "target": d.recording.target}, false)
		d.recording.clear()
	}
	d.drawer, d.card, d.pending = "", nil, nil
	if !keepView {
		d.view = nil
		h.dropPhotoShow(d)
	}
	ctx, cancel := context.WithTimeout(d.client.ctx, turnTimeout)
	ctx = h.b.traceTurn(ctx, d.who.DeviceID, d.cat.Name, source, now)
	d.trace = traceFrom(ctx)
	id := d.turn
	d.cancel, d.busy = cancel, true
	if !keepView {
		h.push(d, object{"op": "chime", "kind": "think"}, false)
	}
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

func (h *wsHub) finishTurn(d *wsDevice, id uint64, view *sceneView, audio *AudioStream) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.active(d) || d.turn != id {
		return
	}
	d.busy = false
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
	if view != nil && d.app == nil && !d.agentSetScene {
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
	h.render(d, now, false)
	d.trace = nil
}

func (h *wsHub) turnError(d *wsDevice, id uint64, message string) {
	view := noticeScene("Try again", message)
	h.finishTurn(d, id, &view, nil)
}

func (h *wsHub) stopCatTurn(d *wsDevice) {
	h.cancelLocalTurn(d)
	h.stopPlayback(d, true)
	if d.recording.active() {
		h.push(d, object{"op": "rec", "action": "stop", "target": d.recording.target}, false)
		d.recording.clear()
	}
}

func (h *wsHub) stopCatTurns(ids ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	for _, id := range ids {
		if d := h.devices[strings.ToLower(id)]; d != nil && h.active(d) {
			h.stopCatTurn(d)
			h.render(d, now, false)
		}
	}
}

func (h *wsHub) runVoice(ctx context.Context, d *wsDevice, id uint64, target string, pcm []byte) {
	if target != "" {
		sender := d.who
		sender.Character = h.b.catFor(sender).Name
		others, err := h.peerSnapshot(d.who.DeviceID)
		if err != nil {
			h.turnError(d, id, "Could not find nearby cats.")
			return
		}
		recipients := h.b.intercomTargets(others, target)
		voice := h.b.voiceFor(sender)
		receipts := h.deliverOnline(ctx, sender, recipients, voice, "", pcm)
		// Connected peers can play the original clip before transcription.
		if len(receipts) != 0 {
			go func() {
				captionCtx, cancel := context.WithTimeout(context.WithValue(h.b.root, traceKey{}, traceFrom(ctx)), 30*time.Second)
				defer cancel()
				transcript, _ := h.b.transcribe(captionCtx, pcm)
				h.caption(receipts, sender, transcript)
				ids := make([]string, 0, len(receipts))
				for _, r := range receipts {
					ids = append(ids, r.device.who.DeviceID)
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
	h.runTurn(ctx, d, id, text, "voice", pcm)
}

func (h *wsHub) runTurn(ctx context.Context, d *wsDevice, id uint64, text, source string, pcm []byte) {
	if traceFrom(ctx) == nil {
		ctx = h.b.traceTurn(ctx, d.who.DeviceID, h.b.catFor(d.who).Name, source, time.Now())
	}
	if ctx.Err() != nil {
		h.finishTurn(d, id, nil, nil)
		return
	}
	if minutes := childPauseMinutes(text); minutes > 0 && source != "timer" {
		h.mu.Lock()
		var err error
		if h.active(d) && d.turn == id && ctx.Err() == nil {
			err = h.childPause(d.key, minutes, time.Now())
		}
		h.mu.Unlock()
		if err != nil {
			h.turnError(d, id, "I couldn't start a break. Please ask a grown-up.")
		}
		return
	}
	if target := parseCallCommand(text); target != "" {
		others, _ := h.peerSnapshot(d.who.DeviceID)
		if matches := targets(others, target); len(matches) == 1 {
			h.mu.Lock()
			if h.active(d) && d.turn == id && ctx.Err() == nil {
				h.startCall(d, matches[0].DeviceID)
			}
			h.mu.Unlock()
			return
		}
	}
	others, direct, err := h.b.intercomIntent(d.who.DeviceID, text)
	if err != nil || others != nil && ctx.Err() != nil {
		h.turnError(d, id, "Could not find nearby cats.")
		return
	}
	if direct != nil {
		h.b.chat(d.who.DeviceID, newChat("user", "You", text, source), true)
		h.sendIntercom(ctx, d, id, others, direct.target, direct.text, pcm)
		return
	}
	if reply, job, err := h.b.buildReply(ctx, d.who, text); err != nil {
		h.finishTurn(d, id, nil, nil)
		return
	} else if reply != "" {
		h.buildAcknowledgement(ctx, d, id, text, source, reply, job)
		return
	}
	if h.b.oc == nil {
		h.turnError(d, id, "Chat is not ready yet.")
		return
	}
	gen, err := h.b.generateReply(ctx, ctx, d.who, text, turnOptions{source: source, web: d.virtual(), scoped: d.client.scope != nil,
		publish: func(stream *AudioStream, start time.Time) { h.publish(d, id, ctx, stream, start) }})
	if err != nil {
		h.turnError(d, id, "Could not finish that turn.")
		return
	}
	var view *sceneView
	if gen.reply != "" && ctx.Err() == nil {
		v := noticeScene("Answer", shortText(gen.reply, maxMessage, true))
		view = &v
	}
	h.finishTurn(d, id, view, gen.audio)
}

func (h *wsHub) sendIntercom(ctx context.Context, d *wsDevice, id uint64, others []peer, target, text string, pcm []byte) {
	if traceFrom(ctx) == nil {
		ctx = h.b.traceTurn(ctx, d.who.DeviceID, h.b.catFor(d.who).Name, "intercom", time.Now())
	}
	recipients := h.b.intercomTargets(others, target)
	res := h.b.deliver(ctx, d.who, recipients, text, pcm)
	h.confirm(ctx, d, id, target, recipients, len(res.sent) != 0, len(res.failed) != 0)
}

func (h *wsHub) confirm(ctx context.Context, d *wsDevice, id uint64, target string, recipients []peer, sent, partial bool) {
	title, reply := intercomConfirmation(target, recipients, sent, partial)
	view := noticeScene(title, reply)
	h.sayTurn(ctx, d, id, reply, &view)
}

func (h *wsHub) sayTurn(ctx context.Context, d *wsDevice, id uint64, text string, view *sceneView) {
	start := time.Now()
	if traceFrom(ctx) == nil {
		ctx = h.b.traceTurn(ctx, d.who.DeviceID, h.b.catFor(d.who).Name, "tap", start)
	}
	w := h.b.worker
	if w != nil && w.ready.Load() {
		stream := newAudioStream()
		h.publish(d, id, ctx, stream, start)
		voice := h.b.voiceFor(d.who)
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

func (h *wsHub) publish(d *wsDevice, id uint64, ctx context.Context, stream *AudioStream, start time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.active(d) || d.turn != id || ctx.Err() != nil || h.parentRefresh(d.key, time.Now()) != "" {
		return
	}
	if stream == nil {
		h.stopPlayback(d, true)
		h.playbackDone(d, time.Now())
		return
	}
	// The turn deadline bounds synthesis; it must not cut off paced playback
	// of PCM that is already available. An interrupt still calls stopPlayback.
	h.playTraced(d, stream, traceFrom(ctx), start)
	d.replyAudio = &turnAudio{stream: stream, turn: id}
}

func (h *wsHub) play(d *wsDevice, stream *AudioStream) context.Context {
	return h.playTraced(d, stream, nil, time.Time{})
}

func (h *wsHub) playTraced(d *wsDevice, stream *AudioStream, trace *turnTrace, ttsStart time.Time) context.Context {
	ctx, cancel := context.WithCancel(d.client.ctx)
	if d.callState != callNone {
		cancel()
		return ctx
	}
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
				} else if a.answer != nil && d.app == nil && !d.agentSetScene {
					d.pending = a.answer
				}
			}
			h.playbackDone(d, time.Now())
		} else {
			d.replyAudio = nil
			d.ttsStarted = false
		}
		if !d.busy && d.cancel != nil {
			d.cancel()
			d.cancel = nil
		}
		h.maybeStartVoice(d, time.Now())
	}()
	return ctx
}

func (h *wsHub) streamPlayback(ctx context.Context, d *wsDevice, id uint64, stream *AudioStream, trace *turnTrace, ttsStart time.Time) error {
	control := func(state string) bool {
		return d.client.enqueueWait(ctx, textFrame(object{"op": "tts", "state": state, "audio": id}))
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
			packet := make([]byte, n+1)
			packet[0] = pcmPrefix
			copy(packet[1:], pcm[:n])
			if !d.client.enqueueWait(ctx, wsFrame{wsBinary, packet}) {
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

func (d *wsDevice) measureAmbient(pcm []byte) {
	if rms, ok := ambientRMS(pcm); ok {
		d.updateAmbient(rms)
	}
}

func (d *wsDevice) updateAmbient(rms float64) {
	if d.hasAmbient {
		d.ambientEMA += 0.45 * (rms - d.ambientEMA)
	} else {
		d.ambientEMA, d.hasAmbient = rms, true
	}
}

func (h *wsHub) applyAmbientVolume(d *wsDevice) {
	if !d.autoVol || !d.hasAmbient {
		return
	}
	level := ambientVolume(d.ambientEMA)
	if delta := level - d.vol; delta >= 3 || delta <= -3 {
		d.vol = level
		h.push(d, d.volumeFrame(false), true)
	}
}

func (h *wsHub) stopPlayback(d *wsDevice, notify bool) {
	if d.stopAudio != nil {
		d.stopAudio()
		d.stopAudio = nil
	}
	if notify && (d.ttsStarted || d.awaitAck) {
		h.push(d, object{"op": "tts", "state": "abort", "audio": d.audio}, false)
	}
	d.ttsStarted, d.awaitAck = false, false
	d.replyAudio = nil
	d.audio++
}

func (h *wsHub) playbackDone(d *wsDevice, now time.Time) {
	d.awaitAck = false
	if d.pending != nil {
		d.view, d.pending = d.pending, nil
	}
	d.last = now
	if !h.maybeStartVoice(d, now) {
		h.render(d, now, false)
	}
}

type wsReceipt struct {
	device *wsDevice
	view   *sceneView
}

func (b *bridge) intercomTargets(others []peer, target string) []peer {
	if !broadcast(target) {
		return targets(others, target)
	}
	if b.ws == nil {
		return nil
	}
	h := b.ws
	h.mu.Lock()
	defer h.mu.Unlock()
	var connected []peer
	for _, p := range others {
		if d := h.devices[strings.ToLower(p.DeviceID)]; d != nil && h.active(d) && d.callState == callNone {
			connected = append(connected, p)
		}
	}
	return connected
}

func (h *wsHub) deliverOnline(ctx context.Context, sender peer, recipients []peer, voice, text string, pcm []byte) []wsReceipt {
	h.mu.Lock()
	var connected []*wsDevice
	for _, p := range recipients {
		if d := h.devices[strings.ToLower(p.DeviceID)]; d != nil && h.active(d) {
			connected = append(connected, d)
		}
	}
	h.mu.Unlock()
	if len(connected) == 0 || len(pcm) > maxPeerPCM {
		return nil
	}
	text = shortText(text, maxMessage, true)
	stream := newAudioStream()
	if pcm != nil {
		intro := h.b.clips.peek(voice, "From "+sender.Character+"!")
		if len(intro)+len(pcm) <= maxPeerPCM && intro != nil {
			stream.append(intro)
		}
		stream.append(NormalizeVoicePCM(pcm))
		stream.finish(nil)
	}
	receipts := make([]wsReceipt, 0, len(connected))
	cat := h.b.catFor(sender)
	h.mu.Lock()
	now := time.Now()
	if ctx.Err() != nil || h.parentRefresh(strings.ToLower(sender.DeviceID), now) != "" {
		h.mu.Unlock()
		return nil
	}
	for _, d := range connected {
		if !h.active(d) || h.parentRefresh(d.key, now) != "" || d.callState != callNone {
			continue
		}
		view := (intercomCard{sender, text, cat}).scene(false)
		h.cancelTurn(d, "intercom")
		h.stopPlayback(d, true)
		if d.recording.active() {
			h.push(d, object{"op": "rec", "action": "stop", "target": d.recording.target}, false)
			d.recording.clear()
		}
		d.drawer, d.pending = "", nil
		if d.app != nil {
			d.card = &view
		} else {
			d.view, d.card = &view, nil
		}
		h.wake(d, now)
		h.render(d, now, false)
		h.push(d, object{"op": "chime", "kind": "intercom"}, false)
		if d.client.ctx.Err() == nil {
			receipts = append(receipts, wsReceipt{d, &view})
			if pcm != nil {
				h.playTraced(d, stream, traceFrom(ctx), time.Time{})
			} else {
				h.play(d, stream)
			}
		}
	}
	h.mu.Unlock()
	if pcm == nil && len(receipts) > 0 {
		go h.speakIntercom(stream, sender, voice, text)
	}
	return receipts
}

func (h *wsHub) speakIntercom(stream *AudioStream, sender peer, voice, text string) {
	w := h.b.worker
	if w == nil || !w.ready.Load() {
		stream.finish(nil) // The card is still delivered when TTS is unavailable.
		return
	}
	ctx, cancel := context.WithTimeout(h.b.root, ttsWait)
	defer cancel()
	intro, _ := h.b.clips.get(ctx, w, voice, "Message from "+sender.Character+"!")
	if len(intro) > 0 {
		stream.append(intro)
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

func (h *wsHub) caption(receipts []wsReceipt, sender peer, text string) {
	if len(receipts) == 0 || text == "" {
		return
	}
	view := (intercomCard{sender, shortText(text, maxMessage, true), h.b.catFor(sender)}).scene(false)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range receipts {
		if d := r.device; h.active(d) {
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

func (b *bridge) deliver(ctx context.Context, sender peer, recipients []peer, text string, raw []byte) delivery {
	sender.Character = b.catFor(sender).Name
	sent := make(map[string]bool, len(recipients))
	if b.ws != nil {
		for _, r := range b.ws.deliverOnline(ctx, sender, recipients, b.voiceFor(sender), text, raw) {
			sent[r.device.who.DeviceID] = true
		}
	}
	out := delivery{sent: []string{}, delivered: []string{}, failed: []string{}}
	for _, p := range recipients {
		if sent[p.DeviceID] {
			out.sent = append(out.sent, p.DeviceID)
			out.delivered = append(out.delivered, p.Character)
		} else {
			out.failed = append(out.failed, p.DeviceID)
		}
	}
	b.recordIntercom(sender, out.sent, text)
	return out
}

func (b *bridge) recordIntercom(sender peer, ids []string, text string) {
	if len(ids) == 0 {
		return
	}
	text = shortText(text, maxMessage, true)
	if text == "" {
		text = "Voice message"
	}
	b.chat(sender.DeviceID, newChat("intercom", sender.Character, text, "intercom"), true)
	for _, id := range ids {
		b.chat(id, newChat("intercom", sender.Character, text, "intercom"), true)
	}
}

func (h *wsHub) speakLocal(d *wsDevice, text string) {
	h.speakLocalAfter(d, text, nil)
}

func (h *wsHub) speakLocalAfter(d *wsDevice, text string, prefix []byte) {
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
	stream := newAudioStream()
	if len(prefix) != 0 {
		stream.append(prefix)
	}
	playback := h.play(d, stream)
	go func() {
		ctx, cancel := context.WithTimeout(playback, ttsWait)
		defer cancel()
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
