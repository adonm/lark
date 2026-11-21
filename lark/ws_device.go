package main

import (
	"bytes"
	"context"
	"image"
	"log"
	"maps"
	"math"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

const idleSleep = 120 * time.Second

var phoneID = regexp.MustCompile(`^phone-[0-9a-f]{4}$`)

type wsCaps struct {
	WakeWord       *bool `json:"wake_word,omitempty"`
	PanelSleep     *bool `json:"panel_sleep,omitempty"`
	PointerRev     *bool `json:"pointer_rev,omitempty"`
	BridgeEndpoint *bool `json:"bridge_endpoint,omitempty"`
	Battery        *bool `json:"battery,omitempty"`
	HeadPads       *bool `json:"head_pads,omitempty"`
	Call           *bool `json:"call,omitempty"`
	CallPCM        *bool `json:"call_pcm,omitempty"`
	Drag           *bool `json:"drag,omitempty"`
}

type deviceCaps struct {
	WakeWord       bool `json:"wake_word,omitempty"`
	PanelSleep     bool `json:"panel_sleep,omitempty"`
	PointerRev     bool `json:"pointer_rev,omitempty"`
	BridgeEndpoint bool `json:"bridge_endpoint,omitempty"`
	Battery        bool `json:"battery"`
	HeadPads       bool `json:"head_pads"`
	Call           bool `json:"call"`
	CallPCM        bool `json:"call_pcm,omitempty"`
	Drag           bool `json:"drag"`
}

func helloCaps(ev wsEvent) deviceCaps {
	value := func(flag *bool, fallback bool) bool {
		if flag != nil {
			return *flag
		}
		return fallback
	}
	return deviceCaps{
		WakeWord:       value(ev.Caps.WakeWord, false),
		PanelSleep:     value(ev.Caps.PanelSleep, false),
		PointerRev:     value(ev.Caps.PointerRev, false),
		BridgeEndpoint: value(ev.Caps.BridgeEndpoint, false),
		Battery:        value(ev.Caps.Battery, false),
		HeadPads:       value(ev.Caps.HeadPads, false),
		Call:           value(ev.Caps.Call, false),
		CallPCM:        value(ev.Caps.Call, false) && value(ev.Caps.CallPCM, false),
		Drag:           value(ev.Caps.Drag, true),
	}
}

type wsStatus struct {
	Battery        *int     `json:"battery_pct"`
	MV             *int     `json:"battery_mv"`
	Charge         *bool    `json:"charging"`
	WiFi           *int     `json:"wifi_rssi"`
	HasBattery     *bool    `json:"has_battery"`
	Volume         *int     `json:"volume"`
	FreeHeap       *uint64  `json:"free_heap"`
	MinHeap        *uint64  `json:"min_free_heap"`
	FreePSRAM      *uint64  `json:"free_psram"`
	Uptime         *uint64  `json:"uptime_s"`
	CPU            *float64 `json:"cpu_pct"`
	Partition      string   `json:"running_partition,omitempty"`
	LastFrameRev   *uint16  `json:"last_frame_rev,omitempty"`
	DisplayFlushMS *uint32  `json:"display_flush_ms,omitempty"`
	InputDrops     *uint32  `json:"input_drops,omitempty"`
	AudioUnderruns *uint32  `json:"audio_underruns,omitempty"`
	AudioDrops     *uint32  `json:"audio_drops,omitempty"`
	CallTXDrops    *uint32  `json:"call_tx_drops,omitempty"`
}

type wsDevice struct {
	client     *wsClient
	who        peer
	key        string
	firmware   string
	screen     screenGeometry
	board      string
	caps       deviceCaps
	voice      string
	announcing bool
	companion  companionState
	vol        int
	autoVol    bool
	ambientEMA float64
	hasAmbient bool
	pct        int
	charging   bool
	telemetry  wsStatus
	frame      *diagFrame
	svg        svgScreen
	grade      atomic.Uint32 // App workers read the board's grade without the hub lock.
	svgWire    wsFrame
	displayApp *appInstance
	encoder    frameEncoder
	renderGen  uint64
	rendering  bool
	renderNext *renderRequest
	app        *appInstance
	appView    svgScreen
	appGen     uint64

	lastAppError object

	scene     sceneView
	view      *sceneView
	pending   *sceneView
	card      *sceneView
	drawer    string
	menuPage  int
	homePage  int
	menuData  *menuContent
	statsAt   time.Time
	rev       uint64
	nativeRev uint16
	pose      companionPose
	moodAt    time.Time
	last      time.Time
	meter     time.Time
	asleep    bool
	wakeWord  bool

	parentReason parentReason

	callState                   callState
	callID                      uint32
	callPeer                    string
	callMuted                   bool
	callOrigin                  bool
	callStarted                 time.Time
	callSecond                  time.Duration
	callCancel                  context.CancelFunc
	callFramesIn, callFramesOut uint64

	recording     deviceCapture
	recSession    uint16
	queuedVoice   *endpointCapture
	turn          uint64
	trace         *turnTrace
	cancel        context.CancelFunc
	busy          bool
	stopTaps      int
	stopTapAt     time.Time
	agentSetScene bool

	audio      uint64
	replyAudio *turnAudio
	stopAudio  context.CancelFunc
	ttsStarted bool
	awaitAck   bool
	audioUntil time.Time
}

func webDeviceID(id string) bool { return virtualID.MatchString(id) || phoneID.MatchString(id) }

func (d *wsDevice) virtual() bool { return d.client.role == "web" || webDeviceID(d.key) }

func (c *wsClient) ping(now time.Time) wsFrame {
	c.pingMu.Lock()
	c.pingSentAt = now
	c.pingMu.Unlock()
	return wsFrame{wsPing, le.AppendUint64(nil, uint64(now.UnixNano()))}
}

func (c *wsClient) pong(data []byte) {
	c.pingMu.Lock()
	defer c.pingMu.Unlock()
	if len(data) == 8 && !c.pingSentAt.IsZero() && le.Uint64(data) == uint64(c.pingSentAt.UnixNano()) {
		c.rtt = new(milliseconds(time.Since(c.pingSentAt)))
		c.pingSentAt = time.Time{}
	}
}

func (c *wsClient) pingRTT() *float64 {
	c.pingMu.Lock()
	defer c.pingMu.Unlock()
	return c.rtt
}

func (h *wsHub) peerSnapshot(sender string) ([]peer, error) {
	stored, err := h.b.peers.snapshot(sender)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	online := h.onlinePeers(sender)
	h.mu.Unlock()
	list := make([]peer, 0, len(stored)+len(online))
	for _, p := range stored {
		if _, connected := online[strings.ToLower(p.DeviceID)]; !connected {
			p.Character = h.b.companionFor(p).Name
			list = append(list, p)
		}
	}
	for _, p := range online {
		list = append(list, p.peer)
	}
	slices.SortFunc(list, func(a, b peer) int { return strings.Compare(a.DeviceID, b.DeviceID) })
	return list, nil
}

func (b *bridge) peerSnapshot(sender string) ([]peer, error) {
	if b.ws == nil {
		list, err := b.peers.snapshot(sender)
		for i := range list {
			list[i].Character = b.companionFor(list[i]).Name
		}
		return list, err
	}
	return b.ws.peerSnapshot(sender)
}

type peerDisplay struct {
	peer
	companion companionLook
	auto      bool
	online    bool
	firmware  string
	caps      deviceCaps
}

func (h *wsHub) onlinePeers(sender string) map[string]peerDisplay {
	out := make(map[string]peerDisplay, len(h.devices))
	for _, d := range h.devices {
		if !h.active(d) || strings.EqualFold(sender, d.who.DeviceID) {
			continue
		}
		p := d.who
		p.Character = d.companion.Name
		if d.virtual() {
			p.IP = ""
		}
		out[d.key] = peerDisplay{p, d.companion.companionLook, d.autoVol, true, d.firmware, d.caps}
	}
	return out
}

func (h *wsHub) peerFrame(scoped bool) wsFrame {
	online := h.onlinePeers("")
	list := make([]peerDisplay, 0, len(h.storedPeers)+len(online))
	if !scoped {
		for _, p := range h.storedPeers {
			if _, ok := online[strings.ToLower(p.DeviceID)]; !ok {
				list = append(list, p)
			}
		}
	}
	for _, p := range online {
		list = append(list, p)
	}
	slices.SortFunc(list, func(a, b peerDisplay) int { return strings.Compare(a.DeviceID, b.DeviceID) })
	devices := make([]object, 0, len(list))
	for _, p := range list {
		if scoped {
			devices = append(devices, object{"device_id": p.DeviceID, "character": p.Character, "online": true, "caps": p.caps})
			continue
		}
		entry := object{"device_id": p.DeviceID, "character": p.Character, "ip": p.IP,
			"port": p.Port, "last_seen": p.LastSeen, "online": p.online,
			"ws": p.online, "companion": p.companion, "auto_volume": p.auto}
		if p.online {
			entry["firmware"] = p.firmware
			entry["caps"] = p.caps
		}
		devices = append(devices, entry)
	}
	return textFrame(object{"op": "peers", "devices": devices})
}

func (h *wsHub) broadcastPeers() {
	h.broadcastPeerFrames()
	for c := range h.clients {
		if c.role == "web" && c.scope == nil {
			h.refreshNeeded = true
			if !h.refreshing {
				h.refreshing = true
				go h.refreshStoredPeers()
			}
			return
		}
	}
}

func (c *wsClient) pushPeers(f wsFrame) {
	if !bytes.Equal(c.lastPeers.data, f.data) {
		c.lastPeers = f
		c.enqueue(f)
	}
}

func (h *wsHub) refreshStoredPeers() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for h.refreshNeeded && !h.closed {
		h.refreshNeeded = false
		h.mu.Unlock()
		list, err := h.b.peers.snapshot("")
		stored := make([]peerDisplay, 0, len(list))
		for _, p := range list {
			companion := h.b.companionFor(p)
			p.Character = companion.Name
			auto, _ := h.b.volumePrefs(p.DeviceID)
			stored = append(stored, peerDisplay{peer: p, companion: companion.companionLook, auto: auto})
		}
		h.mu.Lock()
		if err == nil && !h.closed && !h.refreshNeeded {
			h.storedPeers = stored
			h.broadcastPeerFrames()
		}
	}
	h.refreshing = false
}

