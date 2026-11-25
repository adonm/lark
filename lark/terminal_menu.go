package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	imagecolor "image/color"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"lark/internal/stories"

	"lark/internal/pixel"
)

// handleProto hands one inbound frame to the session actor. The payload aliases
// the connection's read buffer, which the loop reuses, so it is copied here.
func (h *terminalHub) handleProto(c *terminalClient, msgType uint8, payload []byte) {
	d := c.terminal.Load()
	if d == nil || c.ctx.Err() != nil {
		return
	}
	d.post(terminalEvent{kind: evProto, now: time.Now(),
		proto: terminalProto{msgType: msgType, payload: append([]byte(nil), payload...)}})
}

// handleProtoEvent runs in the session actor with the hub lock held.
func (h *terminalHub) handleProtoEvent(d *terminalSession, msgType uint8, payload []byte, now time.Time) {
	if !h.active(d) {
		return
	}

	switch msgType {
	case msgTelemetry:
		if st, ok := parseTelemetry(payload); ok {
			d.status(st)
		}
	case msgPointer:
		evName, x, y, rev, ok := parsePointer(payload)
		if !ok {
			return
		}
		h.clearDoneOrErrorStatus(d)
		if evName == "tap" {
			h.pointer(d, evName, x, y, rev, now)
		} else if d.caps.Drag && !d.asleep && (d.drawer == "" && d.view == nil && d.card == nil) {
			h.pointer(d, evName, x, y, rev, now)
		}
	case msgRecStart:
		session, target, ok := parseRecStart(payload)
		if !ok {
			return
		}
		h.clearDoneOrErrorStatus(d)
		if tgt, ok := recordTarget(target); ok {
			// The event is addressed to this session; active() above rejected a
			// connection that a reconnect already replaced.
			h.takeVoiceOwnership(d)
			if d.recording.acceptsStart() {
				if d.recording.active() {
					// An acknowledgement only belongs to the session this
					// capture requested. A stale ack must not open the next one.
					if session == d.recording.session {
						d.recording.acknowledge()
					}
				} else {
					h.startRecording(d, tgt, now, false)
					d.recording.session = session
				}
			}
		}
	case msgRecEnd:
		session, cancelled, target, ok := parseRecEnd(payload)
		if !ok {
			return
		}
		// A late rec_end from the previous recording can arrive right after a
		// new rec_start. Match the session; a cancelled end is also valid while
		// the start is still unacknowledged (the board failed to open the mic).
		if d.recording.active() && session == d.recording.session &&
			(d.recording.phase != captureRequested || cancelled) {
			d.recording.receivedEnd()
			h.endRecording(d, target, cancelled, now)
		}
	case msgTTSAck:
		aborted, audioID, ok := parseTTSAck(payload)
		if !ok || audioID == 0 || audioID != d.audio {
			return
		}
		if aborted && (d.ttsStarted || d.awaitAck) {
			h.stopPlayback(d, false)
			h.playbackDone(d, now, true)
		} else if !aborted && d.awaitAck {
			d.ttsStarted = false
			h.playbackDone(d, now, false)
		}
	}
}

func (h *terminalHub) pointer(d *terminalSession, evName string, x, y int, rev uint16, now time.Time) {
	width, height := d.screen.width, d.screen.height
	if x < 0 || x >= width || y < 0 || y >= height {
		return
	}
	wasAsleep := d.asleep
	h.wake(d, now)
	if wasAsleep {
		h.render(d, now, false)
		return
	}
	if evName == "tap" && d.recording.active() && d.recording.listen {
		h.stopListeningWindow(d, now)
		return
	}
	inside := d.screen.shape != "round" || inScreenCircle(x, y)
	key := ""
	if inside {
		key = hitID(d.svg.hits, x, y)
	}
	a := d.scene.Actions[key]
	if !d.acceptPointerRevision(a.Kind, &rev) {
		return
	}
	if evName == "drag" || evName == "up" {
		return
	}
	h.pointerAction(d, key, a, now)
}

