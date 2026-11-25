package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"lark/internal/stories"

	"lark/internal/audio"
)

func (b *larkServer) serveRemoteEvents(w http.ResponseWriter, r *http.Request, companionID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		sendError(w, problem(500, "Streaming unavailable"))
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	_, _ = fmt.Fprint(w, "retry: 2000\n\n")
	flusher.Flush()

	send := func(snapshot object) bool {
		data, err := json.Marshal(snapshot)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	sendCapture := func(status object) bool {
		data, err := json.Marshal(status)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: capture\ndata: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	initial := b.remoteSnapshot(r.Context(), companionID, true)
	if !send(initial) {
		return
	}
	last := remoteSnapshotKey(initial)
	captureStatus := b.captures.status(companionID)
	if !sendCapture(captureStatus) {
		return
	}
	lastCapture := remoteSnapshotKey(captureStatus)
	ticker := time.NewTicker(remotePoll)
	defer ticker.Stop()
	keep := time.NewTicker(remoteKeepAlive)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			snapshot := b.remoteSnapshot(r.Context(), companionID, false)
			key := remoteSnapshotKey(snapshot)
			if key != last {
				last = key
				if !send(snapshot) {
					return
				}
			}
			status := b.captures.status(companionID)
			captureKey := remoteSnapshotKey(status)
			if captureKey == lastCapture {
				continue
			}
			lastCapture = captureKey
			if !sendCapture(status) {
				return
			}
		case <-keep.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func remoteSnapshotKey(snapshot object) string {
	comparable := object{}
	for key, value := range snapshot {
		comparable[key] = value
	}
	if comp, ok := comparable["companion"].(object); ok {
		withoutPortrait := object{}
		for key, value := range comp {
			if key != "portrait" {
				withoutPortrait[key] = value
			}
		}
		comparable["companion"] = withoutPortrait
	}
	data, _ := json.Marshal(comparable)
	return string(data)
}

func (b *larkServer) remoteSnapshot(ctx context.Context, companionID string, portrait bool) object {
	c := b.companionFor(companionID)
	comp := object{
		"id":         c.ID,
		"name":       c.Name,
		"form":       c.Form,
		"voice":      b.voiceFor(c.ID),
		"listening":  c.Listening,
		"live_state": b.companionLiveState(c.ID),
	}
	if portrait {
		if pngBytes, err := companionAvatarPNG(b, c); err == nil {
			comp["portrait"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes)
		}
	}

	terminals := []object{}
	seen := make(map[string]bool)
	if dbTerms, err := b.terminalsForCompanion(companionID); err == nil {
		for _, dt := range dbTerms {
			te := b.terminalEntry(dt, true)
			entry := object{
				"id":     dt.ID,
				"name":   dt.ID,
				"online": te["online"],
				"volume": te["volume"],
			}
			if pct, ok := te["battery_pct"]; ok && pct != nil {
				entry["battery_pct"] = pct
			}
			if chg, ok := te["charging"]; ok && chg != nil {
				entry["charging"] = chg
			}
			terminals = append(terminals, entry)
			seen[strings.ToLower(dt.ID)] = true
		}
	}
	if b.hub != nil {
		for _, t := range b.hub.companionTerminals(companionID) {
			tid := strings.ToLower(str(t["id"]))
			if !seen[tid] {
				terminals = append(terminals, t)
			}
		}
	}
	slices.SortFunc(terminals, func(a, b object) int { return strings.Compare(str(a["id"]), str(b["id"])) })

	progressByStory := make(map[string]stories.Progress)
	if plist, err := b.listStoryProgress(ctx, companionID); err == nil {
		for _, p := range plist {
			progressByStory[p.StoryID] = p
		}
	}
	stories := []object{}
	if list, err := b.listStorySummaries(ctx); err == nil {
		for _, st := range list {
			item := object{"name": st.ID, "title": st.Title, "passages": st.Passages, "progress": 0}
			if prog, ok := progressByStory[st.ID]; ok {
				pct := 10
				if cs, err := b.compiledStoryByID(ctx, st.ID); err == nil && len(cs.Passages) > 0 {
					if idx, exists := cs.ByName[prog.Passage]; exists {
						if cs.Passages[idx].End {
							pct = 100
						} else {
							pct = max(5, min(95, (idx+1)*100/len(cs.Passages)))
						}
					}
				}
				item["progress"] = pct
				item["passage"] = prog.Passage
			}
			stories = append(stories, item)
		}
	}

	voices := []object{}
	for _, v := range audio.Catalog {
		voices = append(voices, object{"id": v.ID, "name": v.Name})
	}
	return object{"companion": comp, "terminals": terminals, "stories": stories, "voices": voices}
}

func (b *larkServer) companionLiveState(companionID string) string {
	if b == nil || b.hub == nil {
		return "offline"
	}
	terms := b.hub.liveFor(companionID)
	if len(terms) == 0 {
		return "offline"
	}
	best := b.hub.ownerOf(companionID)
	if best == nil {
		best = terms[0]
	}
	st := best.state()
	switch {
	case st.asleep:
		return "sleeping"
	case st.recording.active():
		return "listening"
	case st.ttsStarted || st.awaitAck:
		return "speaking"
	case st.busy:
		return "thinking"
	case st.pose.mood == "listening" || st.pose.mood == "thinking" || st.pose.mood == "speaking" || st.pose.mood == "sleepy":
		if st.pose.mood == "sleepy" {
			return "sleeping"
		}
		return st.pose.mood
	default:
		return "idle"
	}
}
