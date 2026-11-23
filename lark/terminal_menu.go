package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	imagecolor "image/color"
	"strconv"
	"strings"
	"time"
)

func (h *terminalHub) handleProto(c *terminalClient, msgType uint8, payload []byte) {
	h.mu.Lock()
	if h.closed || c.ctx.Err() != nil {
		h.mu.Unlock()
		return
	}
	now := time.Now()
	defer h.mu.Unlock()
	d := c.terminal
	if d == nil || !h.active(d) {
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
		_, target, ok := parseRecStart(payload)
		if !ok {
			return
		}
		h.clearDoneOrErrorStatus(d)
		if tgt, ok := recordTarget(target); ok {
			if c.terminal == d {
				h.takeVoiceOwnership(d)
			}
			if d.recording.acceptsStart() {
				if d.recording.active() {
					d.recording.acknowledge()
				} else {
					h.startRecording(d, tgt, now, false)
				}
			}
		}
	case msgRecEnd:
		_, cancelled, target, ok := parseRecEnd(payload)
		if !ok {
			return
		}
		d.recording.receivedEnd()
		if d.recording.active() {
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
	a := d.scene.actions[key]
	if !d.acceptPointerRevision(a.kind, &rev) {
		return
	}
	if evName == "drag" || evName == "up" {
		return
	}
	switch a.kind {
	case "menu":
		h.chime(d, "tap")
		d.drawer, d.card, d.pending = drawerMenu, nil, nil
		d.menuPage = 0
		d.view = nil
		h.render(d, now, false)
	case "menu-open", "menu-back", "menu-page":
		if d.drawer != drawerMenu && d.drawer != drawerMore && d.drawer != drawerVolume && d.drawer != drawerVoice &&
			d.drawer != drawerIntercom && d.drawer != drawerStats && d.drawer != drawerStoryReader &&
			d.scene.menu != d.menuData {
			return
		}
		h.chime(d, "tap")
		wasPair := d.drawer == drawerPair
		switch a.kind {
		case "menu-open":
			if d.drawer == drawerMenu {
				d.homePage = d.menuPage
			}
			d.drawer, d.menuPage = a.value, 0
			if d.drawer == drawerPair {
				h.pushPower(d, 100, true)
			}
		case "menu-back":
			if d.drawer == drawerStoryReader {
				h.stopPlayback(d, true)
				d.drawer, d.menuPage = drawerStories, 0
				d.story = nil
				h.loadMenu(d)
				return
			} else if d.menuData != nil && d.menuData.memoryDetail != nil {
				d.menuData.memoryDetail = nil
			} else if d.menuData != nil && d.menuData.detail != nil {
				d.menuData.detail = nil
			} else if d.drawer == drawerMenu {
				d.drawer = ""
			} else if d.drawer == drawerVolume || d.drawer == drawerPair || d.drawer == drawerUnpair || d.drawer == drawerStats {
				d.drawer, d.menuPage = drawerMore, 0
			} else {
				d.drawer, d.menuPage = drawerMenu, d.homePage
			}
		case "menu-page":
			d.menuPage, _ = strconv.Atoi(a.value)
			if d.drawer == drawerStoryReader && d.story != nil {
				h.stopPlayback(d, true)
				d.storyPage = d.menuPage
				if h.b != nil && h.b.db != nil && d.companionID != "" {
					prog := storyProgress{
						CompanionID: d.companionID,
						StoryID:     d.story.id,
						Passage:     d.story.currentPassageName(),
						Page:        d.storyPage,
						Flags:       d.story.compiled.flagsMap(d.story.flags),
					}
					go func() {
						_ = h.b.setStoryProgress(context.Background(), prog)
					}()
				}
			}
		}
		if wasPair && d.drawer != drawerPair {
			h.pushPower(d, d.brightness(), true)
		}
		if a.kind != "menu-back" {
			switch d.drawer {
			case drawerStories, drawerMemories, drawerPair, drawerUnpair:
				h.loadMenu(d)
				return
			}
		}
		h.render(d, now, false)
	case "story-open":
		if d.menuData != nil && d.scene.menu == d.menuData {
			for _, st := range d.menuData.storyList {
				if st.ID == a.value {
					h.chime(d, "tap")
					h.openStory(d, st, false)
					return
				}
			}
		}
	case "story-choice":
		if d.drawer == drawerStoryReader && d.story != nil {
			idx, err := strconv.Atoi(a.value)
			if err == nil && idx >= 0 && idx < len(d.story.choices) {
				h.chime(d, "tap")
				h.advanceStoryPassage(d, d.story.choices[idx].Target, false)
				return
			}
		}
	case "menu-detail":
		i, _ := strconv.Atoi(a.value)
		if d.menuData != nil && d.scene.menu == d.menuData {
			if d.drawer == drawerMemories && i >= 0 && i < len(d.menuData.memories) {
				d.menuData.memoryDetail = &d.menuData.memories[i]
				h.chime(d, "tap")
				h.render(d, now, false)
			} else if i >= 0 && i < len(d.menuData.rows) {
				d.menuData.detail = &d.menuData.rows[i]
				h.chime(d, "tap")
				h.render(d, now, false)
			}
		}
	case "volume-auto":
		if err := h.setVolume(d.client.ctx, d, false, 0, "vol-auto"); err != nil && h.active(d) {
			h.setCompanionStatus(d.companionID, statusError)
		}
	case "volume-step":
		op := "vol-plus"
		if strings.HasPrefix(a.value, "-") {
			op = "vol-minus"
		}
		if err := h.setVolume(d.client.ctx, d, false, 0, op); err != nil && h.active(d) {
			h.setCompanionStatus(d.companionID, statusError)
		}
	case "voice-set":
		h.selectAndPreviewVoice(d, a.value)
	case "intercom-to":
		h.chime(d, "tap")
		h.startRecording(d, a.value, now, true)
	case "close-card":
		h.chime(d, "tap")
		d.card = nil
		h.render(d, now, false)
	case "close-view", "stop-turn":
		h.leaveScene(d, now, "leave-scene")
	case "unpair-contact":
		cid, err := strconv.ParseUint(a.value, 10, 32)
		if err == nil && cid > 0 && h.b.dc != nil && d.companionID != "" {
			compID := d.companionID
			h.mu.Unlock()
			_ = h.b.dc.unpairContact(compID, uint32(cid))
			h.mu.Lock()
			if h.active(d) && d.drawer == drawerUnpair {
				h.chime(d, "tap")
				h.loadMenu(d)
			}
		}
	case "rec-cancel", "rec-send":
		h.recordingControl(d, now, a.kind == "rec-cancel")
	case "rec-continue":
		if d.busy && !d.recording.active() && h.bridgeEndpoint(d) {
			h.startRecording(d, "", now, true)
		}
	case "stop":
		h.tapStop(d, now)
	case "companion":
		h.leaveScene(d, now, "leave-scene")
	case "dismiss":
		d.card = nil
		h.render(d, now, false)
	case "voice-select":
		h.selectAndPreviewVoice(d, a.value)
	case "vol-auto", "vol-minus", "vol-plus":
		if err := h.setVolume(d.client.ctx, d, false, 0, a.kind); err != nil && h.active(d) {
			h.setCompanionStatus(d.companionID, statusError)
		}
	case "listen", "reply":
		if (key == "companion-ring" || key == "listen") && (d.busy || d.ttsStarted || d.awaitAck) {
			return // The previous idle frame may still have a Talk target.
		}
		if d.recording.active() {
			h.recordingControl(d, now, false)
		} else {
			h.startRecording(d, a.value, now, true)
		}
	}
}

func (h *terminalHub) selectAndPreviewVoice(d *terminalSession, voiceID string) {
	h.mu.Unlock()
	err := h.saveVoice(d.companionID, voiceID)
	h.mu.Lock()
	if err != nil || !h.active(d) {
		return
	}
	previewText := voicePreviewText(voiceID)
	if pcm := h.b.clips.peek(voiceID, previewText); len(pcm) > 0 {
		h.playPCM(d, pcm)
		return
	}
	if w := h.b.worker; w != nil && w.ready.Load() {
		go func() {
			ctx, cancel := context.WithTimeout(d.client.ctx, ttsWait)
			defer cancel()
			pcm, err := h.b.clips.get(ctx, w, voiceID, previewText)
			if err != nil || len(pcm) == 0 {
				return
			}
			h.mu.Lock()
			if h.active(d) && d.voice == voiceID {
				h.playPCM(d, pcm)
			}
			h.mu.Unlock()
		}()
		return
	}
	h.chime(d, "tap")
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

func hitsLayoutKey(hits []hitBox) string {
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "%s:%d,%d,%d,%d;", h.id, h.x1, h.y1, h.x2, h.y2)
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
		h.setCompanionStatus(d.companionID, statusIdle)
	}
	h.render(d, now, true)
}

func (h *terminalHub) clearView(d *terminalSession) {
	d.drawer, d.view, d.pending, d.card = "", nil, nil, nil
}

type menuContent struct {
	stories      []pixelStory
	storyList    []savedStory
	games        []tile
	memories     []pixelMemoryItem
	memoryTitles map[string]string
	memoryDetail *pixelMemoryItem
	rows         []menuRow
	detail       *menuRow
	invite       string
	notice       string
}

func (h *terminalHub) statsRows(d *terminalSession) []menuRow {
	var rows []menuRow
	if d.companion.Name != "" {
		rows = append(rows, menuRow{"Companion", d.companion.Name})
	}
	if d.pct >= 0 {
		ch := ""
		if d.charging {
			ch = " (charging)"
		}
		rows = append(rows, menuRow{"Battery", fmt.Sprintf("%d%%%s", d.pct, ch)})
	}
	rows = append(rows, menuRow{"Voice", voiceName(d.voice)})
	return rows
}

func (h *terminalHub) renderPixelDrawer(spec pixelDisplaySpec, r renderRequest, narrator *image.RGBA, th pixelTheme) (*image.RGBA, []hitBox, map[string]sceneAction, error) {
	m := r.view.menu
	if m == nil {
		m = &menuContent{notice: "Loading…"}
	}
	switch r.drawer {
	case drawerMenu:
		img, hits, acts := renderPixelMenuWithHits(spec, r.menuPage, th)
		return img, hits, acts, nil
	case drawerMore:
		img, hits, acts := renderPixelMoreWithHits(spec, r.menuPage, th)
		return img, hits, acts, nil
	case drawerVolume:
		img, hits, acts := renderPixelVolumeWithHits(spec, r.autoVol, r.vol, r.menuPage, th)
		return img, hits, acts, nil
	case drawerVoice:
		img, hits, acts := renderPixelVoiceWithHits(spec, r.voice, r.menuPage, th)
		return img, hits, acts, nil
	case drawerIntercom:
		items := make([]tile, 0, len(r.peers))
		for _, p := range r.peers {
			items = append(items, tile{
				id:     "peer-" + p.id,
				label:  p.name,
				icon:   "talk",
				action: sceneAction{"intercom-to", p.id},
			})
		}
		img, hits, acts := renderPixelListWithHits(spec, "Talk to…", items, "No peers", r.menuPage, th, th.accent)
		return img, hits, acts, nil
	case drawerStats:
		img, hits, acts := renderPixelStatsWithHits(spec, r.statsRows, th)
		return img, hits, acts, nil
	case drawerPair:
		if m.invite == "" {
			img, hits, acts := renderPixelListWithHits(spec, "Pair", nil, m.notice, 0, th, th.accent)
			return img, hits, acts, nil
		}
		compName := ""
		if r.companion != nil {
			compName = r.companion.Name
		}
		return renderPixelPairWithHits(spec, compName, m.invite)
	case drawerStoryReader:
		if r.storyTitle == "" {
			img, hits, acts := renderPixelListWithHits(spec, "Story", nil, "No story open", 0, th, th.accent2)
			return img, hits, acts, nil
		}
		img, hits, acts, _ := renderPixelStoryReaderWithHits(spec, r.storyTitle, r.storyText, r.storyChoices, r.storyEnd, r.storyPage, r.storyBG, narrator, th)
		return img, hits, acts, nil
	case drawerStories:
		if len(m.stories) == 0 {
			img, hits, acts := renderPixelListWithHits(spec, "Stories", nil, m.notice, 0, th, th.accent2)
			return img, hits, acts, nil
		}
		img := renderPixelStoriesList(spec, m.stories, r.menuPage, th)
		perPage := 2
		if !spec.round {
			perPage = 3
		}
		totalPages := max(1, (len(m.stories)+perPage-1)/perPage)
		page := max(0, min(r.menuPage, totalPages-1))
		hits, acts := navBarHits(spec, page, totalPages)
		listTop := spec.safe.Min.Y + 14
		navH := pixelNavHeight(spec)
		listBottom := spec.safe.Max.Y - navH - 3
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
			hits = addLabeledHit(hits, spec, id, m.stories[idx].Title, image.Rect(spec.safe.Min.X, ry, spec.safe.Max.X, ry+rowH))
			acts[id] = sceneAction{"story-open", m.stories[idx].ID}
		}
		return img, hits, acts, nil
	case drawerUnpair:
		img, hits, acts := renderPixelListWithHits(spec, "Unpair", m.games, m.notice, r.menuPage, th, pxDanger)
		return img, hits, acts, nil
	case drawerMemories:
		if m.memoryDetail != nil {
			img := renderPixelMemoryDetail(spec, *m.memoryDetail, m.memoryTitles, r.menuPage, 0, th)
			hits, acts := navBarHits(spec, r.menuPage, r.menuPage+2)
			return img, hits, acts, nil
		}
		if len(m.memories) == 0 {
			img, hits, acts := renderPixelListWithHits(spec, "Memories", nil, m.notice, 0, th, th.accent)
			return img, hits, acts, nil
		}
		img := renderPixelMemoriesList(spec, m.memories, m.memoryTitles, r.menuPage, th)
		perPage := 2
		if !spec.round {
			perPage = 3
		}
		totalPages := max(1, (len(m.memories)+perPage-1)/perPage)
		page := max(0, min(r.menuPage, totalPages-1))
		hits, acts := navBarHits(spec, page, totalPages)
		listTop := spec.safe.Min.Y + 14
		navH := pixelNavHeight(spec)
		listBottom := spec.safe.Max.Y - navH - 3
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
			hits = addLogicalHit(hits, spec, id, image.Rect(spec.safe.Min.X, ry, spec.safe.Max.X, ry+rowH))
			acts[id] = sceneAction{"menu-detail", strconv.Itoa(idx)}
		}
		return img, hits, acts, nil
	default:
		return nil, nil, nil, fmt.Errorf("unknown drawer %q", r.drawer)
	}
}