// pointerAction dispatches by scene family so each family reads on its own.
func (h *terminalHub) pointerAction(d *terminalSession, key string, a pixel.Action, now time.Time) {
	switch a.Kind {
	case "menu", "menu-open", "menu-back", "menu-page":
		h.pointerMenu(d, a, now)
	case "story-open", "story-choice":
		h.pointerStory(d, a)
	case "menu-detail":
		h.pointerDetail(d, a, now)
	case "volume-step", "vol-minus", "vol-plus":
		h.pointerVolume(d, a)
	case "voice-set", "voice-select":
		h.selectAndPreviewVoice(d, a.Value)
	case "intercom-to", "listen", "reply", "rec-cancel", "rec-send", "rec-continue", "stop":
		h.pointerTurnControl(d, key, a, now)
	case "close-card", "close-view", "stop-turn", "companion", "dismiss":
		h.pointerScene(d, a, now)
	case "unpair-contact":
		h.pointerUnpair(d, a)
	}
}

func (h *terminalHub) pointerMenu(d *terminalSession, a pixel.Action, now time.Time) {
	if a.Kind == "menu" {
		h.chime(d, "tap")
		d.drawer, d.card, d.pending = pixel.DrawerMenu, nil, nil
		d.menuPage = 0
		d.view = nil
		h.render(d, now, false)
		return
	}
	if d.drawer != pixel.DrawerMenu && d.drawer != pixel.DrawerMore && d.drawer != pixel.DrawerVolume && d.drawer != pixel.DrawerVoice &&
		d.drawer != pixel.DrawerIntercom && d.drawer != pixel.DrawerStats && d.drawer != pixel.DrawerStoryReader &&
		d.sceneMenu != d.menuData {
		return
	}
	h.chime(d, "tap")
	wasPair := d.drawer == pixel.DrawerPair
	switch a.Kind {
	case "menu-open":
		if d.drawer == pixel.DrawerMenu {
			d.homePage = d.menuPage
		}
		d.drawer, d.menuPage = a.Value, 0
		if d.drawer == pixel.DrawerPair {
			h.pushPower(d, 100, true)
		}
	case "menu-back":
		if d.drawer == pixel.DrawerStoryReader {
			h.stopPlayback(d, true)
			d.drawer, d.menuPage = pixel.DrawerStories, 0
			d.story = nil
			h.loadMenu(d)
			return
		} else if d.menuData != nil && d.menuData.memoryDetail != nil {
			d.menuData.memoryDetail = nil
		} else if d.menuData != nil && d.menuData.detail != nil {
			d.menuData.detail = nil
		} else if d.drawer == pixel.DrawerMenu {
			d.drawer = ""
		} else if d.drawer == pixel.DrawerVolume || d.drawer == pixel.DrawerPair || d.drawer == pixel.DrawerUnpair || d.drawer == pixel.DrawerStats {
			d.drawer, d.menuPage = pixel.DrawerMore, 0
		} else {
			d.drawer, d.menuPage = pixel.DrawerMenu, d.homePage
		}
	case "menu-page":
		d.menuPage, _ = strconv.Atoi(a.Value)
		if d.drawer == pixel.DrawerStoryReader && d.story != nil {
			h.stopPlayback(d, true)
			d.storyPage = d.menuPage
			if h.b != nil && h.b.db != nil && d.companionID.get() != "" {
				prog := stories.Progress{
					CompanionID: d.companionID.get(),
					StoryID:     d.story.id,
					Passage:     d.story.currentPassageName(),
					Page:        d.storyPage,
					Flags:       d.story.compiled.FlagsMap(d.story.flags),
				}
				go func() {
					_ = h.b.setStoryProgress(context.Background(), prog)
				}()
			}
		}
	}
	if wasPair && d.drawer != pixel.DrawerPair {
		h.pushPower(d, d.brightness(), true)
	}
	if a.Kind != "menu-back" {
		switch d.drawer {
		case pixel.DrawerStories, pixel.DrawerMemories, pixel.DrawerPair, pixel.DrawerUnpair:
			h.loadMenu(d)
			return
		}
	}
	h.render(d, now, false)
}