func (h *wsHub) broadcastPeerFrames() {
	if len(h.clients) == 0 {
		h.peers = ""
		return
	}
	f := h.peerFrame(false)
	if string(f.data) != h.peers {
		h.peers = string(f.data)
		var scoped wsFrame
		for c := range h.clients {
			switch {
			case c.role != "web" || c.device != nil && c.device.announcing:
			case c.scope == nil:
				c.pushPeers(f)
			default:
				if scoped.data == nil {
					scoped = h.peerFrame(true)
				}
				c.pushPeers(scoped)
			}
		}
		for _, d := range h.devices {
			if d.drawer == "intercom" || d.drawer == "call" || d.card.hasIntercom() || d.view.hasIntercom() {
				h.render(d, time.Now(), false)
			}
		}
	}
}

// Called under the hub lock, after committing the companion and profile changes.
func (h *wsHub) companionChanged(id string, companion companionState, profilesChanged bool) {
	h.publishCompanion(id, companion, profilesChanged, true)
}

func (h *wsHub) publishCompanion(id string, companion companionState, profilesChanged, clearView bool) {
	key := strings.ToLower(id)
	if d := h.devices[key]; d != nil && h.active(d) {
		d.companion = companion
		d.voice = companion.Voice
		if !h.b.voices.allowed(d.voice) {
			d.voice = h.b.voices.defaultFor(d.voiceTheme())
		}
		if clearView {
			h.clearView(d)
			d.agentSetScene = d.busy
		}
		now := time.Now()
		h.render(d, now, true)
	}
	f := companionFrame(id, companion, companion.Voice)
	for c := range h.clients {
		if c.role == "web" && (c.scope == nil || c.scope.ID == key) {
			c.enqueue(f)
		}
	}
	h.broadcastPeers()
	if profilesChanged {
		h.savedCompanionsChanged()
	}
}

