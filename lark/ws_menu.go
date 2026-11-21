package main

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"
)

func (h *wsHub) handle(c *wsClient, ev wsEvent) {
	h.mu.Lock()
	if h.closed || c.ctx.Err() != nil {
		h.mu.Unlock()
		return
	}
	if c.scope != nil && ev.DeviceID != "" && ev.DeviceID != c.scope.ID {
		h.mu.Unlock()
		c.enqueue(textFrame(object{"op": "error", "status": 403, "error": "Device out of scope"}))
		return
	}
	now := time.Now()
	if c.role == "" {
		h.mu.Unlock()
		if ev.Event == "hello" {
			h.hello(c, ev, now)
		}
		return
	}
	defer h.mu.Unlock()
	d := c.device
	if d == nil {
		if ev.Event == "wake_word" && c.role == "web" && c.scope == nil && ev.Enabled != nil {
			if target := h.devices[strings.ToLower(ev.DeviceID)]; target != nil && h.active(target) && target.caps.WakeWord &&
				target.client.role == "device" {
				target.wakeWord = *ev.Enabled
				enabled := h.parentRefresh(target.key, now) == "" && target.wakeWord
				h.pushWakeWord(target, enabled)
				c.enqueue(textFrame(object{"op": "wake_word", "enabled": enabled, "device_id": target.who.DeviceID}))
			}
			return
		}
		if ev.Event == "watch" && c.role == "web" {
			if ev.DeviceID == "" {
				c.watch = ""
				return
			}
			c.watch = strings.ToLower(ev.DeviceID)
			if d := h.devices[c.watch]; d != nil && h.active(d) {
				if d.svgWire.data != nil {
					c.enqueue(d.svgWire)
				}
				c.enqueue(textFrame(d.volumeFrame(false)))
				c.enqueue(textFrame(object{"op": "power", "brightness": d.brightness()}))
				c.enqueue(textFrame(parentFrame(d)))
			} else {
				c.watch = ""
			}
			return
		}
		d = h.devices[c.watch]
		if ev.Event == "text" && ev.DeviceID != "" {
			d = h.devices[strings.ToLower(ev.DeviceID)]
		}
		if ev.Event != "tap" && ev.Event != "drag" && ev.Event != "up" && ev.Event != "text" {
			return
		}
	}
	if d == nil || !h.active(d) {
		return
	}
	if c.scope != nil && ev.Event == "watch" && ev.DeviceID == c.scope.ID {
		if d.svgWire.data != nil {
			c.enqueue(d.svgWire)
		}
		return
	}
	native := c.role == "device"
	if h.parentRefresh(d.key, now) != "" {
		switch ev.Event {
		case "status", "tts_ack", "rec_end":
		case "rec_start", "rec_request":
			h.pushRecStop(d, "", true)
			return
		default:
			if native && ev.Event == "tap" {
				h.pushPower(d, d.brightness(), false)
			}
			return
		}
	}
	if d.callState != callNone {
		switch ev.Event {
		case "status", "tap", "call", "call_end", "tts_ack":
		case "rec_start":
			return
		case "rec_end":
			d.recording.receivedEnd()
			d.recording.clear()
			return
		default:
			return
		}
	}
	switch ev.Event {
	case "wake_word":
		if native && d.caps.WakeWord && d.wakeWord && !d.recording.active() && !d.busy && !d.ttsStarted && !d.awaitAck {
			h.startRecording(d, "", now, true)
		}
	case "status":
		d.status(ev.wsStatus)
		if d.drawer != "stats" {
			h.render(d, now, false)
		}
	case "tap":
		h.pointer(d, ev, now, native)
	case "drag", "up":
		if d.caps.Drag && (native || c.role == "web") && d.displayApp != nil && d.app == d.displayApp && !d.asleep {
			h.pointer(d, ev, now, native)
		}
	case "call":
		if target, ok := recordTarget(ev.Target); ok && target != "" && c.device == d {
			h.startCall(d, target)
		}
	case "call_end":
		if c.device == d && ev.Call != 0 && d.callID == ev.Call {
			h.endCall(d, "audio unavailable")
		}
	case "rec_start":
		if target, ok := recordTarget(ev.Target); ok && d.recording.acceptsStart() {
			if d.recording.active() {
				d.recording.acknowledge()
			} else {
				h.startRecording(d, target, now, false)
			}
		}
	case "rec_request": // A virtual web cat uses the same bridge-owned handshake as a board.
		if c.role == "web" && c.device == d && h.bridgeEndpoint(d) && !d.recording.active() {
			if target, ok := recordTarget(ev.Target); ok && target == "" {
				h.startRecording(d, target, now, true)
			}
		}
	case "rec_end":
		d.recording.receivedEnd()
		if d.recording.active() {
			h.endRecording(d, ev, now)
		}
	case "text":
		text := strings.TrimSpace(ev.Text)
		if text != "" && len(text) <= 4096 {
			h.startTurn(d, now, false, "typed", func(ctx context.Context, id uint64) {
				h.runTurn(ctx, d, id, text, "typed", nil)
			})
		}
	case "intercom":
		target, ok := recordTarget(ev.Target)
		text := strings.TrimSpace(ev.Text)
		if ok && target != "" && text != "" && len(text) <= 4096 {
			h.startTurn(d, now, false, "intercom", func(ctx context.Context, id uint64) {
				others, err := h.peerSnapshot(d.who.DeviceID)
				if err != nil || ctx.Err() != nil {
					h.turnError(d, id, "Could not find nearby companions.")
					return
				}
				h.sendIntercom(ctx, d, id, others, target, text, nil)
			})
		}
	case "tts_ack":
		if ev.Audio == 0 || ev.Audio != d.audio {
			return
		}
		if ev.State == "interrupted" && (d.ttsStarted || d.awaitAck) {
			h.stopPlayback(d, false)
			h.playbackDone(d, now)
		} else if ev.State == "done" && d.awaitAck {
			d.ttsStarted = false
			h.playbackDone(d, now)
		}
	}
}