func (h *terminalHub) pointerStory(d *terminalSession, a pixel.Action) {
	switch a.Kind {
	case "story-open":
		if d.menuData != nil && d.sceneMenu == d.menuData {
			for _, st := range d.menuData.storyList {
				if st.ID == a.Value {
					h.chime(d, "tap")
					h.openStory(d, st, false)
					return
				}
			}
		}
	case "story-choice":
		if d.drawer == pixel.DrawerStoryReader && d.story != nil {
			idx, err := strconv.Atoi(a.Value)
			if err == nil && idx >= 0 && idx < len(d.story.choices) {
				h.chime(d, "tap")
				h.advanceStoryPassage(d, d.story.choices[idx].Target, false)
				return
			}
		}
	}
}

func (h *terminalHub) pointerDetail(d *terminalSession, a pixel.Action, now time.Time) {
	i, _ := strconv.Atoi(a.Value)
	if d.menuData != nil && d.sceneMenu == d.menuData {
		if d.drawer == pixel.DrawerMemories && i >= 0 && i < len(d.menuData.memories) {
			d.menuData.memoryDetail = &d.menuData.memories[i]
			h.chime(d, "tap")
			h.render(d, now, false)
		} else if i >= 0 && i < len(d.menuData.rows) {
			d.menuData.detail = &d.menuData.rows[i]
			h.chime(d, "tap")
			h.render(d, now, false)
		}
	}
}

func (h *terminalHub) pointerVolume(d *terminalSession, a pixel.Action) {
	op := a.Kind
	if a.Kind == "volume-step" {
		op = "vol-plus"
		if strings.HasPrefix(a.Value, "-") {
			op = "vol-minus"
		}
	}
	go func() {
		if err := h.setVolume(d.client.ctx, d, 0, op); err != nil {
			d.update(func(d *terminalSession) {
				if h.active(d) {
					h.setCompanionStatus(d.companionID.get(), statusError)
				}
			})
		}
	}()
}

func (h *terminalHub) pointerTurnControl(d *terminalSession, key string, a pixel.Action, now time.Time) {
	switch a.Kind {
	case "intercom-to":
		h.chime(d, "tap")
		h.startRecording(d, a.Value, now, true)
	case "rec-cancel", "rec-send":
		h.recordingControl(d, now, a.Kind == "rec-cancel")
	case "rec-continue":
		if d.busy && !d.recording.active() && h.bridgeEndpoint(d) {
			h.startRecording(d, "", now, true)
		}
	case "stop":
		h.tapStop(d, now)
	case "listen", "reply":
		if (key == "companion-ring" || key == "listen") && (d.busy || d.ttsStarted || d.awaitAck) {
			return // The previous idle frame may still have a Talk target.
		}
		if d.recording.active() {
			h.recordingControl(d, now, false)
		} else {
			h.startRecording(d, a.Value, now, true)
		}
	}
}

func (h *terminalHub) pointerScene(d *terminalSession, a pixel.Action, now time.Time) {
	switch a.Kind {
	case "close-card":
		h.chime(d, "tap")
		d.card = nil
		h.render(d, now, false)
	case "close-view", "stop-turn", "companion":
		h.leaveScene(d, now, "leave-scene")
	case "dismiss":
		d.card = nil
		h.render(d, now, false)
	}
}

func (h *terminalHub) pointerUnpair(d *terminalSession, a pixel.Action) {
	cid, err := strconv.ParseUint(a.Value, 10, 32)
	if err != nil || cid == 0 || h.b.dc == nil || d.companionID.get() == "" {
		return
	}
	compID := d.companionID.get()
	go func() {
		_ = h.b.dc.unpairContact(compID, uint32(cid))
		d.update(func(d *terminalSession) {
			if h.active(d) && d.drawer == pixel.DrawerUnpair {
				h.chime(d, "tap")
				h.loadMenu(d)
			}
		})
	}()
}