func companionFrame(id string, companion companionState, voice string) wsFrame {
	return textFrame(object{"op": "companion", "device_id": id, "character": companion.Name, "companion": companion.companionLook, "voice": voice})
}

func (h *wsHub) savedCompanionsChanged() {
	f := textFrame(object{"op": "saved_companions"})
	for c := range h.clients {
		if c.role == "web" && c.scope == nil {
			c.enqueue(f)
		}
	}
}

func (h *wsHub) companionsReassigned(from, to string) {
	for _, id := range []string{from, to} {
		f := textFrame(object{"op": "companion_reassigned", "device_id": id})
		for c := range h.clients {
			if c.role == "web" && (c.scope == nil || c.scope.ID == id) {
				c.enqueue(f)
			}
		}
	}
}

func (h *wsHub) hello(c *wsClient, ev wsEvent, now time.Time) {
	if ev.Role == "device" && ev.Proto < 2 {
		c.enqueue(textFrame(object{"op": "error", "status": 426, "error": "Device firmware upgrade required: WebSocket protocol 2 or newer"}))
		return
	}
	phone := phoneID.MatchString(ev.DeviceID)
	var v virtualClient
	if c.scope != nil {
		ev.Role, ev.DeviceID = "web", c.scope.ID
		v = *c.scope
	} else if virtualID.MatchString(strings.ToLower(ev.DeviceID)) {
		var ok bool
		v, ok = h.b.virtualByID(ev.DeviceID)
		if !ok {
			c.enqueue(textFrame(object{"op": "error", "status": 404, "error": "Virtual client not found"}))
			return
		}
		ev.Role = "web"
	}
	if ev.Role != "device" && ev.Role != "web" ||
		ev.Role == "device" && (!deviceIDRE.MatchString(ev.DeviceID) || phone) ||
		ev.Role == "web" && ev.DeviceID != "" && !deviceIDRE.MatchString(ev.DeviceID) {
		return
	}
	if ev.Role == "web" && !phone && v.ID == "" {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.closed || c.ctx.Err() != nil {
			return
		}
		c.role = "web"
		c.pushPeers(h.peerFrame(false))
		h.broadcastPeers()
		return
	}
	screen, board := roundScreen, ""
	if ev.Role == "device" {
		board = "esp-vocat"
		if ev.Board != "" {
			board = ev.Board
		}
		if board == "esp-box-3" {
			screen = screenGeometry{320, 240, "rect"}
		}
		if ev.Width != nil {
			screen.width = *ev.Width
		}
		if ev.Height != nil {
			screen.height = *ev.Height
		}
		if ev.Shape != "" {
			screen.shape = ev.Shape
		}
		// Bound native frames by the v2 maximum (360×360 RGB565).
		if screen.width < 1 || screen.width > screenSize || screen.height < 1 || screen.height > screenSize ||
			screen.shape != "round" && screen.shape != "rect" || board != "esp-vocat" && board != "esp-box-3" {
			c.enqueue(textFrame(object{"op": "error", "status": 400, "error": "Invalid display capabilities"}))
			return
		}
	}
	character := strings.TrimSpace(ev.Character)
	if v.ID != "" {
		character = v.Character
	} else if character == "" && ev.Role == "device" {
		character = "New Cat"
		if h.b.peers.store != nil {
			if prior, ok, _ := h.b.peers.get(ev.DeviceID); ok {
				character = prior.Character
			}
		}
	}
	if !characterRE.MatchString(character) {
		return
	}
	host, _, _ := net.SplitHostPort(c.remoteAddr().String())
	if addr, err := netip.ParseAddr(host); err == nil {
		host = addr.Unmap().String()
	}
	who := peer{DeviceID: ev.DeviceID, Character: character, LastSeen: now.Unix()}
	if ev.Role == "device" {
		who.IP, who.Port = host, 80
		if ev.Port != nil {
			if *ev.Port < 1 || *ev.Port > 65535 {
				c.enqueue(textFrame(object{"op": "error", "status": 400, "error": "Invalid device port"}))
				return
			}
			who.Port = *ev.Port
		}
		if h.b.peers.store != nil {
			if err := h.b.peers.register(who); err != nil {
				c.enqueue(textFrame(object{"op": "error", "status": 502, "error": "Cannot remember device"}))
				return
			}
		}
	}
	d := &wsDevice{client: c, key: strings.ToLower(ev.DeviceID), who: who,
		firmware: shortText(ev.Firmware, 24, false), screen: screen, board: board,
		caps: helloCaps(ev), pct: -1, last: now, moodAt: now, announcing: true}
	h.b.companionMu.Lock()
	defer h.b.companionMu.Unlock()
	d.companion = h.b.companionFor(d.who)
	d.voice = h.b.voices.resolve(d.companion.Voice, d.voiceTheme())
	d.autoVol, d.vol = h.b.volumePrefs(d.key)
	d.telemetry.HasBattery = new(d.caps.Battery)
	d.status(ev.wsStatus)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || c.ctx.Err() != nil {
		return
	}
	if old := h.devices[d.key]; old != nil {
		if ev.Role == "web" && ev.IfFree && h.active(old) {
			c.stop(wsCompanionOccupied)
			return
		}
		if old.virtual() {
			old.client.stop(wsCompanionOccupied)
		} else {
			old.client.cancel()
		}
		h.endCall(old, "disconnected")
		h.dropApp(old)
		old.recording.clear()
		h.cancelTurn(old, "reconnect")
		h.stopPlayback(old, true)
		d.rev = old.rev
	}
	h.devices[d.key], c.device, c.role = d, d, ev.Role
	if ev.Role == "device" {
		c.enqueue(textFrame(object{"op": "hello_ok"}))
	}
	if h.parentRefresh(d.key, now) == "" {
		h.render(d, now, true)
	}
}

