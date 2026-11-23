package main

import (
	"context"
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
	"time"
)

const idleSleep = 120 * time.Second

type terminalCaps struct {
	PanelSleep     bool `json:"panel_sleep,omitempty"`
	PointerRev     bool `json:"pointer_rev,omitempty"`
	BridgeEndpoint bool `json:"bridge_endpoint,omitempty"`
	Battery        bool `json:"battery"`
	Drag           bool `json:"drag"`
}

type terminalTelemetry struct {
	Battery    *int     `json:"battery_pct"`
	Charge     *bool    `json:"charging"`
	WiFi       *int     `json:"wifi_rssi"`
	HasBattery *bool    `json:"has_battery"`
	Volume     *int     `json:"volume"`
	FreeHeap   *uint64  `json:"free_heap"`
	Uptime     *uint64  `json:"uptime_s"`
	CPU        *float64 `json:"cpu_pct"`
}

type terminalSession struct {
	client         *terminalClient
	id             string
	key            string
	kind           string // "hardware"
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
	voice          string
	announcing     bool
	companion      companionState
	vol            int
	autoVol        bool
	ambientEMA     float64
	hasAmbient     bool
	pct            int
	charging       bool
	telemetry      terminalTelemetry
	frame          *diagFrame
	svg            svgScreen
	scratchFB      []uint16
	encoder        frameEncoder
	renderGen      uint64
	rendering      bool
	renderNext     *renderRequest

	scene         sceneView
	view          *sceneView
	pending       *sceneView
	card          *sceneView
	drawer        string
	menuPage      int
	homePage      int
	menuData      *menuContent
	story         *storySession
	storyPage     int
	statsAt       time.Time
	rev           uint64
	nativeRev     uint16
	actionRev     uint16
	hasActionRev  bool
	actionHitsKey string
	pose          companionPose
	moodAt        time.Time
	last          time.Time
	meter         time.Time
	asleep        bool
	reply         *turnReply // Delta Chat read-out awaiting an in-person reply
	replyUntil    time.Time

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

func (c *terminalClient) pingRTT() *float64 {
	c.pingMu.Lock()
	defer c.pingMu.Unlock()
	return c.rtt
}

func (h *terminalHub) peerSnapshot(sender string) ([]companionTarget, error) {
	return h.b.companionTargets(sender)
}

func (b *larkServer) peerSnapshot(sender string) ([]companionTarget, error) {
	return b.companionTargets(sender)
}

func (h *terminalHub) broadcastPeers() {
	for _, d := range h.terminals {
		if d.drawer == "intercom" || d.card.hasIntercom() || d.view.hasIntercom() {
			h.render(d, time.Now(), false)
		}
	}
}

// Called under the hub lock, after committing the companion changes.
func (h *terminalHub) companionChanged(id string, companion companionState) {
	h.publishCompanion(id, companion, true)
}

func (h *terminalHub) publishCompanion(id string, companion companionState, clearView bool) {
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

func (h *terminalHub) registerHardwareTerminal(termID, hintName, ip string, port int, now time.Time) (terminalRecord, error) {
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

func (h *terminalHub) sendSnapshotState(d *terminalSession) {
	c := d.client
	if d.companionID != "" {
		if st, ok := h.companionStatuses[d.companionID]; ok {
			c.enqueueProto(msgStatus, encodeStatus(st.state))
		} else {
			c.enqueueProto(msgStatus, encodeStatus(statusIdle))
		}
		if owner := h.voiceOwner(d.companionID); owner != nil {
			c.enqueueProto(msgVoiceOwner, encodeVoiceOwner(d.companionID, owner.id))
		}
	}
	h.pushVolume(d, false, true)
	h.pushPower(d, d.brightness(), true)
}

func (h *terminalHub) handleHello(c *terminalClient, ev terminalHello, now time.Time) bool {
	if ev.protoVersion != protoVersion1 {
		c.enqueueProto(msgReject, encodeReject(rejectBadVersion, "protocol version mismatch"))
		return false
	}
	if ev.kind != termKindHardware {
		c.enqueueProto(msgReject, encodeReject(rejectBadCaps, "unknown terminal kind"))
		return false
	}
	termID := strings.ToLower(strings.TrimSpace(ev.terminalID))
	screen, board := roundScreen, ""
	dispFmt := ev.displayFormats
	if dispFmt == 0 {
		dispFmt = dispRGB565
	}
	frameCodecs := ev.frameCodecs
	if frameCodecs == 0 && dispFmt&dispRGB565 != 0 {
		frameCodecs = codecBitAll
	}
	if !terminalIDRE.MatchString(termID) {
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

	d := &terminalSession{
		client:         c,
		id:             rec.ID,
		key:            rec.ID,
		kind:           "hardware",
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
		old.recording.clear()
		h.cancelTurn(old, "reconnect")
		h.stopPlayback(old, true)
		d.rev = old.rev
	}
	h.terminals[d.key], c.terminal = d, d
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
	c.enqueueProto(msgWelcome, encodeWelcome(wFlags, udpPort, ssrc, protoMaxPayload, resumeWindowMS, d.resumeToken, d.id, d.companionID))
	h.render(d, now, true)
	if d.companionID != "" && h.b.oc != nil {
		go h.b.oc.prewarm(c.ctx, d.companionID, d.companion.Name)
	}
	return true
}

func (h *terminalHub) handleResume(c *terminalClient, r terminalResume, now time.Time) bool {
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
	d := &terminalSession{
		client:         c,
		id:             entry.terminalID,
		key:            entry.terminalID,
		kind:           "hardware",
		protoKind:      termKindHardware,
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
		old.recording.clear()
		h.cancelTurn(old, "reconnect")
		h.stopPlayback(old, true)
		d.rev = old.rev
	}
	h.terminals[d.key], c.terminal = d, d
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
	c.enqueueProto(msgWelcome, encodeWelcome(wFlags, udpPort, ssrc, protoMaxPayload, resumeWindowMS, d.resumeToken, d.id, d.companionID))
	h.render(d, now, true)
	if d.companionID != "" && h.b.oc != nil {
		go h.b.oc.prewarm(c.ctx, d.companionID, d.companion.Name)
	}
	return true
}

// pendingReply returns the armed read-out context, dropping it once its TTL
// expires so a later in-person turn cannot answer an old Delta Chat message.
func (d *terminalSession) pendingReply(now time.Time) *turnReply {
	if d.reply == nil {
		return nil
	}
	if now.Before(d.replyUntil) {
		return d.reply
	}
	d.reply, d.replyUntil = nil, time.Time{}
	return nil
}

func (h *terminalHub) announce(d *terminalSession) {
	d.announcing = false
	h.sendSnapshotState(d)
	h.broadcastPeers()
}

func (d *terminalSession) status(s terminalTelemetry) {
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
	if s.Uptime != nil {
		t.Uptime = s.Uptime
	}
	if s.CPU != nil && !math.IsNaN(*s.CPU) {
		t.CPU = new(min(100., max(0., *s.CPU)))
	}
}

func (d *terminalSession) voiceTheme() string {
	return companionTheme(d.companion.Name)
}

type renderRequest struct {
	view         sceneView
	companion    *companionState
	pose         companionPose
	screen       svgScreen
	grade        gradeKey
	drawer       string
	menuPage     int
	autoVol      bool
	vol          int
	voice        string
	peers        []menuPeer
	statsRows    []menuRow
	storyTitle   string
	storyText    string
	storyChoices []storyChoice
	storyEnd     bool
	storyBG      *image.RGBA
	storyPage    int
	full         bool
	listenWindow bool
	trace        *turnTrace
}

func (h *terminalHub) companionPose(d *terminalSession, now time.Time, mood string) companionPose {
	p := poseFrame(now, d.moodAt, d.last, mood, d.pct, d.vol, d.charging, now.Before(d.meter))
	p.stopTaps = d.stopTaps
	p.canContinue = mood == "thinking" && h.bridgeEndpoint(d) && d.queuedVoice == nil
	return p
}

func (h *terminalHub) frameGrade(d *terminalSession, now time.Time) gradeKey {
	if !h.atmosphere {
		return gradeDay
	}
	return gradeAt(now, nil)
}

func (h *terminalHub) renderCompanion(companionID string, now time.Time, full bool) {
	for _, d := range h.terminalsFor(companionID) {
		h.render(d, now, full)
	}
}

// openStory opens the reader at saved progress. Menu and panel entry points
// are silent; narration belongs to conversational story turns.
func (h *terminalHub) openStory(d *terminalSession, st savedStory, narrate bool) {
	cs, err := parseTwee(st.Twee)
	if err != nil {
		return
	}
	sess := &storySession{
		id:         st.ID,
		title:      cs.Title,
		compiled:   cs,
		passageIdx: -1,
		flags:      cs.flagsSlice(nil),
	}
	page := 0
	entered := false
	if h.b != nil && h.b.db != nil && d.companionID != "" {
		if prog, ok, perr := h.b.getStoryProgress(d.client.ctx, d.companionID, st.ID); perr == nil && ok {
			if idx, exists := cs.ByName[prog.Passage]; exists {
				sess.flags = cs.flagsSlice(prog.Flags)
				sess.enterPassage(idx, false)
				page = prog.Page
				entered = true
			}
		}
	}
	if !entered {
		sess.enterPassage(cs.Start, true)
	}
	if sess.bg != "" && h.b != nil {
		if img := h.b.loadStoryBG(st.ID, sess.currentPassageName(), sess.bg); img != nil {
			sess.bgImg = img
		} else {
			h.b.ensureStoryBGAsync(d, st.ID, sess.currentPassageName(), sess.bg)
		}
	}
	d.drawer = drawerStoryReader
	d.story = sess
	d.storyPage = page
	now := time.Now()
	h.stopPlayback(d, true)
	if narrate && sess.text != "" {
		h.speakLocal(d, sess.text)
	}
	h.render(d, now, false)
}

func (h *terminalHub) advanceStoryPassage(d *terminalSession, targetIdx int, narrate bool) {
	if d.story == nil || d.story.compiled == nil {
		return
	}
	sess := d.story
	sess.enterPassage(targetIdx, true)
	d.storyPage = 0
	if sess.bg != "" && h.b != nil {
		if img := h.b.loadStoryBG(sess.id, sess.currentPassageName(), sess.bg); img != nil {
			sess.bgImg = img
		} else {
			h.b.ensureStoryBGAsync(d, sess.id, sess.currentPassageName(), sess.bg)
		}
	}
	if h.b != nil && h.b.db != nil && d.companionID != "" {
		prog := storyProgress{
			CompanionID: d.companionID,
			StoryID:     sess.id,
			Passage:     sess.currentPassageName(),
			Page:        0,
			Flags:       sess.compiled.flagsMap(sess.flags),
		}
		go func() {
			_ = h.b.setStoryProgress(context.Background(), prog)
		}()
	}
	h.stopPlayback(d, true)
	if narrate && sess.text != "" {
		h.speakLocal(d, sess.text)
	}
	h.render(d, time.Now(), false)
}

func (h *terminalHub) render(d *terminalSession, now time.Time, full bool) {
	switch d.drawer {
	case drawerStories, drawerMemories, drawerPair, drawerUnpair:
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
	r := renderRequest{
		full:         full || d.renderNext != nil && d.renderNext.full,
		trace:        d.trace,
		grade:        h.frameGrade(d, now),
		companion:    new(d.companion),
		pose:         pose,
		listenWindow: d.recording.active() && d.recording.listen,
	}
	switch {
	case d.drawer != "" && (mood == "idle" || (d.drawer == drawerStoryReader && (mood == "speaking" || d.recording.listen))):
		r.drawer = d.drawer
		r.menuPage = d.menuPage
		r.autoVol = d.autoVol
		r.vol = d.vol
		r.voice = d.voice
		if d.story != nil {
			r.storyTitle = d.story.title
			r.storyText = d.story.text
			r.storyChoices = slices.Clone(d.story.choices)
			r.storyEnd = d.story.end
			r.storyBG = d.story.bgImg
		}
		r.storyPage = d.storyPage
		switch d.drawer {
		case drawerStories, drawerMemories, drawerPair, drawerUnpair:
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
			perPage := 2
			if d.screen.shape == "rect" {
				perPage = 3
			}
			d.menuPage = clampMenuPage(d.menuPage, len(choices), perPage)
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
	case d.view != nil && mood != "sleepy":
		r.view = *d.view
	default:
		r.drawer = ""
	}
	if card := r.view.intercom; card != nil {
		r.view = card.scene()
	}
	if r.drawer == "" && r.view.title == "" && (mood == "thinking" || mood == "speaking") {
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
func (h *terminalHub) companionAssets(c *companionState) (*image.RGBA, [4]*image.RGBA, pixelTheme) {
	if c != nil && c.PortraitID != "" && h != nil && h.b != nil && h.b.portraits != nil {
		h.assetsMu.Lock()
		h.assetsClock++
		clock := h.assetsClock
		if cached, ok := h.assets[c.PortraitID]; ok {
			cached.used = clock
			h.assets[c.PortraitID] = cached
			h.assetsMu.Unlock()
			return cached.bg, cached.poses, cached.theme
		}
		h.assetsMu.Unlock()
		dir := filepath.Join(h.b.portraits.dir, c.PortraitID)
		var bg *image.RGBA
		var poses [4]*image.RGBA
		loaded := false
		if p, err := loadOrNormalizePoses(dir, pixelPortraitHeight); err == nil {
			poses = p
			loaded = true
		}
		if b, err := loadBackgroundPNG(dir); err == nil {
			bg = b
		}
		if !loaded {
			poses = pixelPlaceholderPoses()
		}
		th := derivePixelTheme(bg, poses[:])
		if loaded {
			h.assetsMu.Lock()
			if h.assets == nil {
				h.assets = make(map[string]cachedAssets)
			}
			h.assets[c.PortraitID] = cachedAssets{bg: bg, poses: poses, theme: th, used: clock}
			h.evictAssetsLocked()
			h.assetsMu.Unlock()
		}
		return bg, poses, th
	}
	poses := pixelPlaceholderPoses()
	return nil, poses, placeholderPixelTheme()
}

// invalidateAssets drops the decoded background and poses for a portrait so
// the next render reloads them from disk.
func (h *terminalHub) invalidateAssets(portraitID string) {
	if h == nil || portraitID == "" {
		return
	}
	h.assetsMu.Lock()
	delete(h.assets, portraitID)
	h.assetsMu.Unlock()
}

func (h *terminalHub) evictAssetsLocked() {
	for len(h.assets) > assetsCacheLimit {
		var oldest string
		var oldestUsed uint64
		for id, a := range h.assets {
			if oldest == "" || a.used < oldestUsed {
				oldest, oldestUsed = id, a.used
			}
		}
		delete(h.assets, oldest)
	}
}

func poseIndexForMood(pose companionPose) int {
	switch pose.mood {
	case "sleepy":
		return 1
	case "speaking":
		return 2
	case "thinking":
		return 3
	default:
		return 0
	}
}

func (h *terminalHub) renderFrames(d *terminalSession) {
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
		if true {
			spec := pixelSpecForGeometry(geometry)
			bg, poses, th := h.companionAssets(r.companion)
			var img *image.RGBA
			var hits []hitBox
			switch {
			case r.drawer != "":
				img, hits, r.view.actions, err = h.renderPixelDrawer(spec, r, poses[poseIndexForMood(r.pose)], th)
			case r.view.title != "" || r.view.heading != "" || r.view.text != "":
				img, hits = renderPixelSceneCard(spec, r.view, th)
			default:
				poseIdx := poseIndexForMood(r.pose)
				status := ""
				switch r.pose.mood {
				case "listening", "speaking", "thinking":
					status = r.pose.mood
				}
				img = renderPixelHomeListen(spec, bg, poses[poseIdx], r.grade, status, "", r.listenWindow)
				switch {
				case status == "listening" && r.listenWindow:
					hits = []hitBox{{id: "listen-stop", label: "Stop", x1: 0, y1: 0, x2: spec.logicalW*spec.artScale - 1, y2: spec.logicalH*spec.artScale - 1}}
					r.view.actions = map[string]sceneAction{"listen-stop": {"listen-stop", ""}}
				case status == "listening":
					btnH := pixelNavHeight(spec)
					y := spec.safe.Max.Y - btnH
					gap := 4
					bw := (spec.safe.Dx() - gap) / 2
					hits = addLabeledHit(hits, spec, "rec-cancel", "Cancel", image.Rect(spec.safe.Min.X, y, spec.safe.Min.X+bw, y+btnH))
					hits = addLabeledHit(hits, spec, "rec-send", "Send", image.Rect(spec.safe.Min.X+bw+gap, y, spec.safe.Max.X, y+btnH))
					r.view.actions = map[string]sceneAction{"rec-cancel": {"rec-cancel", ""}, "rec-send": {"rec-send", ""}}
				case status == "thinking" || status == "speaking":
					btnW, btnH := 42, pixelNavHeight(spec)
					if !spec.round {
						btnW = 48
					}
					btn := image.Rect((spec.logicalW-btnW)/2, spec.safe.Max.Y-btnH, (spec.logicalW+btnW)/2, spec.safe.Max.Y)
					hits = addLabeledHit(hits, spec, "stop", "Stop", btn)
					r.view.actions = map[string]sceneAction{"stop": {"stop", ""}}
				default:
					hits = []hitBox{{id: "listen", label: "Listen", x1: 0, y1: 0, x2: spec.logicalW*spec.artScale - 1, y2: spec.logicalH*spec.artScale - 1}}
					if status == "" {
						hits = addLabeledHit(hits, spec, "menu", "Menu", pixelHomeMenuButtonRect(spec))
					}
					r.view.actions = map[string]sceneAction{"listen": {"listen", ""}, "menu": {"menu", ""}}
				}
			}
			if err == nil && img != nil {
				b := img.Bounds()
				w, h := b.Dx(), b.Dy()
				var fb []uint16
				if cap(d.scratchFB) >= w*h {
					fb = d.scratchFB[:w*h]
				} else {
					fb = make([]uint16, w*h)
				}
				for y := range h {
					for x := range w {
						c := img.RGBAAt(b.Min.X+x, b.Min.Y+y)
						fb[y*w+x] = pack565(c.R, c.G, c.B)
					}
				}
				screen = svgScreen{fb: fb, hits: hits}
				screen.grade = r.grade
				releasePixelImage(img)
				var srcParts []string
				if debugScreenText {
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
							for _, st := range r.view.menu.stories {
								srcParts = append(srcParts, st.Title)
							}
							for _, row := range r.view.menu.rows {
								srcParts = append(srcParts, row.title, row.text)
							}
						}
						if r.drawer == drawerStoryReader {
							srcParts = append(srcParts, r.storyTitle, r.storyText)
							for _, ch := range r.storyChoices {
								srcParts = append(srcParts, ch.Label)
							}
						}
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
		}

		var native protocolFrame
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
					frameCodecs := codecs
					fullScene := prev.fb == nil || r.full || screen.grade != prev.grade
					if !fullScene || r.drawer != "" || r.view.title != "" || r.view.heading != "" || r.view.text != "" {
						frameCodecs &^= codecBitJPEG
					}
					native = d.encoder.encodeWithCodecs(screen.fb, nil, rect, rev, geometry.width, frameCodecs)
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
		if prev.fb != nil && len(prev.fb) == d.screen.width*d.screen.height {
			d.scratchFB = prev.fb
		}
		d.scene, d.svg = r.view, screen
		h.sig.notify()
		if native.data != nil {
			d.rev = rev
			d.client.enqueueProto(msgFrame, native.data)
			d.nativeRev = uint16(rev)
			hk := hitsLayoutKey(screen.hits)
			if !d.hasActionRev || hk != d.actionHitsKey {
				d.actionHitsKey = hk
				d.actionRev = d.nativeRev
				d.hasActionRev = true
			}
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

func (h *terminalHub) wake(d *terminalSession, now time.Time) {
	d.last = now
	h.setSleeping(d, false)
}

func (h *terminalHub) setSleeping(d *terminalSession, asleep bool) {
	if d.asleep != asleep {
		d.asleep = asleep
		h.pushPower(d, d.brightness(), true)
	}
}

func (d *terminalSession) brightness() int {
	if d.drawer == drawerPair {
		return 100
	}
	if d.asleep {
		return 12
	}
	return 85
}

func (h *terminalHub) animate(ctx context.Context) {
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
				nextGrade = now.Truncate(30 * time.Minute).Add(30 * time.Minute)
			}
			for _, d := range h.terminals {
				if !h.active(d) {
					continue
				}
				turnActive := d.recording.active() || d.busy || d.ttsStarted || d.awaitAck
				if checkGrade && !turnActive && !(d.asleep && d.caps.PanelSleep) &&
					d.svg.grade != h.frameGrade(d, now) {
					h.render(d, now, false)
				}
				switch {
				case d.recording.active() && !d.recording.listen && d.recording.stopTimer == nil && now.Sub(d.recording.started) > 90*time.Second:
					h.stopRecordingAtLimit(d, now)
				case d.awaitAck && !d.ttsStarted && now.After(d.audioUntil.Add(4*time.Second)):
					h.playbackDone(d, now, false)
				case !d.asleep && d.drawer != drawerPair && !turnActive && now.Sub(d.last) >= idleSleep:
					h.setSleeping(d, true)
					h.render(d, now, false)
				case d.asleep && d.caps.PanelSleep:
					// The battery board's panel is off; skip idle updates.
				case d.drawer == "stats" && !d.asleep && !turnActive && now.Sub(d.statsAt) >= diagInterval:
					h.render(d, now, false)
				case d.stopTapsExpired(now):
					h.render(d, now, false)
				}
			}
			if now.Sub(sincePeers) > 15*time.Second {
				h.broadcastPeers()
				sincePeers = now
			}
			h.mu.Unlock()
		}
	}
}

func (d *terminalSession) volumeFrame(persist bool) object {
	return object{"op": "volume", "terminal_id": d.id, "companion_id": d.companionID, "level": d.vol, "auto_volume": d.autoVol, "persist": persist}
}

func (h *terminalHub) companionTerminals(companionID string) []object {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []object
	for _, d := range h.terminalsFor(companionID) {
		entry := object{
			"id":     d.id,
			"name":   d.id,
			"online": true,
			"volume": d.vol,
		}
		if d.caps.Battery && d.pct >= 0 {
			entry["battery_pct"] = d.pct
			entry["charging"] = d.charging
		}
		out = append(out, entry)
	}
	return out
}

func (b *larkServer) setCompanionTerminalVolume(ctx context.Context, companionID, terminalID string, level int) error {
	if b.hub == nil {
		return problem(503, "Terminal hub unavailable")
	}
	h := b.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.terminals[strings.ToLower(terminalID)]
	if d == nil || !h.active(d) || d.companionID != companionID {
		return problem(404, "Terminal not attached or offline")
	}
	return h.setVolume(ctx, d, false, level, "")
}

func (h *terminalHub) saveVoice(id, voice string) error {
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
func (h *terminalHub) setVolume(ctx context.Context, d *terminalSession, auto bool, level int, action string) error {
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