func (h *terminalHub) selectAndPreviewVoice(d *terminalSession, voiceID string) {
	companionID := d.companionID.get()
	go func() {
		if err := h.saveVoice(companionID, voiceID); err != nil {
			return
		}
		previewText := voicePreviewText(voiceID)
		if pcm := h.b.clips.peek(voiceID, previewText); len(pcm) > 0 {
			d.update(func(d *terminalSession) {
				if h.active(d) && d.voice == voiceID {
					h.playPCMNow(d, pcm)
				}
			})
			return
		}
		if w := h.b.worker; w != nil && w.ready.Load() {
			ctx, cancel := context.WithTimeout(d.client.ctx, ttsWait)
			defer cancel()
			pcm, err := h.b.clips.get(ctx, w, voiceID, previewText)
			if err != nil || len(pcm) == 0 {
				return
			}
			d.update(func(d *terminalSession) {
				if h.active(d) && d.voice == voiceID {
					h.playPCMNow(d, pcm)
				}
			})
			return
		}
		d.update(func(d *terminalSession) {
			if h.active(d) {
				h.chime(d, "tap")
			}
		})
	}()
}

func (h *terminalHub) playPCMNow(d *terminalSession, pcm []byte) bool {
	stream := newAudioStream()
	stream.append(pcm)
	stream.finish(nil)
	return h.playTraced(d, stream, nil, time.Time{}).Err() == nil
}

func (d *terminalSession) acceptPointerRevision(action string, rev *uint16) bool {
	switch action {
	case "stop", "rec-cancel", "rec-send", "rec-continue", "listen", "reply":
		if !d.caps.PointerRev {
			return true
		}
		if rev == nil {
			return false
		}
		if !d.hasActionRev {
			return *rev == d.nativeRev
		}
		return uint16(d.nativeRev-*rev) <= uint16(d.nativeRev-d.actionRev)
	default:
		return true
	}
}

// pointerKey fingerprints both hit rectangles and the actions behind them.
// Rectangles alone let a stale tap run a new action under the same box, such
// as "reply" retargeted to the next peer.
func pointerKey(hits []pixel.HitBox, actions map[string]pixel.Action) string {
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "%s:%d,%d,%d,%d;", h.ID, h.X1, h.Y1, h.X2, h.Y2)
	}
	for _, id := range slices.Sorted(maps.Keys(actions)) {
		a := actions[id]
		fmt.Fprintf(&b, "%s=%s:%s;", id, a.Kind, a.Value)
	}
	return b.String()
}

const stopTapWindow = 2500 * time.Millisecond

func (d *terminalSession) resetStopTaps() {
	d.stopTaps, d.stopTapAt = 0, time.Time{}
}

func (d *terminalSession) stopTapsExpired(now time.Time) bool {
	return d.stopTaps != 0 && now.Sub(d.stopTapAt) > stopTapWindow
}

func (h *terminalHub) tapStop(d *terminalSession, now time.Time) {
	if d.busy && !d.ttsStarted && !d.awaitAck {
		if d.stopTapsExpired(now) {
			d.resetStopTaps()
		}
		if d.stopTaps == 0 {
			d.stopTapAt = now
		}
		d.stopTaps++
		if d.stopTaps < 3 {
			h.chime(d, "tap")
			h.render(d, now, false)
			return
		}
	} else if !d.ttsStarted && !d.awaitAck {
		return // A tap on an old frame after playback ended.
	}
	h.leaveScene(d, now, "stop")
	h.chime(d, "cancel")
}

func (h *terminalHub) leaveScene(d *terminalSession, now time.Time, reason string) {
	h.cancelTurn(d, reason)
	h.stopPlayback(d, true)
	h.clearView(d)
	if !d.recording.active() {
		h.setCompanionStatus(d.companionID.get(), statusIdle)
	}
	h.render(d, now, true)
}

func (h *terminalHub) clearView(d *terminalSession) {
	d.drawer, d.view, d.pending, d.card = "", nil, nil, nil
}

type menuContent struct {
	stories      []pixel.Story
	storyList    []stories.Saved
	games        []pixel.Tile
	memories     []pixel.MemoryItem
	memoryTitles map[string]string
	memoryDetail *pixel.MemoryItem
	rows         []pixel.Row
	detail       *pixel.Row
	invite       string
	notice       string
}