func (h *wsHub) helloNative(nc *nativeClient, ev nativeHello, now time.Time) bool {
	c := nc.client
	if !deviceIDRE.MatchString(ev.deviceID) || phoneID.MatchString(ev.deviceID) {
		return false
	}
	board := "esp-vocat"
	if ev.board != "" {
		board = ev.board
	}
	screen := roundScreen
	if board == "esp-box-3" {
		screen = screenGeometry{320, 240, "rect"}
	}
	if ev.width > 0 {
		screen.width = int(ev.width)
	}
	if ev.height > 0 {
		screen.height = int(ev.height)
	}
	if screen.width < 1 || screen.width > screenSize || screen.height < 1 || screen.height > screenSize ||
		(board != "esp-vocat" && board != "esp-box-3") {
		return false
	}
	character := "New Cat"
	if h.b.peers.store != nil {
		if prior, ok, _ := h.b.peers.get(ev.deviceID); ok {
			character = prior.Character
		}
	}
	if !characterRE.MatchString(character) {
		return false
	}
	who := peer{DeviceID: ev.deviceID, Character: character, LastSeen: now.Unix(), IP: nc.remoteIP.String(), Port: 80}
	if h.b.peers.store != nil {
		if err := h.b.peers.register(who); err != nil {
			return false
		}
	}
	d := &wsDevice{
		client:     c,
		key:        strings.ToLower(ev.deviceID),
		who:        who,
		firmware:   shortText(ev.firmware, 24, false),
		screen:     screen,
		board:      board,
		caps:       helloCapsFromMask(ev.capsMask),
		pct:        -1,
		last:       now,
		moodAt:     now,
		announcing: true,
	}
	h.b.companionMu.Lock()
	defer h.b.companionMu.Unlock()
	d.companion = h.b.companionFor(d.who)
	d.voice = h.b.voices.resolve(d.companion.Voice, d.voiceTheme())
	d.autoVol, d.vol = h.b.volumePrefs(d.key)
	d.telemetry.HasBattery = new(d.caps.Battery)

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || c.ctx.Err() != nil {
		return false
	}
	if old := h.devices[d.key]; old != nil {
		if old.virtual() {
			old.client.stop(wsCompanionOccupied)
		} else {
			old.client.cancel()
		}
		h.endCall(old, "disconnected")
		h.dropApp(old)
		old.recording.clear()
		h.cancelTurn(old, "reconnect")
		h.stopPlayback(old, true)
		d.rev = old.rev
	}
	h.devices[d.key], c.device, c.role = d, d, "device"
	var flags uint16
	if h.bridgeEndpoint(d) {
		flags |= helloFlagBridgeEP
	}
	nc.enqueue(msgHelloOK, encodeHelloOK(nc.server.udpPort, flags, nc.ssrc, nc.ssrc))
	if h.parentRefresh(d.key, now) == "" {
		h.render(d, now, true)
	}
	return true
}

