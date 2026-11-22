package main

import (
	"context"
	"errors"
	"time"
)

func (b *larkServer) buildReply(ctx context.Context, companionID, text string) (reply, job string, err error) {
	if b.builders == nil {
		return "", "", nil
	}
	intent := parseBuildIntent(text)
	if intent.kind == "" {
		return "", "", nil
	}
	if err = ctx.Err(); err != nil {
		return "", "", err
	}
	switch intent.kind {
	case "clarify":
		reply = "Are we making a new app, editing a saved app, or changing my face?"
	case "clarify-app":
		reply = "What would you like the new app to do?"
	case "clarify-face":
		reply = "What would you like me to change about my face?"
	case "cancel":
		cancelled, err := b.builders.cancel(ctx, companionID, "")
		switch {
		case err != nil:
			reply = "I couldn't cancel the build."
		case cancelled:
			reply = "I cancelled the build."
		default:
			reply = "There isn't a build to cancel."
		}
	default:
		id, err := b.builders.enqueue(ctx, companionID, intent, text)
		var clientError *apiError
		switch {
		case ctx.Err() != nil:
			return "", "", ctx.Err()
		case errors.As(err, &clientError) && clientError.status == 423:
			return "", "", err
		case errors.As(err, &clientError) && clientError.status < 500:
			reply = clientError.Error()
		case err != nil:
			reply = "I couldn't start the build. Try again."
		case id == "":
			reply = "The builder is busy. Try again in a moment, or say cancel the build."
		default:
			reply, job = "Building it now!", id
		}
	}
	return reply, job, nil
}

// Serialize persistence and the web chat frames with the visual and audio
// admission checks. Callers hold the hub lock when a hub exists.
func (b *larkServer) buildChat(id string, entries ...chatEntry) {
	for _, entry := range entries {
		if entry, ok := b.recordChat(id, entry, true); ok && b.ws != nil {
			b.ws.broadcastChat(id, entry)
		}
	}
}

func (b *larkServer) deliverOfflineBuildReply(ctx context.Context, who companionState, text, reply, source string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.buildChat(who.ID, newChat("user", "You", text, source), newChat("companion", who.Name, reply, source))
	return nil
}

func buildingScene(id string) sceneView {
	view := messageScene("Building", "Building", "I'll tell you when it's ready.", blankCompanion())
	view.build = id
	view.duringTurn = true
	view.control(bounds{63, 218, 98, 48}, actionTile("listen", "Talk", "", answerPalette))
	view.control(bounds{169, 218, 128, 48}, tile{id: "build-cancel", label: "Cancel build", action: sceneAction{"build-cancel", id}, colors: dangerPalette})
	return view
}

func (h *wsHub) buildAcknowledgement(ctx context.Context, d *wsTerminal, turn uint64, text, source, reply, job string) {
	view := noticeScene("Build", reply)
	if job != "" {
		view = buildingScene(job)
	}
	h.mu.Lock()
	allowed := h.active(d) && turn == d.turn && d.companionID != "" && ctx.Err() == nil
	if allowed {
		h.b.buildChat(d.companionID, newChat("user", "You", text, source), newChat("companion", d.companion.Name, reply, source))
		for _, td := range h.terminalsFor(d.companionID) {
			td.card = &view
			h.render(td, time.Now(), false)
		}
	}
	h.mu.Unlock()
	if allowed {
		h.sayTurn(ctx, d, turn, reply, nil)
	} else {
		h.finishTurn(d, turn, nil, nil)
	}
}

func (h *wsHub) announceBuild(ctx context.Context, job buildJob) {
	h.mu.Lock()
	d := h.voiceOwner(job.device)
	if d == nil || !h.active(d) ||
		d.announcing || d.busy || d.queuedVoice != nil || d.recording.active() || d.callState != callNone ||
		d.stopAudio != nil || d.awaitAck || d.asleep || d.drawer != "" || d.card != nil && d.card.build != job.id {
		h.mu.Unlock()
		return
	}
	// Claim before playback: reconnects and process restarts never repeat a
	// spoken notification, even if the listener leaves mid-announcement.
	var claimed string
	err := h.b.db.QueryRowContext(ctx, `UPDATE build_jobs SET announced=1
		WHERE id=? AND state=? AND announced=0 RETURNING id`, job.id, job.state).Scan(&claimed)
	if err != nil {
		h.mu.Unlock()
		return
	}
	title := "Build stopped"
	if job.state == "ready" {
		title = "Ready"
	}
	view := messageScene(title, title, job.result, d.companion)
	view.duringTurn = true
	view.control(bounds{129, 218, 102, 48}, tile{id: "build-done", label: "Done", action: sceneAction{"dismiss", ""}, colors: answerPalette})
	now := time.Now()
	d.last = now
	h.speakLocal(d, job.result)
	for _, td := range h.terminalsFor(d.companionID) {
		td.card = &view
		h.render(td, now, false)
	}
	entry := newChat("card", d.companion.Name, job.result, "builder")
	entry.SceneTitle = title
	h.b.buildChat(job.device, entry)
	h.mu.Unlock()
}

func (h *wsHub) cancelBuildCard(d *wsTerminal, id string) {
	if h.b.builders == nil || d.card == nil || d.card.build != id {
		return
	}
	card := d.card
	h.mu.Unlock()
	cancelled, _ := h.b.builders.cancel(d.client.ctx, d.companionID, id)
	h.mu.Lock()
	if cancelled && h.active(d) && d.card == card {
		view := messageScene("Build cancelled", "Build cancelled", "", d.companion)
		view.duringTurn = true
		view.control(bounds{129, 218, 102, 48}, tile{id: "build-done", label: "Done", action: sceneAction{"dismiss", ""}, colors: answerPalette})
		for _, td := range h.terminalsFor(d.companionID) {
			td.card = &view
			h.render(td, time.Now(), false)
		}
	}
}
