package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"slices"
	"strconv"
	"strings"
	"time"

	"lark/fonts/dogica"
)

func (h *wsHub) handleProto(c *wsClient, msgType uint8, payload []byte) {
	h.mu.Lock()
	if h.closed || c.ctx.Err() != nil {
		h.mu.Unlock()
		return
	}
	now := time.Now()
	if c.kind == "" {
		h.mu.Unlock()
		switch msgType {
		case msgHello:
			hello, err := parseTerminalHello(payload)
			if err != nil {
				c.enqueueProto(msgReject, encodeReject(rejectBadCaps, "invalid hello"))
				return
			}
			h.handleHello(c, hello, now)
		case msgResume:
			res, err := parseTerminalResume(payload)
			if err != nil {
				c.enqueueProto(msgReject, encodeReject(rejectUnknownTerminal, "invalid resume"))
				return
			}
			h.handleResume(c, res, now)
		default:
			c.enqueueProto(msgReject, encodeReject(rejectBadVersion, "expected hello or resume"))
		}
		return
	}
	if msgType == msgMedia {
		h.mu.Unlock()
		h.handleRTPPacket(c, payload)
		return
	}
	defer h.mu.Unlock()
	d := c.terminal
	if d == nil || !h.active(d) {
		return
	}

	native := c.kind == "hardware"
	switch msgType {
	case msgWakeWord:
		h.clearDoneOrErrorStatus(d)
		h.takeVoiceOwnership(d)
		if native && d.caps.WakeWord && d.wakeWord && !d.recording.active() && !d.busy && !d.ttsStarted && !d.awaitAck {
			h.startRecording(d, "", now, true)
		}
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
			h.pointer(d, evName, x, y, rev, now, native)
		} else if d.caps.Drag && (native || c.kind == "web") && !d.asleep &&
			(d.displayApp != nil && d.app == d.displayApp || d.drawer == "" && d.view == nil && d.card == nil) {
			h.pointer(d, evName, x, y, rev, now, native)
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
			if c.kind == "web" && c.terminal == d && h.bridgeEndpoint(d) && !d.recording.active() && tgt == "" {
				h.startRecording(d, tgt, now, true)
			} else if d.recording.acceptsStart() {
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
	case msgSetVolume:
		auto, level, ok := parseSetVolume(payload)
		if !ok || c.terminal != d {
			return
		}
		_ = h.setVolume(c.ctx, d, auto, level, "")
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

func (h *wsHub) pointer(d *wsTerminal, evName string, x, y int, rev uint16, now time.Time, native bool) {
	width, height := screenSize, screenSize
	if native {
		width, height = d.screen.width, d.screen.height
	}
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
	if native && !d.acceptPointerRevision(a.kind, &rev) {
		return
	}
	if d.displayApp != nil {
		if d.app == d.displayApp {
			if i := slices.Index(d.app.terminals, d); i >= 0 {
				h.updateApp(d.app, appEvent{kind: evName, i: i, id: key, x: x, y: y, now: now})
			}
		}
		return
	}
	if evName == "drag" || evName == "up" {
		return
	}
	switch a.kind {
	case "menu":
		h.chime(d, "tap")
		d.bootHint = true
		d.drawer, d.card, d.pending = drawerMenu, nil, nil
		d.menuPage = 0
		d.view = nil
		h.render(d, now, false)
	case "menu-open", "menu-back", "menu-page":
		if d.drawer != drawerMenu && d.drawer != drawerMore && d.drawer != drawerVolume && d.drawer != drawerVoice &&
			d.drawer != drawerIntercom && d.drawer != drawerStats && d.drawer != drawerBookReader &&
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
			if d.drawer == drawerBookReader {
				d.drawer, d.menuPage = drawerBooks, 0
				d.readingBook = nil
				h.loadMenu(d)
				return
			} else if d.menuData != nil && d.menuData.memoryDetail != nil {
				d.menuData.memoryDetail = nil
			} else if d.menuData != nil && d.menuData.detail != nil {
				d.menuData.detail = nil
			} else if d.drawer == drawerMenu {
				d.drawer = ""
				d.menuHintUntil = now.Add(3 * time.Second)
			} else if d.drawer == drawerVolume || d.drawer == drawerPair || d.drawer == drawerUnpair || d.drawer == drawerStats {
				d.drawer, d.menuPage = drawerMore, 0
			} else {
				d.drawer, d.menuPage = drawerMenu, d.homePage
			}
		case "menu-page":
			d.menuPage, _ = strconv.Atoi(a.value)
			if d.drawer == drawerBookReader && d.readingBook != nil {
				d.readingPage = d.menuPage
				if h.b != nil && h.b.db != nil && d.companionID != "" {
					bkName := d.readingBook.Name
					compID := d.companionID
					pageNum := d.readingPage + 1
					go func() {
						_, _ = h.b.patchMemory(context.Background(), object{
							"scope": "companion:" + compID,
							"key":   "reading:" + bkName,
							"value": object{"page": pageNum},
						})
					}()
				}
			}
		}
		if wasPair && d.drawer != drawerPair {
			h.pushPower(d, d.brightness(), true)
		}
		if a.kind != "menu-back" {
			switch d.drawer {
			case drawerBooks, drawerGames, drawerMemories, drawerPair, drawerUnpair:
				h.loadMenu(d)
				return
			}
		}
		h.render(d, now, false)
	case "book-open":
		if d.menuData != nil && d.scene.menu == d.menuData {
			for _, bk := range d.menuData.bookApps {
				if bk.Name == a.value {
					h.chime(d, "tap")
					h.openBook(d, bk)
					return
				}
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
	case "game-run":
		if d.scene.menu == d.menuData {
			h.launchMenuGame(d, a.value)
		}
	case "volume-auto":
		if err := h.setVolume(d.client.ctx, d, false, 0, "vol-auto"); err != nil && h.active(d) {
			h.setCompanionStatus(d.companionID, statusError, 0, promptNone, err.Error())
		}
	case "volume-step":
		op := "vol-plus"
		if strings.HasPrefix(a.value, "-") {
			op = "vol-minus"
		}
		if err := h.setVolume(d.client.ctx, d, false, 0, op); err != nil && h.active(d) {
			h.setCompanionStatus(d.companionID, statusError, 0, promptNone, err.Error())
		}
	case "voice-set":
		h.mu.Unlock()
		err := h.saveVoice(d.companionID, a.value)
		h.mu.Lock()
		if err == nil && h.active(d) {
			h.chime(d, "tap")
		}
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
	case "build-cancel":
		h.cancelBuildCard(d, a.value)
	case "companion":
		h.leaveScene(d, now, "leave-scene")
	case "dismiss":
		d.card = nil
		h.render(d, now, false)
	case "voice-select":
		h.mu.Unlock()
		err := h.saveVoice(d.companionID, a.value)
		h.mu.Lock()
		if err == nil && h.active(d) {
			h.chime(d, "tap")
		}
	case "vol-auto", "vol-minus", "vol-plus":
		if err := h.setVolume(d.client.ctx, d, false, 0, a.kind); err != nil && h.active(d) {
			h.setCompanionStatus(d.companionID, statusError, 0, promptNone, err.Error())
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

func (d *wsTerminal) acceptPointerRevision(action string, rev *uint16) bool {
	switch action {
	case "stop", "rec-cancel", "rec-send", "rec-continue", "listen", "reply", "build-cancel":
		if !d.caps.PointerRev {
			return true
		}
		if rev == nil {
			return false
		}
		return *rev == d.nativeRev
	default:
		return true
	}
}

const stopTapWindow = 2500 * time.Millisecond

func (d *wsTerminal) resetStopTaps() {
	d.stopTaps, d.stopTapAt = 0, time.Time{}
}

func (d *wsTerminal) stopTapsExpired(now time.Time) bool {
	return d.stopTaps != 0 && now.Sub(d.stopTapAt) > stopTapWindow
}

func (h *wsHub) tapStop(d *wsTerminal, now time.Time) {
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

func (h *wsHub) leaveScene(d *wsTerminal, now time.Time, reason string) {
	h.cancelTurn(d, reason)
	h.stopPlayback(d, true)
	h.clearView(d)
	h.render(d, now, true)
}

func (h *wsHub) clearView(d *wsTerminal) {
	d.appGen++
	h.dropApp(d)
	d.drawer, d.view, d.pending, d.card = "", nil, nil, nil
}

type menuContent struct {
	books        []pixelBook
	bookApps     []savedApp
	games        []tile
	pixelGames   []pixelGame
	memories     []pixelMemoryItem
	memoryTitles map[string]string
	memoryDetail *pixelMemoryItem
	rows         []menuRow
	detail       *menuRow
	invite       string
	notice       string
}

func (h *wsHub) statsRows(d *wsTerminal) []menuRow {
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

func (h *wsHub) renderPixelDrawer(spec pixelDisplaySpec, r renderRequest, th pixelTheme) (*image.RGBA, []hitBox, map[string]sceneAction, error) {
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
	case drawerBookReader:
		if r.readingBook == nil {
			img, hits, acts := renderPixelListWithHits(spec, "Reader", nil, "No book open", 0, th, th.accent2)
			return img, hits, acts, nil
		}
		text, _ := bookTextAndCover(*r.readingBook, th.accent2)
		img := renderPixelReader(spec, r.readingBook.Title, text, r.readingPage, 0, th)
		hits, acts := navBarHits(spec, r.readingPage, r.readingPage+2)
		if r.readingPage == 0 {
			acts["menu-back"] = sceneAction{"menu-back", ""}
		}
		return img, hits, acts, nil
	case drawerBooks:
		if len(m.books) == 0 {
			img, hits, acts := renderPixelListWithHits(spec, "Books", nil, m.notice, 0, th, th.accent2)
			return img, hits, acts, nil
		}
		img := renderPixelBooksList(spec, m.books, r.menuPage, th)
		perPage := 2
		if !spec.round {
			perPage = 3
		}
		totalPages := max(1, (len(m.books)+perPage-1)/perPage)
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
			if idx >= len(m.books) {
				break
			}
			ry := listTop + i*(rowH+gapY)
			id := fmt.Sprintf("book-%d", idx)
			hits = addLogicalHit(hits, spec, id, image.Rect(spec.safe.Min.X, ry, spec.safe.Max.X, ry+rowH))
			acts[id] = sceneAction{"book-open", m.books[idx].Name}
		}
		return img, hits, acts, nil
	case drawerGames:
		if len(m.pixelGames) == 0 {
			img, hits, acts := renderPixelListWithHits(spec, "Games", nil, m.notice, 0, th, th.accent)
			return img, hits, acts, nil
		}
		img := renderPixelGamesList(spec, m.pixelGames, r.menuPage, th)
		perPage := 2
		if !spec.round {
			perPage = 3
		}
		totalPages := max(1, (len(m.pixelGames)+perPage-1)/perPage)
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
			if idx >= len(m.pixelGames) {
				break
			}
			ry := listTop + i*(rowH+gapY)
			id := fmt.Sprintf("game-%d", idx)
			hits = addLogicalHit(hits, spec, id, image.Rect(spec.safe.Min.X, ry, spec.safe.Max.X, ry+rowH))
			acts[id] = sceneAction{"game-run", m.pixelGames[idx].Name}
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
func (h *wsHub) loadMenu(d *wsTerminal) {
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
	case drawerBooks:
		count = len(menu.books)
	case drawerGames:
		count = len(menu.pixelGames)
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
	case drawerBooks:
		apps, err := b.listApps(ctx, true)
		m.notice = "No books yet"
		progMap := map[string]int{}
		if entries, merr := b.memoryEntries(ctx, "companion:"+id); merr == nil {
			for _, e := range entries {
				k := str(e["key"])
				if strings.HasPrefix(k, "reading:") {
					var p struct {
						Page int `json:"page"`
					}
					if json.Unmarshal(e["value"].(json.RawMessage), &p) == nil && p.Page > 0 {
						progMap[strings.TrimPrefix(k, "reading:")] = p.Page
					}
				}
			}
		}
		for _, app := range apps {
			if str(app["kind"]) != "book" {
				continue
			}
			a, _ := b.loadApp(ctx, str(app["name"]))
			text, cover := bookTextAndCover(a, pxMint)
			lines := wrapTextPx(text, dogica.Regular, 2, 220)
			total := max(1, (len(lines)+4)/5)
			m.bookApps = append(m.bookApps, a)
			m.books = append(m.books, pixelBook{
				Name:  a.Name,
				Title: a.Title,
				Page:  max(1, progMap[a.Name]),
				Total: total,
				Color: cover,
			})
		}
		if len(m.books) != 0 {
			m.notice = ""
		}
		return m, err
	case drawerGames:
		apps, err := b.listApps(ctx, false)
		m.notice = "No saved games yet"
		for _, app := range apps {
			if str(app["kind"]) != "game" {
				continue
			}
			appName := str(app["name"])
			saved := false
			if entries, merr := b.memoryEntries(ctx, "app:"+appName); merr == nil && len(entries) > 0 {
				saved = true
			}
			m.pixelGames = append(m.pixelGames, pixelGame{
				Name:  appName,
				Title: str(app["title"]),
				Kind:  "game",
				Saved: saved,
			})
		}
		if len(m.pixelGames) != 0 {
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
		if apps, aerr := b.listApps(ctx, false); aerr == nil {
			for _, app := range apps {
				m.memoryTitles[str(app["name"])] = str(app["title"])
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

func (d *wsTerminal) canLaunchMenuGame(menu *menuContent) bool {
	return d.drawer == drawerGames && d.menuData == menu && d.pose.mood == "idle"
}

func (h *wsHub) launchMenuGame(d *wsTerminal, name string) {
	menu := d.menuData
	if menu == nil || !d.canLaunchMenuGame(menu) || d.companionID == "" {
		return
	}
	h.chime(d, "tap")
	h.mu.Unlock()
	_, err := h.b.runSavedApp(d.client.ctx, d.companionID, name, menu)
	h.mu.Lock()
	if err != nil && h.active(d) && d.canLaunchMenuGame(menu) {
		menu.notice = "Game unavailable"
		h.render(d, time.Now(), false)
	}
}