func (h *wsHub) announce(d *wsDevice) {
	d.announcing = false
	c := d.client
	if c.role == "web" {
		c.enqueue(companionFrame(d.who.DeviceID, d.companion, d.voice))
		if h.bridgeEndpoint(d) {
			h.pushWS(d, object{"op": "capabilities", "bridge_endpoint": true}, false)
		}
	}
	h.pushVolume(d, false, true)
	h.pushPower(d, d.brightness(), true)
	if d.parentReason != "" {
		h.pushWS(d, parentFrame(d), true)
	}
	h.broadcastPeers()
	if c.role == "web" {
		c.pushPeers(h.peerFrame(c.scope != nil))
	}
}

func (d *wsDevice) status(s wsStatus) {
	t := &d.telemetry
	if d.caps.Battery {
		if s.Battery != nil {
			d.pct = min(100, max(-1, *s.Battery))
			if d.pct >= 0 {
				t.Battery = new(d.pct)
			} else {
				t.Battery = nil
			}
		}
		if s.MV != nil {
			t.MV = new(min(6000, max(0, *s.MV)))
		}
		if s.Charge != nil {
			d.charging = *s.Charge
			t.Charge = s.Charge
		}
	}
	if s.WiFi != nil && *s.WiFi < 0 {
		t.WiFi = new(max(-127, *s.WiFi))
	}
	if s.Volume != nil {
		t.Volume = new(min(100, max(0, *s.Volume)))
	}
	if s.FreeHeap != nil {
		t.FreeHeap = s.FreeHeap
	}
	if s.MinHeap != nil {
		t.MinHeap = s.MinHeap
	}
	if s.FreePSRAM != nil {
		t.FreePSRAM = s.FreePSRAM
	}
	if s.Uptime != nil {
		t.Uptime = s.Uptime
	}
	if s.CPU != nil && !math.IsNaN(*s.CPU) {
		t.CPU = new(min(100., max(0., *s.CPU)))
	}
	if s.Partition != "" {
		t.Partition = shortText(s.Partition, 24, false)
	}
	if s.LastFrameRev != nil {
		t.LastFrameRev = s.LastFrameRev
	}
	if s.DisplayFlushMS != nil {
		t.DisplayFlushMS = s.DisplayFlushMS
	}
	if s.InputDrops != nil {
		t.InputDrops = s.InputDrops
	}
	if s.AudioUnderruns != nil {
		t.AudioUnderruns = s.AudioUnderruns
	}
	if s.AudioDrops != nil {
		t.AudioDrops = s.AudioDrops
	}
	if s.CallTXDrops != nil {
		t.CallTXDrops = s.CallTXDrops
	}
}

func (d *wsDevice) voiceTheme() string {
	return deviceTheme(d.who.Character)
}

type renderRequest struct {
	view      sceneView
	companion *companionState
	pose      companionPose
	app       *appInstance
	screen    svgScreen
	grade     gradeKey
	full      bool
	trace     *turnTrace
}

