package main

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"
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
			if c.scope != nil && hello.companionID != "" && !strings.EqualFold(hello.companionID, c.scope.ID) {
				c.enqueueProto(msgReject, encodeReject(rejectBadToken, "companion out of scope"))
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
	if d == nil {
		if msgType == msgWatch && c.kind == "watcher" {
			watchID, ok := parseWatch(payload)
			if !ok {
				return
			}
			watchID = strings.ToLower(strings.TrimSpace(watchID))
			if watchID == "" {
				c.watch = ""
				return
			}
			if c.scope != nil && !strings.EqualFold(watchID, c.scope.ID) {
				return
			}
			c.watch = watchID
			target := h.terminals[c.watch]
			if target == nil || !h.active(target) {
				target = h.voiceOwner(c.watch)
			}
			if target != nil && h.active(target) {
				if target.svgWire != nil {
					c.enqueueProto(msgFrameSVG, target.svgWire)
				}
				c.enqueueProto(msgVolume, encodeVolumeMsg(target.vol, false, target.autoVol, target.id, target.companionID))
				c.enqueueProto(msgPower, []byte{byte(target.brightness())})
				if st, ok := h.companionStatuses[target.companionID]; ok {
					c.enqueueProto(msgStatus, encodeStatus(st.seq, st.state, st.progress, st.kind, st.text))
				}
				if owner := h.voiceOwner(target.companionID); owner != nil {
					c.enqueueProto(msgVoiceOwner, encodeVoiceOwner(h.nextSeq(), target.companionID, owner.id))
				}
				if target.parentReason != "" {
					c.enqueueProto(msgEvent, encodeEventMsg(h.nextSeq(), eventParentPolicy, target.companionID, string(target.parentReason), parentMessage(target.parentReason)))
				}
			} else {
				c.watch = ""
			}
			return
		}
		d = h.terminals[c.watch]
		if d == nil || !h.active(d) {
			d = h.voiceOwner(c.watch)
		}
		if msgType != msgPointer && msgType != msgTextInput && msgType != msgPromptReply {
			return
		}
	}
	if d == nil || !h.active(d) {
		return
	}
	if c.scope != nil && msgType == msgWatch {
		if watchID, ok := parseWatch(payload); ok && strings.EqualFold(watchID, c.scope.ID) && d.svgWire != nil {
			c.enqueueProto(msgFrameSVG, d.svgWire)
		}
		return
	}

	native := c.kind == "hardware"
	if d.companionID != "" && h.parentRefresh(d.companionID, now) != "" {
		switch msgType {
		case msgTelemetry, msgTTSAck, msgRecEnd:
		case msgRecStart:
			h.pushRecStop(d, "", true)
			return
		default:
			if native && msgType == msgPointer {
				if evName, _, _, _, ok := parsePointer(payload); ok && evName == "tap" {
					h.pushPower(d, d.brightness(), false)
				}
			}
			return
		}
	}
	if d.callState != callNone {
		switch msgType {
		case msgTelemetry, msgPointer, msgCallReq, msgTTSAck:
		case msgRecStart:
			return
		case msgRecEnd:
			d.recording.receivedEnd()
			d.recording.clear()
			return
		default:
			return
		}
	}
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
			if d.drawer != "stats" {
				h.render(d, now, false)
			}
		}
	case msgPointer:
		evName, x, y, rev, ok := parsePointer(payload)
		if !ok {
			return
		}
		h.clearDoneOrErrorStatus(d)
		if evName == "tap" {
			h.pointer(d, evName, x, y, rev, now, native)
		} else if d.caps.Drag && (native || c.kind == "web") && d.displayApp != nil && d.app == d.displayApp && !d.asleep {
			h.pointer(d, evName, x, y, rev, now, native)
		}
	case msgCallReq:
		action, callID, target, ok := parseCallReq(payload)
		if !ok || c.terminal != d {
			return
		}
		h.clearDoneOrErrorStatus(d)
		if action == callOpStart {
			if tgt, ok := recordTarget(target); ok && tgt != "" {
				h.takeVoiceOwnership(d)
				h.startCall(d, tgt)
			}
		} else if action == callOpEnd && callID != 0 && d.callID == callID {
			h.endCall(d, "audio unavailable")
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
	case msgTextInput:
		raw, ok := parseTextInput(payload)
		if !ok {
			return
		}
		h.clearDoneOrErrorStatus(d)
		text := strings.TrimSpace(raw)
		if text != "" && len(text) <= 4096 {
			h.startTurn(d, now, false, "typed", func(ctx context.Context, id uint64) {
				h.runTurn(ctx, d, id, text, "typed", nil)
			})
		}
	case msgPromptReply:
		id, choice, ok := parsePromptReply(payload)
		if !ok || d.companionID == "" {
			return
		}
		h.resolvePrompt(d.companionID, id, choice)
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
			h.playbackDone(d, now)
		} else if !aborted && d.awaitAck {
			d.ttsStarted = false
			h.playbackDone(d, now)
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
	if native {
		x, y = d.screen.inverseTap(x, y)
	}
	wasAsleep := d.asleep
	h.wake(d, now)
	if wasAsleep {
		h.render(d, now, false)
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
			if i := slices.Index(d.app.terminals, d); i >= 0 {
				h.updateApp(d.app, appEvent{kind: evName, i: i, id: key, x: x, y: y, now: now})
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
		if key == "companion-ring" && (d.busy || d.ttsStarted || d.awaitAck) {
			return // The previous idle frame may still have a Talk target.
		}
		if d.recording.active() {
			h.recordingControl(d, now, false)
		} else {
			h.startRecording(d, a.value, now, true)
		}
	case "call-peer":
		h.takeVoiceOwnership(d)
		h.startCall(d, a.value)
	}
}

func (d *wsTerminal) acceptPointerRevision(action string, rev *uint16) bool {
	switch action {
	case "stop", "rec-cancel", "rec-send", "rec-continue", "listen", "reply", "call-peer",
		"call-answer", "call-decline", "call-hangup", "call-mute", "build-cancel":
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
	count, pageSize := len(menu.rows), menuRowsPerPage
	if drawer == drawerGames {
		count, pageSize = len(menu.games), tilesPerPage
	}
	d.menuData = &menu
	d.menuPage = clampMenuPage(d.menuPage, count, pageSize)
	h.render(d, time.Now(), false)
}

func (b *larkServer) readMenu(ctx context.Context, drawer, id, name string) (menuContent, error) {
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
		entries, err := b.memoryEntries(ctx, "companion:"+id)
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
