package main

import (
	"context"
	"time"
)

func buildingScene(id string) sceneView {
	view := messageScene("Building", "Building", "I'll tell you when it's ready.", blankCompanion())
	view.building = id
	view.duringTurn = true
	view.actions = map[string]sceneAction{
		"listen":       {"listen", ""},
		"build-cancel": {"build-cancel", id},
	}
	return view
}

func (h *wsHub) announceBuild(ctx context.Context, job buildJob) {
	h.mu.Lock()
	d := h.voiceOwner(job.device)
	if d == nil || !h.active(d) ||
		d.announcing || d.busy || d.queuedVoice != nil || d.recording.active() ||
		d.stopAudio != nil || d.awaitAck || d.asleep || d.drawer != "" || d.card != nil && d.card.building != job.id {
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
	view.actions = map[string]sceneAction{"close": {"dismiss", ""}}
	now := time.Now()
	d.last = now
	h.speakLocal(d, job.result)
	for _, td := range h.terminalsFor(d.companionID) {
		td.card = &view
		h.render(td, now, false)
	}
	entry := newChat("card", d.companion.Name, job.result, "builder")
	entry.SceneTitle = title
	if entry, ok := h.b.recordChat(job.device, entry, true); ok {
		h.broadcastChat(job.device, entry)
	}
	h.mu.Unlock()
}

func (h *wsHub) cancelBuildCard(d *wsTerminal, id string) {
	if h.b.builders == nil || d.card == nil || d.card.building != id {
		return
	}
	card := d.card
	h.mu.Unlock()
	cancelled, _ := h.b.builders.cancel(d.client.ctx, d.companionID, id)
	h.mu.Lock()
	if cancelled && h.active(d) && d.card == card {
		view := messageScene("Build cancelled", "Build cancelled", "", d.companion)
		view.duringTurn = true
		view.actions = map[string]sceneAction{"close": {"dismiss", ""}}
		for _, td := range h.terminalsFor(d.companionID) {
			td.card = &view
			h.render(td, time.Now(), false)
		}
	}
}
