package main

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"log"
	"maps"
	"math"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

const idleSleep = 120 * time.Second

type terminalCaps struct {
	WakeWord       bool `json:"wake_word,omitempty"`
	PanelSleep     bool `json:"panel_sleep,omitempty"`
	PointerRev     bool `json:"pointer_rev,omitempty"`
	BridgeEndpoint bool `json:"bridge_endpoint,omitempty"`
	Battery        bool `json:"battery"`
	Drag           bool `json:"drag"`
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
}

type wsTerminal struct {
	client      *wsClient
	id             string
	key            string
	kind           string // "hardware" or "web"
	protoKind      uint8
	companionID    string
	ip             string
	port           int
	lastSeen       int64
	firmware       string
	screen         screenGeometry
	displayFormats uint8
	frameCodecs    uint8
	board          string
	caps           terminalCaps
	resumeToken    [resumeTokenLen]byte
	hasResumeToken bool
	voice       string
	announcing  bool
	companion   companionState
	vol         int
	autoVol     bool
	ambientEMA  float64
	hasAmbient  bool
	pct         int
	charging    bool
	telemetry   wsStatus
	frame       *diagFrame
	svg         svgScreen
	grade       atomic.Uint32 // App workers read the board's grade without the hub lock.
	displayApp  *appInstance
	encoder     frameEncoder
	renderGen   uint64
	rendering   bool
	renderNext  *renderRequest
	app         *appInstance
	appView     svgScreen
	appGen      uint64

	lastAppError object

	scene     sceneView
	view      *sceneView
	pending   *sceneView
	card      *sceneView
	drawer        string
	menuPage      int
	homePage      int
	menuData      *menuContent
	menuHintUntil time.Time
	bootHint      bool
	swipeActive   bool
	swipeStartX   int
	swipeStartY   int
	readingBook   *savedApp
	readingPage   int
	statsAt       time.Time
	weatherAt     time.Time
	weatherTick   int
	rev       uint64
	nativeRev uint16
	pose      companionPose
	moodAt    time.Time
	last      time.Time
	meter     time.Time
	asleep         bool
	wakeWord       bool
	replyTo        string
	listenRenderAt time.Time

	recording     deviceCapture
	recSession    uint16
	queuedVoice   *endpointCapture
	queuedNotices []queuedNotice
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

func (d *wsTerminal) virtual() bool { return d.kind == "web" || d.client.kind == "web" }

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

func (h *wsHub) peerSnapshot(sender string) ([]companionTarget, error) {
	return h.b.companionTargets(sender)
}

func (b *larkServer) peerSnapshot(sender string) ([]companionTarget, error) {
	return b.companionTargets(sender)
}

type peerDisplay struct {
	id          string
	kind        string
	companionID string
	ip          string
	port        int
	lastSeen    int64
	auto        bool
	online      bool
	firmware    string
	caps        terminalCaps
}

func (h *wsHub) onlineTerminals() map[string]peerDisplay {
	out := make(map[string]peerDisplay, len(h.terminals))
	for _, d := range h.terminals {
		if !h.active(d) {
			continue
		}
		ip := d.ip
		if d.virtual() {
			ip = ""
		}
		out[d.key] = peerDisplay{
			id:          d.id,
			kind:        d.kind,
			companionID: d.companionID,
			ip:          ip,
			port:        d.port,
			lastSeen:    d.lastSeen,
			auto:        d.autoVol,
			online:      true,
			firmware:    d.firmware,
			caps:        d.caps,
		}
	}
	return out
}

func (h *wsHub) broadcastPeers() {
	for _, d := range h.terminals {
		if d.drawer == "intercom" || d.card.hasIntercom() || d.view.hasIntercom() {
			h.render(d, time.Now(), false)
		}
	}
}

// Called under the hub lock, after committing the companion changes.
func (h *wsHub) companionChanged(id string, companion companionState) {
	h.publishCompanion(id, companion, true)
}

func (h *wsHub) publishCompanion(id string, companion companionState, clearView bool) {
	key := strings.ToLower(id)
	now := time.Now()
	for _, d := range h.terminalsFor(key) {
		d.companion = companion
		d.voice = companion.Voice
		if !h.b.voices.allowed(d.voice) {
			d.voice = h.b.voices.defaultFor(d.voiceTheme())
		}
		if clearView {
			h.clearView(d)
			d.agentSetScene = d.busy
		}
		if companion.ListenSeconds <= 0 && d.recording.active() && d.recording.listen {
			h.stopListeningWindow(d, now)
		}
		h.render(d, now, true)
	}
	h.broadcastPeers()
}

func (h *wsHub) registerHardwareTerminal(termID, hintName, ip string, port int, now time.Time) (terminalRecord, error) {
	termID = strings.ToLower(termID)
	if t, ok := h.b.terminalByID(termID); ok {
		t.Kind = "hardware"
		t.IP = ip
		t.Port = port
		t.LastSeen = now.Unix()
		if err := h.b.terminals.save(t.ID, t, false); err != nil {
			return terminalRecord{}, err
		}
		return t, nil
	}
	if hintName != "" {
		if comp, ok := h.b.companionByID(strings.ToLower(hintName)); ok {
			t := terminalRecord{
				ID:          termID,
				Kind:        "hardware",
				CompanionID: comp.ID,
				IP:          ip,
				Port:        port,
				LastSeen:    now.Unix(),
				CreatedAt:   now.UTC().Format(time.RFC3339Nano),
			}
			if err := h.b.terminals.save(t.ID, t, false); err != nil {
				return terminalRecord{}, err
			}
			return t, nil
		}
	}
	c := blankCompanion()
	if hintName != "" && hintName != c.Name && !strings.EqualFold(hintName, termID) && characterRE.MatchString(hintName) {
		c.Name = hintName
		c.Configured = true
	}
	if !h.b.voices.allowed(c.Voice) {
		c.Voice = h.b.voices.defaultFor(companionTheme(c.Name))
	}
	ctx := h.b.root
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := h.b.db.BeginTx(ctx, nil)
	if err != nil {
		return terminalRecord{}, problem(502, "Cannot register terminal")
	}
	defer tx.Rollback()
	slug, err := h.b.uniqueCompanionSlug(ctx, tx, c.Name)
	if err != nil {
		return terminalRecord{}, problem(502, "Cannot register terminal")
	}
	c.ID = slug
	if err := writeCompanion(ctx, tx, slug, c); err != nil {
		return terminalRecord{}, problem(502, "Cannot register terminal")
	}
	if err := tx.Commit(); err != nil {
		return terminalRecord{}, problem(502, "Cannot register terminal")
	}
	t := terminalRecord{
		ID:          termID,
		Kind:        "hardware",
		CompanionID: slug,
		IP:          ip,
		Port:        port,
		LastSeen:    now.Unix(),
		CreatedAt:   now.UTC().Format(time.RFC3339Nano),
	}
	if err := h.b.terminals.save(t.ID, t, false); err != nil {
		return terminalRecord{}, err
	}
	return t, nil
}

func (h *wsHub) sendSnapshotState(d *wsTerminal) {
	c := d.client
	if d.companionID != "" {
		if st, ok := h.companionStatuses[d.companionID]; ok {
			c.enqueueProto(msgStatus, encodeStatus(st.seq, st.state, st.progress, st.kind, st.text))
		} else {
			c.enqueueProto(msgStatus, encodeStatus(h.nextSeq(), statusIdle, 0, promptNone, ""))
		}
		if owner := h.voiceOwner(d.companionID); owner != nil {
			c.enqueueProto(msgVoiceOwner, encodeVoiceOwner(h.nextSeq(), d.companionID, owner.id))
		}
	}
	h.pushVolume(d, false, true)
	h.pushPower(d, d.brightness(), true)
}

func (h *wsHub) handleHello(c *wsClient, ev terminalHello, now time.Time) bool {
	if ev.protoVersion != protoVersion1 {
		c.enqueueProto(msgReject, encodeReject(rejectBadVersion, "protocol version mismatch"))
		return false
	}
	if ev.kind != termKindHardware {
		c.enqueueProto(msgReject, encodeReject(rejectBadCaps, "unknown terminal kind"))
		return false
	}
	termID := strings.ToLower(strings.TrimSpace(ev.terminalID))
	kindStr := "hardware"
	clientKind := "hardware"
	screen, board := roundScreen, ""
	dispFmt := ev.displayFormats
	if dispFmt == 0 {
		dispFmt = dispRGB565
	}
	frameCodecs := ev.frameCodecs
	if frameCodecs == 0 && dispFmt&dispRGB565 != 0 {
		frameCodecs = codecBitAll
	}
	if !terminalIDRE.MatchString(termID) || webTerminalID.MatchString(termID) {
		c.enqueueProto(msgReject, encodeReject(rejectUnknownTerminal, "invalid hardware terminal ID"))
		return false
	}
	board = "esp-vocat"
	if ev.board != "" {
		board = ev.board
	}
	if board == "esp-box-3" {
		screen = screenGeometry{320, 240, "rect"}
	}
	if ev.width > 0 {
		screen.width = int(ev.width)
	}
	if ev.height > 0 {
		screen.height = int(ev.height)
	}
	if ev.shape == shapeRect {
		screen.shape = "rect"
	} else if ev.shape == shapeRound {
		screen.shape = "round"
	}
	if screen.width < 1 || screen.width > screenSize || screen.height < 1 || screen.height > screenSize ||
		screen.shape != "round" && screen.shape != "rect" || board != "esp-vocat" && board != "esp-box-3" ||
		dispFmt&dispRGB565 == 0 {
		c.enqueueProto(msgReject, encodeReject(rejectBadCaps, "invalid display capabilities"))
		return false
	}
	host, _, _ := net.SplitHostPort(c.remoteAddr().String())
	if addr, err := netip.ParseAddr(host); err == nil {
		host = addr.Unmap().String()
	}
	ip, port := host, 80

	h.b.companionMu.Lock()
	rec, err := h.registerHardwareTerminal(termID, strings.TrimSpace(ev.companionID), ip, port, now)
	h.b.companionMu.Unlock()
	if err != nil {
		reason := rejectBadCaps
		if api, ok := err.(*apiError); ok && api.status == 404 {
			reason = rejectUnknownTerminal
		}
		c.enqueueProto(msgReject, encodeReject(reason, err.Error()))
		return false
	}

	d := &wsTerminal{
		client:         c,
		id:             rec.ID,
		key:            rec.ID,
		kind:           kindStr,
		protoKind:      ev.kind,
		companionID:    rec.CompanionID,
		ip:             rec.IP,
		port:           rec.Port,
		lastSeen:       rec.LastSeen,
		firmware:       shortText(ev.firmware, 24, false),
		screen:         screen,
		displayFormats: dispFmt,
		frameCodecs:    frameCodecs,
		board:          board,
		caps:           helloCapsFromMask(ev.capsMask),
		pct:            -1,
		last:           now,
		moodAt:         now,
		announcing:     true,
		resumeToken:    newResumeToken(),
		hasResumeToken: true,
	}
	if d.companionID != "" {
		d.companion = h.b.companionFor(d.companionID)
	} else {
		d.companion = blankCompanion()
	}
	d.voice = h.b.voices.resolve(d.companion.Voice, d.voiceTheme())
	d.autoVol, d.vol = h.b.volumePrefs(d.key)
	d.telemetry.HasBattery = new(d.caps.Battery)

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || c.ctx.Err() != nil {
		return false
	}
	if old := h.terminals[d.key]; old != nil {
		old.client.cancel()
		h.dropApp(old)
		old.recording.clear()
		h.cancelTurn(old, "reconnect")
		h.stopPlayback(old, true)
		d.rev = old.rev
	}
	h.terminals[d.key], c.terminal, c.kind = d, d, clientKind
	if d.companionID != "" && h.voiceOwners[d.companionID] == "" {
		h.voiceOwners[d.companionID] = d.id
	}
	var wFlags uint8
	if h.bridgeEndpoint(d) {
		wFlags |= 1
	}
	var udpPort uint16
	var ssrc uint32
	if c.native != nil {
		udpPort = c.native.server.udpPort
		ssrc = c.native.ssrc
	}
	snapSeq := h.nextSeq()
	c.enqueueProto(msgWelcome, encodeWelcome(wFlags, udpPort, ssrc, protoMaxPayload, resumeWindowMS, snapSeq, d.resumeToken, d.id, d.companionID))
	h.render(d, now, true)
	return true
}

func (h *wsHub) handleResume(c *wsClient, r terminalResume, now time.Time) bool {
	if r.protoVersion != protoVersion1 {
		c.enqueueProto(msgReject, encodeReject(rejectBadVersion, "protocol version mismatch"))
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || c.ctx.Err() != nil {
		return false
	}
	h.pruneResumes(now)
	entry, ok := h.resumes[r.resumeToken]
	if !ok || !strings.EqualFold(entry.terminalID, strings.TrimSpace(r.terminalID)) {
		c.enqueueProto(msgReject, encodeReject(rejectUnknownTerminal, "resume token expired or unknown"))
		return false
	}
	delete(h.resumes, r.resumeToken)
	rec, exists := h.b.terminalByID(entry.terminalID)
	compID := entry.companionID
	if exists {
		compID = rec.CompanionID
	}
	kindStr := "web"
	clientKind := "web"
	if entry.kind == termKindHardware {
		kindStr = "hardware"
		clientKind = "hardware"
	}
	d := &wsTerminal{
		client:         c,
		id:             entry.terminalID,
		key:            entry.terminalID,
		kind:           kindStr,
		protoKind:      entry.kind,
		companionID:    compID,
		ip:             rec.IP,
		port:           rec.Port,
		lastSeen:       now.Unix(),
		firmware:       entry.firmware,
		screen:         entry.screen,
		displayFormats: entry.displayFormats,
		frameCodecs:    entry.frameCodecs,
		board:          entry.board,
		caps:           entry.caps,
		pct:            -1,
		last:           now,
		moodAt:         now,
		announcing:     true,
		resumeToken:    newResumeToken(),
		hasResumeToken: true,
	}
	if d.companionID != "" {
		d.companion = h.b.companionFor(d.companionID)
	} else {
		d.companion = blankCompanion()
	}
	d.voice = h.b.voices.resolve(d.companion.Voice, d.voiceTheme())
	d.autoVol, d.vol = h.b.volumePrefs(d.key)
	d.telemetry.HasBattery = new(d.caps.Battery)

	if old := h.terminals[d.key]; old != nil {
		old.client.cancel()
		h.dropApp(old)
		old.recording.clear()
		h.cancelTurn(old, "reconnect")
		h.stopPlayback(old, true)
		d.rev = old.rev
	}
	h.terminals[d.key], c.terminal, c.kind = d, d, clientKind
	if d.companionID != "" {
		if entry.hadVoiceOwner {
			h.voiceOwners[d.companionID] = d.id
		} else if h.voiceOwners[d.companionID] == "" {
			h.voiceOwners[d.companionID] = d.id
		}
	}
	var wFlags uint8
	if h.bridgeEndpoint(d) {
		wFlags |= 1
	}
	var udpPort uint16
	var ssrc uint32
	if c.native != nil {
		udpPort = c.native.server.udpPort
		ssrc = c.native.ssrc
	}
	snapSeq := h.nextSeq()
	c.enqueueProto(msgWelcome, encodeWelcome(wFlags, udpPort, ssrc, protoMaxPayload, resumeWindowMS, snapSeq, d.resumeToken, d.id, d.companionID))
	h.render(d, now, true)
	return true
}

func (h *wsHub) announce(d *wsTerminal) {
	d.announcing = false
	h.sendSnapshotState(d)
	h.broadcastPeers()
}

func (d *wsTerminal) status(s wsStatus) {
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
}

func (d *wsTerminal) voiceTheme() string {
	return companionTheme(d.companion.Name)
}

type renderRequest struct {
	view        sceneView
	companion   *companionState
	pose        companionPose
	app         *appInstance
	screen      svgScreen
	grade       gradeKey
	weather     weatherKind
	weatherTick int
	menuHint    bool
	drawer      string
	menuPage    int
	autoVol     bool
	vol         int
	voice       string
	peers       []menuPeer
	statsRows   []menuRow
	readingBook *savedApp
	readingPage int
	full        bool
	listenFrac  float64
	trace       *turnTrace
}

func (h *wsHub) companionPose(d *wsTerminal, now time.Time, mood string) companionPose {
	p := poseFrame(now, d.moodAt, d.last, mood, d.pct, d.vol, d.charging, now.Before(d.meter))
	p.stopTaps = d.stopTaps
	p.canContinue = mood == "thinking" && h.bridgeEndpoint(d) && d.queuedVoice == nil
	return p
}

func (h *wsHub) frameGrade(d *wsTerminal, now time.Time) gradeKey {
	if !h.atmosphere || d.virtual() {
		return gradeDay
	}
	return gradeAt(now, nil)
}

func (h *wsHub) renderCompanion(companionID string, now time.Time, full bool) {
	for _, d := range h.terminalsFor(companionID) {
		h.render(d, now, full)
	}
}

func (h *wsHub) openBook(d *wsTerminal, book savedApp) {
	d.drawer = drawerBookReader
	d.readingBook = &book
	d.readingPage = 0
	if h.b != nil && h.b.db != nil && d.companionID != "" {
		var raw string
		if h.b.db.readDB().QueryRowContext(d.client.ctx, `SELECT value_json FROM memory WHERE scope=? AND key=?`,
			"companion:"+d.companionID, "reading:"+book.Name).Scan(&raw) == nil {
			var prog struct {
				Page int `json:"page"`
			}
			if json.Unmarshal([]byte(raw), &prog) == nil && prog.Page > 0 {
				d.readingPage = prog.Page - 1
			}
		}
	}
	h.render(d, time.Now(), false)
}

func (h *wsHub) render(d *wsTerminal, now time.Time, full bool) {
	switch d.drawer {
	case drawerBooks, drawerGames, drawerMemories, drawerHistory, drawerPair, drawerUnpair:
	default:
		d.menuData = nil
	}
	mood := "idle"
	switch {
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
	pose := h.companionPose(d, now, mood)
	d.pose = pose
	wx := weatherClear
	if h.b != nil && h.b.weather != nil {
		wx = h.b.weather.kind()
	}
	r := renderRequest{
		full:        full || d.renderNext != nil && d.renderNext.full,
		trace:       d.trace,
		grade:       h.frameGrade(d, now),
		weather:     wx,
		weatherTick: d.weatherTick,
		menuHint:    !d.bootHint || now.Before(d.menuHintUntil),
		companion:   new(d.companion),
		pose:        pose,
		listenFrac:  -1,
	}
	if d.recording.active() && d.recording.listen && d.recording.listenSpan > 0 {
		rem := max(0, d.recording.listenUntil.Sub(now))
		r.listenFrac = min(1.0, float64(rem)/float64(d.recording.listenSpan))
	}
	d.grade.Store(uint32(r.grade))
	switch {
	case d.drawer != "" && mood == "idle":
		r.drawer = d.drawer
		r.menuPage = d.menuPage
		r.autoVol = d.autoVol
		r.vol = d.vol
		r.voice = d.voice
		r.readingBook = d.readingBook
		r.readingPage = d.readingPage
		switch d.drawer {
		case drawerBooks, drawerGames, drawerMemories, drawerHistory, drawerPair, drawerUnpair:
			r.view.menu = d.menuData
		case drawerIntercom:
			var choices []menuPeer
			seen := make(map[string]bool)
			for _, other := range h.terminals {
				if other.companionID == "" || other.companionID == d.companionID || !h.active(other) {
					continue
				}
				if seen[other.companionID] {
					continue
				}
				seen[other.companionID] = true
				choices = append(choices, menuPeer{other.companionID, other.companion.Name})
			}
			slices.SortFunc(choices, func(a, b menuPeer) int { return strings.Compare(a.id, b.id) })
			choices = append([]menuPeer{{"all", "Everyone"}}, choices...)
			d.menuPage = clampMenuPage(d.menuPage, len(choices), tilesPerPage)
			r.menuPage = d.menuPage
			r.peers = choices
		case drawerStats:
			d.statsAt = now
			r.statsRows = h.statsRows(d)
		}
	case mood == "listening":
		r.drawer = ""
	case d.card != nil && (mood == "idle" || mood == "speaking" || mood != "sleepy" && d.card.duringTurn):
		r.view = *d.card
	case d.app != nil && mood != "sleepy":
		r.app, r.screen = d.app, d.appView
		r.view.title = d.app.spec.Title
	case d.view != nil && mood != "sleepy":
		r.view = *d.view
	default:
		r.drawer = ""
	}
	if card := r.view.intercom; card != nil {
		r.view = card.scene()
	}
	if r.drawer == "" && r.view.title == "" && r.app == nil && (mood == "thinking" || mood == "speaking") {
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

// A terminal has one frame builder and one replaceable pending request. A dirty
// rectangle is always based on the last frame committed to that terminal's queue.
func (h *wsHub) companionAssets(c *companionState) (*image.RGBA, [4]*image.RGBA, pixelTheme) {
	var bg *image.RGBA
	var poses [4]*image.RGBA
	loaded := false
	if c != nil && c.PortraitID != "" && h != nil && h.b != nil && h.b.portraits != nil {
		dir := filepath.Join(h.b.portraits.dir, c.PortraitID)
		if p, err := loadOrNormalizePoses(dir, pixelPortraitHeight); err == nil {
			poses = p
			loaded = true
		}
		if b, err := loadBackgroundPNG(dir); err == nil {
			bg = b
		}
	}
	if !loaded {
		poses = pixelPlaceholderPoses()
	}
	th := derivePixelTheme(bg, poses[:])
	return bg, poses, th
}

func poseIndexForMood(pose companionPose) int {
	switch pose.mood {
	case "sleepy":
		return 1
	case "speaking":
		if pose.talkFrame%2 == 1 {
			return 2
		}
		return 0
	case "thinking":
		return 3
	default:
		if pose.blinking {
			return 1
		}
		return 0
	}
}

func (h *wsHub) renderFrames(d *wsTerminal) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for d.renderNext != nil && h.active(d) {
		r, gen, prev, rev := *d.renderNext, d.renderGen, d.svg, d.rev+1
		wantRGB := d.displayFormats&dispRGB565 != 0
		codecs := d.frameCodecs
		oldScene, pointerRev := d.scene, d.caps.PointerRev
		geometry := d.screen
		if geometry.width == 0 || geometry.height == 0 {
			geometry = roundScreen
		}
		d.renderNext = nil
		h.mu.Unlock()

		screen := r.screen
		var err error
		if r.app == nil {
			spec := pixelSpecForGeometry(geometry)
			bg, poses, th := h.companionAssets(r.companion)
			var img *image.RGBA
			var hits []hitBox
			switch {
			case r.drawer != "":
				img, hits, r.view.actions, err = h.renderPixelDrawer(spec, r, th)
			case r.view.title != "" || r.view.heading != "" || r.view.text != "" || r.view.building != "":
				img, hits = renderPixelSceneCard(spec, r.view, th)
			default:
				poseIdx := poseIndexForMood(r.pose)
				status := ""
				switch r.pose.mood {
				case "listening", "speaking", "thinking":
					status = r.pose.mood
				}
				bobY := 0
				if r.pose.mood == "idle" && r.pose.bobFrame%2 == 1 {
					bobY = 1
				}
				img = renderPixelHomeListen(spec, bg, poses[poseIdx], r.grade, r.weather, r.weatherTick, status, bobY, "", r.menuHint, r.listenFrac)
				switch {
				case status == "listening" && r.listenFrac >= 0:
					hits = []hitBox{{id: "listen-stop", x1: 0, y1: 0, x2: spec.logicalW*spec.artScale - 1, y2: spec.logicalH*spec.artScale - 1}}
					r.view.actions = map[string]sceneAction{"listen-stop": {"listen-stop", ""}}
				case status == "listening":
					btnH := pixelNavHeight(spec)
					y := spec.safe.Max.Y - btnH
					gap := 4
					bw := (spec.safe.Dx() - gap) / 2
					hits = addLogicalHit(hits, spec, "rec-cancel", image.Rect(spec.safe.Min.X, y, spec.safe.Min.X+bw, y+btnH))
					hits = addLogicalHit(hits, spec, "rec-send", image.Rect(spec.safe.Min.X+bw+gap, y, spec.safe.Max.X, y+btnH))
					r.view.actions = map[string]sceneAction{"rec-cancel": {"rec-cancel", ""}, "rec-send": {"rec-send", ""}}
				case status == "thinking" || status == "speaking":
					btnW, btnH := 42, pixelNavHeight(spec)
					if !spec.round {
						btnW = 48
					}
					btn := image.Rect((spec.logicalW-btnW)/2, spec.safe.Max.Y-btnH, (spec.logicalW+btnW)/2, spec.safe.Max.Y)
					hits = addLogicalHit(hits, spec, "stop", btn)
					r.view.actions = map[string]sceneAction{"stop": {"stop", ""}}
				default:
					hits = []hitBox{{id: "listen", x1: 0, y1: 0, x2: spec.logicalW*spec.artScale - 1, y2: spec.logicalH*spec.artScale - 1}}
					r.view.actions = map[string]sceneAction{"listen": {"listen", ""}}
				}
			}
			if err == nil && img != nil {
				screen, err = rgbaToSVGScreenCompat(img, hits)
				screen.grade = r.grade
				var srcParts []string
				if r.drawer != "" {
					srcParts = append(srcParts, r.drawer)
					if r.drawer == drawerMenu {
						for _, it := range pixelHomeMenuItems {
							srcParts = append(srcParts, it.label)
						}
					}
					if r.drawer == drawerMore {
						for _, it := range pixelMoreMenuItems {
							srcParts = append(srcParts, it.label)
						}
					}
					if r.drawer == drawerVoice {
						for _, v := range voiceCatalog {
							srcParts = append(srcParts, v.Name)
						}
					}
					if r.drawer == drawerStats {
						for _, row := range r.statsRows {
							srcParts = append(srcParts, row.title, row.text)
						}
					}
					if r.drawer == drawerVolume {
						mode := "Manual"
						if r.autoVol {
							mode = "Auto"
						}
						srcParts = append(srcParts, fmt.Sprintf("%s · %d%%", mode, r.vol))
					}
					if r.view.menu != nil {
						if r.view.menu.detail != nil || r.view.menu.memoryDetail != nil {
							srcParts = append(srcParts, "menu-detail-view")
						}
						if r.view.menu.notice != "" {
							srcParts = append(srcParts, r.view.menu.notice)
						}
						for _, g := range r.view.menu.games {
							srcParts = append(srcParts, g.label)
						}
						for _, g := range r.view.menu.pixelGames {
							srcParts = append(srcParts, g.Title)
						}
						for _, bk := range r.view.menu.books {
							srcParts = append(srcParts, bk.Title)
						}
						for _, row := range r.view.menu.rows {
							srcParts = append(srcParts, row.title, row.text)
						}
					}
				} else if r.view.building != "" {
					srcParts = append(srcParts, "Cancel build")
				} else if r.view.title == "" && r.view.heading == "" && r.view.text == "" {
					srcParts = append(srcParts, "home")
				}
				if r.view.title != "" {
					srcParts = append(srcParts, r.view.title)
				}
				if r.view.heading != "" {
					srcParts = append(srcParts, r.view.heading)
				}
				if r.view.text != "" {
					srcParts = append(srcParts, r.view.text)
				}
				for _, hb := range hits {
					srcParts = append(srcParts, fmt.Sprintf(`<g data-id=%q data-lark-hit="%d %d %d %d"/>`, hb.id, hb.x1, hb.y1, hb.x2, hb.y2))
				}
				screen.source = strings.Join(srcParts, " ")
			}
		}

		var native wsFrame
		var rawBytes int
		fbChanged := !slices.Equal(prev.fb, screen.fb)
		interactionChanged := pointerRev && wantRGB &&
			(!maps.Equal(r.view.actions, oldScene.actions) || !slices.Equal(screen.hits, prev.hits))
		if err == nil && (r.full || fbChanged || screen.grade != prev.grade || interactionChanged) {
			start := time.Now()
			if screen.fb != nil && wantRGB {
				rect := dirtyRect(prev.fb, screen.fb, geometry)
				if rect.Empty() && (interactionChanged || r.full) {
					rect = image.Rect(0, 0, 1, 1)
				}
				if !rect.Empty() {
					native = d.encoder.encodeWithCodecs(screen.fb, nil, rect, rev, geometry.width, codecs)
					rawBytes = 12 + rect.Dx()*rect.Dy()*2
				}
			}
			screen.render += time.Since(start)
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
		h.sig.notify()
		if native.data != nil {
			d.rev = rev
			d.client.enqueueProto(msgFrame, native.data)
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
		if d.announcing {
			h.announce(d)
		}
	}
	d.rendering, d.renderNext = false, nil
}

func (h *wsHub) wake(d *wsTerminal, now time.Time) {
	d.last = now
	h.setSleeping(d, false)
}

func (h *wsHub) setSleeping(d *wsTerminal, asleep bool) {
	if d.asleep != asleep {
		d.asleep = asleep
		h.pushPower(d, d.brightness(), true)
	}
}

func (d *wsTerminal) brightness() int {
	if d.drawer == drawerPair {
		return 100
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
			for _, d := range h.terminals {
				if !h.active(d) {
					continue
				}
				if checkGrade && !d.virtual() && !(d.asleep && d.caps.PanelSleep) &&
					d.svg.grade != h.frameGrade(d, now) {
					h.render(d, now, false)
				}
				switch {
				case d.recording.active() && !d.recording.listen && d.recording.stopTimer == nil && now.Sub(d.recording.started) > 90*time.Second:
					h.stopRecordingAtLimit(d, now)
				case d.awaitAck && !d.ttsStarted && now.After(d.audioUntil.Add(4*time.Second)):
					h.playbackDone(d, now, false)
				case !d.asleep && d.drawer != drawerPair && !d.busy && !d.recording.active() && !d.ttsStarted && !d.awaitAck && now.Sub(d.last) >= idleSleep:
					h.setSleeping(d, true)
					h.render(d, now, false)
				case d.asleep && d.caps.PanelSleep && !d.virtual():
					// The battery board's panel is off; skip idle animation traffic.
				case d.drawer == "stats" && !d.asleep && now.Sub(d.statsAt) >= diagInterval:
					h.render(d, now, false)
				case d.stopTapsExpired(now):
					h.render(d, now, false)
				case (d.drawer == "" || d.asleep) && (d.app == nil && d.view == nil || d.asleep || d.recording.active()):
					if d.recording.active() && d.recording.listen && !d.asleep && d.drawer == "" && now.Sub(d.listenRenderAt) >= time.Second {
						d.listenRenderAt = now
						h.render(d, now, false)
					} else if !d.asleep && d.drawer == "" && h.b != nil && h.b.weather != nil && h.b.weather.kind() != weatherClear && now.Sub(d.weatherAt) >= 400*time.Millisecond {
						d.weatherAt = now
						d.weatherTick++
						h.render(d, now, false)
					} else if h.companionPose(d, now, d.pose.mood) != d.pose {
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

func (d *wsTerminal) volumeFrame(persist bool) object {
	return object{"op": "volume", "terminal_id": d.id, "companion_id": d.companionID, "level": d.vol, "auto_volume": d.autoVol, "persist": persist}
}

func (h *wsHub) saveVoice(id, voice string) error {
	h.b.companionMu.Lock()
	defer h.b.companionMu.Unlock()
	if err := h.b.voices.save(id, voice); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	for _, d := range h.terminalsFor(id) {
		d.voice = voice
		d.companion.Voice = voice
		if d.drawer != "" {
			h.render(d, now, false)
		}
	}
	return nil
}

// Takes and returns with h.mu held. Serialize writes without blocking the hub.
func (h *wsHub) setVolume(ctx context.Context, d *wsTerminal, auto bool, level int, action string) error {
	h.mu.Unlock()
	h.settingsMu.Lock()
	defer h.settingsMu.Unlock()
	h.mu.Lock()
	if !h.active(d) {
		return problem(409, "Terminal is not connected")
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