func (h *wsHub) companionPose(d *wsDevice, now time.Time, mood string) companionPose {
	p := poseFrame(now, d.moodAt, d.last, mood, d.pct, d.vol, d.charging, now.Before(d.meter))
	p.stopTaps = d.stopTaps
	p.canContinue = mood == "thinking" && h.bridgeEndpoint(d) && d.queuedVoice == nil
	return p
}

func (h *wsHub) frameGrade(d *wsDevice, now time.Time) gradeKey {
	if !h.atmosphere || d.client.role != "device" {
		return gradeDay
	}
	var loc *time.Location
	if p := h.parents[d.key]; p != nil && p.configured {
		loc = p.loc
	}
	return gradeAt(now, loc)
}

func (h *wsHub) render(d *wsDevice, now time.Time, full bool) {
	h.parentObserve(d, now)
	switch d.drawer {
	case drawerGames, drawerMemories, drawerHistory:
	default:
		d.menuData = nil
	}
	mood := "idle"
	switch {
	case d.callState == callActive:
		mood = "call"
	case d.callState != callNone:
		mood = "calling"
	case d.asleep:
		mood = "sleepy"
	case d.recording.active():
		mood = "listening"
	case d.ttsStarted || d.awaitAck:
		mood = "speaking"
	case d.busy:
		mood = "thinking"
	}
	if mood != "thinking" || d.stopTapsExpired(now) {
		d.resetStopTaps()
	}
	changed := mood != d.pose.mood
	if changed {
		d.moodAt = now
	}
	if changed && d.virtual() {
		h.pushWS(d, object{"op": "mode", "mode": mood}, false)
	}
	pose := h.companionPose(d, now, mood)
	d.pose = pose
	r := renderRequest{full: full || d.renderNext != nil && d.renderNext.full, trace: d.trace, grade: h.frameGrade(d, now)}
	d.grade.Store(uint32(r.grade))
	switch {
	case d.parentReason != "":
		r.view = parentScene(d.parentReason)
	case d.callState != callNone:
		if p := h.callPeer(d); p != nil {
			var elapsed time.Duration
			if d.callState == callActive {
				elapsed = now.Sub(d.callStarted)
				d.callSecond = elapsed.Truncate(time.Second)
			}
			r.view = callScene(d.callState, d.callID, p.companion, d.callMuted, elapsed)
		}
	case d.drawer != "" && mood == "idle":
		switch d.drawer {
		case drawerGames, drawerMemories, drawerHistory:
			r.view = d.menuData.scene(d.drawer, d.menuPage)
			r.view.menu = d.menuData
		case "volume":
			r.view = volumeScene(d.autoVol, d.vol)
		case "voice":
			r.view = voiceScene(d.voice, d.menuPage)
		case "intercom", "call":
			calling := d.drawer == "call"
			var choices []menuPeer
			for _, other := range h.devices {
				if other != d && h.active(other) && (!calling || other.caps.Call) {
					choices = append(choices, menuPeer{other.who.DeviceID, other.companion.Name, other.companion.FaceBG, other.companion.AccentColor})
				}
			}
			slices.SortFunc(choices, func(a, b menuPeer) int { return strings.Compare(a.id, b.id) })
			if !calling {
				choices = append([]menuPeer{{"all", "Everyone", "#FFADAC", "#FFD68B"}}, choices...)
			}
			d.menuPage = clampMenuPage(d.menuPage, len(choices), tilesPerPage)
			r.view = peerScene(choices, d.menuPage, calling)
		case "stats":
			if d.client.scope == nil {
				d.statsAt = now
				r.view = h.statsScene(d)
				break
			}
			fallthrough
		default:
			r.view = menuHome(d.voice, d.autoVol, d.vol, d.client.scope != nil, d.caps.Call, d.menuPage)
		}
	case mood == "listening":
		r.companion, r.pose = new(d.companion), pose
	case d.card != nil && (mood == "idle" || mood == "speaking" || mood != "sleepy" && d.card.duringTurn):
		r.view = *d.card
	case d.app != nil && mood != "sleepy":
		r.app, r.screen = d.app, d.appView
		r.view.title = d.app.spec.Title
	case d.view != nil && mood != "sleepy":
		r.view = *d.view
	default:
		r.companion, r.pose = new(d.companion), pose
	}
	if card := r.view.intercom; card != nil {
		r.view = card.scene(h.canCall(d, card.sender.DeviceID))
	}
	if r.companion == nil && r.app == nil && (mood == "thinking" || mood == "speaking") {
		r.view.actions = maps.Clone(r.view.actions)
		r.view.stopControl(pose.stopTaps)
	}
	d.renderGen++
	d.renderNext = &r
	if !d.rendering {
		d.rendering = true
		go h.renderFrames(d)
	}
}