// The placeholder identifies this request even if another client moves the menu while SQL runs.
func (h *terminalHub) loadMenu(d *terminalSession) {
	drawer, id, name := d.drawer, d.companionID, d.companion.Name
	pending := &menuContent{notice: "Loading…"}
	d.menuData = pending
	h.mu.Unlock()
	menu, err := h.b.readMenu(d.client.ctx, drawer, id, name)
	h.mu.Lock()
	if !h.active(d) || d.drawer != drawer || d.menuData != pending {
		return
	}
	if err != nil {
		menu = menuContent{notice: "Menu unavailable"}
	}
	perPage := 2
	if d.screen.shape == "rect" {
		perPage = 3
	}
	count, pageSize := len(menu.rows), perPage
	switch drawer {
	case drawerStories:
		count = len(menu.stories)
	case drawerMemories:
		count = len(menu.memories)
	case drawerUnpair:
		count, pageSize = len(menu.games), perPage
	}
	d.menuData = &menu
	d.menuPage = clampMenuPage(d.menuPage, count, pageSize)
	h.render(d, time.Now(), false)
}

func (b *larkServer) readMenu(ctx context.Context, drawer, id, name string) (menuContent, error) {
	var m menuContent
	switch drawer {
	case drawerStories:
		stories, err := b.listStories(ctx)
		m.notice = "No stories yet"
		progMap := map[string]string{}
		if progs, perr := b.listStoryProgress(ctx, id); perr == nil {
			for _, p := range progs {
				progMap[p.StoryID] = p.Passage
			}
		}
		palette := []imagecolor.RGBA{pxMint, pxPeach, pxSky}
		for i, st := range stories {
			m.storyList = append(m.storyList, st)
			m.stories = append(m.stories, pixelStory{
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
	case drawerPair:
		if b.dc == nil {
			return m, errors.New("Delta Chat unavailable")
		}
		invite, err := b.dc.pairInvite(id)
		m.invite = invite
		return m, err
	case drawerUnpair:
		if b.dc == nil {
			m.notice = "Delta Chat unavailable"
			return m, nil
		}
		contacts, err := b.dc.listPairedContacts(id)
		m.notice = "No paired contacts"
		for i, ct := range contacts {
			m.games = append(m.games, tile{
				id:     "unpair-" + strconv.Itoa(i),
				label:  menuClip(ct.Name, 13),
				detail: "Remove",
				icon:   "unpair",
				action: sceneAction{"unpair-contact", strconv.FormatUint(uint64(ct.ID), 10)},
			})
		}
		if len(m.games) != 0 {
			m.notice = ""
		}
		return m, err
	case drawerMemories:
		entries, err := b.memoryEntries(ctx, "companion:"+id)
		m.notice = "No memories yet"
		m.memoryTitles = map[string]string{}
		if stories, serr := b.listStories(ctx); serr == nil {
			for _, st := range stories {
				m.memoryTitles[st.ID] = st.Title
			}
		}
		if progs, perr := b.listStoryProgress(ctx, id); perr == nil {
			for _, p := range progs {
				val, _ := json.Marshal(object{"passage": p.Passage, "page": p.Page + 1})
				m.memories = append(m.memories, pixelMemoryItem{
					Section: "Reading",
					Key:     p.StoryID,
					Value:   string(val),
				})
				m.rows = append(m.rows, menuRow{formatMemoryTitle("Reading", p.StoryID, m.memoryTitles), menuText(p.Passage)})
			}
		}
		for _, entry := range entries {
			k := str(entry["key"])
			raw := string(entry["value"].(json.RawMessage))
			sec := "Notes"
			if strings.HasPrefix(k, "reading:") {
				sec = "Reading"
			}
			m.memories = append(m.memories, pixelMemoryItem{
				Section: sec,
				Key:     k,
				Value:   raw,
			})
			m.rows = append(m.rows, menuRow{formatMemoryTitle(sec, k, m.memoryTitles), menuText(strings.Join(formatMemoryLines(raw), " "))})
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