func (h *wsHub) pointer(d *wsDevice, ev wsEvent, now time.Time, native bool) {
	width, height := screenSize, screenSize
	if native {
		width, height = d.screen.width, d.screen.height
	}
	if ev.X < 0 || ev.X >= width || ev.Y < 0 || ev.Y >= height || len(ev.ID) > 64 {
		return
	}
	if native {
		ev.X, ev.Y = d.screen.inverseTap(ev.X, ev.Y)
	}
	wasAsleep := d.asleep
	h.wake(d, now)
	if wasAsleep {
		h.render(d, now, false)
		return
	}
	inside := d.screen.shape != "round" || inScreenCircle(ev.X, ev.Y)
	key := ""
	if ev.ID == "" || native {
		if inside {
			key = hitID(d.svg.hits, ev.X, ev.Y)
		}
	} else if slices.ContainsFunc(d.svg.hits, func(hit hitBox) bool { return hit.id == ev.ID }) {
		key = ev.ID
	}
	a := d.scene.actions[key]
	if native && !d.acceptPointerRevision(a.kind, ev.RevLo) {
		return
	}
	if d.callState != callNone {
		if d.scene.call != d.callID {
			return
		}
		switch a.kind {
		case "call-answer":
			h.answerCall(d)
		case "call-decline":
			h.declineCall(d)
		case "call-hangup":
			h.endCall(d, "hung up")
		case "call-mute":
			h.toggleCallMute(d)
		}
		return
	}
	if d.displayApp != nil {
		if d.app == d.displayApp {
			if i := slices.Index(d.app.devices, d); i >= 0 {
				h.updateApp(d.app, appEvent{kind: ev.Event, i: i, id: key, x: ev.X, y: ev.Y, now: now})
			}
		}
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
		if d.scene.menu != d.menuData {
			return
		}
		h.chime(d, "tap")
		switch a.kind {
		case "menu-open":
			d.homePage = d.menuPage
			d.drawer, d.menuPage = a.value, 0
		case "menu-back":
			if d.menuData != nil && d.menuData.detail != nil {
				d.menuData.detail = nil
			} else if d.drawer == drawerMenu {
				d.drawer = ""
			} else {
				d.drawer, d.menuPage = drawerMenu, d.homePage
			}
		case "menu-page":
			d.menuPage, _ = strconv.Atoi(a.value)
		}
		if a.kind != "menu-back" {
			switch d.drawer {
			case drawerGames, drawerMemories, drawerHistory:
				h.loadMenu(d)
				return
			}
		}
		h.render(d, now, false)
	case "menu-detail":
		i, _ := strconv.Atoi(a.value)
		if d.menuData != nil && d.scene.menu == d.menuData && i >= 0 && i < len(d.menuData.rows) {
			d.menuData.detail = &d.menuData.rows[i]
			h.chime(d, "tap")
			h.render(d, now, false)
		}
	case "game-run":
		if d.scene.menu == d.menuData {
			h.launchMenuGame(d, a.value)
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
		err := h.saveVoice(d.who.DeviceID, a.value)
		h.mu.Lock()
		if err == nil && h.active(d) {
			h.chime(d, "tap")
		}
	case "vol-auto", "vol-minus", "vol-plus":
		if err := h.setVolume(d.client.ctx, d, false, 0, a.kind); err != nil && h.active(d) {
			h.pushWS(d, object{"op": "error", "status": 502, "error": err.Error()}, true)
		}
	case "listen", "reply":
		if key == "companion-ring" && (d.busy || d.ttsStarted || d.awaitAck) {
			return // The previous idle frame may still have a Talk target.
		}
		if d.recording.active() {
			h.recordingControl(d, now, false)
		} else {
			h.startRecording(d, a.value, now, true)
		}
	case "call-peer":
		h.startCall(d, a.value)
	}
}

func (d *wsDevice) acceptPointerRevision(action string, rev *uint16) bool {
	switch action {
	case "stop", "rec-cancel", "rec-send", "rec-continue", "listen", "reply", "call-peer",
		"call-answer", "call-decline", "call-hangup", "call-mute", "build-cancel":
		if rev == nil {
			return !d.caps.PointerRev
		}
		return *rev == d.nativeRev
	default:
		return true
	}
}

const stopTapWindow = 2500 * time.Millisecond

func (d *wsDevice) resetStopTaps() {
	d.stopTaps, d.stopTapAt = 0, time.Time{}
}

func (d *wsDevice) stopTapsExpired(now time.Time) bool {
	return d.stopTaps != 0 && now.Sub(d.stopTapAt) > stopTapWindow
}

func (h *wsHub) tapStop(d *wsDevice, now time.Time) {
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

func (h *wsHub) leaveScene(d *wsDevice, now time.Time, reason string) {
	h.cancelTurn(d, reason)
	h.stopPlayback(d, true)
	h.clearView(d)
	h.render(d, now, true)
}

func (h *wsHub) clearView(d *wsDevice) {
	d.appGen++
	h.dropApp(d)
	d.drawer, d.view, d.pending, d.card = "", nil, nil, nil
}

type menuContent struct {
	games  []tile
	rows   []menuRow
	detail *menuRow
	notice string
}

func (m *menuContent) scene(drawer string, page int) sceneView {
	title := "History"
	switch drawer {
	case drawerGames:
		return gamesScene(m.games, page, m.notice)
	case drawerMemories:
		title = "Memories"
	}
	if m.detail != nil {
		return menuDetail(title, *m.detail)
	}
	return menuList(title, m.notice, m.rows, page)
}

// The placeholder identifies this request even if another client moves the menu while SQL runs.
func (h *wsHub) loadMenu(d *wsDevice) {
	drawer, id, name := d.drawer, d.who.DeviceID, d.companion.Name
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
	count, pageSize := len(menu.rows), menuRowsPerPage
	if drawer == drawerGames {
		count, pageSize = len(menu.games), tilesPerPage
	}
	d.menuData = &menu
	d.menuPage = clampMenuPage(d.menuPage, count, pageSize)
	h.render(d, time.Now(), false)
}

func (b *bridge) readMenu(ctx context.Context, drawer, id, name string) (menuContent, error) {
	var m menuContent
	switch drawer {
	case drawerGames:
		apps, err := b.listApps(ctx, false)
		m.notice = "No saved games yet"
		for i, app := range apps {
			detail := "Saved app"
			if app["kind"] == "drawing" {
				detail = "Drawing"
			}
			m.games = append(m.games, tile{id: "game-" + strconv.Itoa(i), label: menuClip(str(app["title"]), 13),
				detail: detail, icon: "games", action: sceneAction{"game-run", str(app["name"])}, colors: navPalette})
		}
		if len(m.games) != 0 {
			m.notice = ""
		}
		return m, err
	case drawerMemories:
		entries, err := b.memoryEntries(ctx, id)
		m.notice = "No memories yet"
		for _, entry := range entries {
			raw := entry["value"].(json.RawMessage)
			var value string
			if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &value) != nil {
				value = string(raw)
			}
			m.rows = append(m.rows, menuRow{str(entry["key"]), menuText(value)})
		}
		return m, err
	default:
		page, err := b.readHistoryPage(ctx, id, 20, nil)
		m.notice = "No messages yet"
		for _, e := range slices.Backward(page.Messages) {
			speaker := name
			switch e.Role {
			case "user":
				speaker = "You"
			case "intercom":
				speaker = "From " + e.Sender
			}
			m.rows = append(m.rows, menuRow{menuText(speaker), menuText(e.Text)})
		}
		return m, err
	}
}

func menuText(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	return shortText(text, len(text), false)
}

func (d *wsDevice) canLaunchMenuGame(menu *menuContent) bool {
	return d.drawer == drawerGames && d.menuData == menu && d.pose.mood == "idle"
}

func (h *wsHub) launchMenuGame(d *wsDevice, name string) {
	menu := d.menuData
	if menu == nil || !d.canLaunchMenuGame(menu) {
		return
	}
	h.chime(d, "tap")
	h.mu.Unlock()
	_, err := h.b.runSavedApp(d.client.ctx, d.who.DeviceID, name, menu)
	h.mu.Lock()
	if err != nil && h.active(d) && d.canLaunchMenuGame(menu) {
		menu.notice = "Game unavailable"
		h.render(d, time.Now(), false)
	}
}