func (h *terminalHub) statsRows(d *terminalSession) []pixel.Row {
	var rows []pixel.Row
	if d.companion.Name != "" {
		rows = append(rows, pixel.Row{Title: "Companion", Text: d.companion.Name})
	}
	if d.pct >= 0 {
		ch := ""
		if d.charging {
			ch = " (charging)"
		}
		rows = append(rows, pixel.Row{Title: "Battery", Text: fmt.Sprintf("%d%%%s", d.pct, ch)})
	}
	rows = append(rows, pixel.Row{Title: "Voice", Text: voiceName(d.voice)})
	return rows
}

func (h *terminalHub) renderPixelDrawer(spec pixel.Spec, r renderRequest, narrator *image.RGBA, th pixel.Theme) (*image.RGBA, []pixel.HitBox, map[string]pixel.Action, error) {
	m := r.menu
	if m == nil {
		m = &menuContent{notice: "Loading…"}
	}
	switch r.drawer {
	case pixel.DrawerMenu:
		img, hits, acts := pixel.RenderMenuWithHits(spec, r.menuPage, th)
		return img, hits, acts, nil
	case pixel.DrawerMore:
		img, hits, acts := pixel.RenderMoreWithHits(spec, r.menuPage, th)
		return img, hits, acts, nil
	case pixel.DrawerVolume:
		img, hits, acts := pixel.RenderVolumeWithHits(spec, r.vol, r.menuPage, th)
		return img, hits, acts, nil
	case pixel.DrawerVoice:
		voices := make([]pixel.Tile, 0, len(voiceCatalog))
		for _, v := range voiceCatalog {
			voices = append(voices, pixel.Tile{ID: v.ID, Label: v.Name})
		}
		img, hits, acts := pixel.RenderVoiceWithHits(spec, voices, r.voice, r.menuPage, th)
		return img, hits, acts, nil
	case pixel.DrawerIntercom:
		items := make([]pixel.Tile, 0, len(r.peers))
		for _, p := range r.peers {
			items = append(items, pixel.Tile{
				ID:     "peer-" + p.ID,
				Label:  p.Name,
				Icon:   "talk",
				Action: pixel.Action{Kind: "intercom-to", Value: p.ID},
			})
		}
		img, hits, acts := pixel.RenderListWithHits(spec, "Talk to…", items, "No peers", r.menuPage, th, th.Accent)
		return img, hits, acts, nil
	case pixel.DrawerStats:
		img, hits, acts := pixel.RenderStatsWithHits(spec, r.statsRows, th)
		return img, hits, acts, nil
	case pixel.DrawerPair:
		if m.invite == "" {
			img, hits, acts := pixel.RenderListWithHits(spec, "Pair", nil, m.notice, 0, th, th.Accent)
			return img, hits, acts, nil
		}
		compName := ""
		if r.companion != nil {
			compName = r.companion.Name
		}
		return pixel.RenderPairWithHits(spec, compName, m.invite)
	case pixel.DrawerStoryReader:
		if r.storyTitle == "" {
			img, hits, acts := pixel.RenderListWithHits(spec, "Story", nil, "No story open", 0, th, th.Accent2)
			return img, hits, acts, nil
		}
		labels := make([]string, len(r.storyChoices))
		for i, ch := range r.storyChoices {
			labels[i] = ch.Label
		}
		img, hits, acts, _ := pixel.RenderStoryReaderWithHits(spec, r.storyTitle, r.storyText, labels, r.storyEnd, r.storyPage, r.storyBG, narrator, th)
		return img, hits, acts, nil
	case pixel.DrawerStories:
		if len(m.stories) == 0 {
			img, hits, acts := pixel.RenderListWithHits(spec, "Stories", nil, m.notice, 0, th, th.Accent2)
			return img, hits, acts, nil
		}
		img := pixel.RenderStoriesList(spec, m.stories, r.menuPage, th)
		perPage := 2
		if !spec.Round {
			perPage = 3
		}
		totalPages := max(1, (len(m.stories)+perPage-1)/perPage)
		page := max(0, min(r.menuPage, totalPages-1))
		hits, acts := pixel.NavHits(spec, page, totalPages)
		listTop := spec.Safe.Min.Y + 14
		navH := pixel.NavHeight(spec)
		listBottom := spec.Safe.Max.Y - navH - 3
		gapY := 3
		rowH := (listBottom - listTop - (perPage-1)*gapY) / perPage
		start := page * perPage
		for i := range perPage {
			idx := start + i
			if idx >= len(m.stories) {
				break
			}
			ry := listTop + i*(rowH+gapY)
			id := fmt.Sprintf("story-%d", idx)
			hits = pixel.AddLabeledHit(hits, spec, id, m.stories[idx].Title, image.Rect(spec.Safe.Min.X, ry, spec.Safe.Max.X, ry+rowH))
			acts[id] = pixel.Action{Kind: "story-open", Value: m.stories[idx].ID}
		}
		return img, hits, acts, nil
	case pixel.DrawerUnpair:
		img, hits, acts := pixel.RenderListWithHits(spec, "Unpair", m.games, m.notice, r.menuPage, th, pixel.Danger)
		return img, hits, acts, nil
	case pixel.DrawerMemories:
		if m.memoryDetail != nil {
			img := pixel.RenderMemoryDetail(spec, *m.memoryDetail, m.memoryTitles, r.menuPage, 0, th)
			hits, acts := pixel.NavHits(spec, r.menuPage, r.menuPage+2)
			return img, hits, acts, nil
		}
		if len(m.memories) == 0 {
			img, hits, acts := pixel.RenderListWithHits(spec, "Memories", nil, m.notice, 0, th, th.Accent)
			return img, hits, acts, nil
		}
		img := pixel.RenderMemoriesList(spec, m.memories, m.memoryTitles, r.menuPage, th)
		perPage := 2
		if !spec.Round {
			perPage = 3
		}
		totalPages := max(1, (len(m.memories)+perPage-1)/perPage)
		page := max(0, min(r.menuPage, totalPages-1))
		hits, acts := pixel.NavHits(spec, page, totalPages)
		listTop := spec.Safe.Min.Y + 14
		navH := pixel.NavHeight(spec)
		listBottom := spec.Safe.Max.Y - navH - 3
		gapY := 3
		rowH := (listBottom - listTop - (perPage-1)*gapY) / perPage
		start := page * perPage
		for i := range perPage {
			idx := start + i
			if idx >= len(m.memories) {
				break
			}
			ry := listTop + i*(rowH+gapY)
			id := fmt.Sprintf("memory-%d", idx)
			hits = pixel.AddLogicalHit(hits, spec, id, image.Rect(spec.Safe.Min.X, ry, spec.Safe.Max.X, ry+rowH))
			acts[id] = pixel.Action{Kind: "menu-detail", Value: strconv.Itoa(idx)}
		}
		return img, hits, acts, nil
	default:
		return nil, nil, nil, fmt.Errorf("unknown drawer %q", r.drawer)
	}
}