// A device has one frame builder and one replaceable pending request. A dirty
// rectangle is always based on the last frame committed to that device's queue.
func (h *wsHub) renderFrames(d *wsDevice) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for d.renderNext != nil && h.active(d) {
		r, gen, prev, rev := *d.renderNext, d.renderGen, d.svg, d.rev+1
		web := d.client.role == "web"
		oldScene, pointerRev := d.scene, d.caps.PointerRev
		var geometry screenGeometry
		if d.client.role == "device" {
			geometry = d.screen
		}
		d.renderNext = nil
		h.mu.Unlock()

		screen := r.screen
		var err error
		if r.app == nil {
			var face string
			portrait := false
			if r.companion != nil {
				if id := activePortrait(r.companion.FaceSVG); id != "" {
					portrait = h.b.portraits.has(id)
					if !portrait {
						r.companion.FaceSVG = r.companion.defaultFace()
					}
				}
				r.view = companionChrome(*r.companion, r.pose)
				face = companionArt(*r.companion, r.pose)
			}
			raw := r.view.svg(face)
			grade := r.grade
			if !r.full && raw == prev.raw && grade == prev.grade {
				screen = prev
			} else if portrait {
				p := h.b.portraits
				screen, err = p.frames.buildGradedWithAssets(raw, false, geometry, grade, p.image)
				if err != nil {
					screen, err = screens.buildGraded(r.view.svg(companionArt(companionState{FaceSVG: r.companion.defaultFace()}, r.pose)), false, geometry, grade)
				}
			} else {
				screen, err = screens.buildGraded(raw, false, geometry, grade)
			}
		} else if r.grade != screen.grade && screen.fb != nil {
			if !r.full && screen.raw == prev.raw && r.grade == prev.grade {
				screen = prev
			} else {
				screen, err = screens.buildGraded(screen.raw, true, geometry, r.grade)
			}
		}
		var svgWire, native wsFrame
		var rawBytes int
		interactionChanged := pointerRev && !web &&
			(r.view.call != oldScene.call || !maps.Equal(r.view.actions, oldScene.actions) || !slices.Equal(screen.hits, prev.hits))
		if err == nil && (r.full && r.app == nil || screen.source != prev.source || screen.grade != prev.grade || interactionChanged) {
			start := time.Now()
			if screen.fb != nil {
				rect := dirtyRect(prev.fb, screen.fb, geometry)
				if rect.Empty() && interactionChanged {
					// Identical pixels can still map to different call peers or targets.
					rect = image.Rect(0, 0, 1, 1)
				}
				if !rect.Empty() {
					native = d.encoder.encode(screen.fb, nil, rect, rev, geometry.width)
					rawBytes = 12 + rect.Dx()*rect.Dy()*2
				}
			}
			screen.render += time.Since(start)
			svgWire = textFrame(object{"op": "svg", "device_id": d.who.DeviceID, "rev": rev, "svg": screen.source})
		}

		h.mu.Lock()
		if !h.active(d) || d.renderGen != gen {
			continue
		}
		if err != nil {
			log.Printf("Lark screen: %v", err)
			continue
		}
		d.scene, d.displayApp, d.svg = r.view, r.app, screen
		if svgWire.data == nil {
			continue
		}
		d.rev, d.svgWire = rev, svgWire
		if web {
			d.client.enqueue(svgWire)
			r.trace.measure(diagRender, screen.render)
			h.trackFrame(d, screen.render, len(svgWire.data), len(screen.source), "svg")
		} else if native.data != nil {
			d.client.enqueue(native)
			d.nativeRev = uint16(rev)
			r.trace.measure(diagRender, screen.render)
			codec := "RGB565"
			switch native.data[9] {
			case frameCodecRLE565:
				codec = "RLE565"
			case frameCodecJPEG:
				codec = "JPEG"
			case frameCodecLZ4:
				codec = "LZ4"
			}
			h.trackFrame(d, screen.render, len(native.data), rawBytes, codec)
		}
		for c := range h.clients {
			if c.watch == d.key {
				c.enqueue(svgWire)
			}
		}
		if d.announcing {
			h.announce(d)
		}
	}
	d.rendering, d.renderNext = false, nil
}

func (h *wsHub) wake(d *wsDevice, now time.Time) {
	if d.parentReason != "" {
		return
	}
	d.last = now
	h.setSleeping(d, false)
}

func (h *wsHub) setSleeping(d *wsDevice, asleep bool) {
	if !asleep && d.parentReason != "" {
		return
	}
	if asleep && d.callState != callNone {
		return
	}
	if d.asleep != asleep {
		d.asleep = asleep
		h.pushPower(d, d.brightness(), true)
	}
}

func (d *wsDevice) brightness() int {
	if d.parentReason != "" {
		// Firmware turns the battery panel off at 12; keep the notice visible.
		return 20
	}
	if d.asleep {
		return 12
	}
	return 85
}