// loadMenu shows a placeholder, reads the menu off the actor, then posts the
// result. The placeholder identifies this request even if another client moves
// the menu while SQL runs.
func (h *terminalHub) loadMenu(d *terminalSession) {
	drawer, id, name := d.drawer, d.companionID.get(), d.companion.Name
	pending := &menuContent{notice: "Loading…"}
	d.menuData = pending
	ctx := d.client.ctx
	go func() {
		menu, err := h.b.readMenu(ctx, drawer, id, name)
		d.post(terminalEvent{kind: evMenuLoaded, now: time.Now(),
			menu: terminalMenu{drawer: drawer, pending: pending, menu: menu, err: err}})
	}()
}

func (h *terminalHub) menuLoaded(d *terminalSession, m terminalMenu, now time.Time) {
	if !h.active(d) || d.drawer != m.drawer || d.menuData != m.pending {
		return
	}
	menu := m.menu
	if m.err != nil {
		menu = menuContent{notice: "Menu unavailable"}
	}
	perPage := 2
	if d.screen.shape == "rect" {
		perPage = 3
	}
	count, pageSize := len(menu.rows), perPage
	switch m.drawer {
	case pixel.DrawerStories:
		count = len(menu.stories)
	case pixel.DrawerMemories:
		count = len(menu.memories)
	case pixel.DrawerUnpair:
		count, pageSize = len(menu.games), perPage
	}
	d.menuData = &menu
	d.menuPage = pixel.ClampPage(d.menuPage, count, pageSize)
	h.render(d, now, false)
}