func (h *wsHub) animate(ctx context.Context) {
	tick := time.NewTicker(40 * time.Millisecond)
	defer tick.Stop()
	sincePeers := time.Now()
	var nextGrade time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			h.mu.Lock()
			if h.closed {
				h.mu.Unlock()
				return
			}
			now := time.Now()
			checkGrade := h.atmosphere && !now.Before(nextGrade)
			if checkGrade {
				nextGrade = now.Truncate(time.Minute).Add(time.Minute)
			}
			for key, p := range h.parents {
				if len(p.offline) > 0 && h.devices[key] == nil {
					h.parentRefresh(key, now)
				}
			}
			for _, d := range h.devices {
				if !h.active(d) {
					continue
				}
				if checkGrade && d.client.role == "device" && !(d.asleep && d.caps.PanelSleep) &&
					d.svg.grade != h.frameGrade(d, now) {
					h.render(d, now, false)
				}
				if h.parentRefresh(d.key, now) != "" {
					if d.awaitAck && !d.ttsStarted && now.After(d.audioUntil.Add(4*time.Second)) {
						h.playbackDone(d, now)
					}
					continue
				}
				if d.callState != callNone {
					if d.callState == callActive && now.Sub(d.callStarted).Truncate(time.Second) > d.callSecond {
						h.render(d, now, false)
					}
					continue
				}
				switch {
				case d.recording.active() && d.recording.stopTimer == nil && now.Sub(d.recording.started) > 90*time.Second:
					h.stopRecordingAtLimit(d, now)
				case d.awaitAck && !d.ttsStarted && now.After(d.audioUntil.Add(4*time.Second)):
					h.playbackDone(d, now)
				case !d.asleep && !d.busy && !d.recording.active() && !d.ttsStarted && !d.awaitAck && now.Sub(d.last) >= idleSleep:
					h.setSleeping(d, true)
					h.render(d, now, false)
				case d.asleep && d.caps.PanelSleep && d.client.role == "device":
					// The battery board's panel is off; skip idle animation traffic.
				case d.drawer == "stats" && !d.asleep && now.Sub(d.statsAt) >= diagInterval:
					h.render(d, now, false)
				case d.stopTapsExpired(now):
					h.render(d, now, false)
				case (d.drawer == "" || d.asleep) && (d.app == nil && d.view == nil || d.asleep || d.recording.active()):
					if h.companionPose(d, now, d.pose.mood) != d.pose {
						h.render(d, now, false)
					}
				}
			}
			h.tickApps(now)
			if now.Sub(sincePeers) > 15*time.Second {
				h.broadcastPeers()
				sincePeers = now
			}
			h.mu.Unlock()
		}
	}
}

func (d *wsDevice) volumeFrame(persist bool) object {
	return object{"op": "volume", "device_id": d.who.DeviceID, "level": d.vol, "auto_volume": d.autoVol, "persist": persist}
}

func (h *wsHub) saveVoice(id, voice string) error {
	h.b.companionMu.Lock()
	defer h.b.companionMu.Unlock()
	if err := h.b.voices.save(id, voice); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if d := h.devices[strings.ToLower(id)]; d != nil && h.active(d) {
		d.voice = voice
		if d.drawer != "" {
			h.render(d, time.Now(), false)
		}
	}
	return nil
}

// Takes and returns with h.mu held. Serialize writes without blocking the hub.
func (h *wsHub) setVolume(ctx context.Context, d *wsDevice, auto bool, level int, action string) error {
	h.mu.Unlock()
	h.settingsMu.Lock()
	defer h.settingsMu.Unlock()
	h.mu.Lock()
	if !h.active(d) {
		return problem(409, "Device is not connected")
	}
	switch action {
	case "vol-minus":
		level = max(0, d.vol-10)
	case "vol-plus":
		level = min(100, d.vol+10)
	case "vol-auto":
		auto, level = !d.autoVol, d.vol
	}
	if level < 0 {
		level = d.vol
	}
	savedLevel := level
	if auto {
		savedLevel = initialAutoVolume
		if !d.autoVol {
			level = initialAutoVolume
		} else {
			level = d.vol
		}
	}
	h.mu.Unlock()
	err := h.b.saveVolumePrefs(ctx, d.key, auto, savedLevel)
	h.mu.Lock()
	if err != nil {
		return err
	}
	if !h.active(d) {
		return problem(409, "Device disconnected while saving volume")
	}
	if auto != d.autoVol {
		d.hasAmbient = false
	}
	changedMode := d.autoVol != auto
	d.autoVol, d.vol = auto, level
	h.pushVolume(d, !auto, true)
	now := time.Now()
	d.meter = now.Add(1800 * time.Millisecond)
	h.render(d, now, false)
	if changedMode {
		h.broadcastPeers()
	}
	return nil
}