func (b *larkServer) readMenu(ctx context.Context, drawer, id, name string) (menuContent, error) {
	var m menuContent
	switch drawer {
	case pixel.DrawerStories:
		list, err := b.listStories(ctx)
		m.notice = "No stories yet"
		progMap := map[string]string{}
		if progs, perr := b.listStoryProgress(ctx, id); perr == nil {
			for _, p := range progs {
				progMap[p.StoryID] = p.Passage
			}
		}
		palette := []imagecolor.RGBA{pixel.Mint, pixel.Peach, pixel.Sky}
		for i, st := range list {
			m.storyList = append(m.storyList, st)
			m.stories = append(m.stories, pixel.Story{
				ID:       st.ID,
				Title:    st.Title,
				Passage:  progMap[st.ID],
				Passages: st.Passages,
				Color:    palette[i%len(palette)],
			})
		}
		if len(m.stories) != 0 {
			m.notice = ""
		}
		return m, err
	case pixel.DrawerPair:
		if b.dc == nil {
			return m, errors.New("Delta Chat unavailable")
		}
		invite, err := b.dc.pairInvite(id)
		m.invite = invite
		return m, err
	case pixel.DrawerUnpair:
		if b.dc == nil {
			m.notice = "Delta Chat unavailable"
			return m, nil
		}
		contacts, err := b.dc.listPairedContacts(id)
		m.notice = "No paired contacts"
		for i, ct := range contacts {
			m.games = append(m.games, pixel.Tile{
				ID:     "unpair-" + strconv.Itoa(i),
				Label:  pixel.ClipMenu(ct.Name, 13),
				Detail: "Remove",
				Icon:   "unpair",
				Action: pixel.Action{Kind: "unpair-contact", Value: strconv.FormatUint(uint64(ct.ID), 10)},
			})
		}
		if len(m.games) != 0 {
			m.notice = ""
		}
		return m, err
	case pixel.DrawerMemories:
		entries, err := b.memoryEntries(ctx, "companion:"+id)
		m.notice = "No memories yet"
		m.memoryTitles = map[string]string{}
		if list, serr := b.listStories(ctx); serr == nil {
			for _, st := range list {
				m.memoryTitles[st.ID] = st.Title
			}
		}
		if progs, perr := b.listStoryProgress(ctx, id); perr == nil {
			for _, p := range progs {
				val, _ := json.Marshal(object{"passage": p.Passage, "page": p.Page + 1})
				m.memories = append(m.memories, pixel.MemoryItem{
					Section: "Reading",
					Key:     p.StoryID,
					Value:   string(val),
				})
				m.rows = append(m.rows, pixel.Row{Title: pixel.FormatMemoryTitle("Reading", p.StoryID, m.memoryTitles), Text: menuText(p.Passage)})
			}
		}
		for _, entry := range entries {
			k := str(entry["key"])
			raw := string(entry["value"].(json.RawMessage))
			sec := "Notes"
			if strings.HasPrefix(k, "reading:") {
				sec = "Reading"
			}
			m.memories = append(m.memories, pixel.MemoryItem{
				Section: sec,
				Key:     k,
				Value:   raw,
			})
			m.rows = append(m.rows, pixel.Row{Title: pixel.FormatMemoryTitle(sec, k, m.memoryTitles), Text: menuText(strings.Join(pixel.FormatMemoryLines(raw), " "))})
		}
		return m, err
	default:
		return m, nil
	}
}

func menuText(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	return shortText(text, len(text), false)
}
