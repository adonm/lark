package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"log"
	"maps"
	"math"
	"net"
	"net/netip"
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
	HeadPads       bool `json:"head_pads"`
	Call           bool `json:"call"`
	CallPCM        bool `json:"call_pcm,omitempty"`
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
	CallTXDrops    *uint32  `json:"call_tx_drops,omitempty"`
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
	svgWire     []byte
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
	callRTPSeq                  uint16
	callRTPTS                   uint32

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

func (h *wsHub) peerPayload(scoped bool, seq uint32) []byte {
	online := h.onlineTerminals()
	list := make([]peerDisplay, 0, len(h.storedPeers)+len(online))
	if !scoped {
		for _, p := range h.storedPeers {
			if _, ok := online[strings.ToLower(p.id)]; !ok {
				list = append(list, p)
			}
		}
	}
	for _, p := range online {
		list = append(list, p)
	}
	slices.SortFunc(list, func(a, b peerDisplay) int { return strings.Compare(a.id, b.id) })
	compOnline := make(map[string]bool)
	compCaps := make(map[string]terminalCaps)
	for _, p := range list {
		if p.online && p.companionID != "" {
			compOnline[p.companionID] = true
			if p.caps.Call {
				c := compCaps[p.companionID]
				c.Call = true
				compCaps[p.companionID] = c
			}
		}
	}
	b := make([]byte, 0, 256)
	b = binary.LittleEndian.AppendUint32(b, seq)
	var sc byte
	if scoped {
		sc = 1
	}
	b = append(b, sc)
	b = binary.LittleEndian.AppendUint16(b, uint16(len(h.storedCompanions)))
	for _, c := range h.storedCompanions {
		b = appendStr8(b, c.ID)
		b = appendStr8(b, c.Name)
		var cfg, on, callCap byte
		if c.Configured {
			cfg = 1
		}
		if compOnline[c.ID] {
			on = 1
		}
		if compCaps[c.ID].Call {
			callCap = 1
		}
		b = append(b, cfg)
		b = appendStr8(b, c.FaceBG)
		b = appendStr8(b, c.MarkColor)
		b = appendStr8(b, c.AccentColor)
		if scoped {
			b = appendStr16(b, "")
			b = appendStr8(b, "")
		} else {
			b = appendStr16(b, c.Personality)
			b = appendStr8(b, h.b.voices.resolve(c.Voice, companionTheme(c.Name)))
		}
		b = append(b, on, callCap)
	}
	if scoped {
		b = binary.LittleEndian.AppendUint16(b, 0)
		return b
	}
	b = binary.LittleEndian.AppendUint16(b, uint16(len(list)))
	for _, p := range list {
		b = appendStr8(b, p.id)
		var kind, on, auto byte
		if p.kind == "web" {
			kind = 1
		}
		if p.online {
			on = 1
		}
		if p.auto {
			auto = 1
		}
		b = append(b, kind)
		b = appendStr8(b, p.companionID)
		b = append(b, on, auto)
		if p.kind == "hardware" {
			b = appendStr8(b, p.ip)
			b = binary.LittleEndian.AppendUint16(b, uint16(p.port))
			b = binary.LittleEndian.AppendUint64(b, uint64(p.lastSeen))
		} else {
			b = appendStr8(b, "")
			b = binary.LittleEndian.AppendUint16(b, 0)
			b = binary.LittleEndian.AppendUint64(b, 0)
		}
		if p.online {
			b = appendStr8(b, p.firmware)
			b = binary.LittleEndian.AppendUint32(b, capsMaskFromCaps(p.caps))
		} else {
			b = appendStr8(b, "")
			b = binary.LittleEndian.AppendUint32(b, 0)
		}
	}
	return b
}

func (h *wsHub) broadcastPeers() {
	h.broadcastPeerFrames()
	for c := range h.clients {
		if (c.kind == "web" || c.kind == "watcher") && c.scope == nil {
			h.refreshNeeded = true
			if !h.refreshing {
				h.refreshing = true
				go h.refreshStoredPeers()
			}
			return
		}
	}
}

func (c *wsClient) pushPeers(pkt []byte) {
	if !bytes.Equal(c.lastPeers, pkt) {
		c.lastPeers = pkt
		c.enqueueProto(msgPeers, pkt)
	}
}

func (h *wsHub) refreshStoredPeers() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for h.refreshNeeded && !h.closed {
		h.refreshNeeded = false
		h.mu.Unlock()
		terms, err := h.b.terminalSnapshot()
		comps, compErr := h.b.allCompanions(context.Background())
		stored := make([]peerDisplay, 0, len(terms))
		for _, t := range terms {
			auto, _ := h.b.volumePrefs(t.ID)
			stored = append(stored, peerDisplay{
				id:          t.ID,
				kind:        t.Kind,
				companionID: t.CompanionID,
				ip:          t.IP,
				port:        t.Port,
				lastSeen:    t.LastSeen,
				auto:        auto,
			})
		}
		h.mu.Lock()
		if err == nil && compErr == nil && !h.closed && !h.refreshNeeded {
			h.storedPeers = stored
			h.storedCompanions = comps
			h.broadcastPeerFrames()
		}
	}
	h.refreshing = false
}

func (h *wsHub) broadcastPeerFrames() {
	if len(h.clients) == 0 {
		h.peersKey = ""
		return
	}
	f := h.peerPayload(false, 0)
	// Compare ignoring the 4-byte seq prefix so we only bump seq on actual changes.
	if string(f[4:]) != h.peersKey {
		h.peersKey = string(f[4:])
		seq := h.nextSeq()
		binary.LittleEndian.PutUint32(f[0:4], seq)
		var scoped []byte
		for c := range h.clients {
			switch {
			case (c.kind != "web" && c.kind != "watcher") || c.terminal != nil && c.terminal.announcing:
			case c.scope == nil:
				c.pushPeers(f)
			default:
				if scoped == nil {
					scoped = h.peerPayload(true, seq)
				}
				c.pushPeers(scoped)
			}
		}
		for _, d := range h.terminals {
			if d.drawer == "intercom" || d.drawer == "call" || d.card.hasIntercom() || d.view.hasIntercom() {
				h.render(d, time.Now(), false)
			}
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
		h.render(d, now, true)
	}
	pkt := encodeCompanionMsg(h.nextSeq(), id, companion.Name, companion.Voice, companion.FaceBG, companion.MarkColor, companion.AccentColor)
	for c := range h.clients {
		if (c.kind == "web" || c.kind == "watcher") && (c.scope == nil || strings.EqualFold(c.scope.ID, key)) {
			c.enqueueProto(msgCompanion, pkt)
		}
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
		c.FaceSVG = c.defaultFace()
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

func (h *wsHub) registerWebTerminal(termID, compID string, scope *companionState, now time.Time) (terminalRecord, error) {
	termID = strings.ToLower(strings.TrimSpace(termID))
	compID = strings.ToLower(strings.TrimSpace(compID))
	if scope != nil {
		compID = strings.ToLower(scope.ID)
		if termID == "" {
			termID = "web-" + randomHex(4)
		}
	}
	if !webTerminalID.MatchString(termID) {
		return terminalRecord{}, problem(400, "Invalid web terminal ID")
	}
	existing, exists := h.b.terminalByID(termID)
	if compID != "" {
		if !validCompanionID(compID) {
			return terminalRecord{}, problem(400, "Invalid companion ID")
		}
		if _, ok := h.b.companionByID(compID); !ok {
			return terminalRecord{}, problem(404, "Companion not found")
		}
	} else if exists {
		compID = existing.CompanionID
	}
	createdAt := now.UTC().Format(time.RFC3339Nano)
	if exists && existing.CreatedAt != "" {
		createdAt = existing.CreatedAt
	}
	t := terminalRecord{
		ID:          termID,
		Kind:        "web",
		CompanionID: compID,
		LastSeen:    now.Unix(),
		CreatedAt:   createdAt,
	}
	if h.b.terminals != nil {
		if err := h.b.terminals.save(t.ID, t, false); err != nil {
			return terminalRecord{}, err
		}
	}
	return t, nil
}

func (h *wsHub) sendSnapshotState(d *wsTerminal) {
	c := d.client
	if d.companionID != "" {
		if d.virtual() {
			c.enqueueProto(msgCompanion, encodeCompanionMsg(h.nextSeq(), d.companionID, d.companion.Name, d.voice, d.companion.FaceBG, d.companion.MarkColor, d.companion.AccentColor))
		}
		if st, ok := h.companionStatuses[d.companionID]; ok {
			c.enqueueProto(msgStatus, encodeStatus(st.seq, st.state, st.progress, st.kind, st.text))
		} else {
			c.enqueueProto(msgStatus, encodeStatus(h.nextSeq(), statusIdle, 0, promptNone, ""))
		}
		if owner := h.voiceOwner(d.companionID); owner != nil {
			c.enqueueProto(msgVoiceOwner, encodeVoiceOwner(h.nextSeq(), d.companionID, owner.id))
		}
		if p := h.prompts[d.companionID]; p != nil {
			c.enqueueProto(msgPrompt, encodePrompt(p.seq, p.kind, p.id, p.text, p.options))
		}
	}
	h.pushVolume(d, false, true)
	h.pushPower(d, d.brightness(), true)
	if d.parentReason != "" {
		h.pushProto(d, msgEvent, encodeEventMsg(h.nextSeq(), eventParentPolicy, d.companionID, string(d.parentReason), parentMessage(d.parentReason)), true)
	}
}

func (h *wsHub) handleHello(c *wsClient, ev terminalHello, now time.Time) bool {
	if ev.protoVersion != protoVersion1 {
		c.enqueueProto(msgReject, encodeReject(rejectBadVersion, "protocol version mismatch"))
		return false
	}
	termID := strings.ToLower(strings.TrimSpace(ev.terminalID))
	if ev.kind == termKindWeb && c.scope == nil && termID == "" {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.closed || c.ctx.Err() != nil {
			return false
		}
		c.kind = "watcher"
		seq := h.nextSeq()
		tok := newResumeToken()
		c.enqueueProto(msgWelcome, encodeWelcome(0, 0, 0, protoMaxPayload, resumeWindowMS, seq, tok, "", ""))
		c.pushPeers(h.peerPayload(false, seq))
		h.broadcastPeers()
		return true
	}

	kindStr := "web"
	clientKind := "web"
	screen, board := roundScreen, ""
	dispFmt := ev.displayFormats
	if dispFmt == 0 {
		if ev.kind == termKindHardware {
			dispFmt = dispRGB565
		} else {
			dispFmt = dispSVG
		}
	}
	frameCodecs := ev.frameCodecs
	if frameCodecs == 0 && dispFmt&dispRGB565 != 0 {
		frameCodecs = codecBitAll
	}
	var ip string
	var port int
	if ev.kind == termKindHardware {
		kindStr = "hardware"
		clientKind = "hardware"
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
			dispFmt&(dispRGB565|dispSVG) == 0 {
			c.enqueueProto(msgReject, encodeReject(rejectBadCaps, "invalid display capabilities"))
			return false
		}
		host, _, _ := net.SplitHostPort(c.remoteAddr().String())
		if addr, err := netip.ParseAddr(host); err == nil {
			host = addr.Unmap().String()
		}
		ip, port = host, 80
	} else if ev.kind != termKindWeb {
		c.enqueueProto(msgReject, encodeReject(rejectBadCaps, "unknown terminal kind"))
		return false
	}

	h.b.companionMu.Lock()
	var rec terminalRecord
	var err error
	if kindStr == "hardware" {
		rec, err = h.registerHardwareTerminal(termID, strings.TrimSpace(ev.companionID), ip, port, now)
	} else {
		rec, err = h.registerWebTerminal(termID, ev.companionID, c.scope, now)
	}
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
		if ev.kind == termKindWeb && ev.flags&helloFlagIfFree != 0 && h.active(old) {
			c.enqueueProto(msgReject, encodeReject(rejectOccupied, "terminal occupied"))
			c.stop(wsCompanionOccupied)
			return false
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
	if d.companionID == "" || h.parentRefresh(d.companionID, now) == "" {
		h.render(d, now, true)
	} else {
		h.announce(d)
	}
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
	if d.companionID == "" || h.parentRefresh(d.companionID, now) == "" {
		h.render(d, now, true)
	} else {
		h.announce(d)
	}
	return true
}

func (h *wsHub) announce(d *wsTerminal) {
	d.announcing = false
	c := d.client
	h.sendSnapshotState(d)
	h.broadcastPeers()
	if c.kind == "web" {
		c.pushPeers(h.peerPayload(c.scope != nil, h.nextSeq()))
	}
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
	if s.CallTXDrops != nil {
		t.CallTXDrops = s.CallTXDrops
	}
}

func (d *wsTerminal) voiceTheme() string {
	return companionTheme(d.companion.Name)
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
	var loc *time.Location
	if p := h.parents[d.companionID]; p != nil && p.configured {
		loc = p.loc
	}
	return gradeAt(now, loc)
}

func (h *wsHub) renderCompanion(companionID string, now time.Time, full bool) {
	for _, d := range h.terminalsFor(companionID) {
		h.render(d, now, full)
	}
}

func (h *wsHub) render(d *wsTerminal, now time.Time, full bool) {
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
			seen := make(map[string]bool)
			for _, other := range h.terminals {
				if other.companionID == "" || other.companionID == d.companionID || !h.active(other) || (calling && !other.caps.Call) {
					continue
				}
				if seen[other.companionID] {
					continue
				}
				seen[other.companionID] = true
				choices = append(choices, menuPeer{other.companionID, other.companion.Name, other.companion.FaceBG, other.companion.AccentColor})
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
		r.view = card.scene(h.canCall(d, card.sender.ID))
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

// A terminal has one frame builder and one replaceable pending request. A dirty
// rectangle is always based on the last frame committed to that terminal's queue.
func (h *wsHub) renderFrames(d *wsTerminal) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for d.renderNext != nil && h.active(d) {
		r, gen, prev, rev := *d.renderNext, d.renderGen, d.svg, d.rev+1
		wantRGB := d.displayFormats&dispRGB565 != 0
		wantSVG := d.displayFormats&dispSVG != 0
		codecs := d.frameCodecs
		oldScene, pointerRev := d.scene, d.caps.PointerRev
		var geometry screenGeometry
		if wantRGB {
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
		var svgWire []byte
		var native wsFrame
		var rawBytes int
		interactionChanged := pointerRev && wantRGB &&
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
					native = d.encoder.encodeWithCodecs(screen.fb, nil, rect, rev, geometry.width, codecs)
					rawBytes = 12 + rect.Dx()*rect.Dy()*2
				}
			}
			screen.render += time.Since(start)
			svgWire = encodeFrameSVG(uint32(rev), d.id, d.companionID, screen.source)
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
		if svgWire == nil {
			continue
		}
		d.rev, d.svgWire = rev, svgWire
		if wantSVG {
			d.client.enqueueProto(msgFrameSVG, svgWire)
			r.trace.measure(diagRender, screen.render)
			h.trackFrame(d, screen.render, len(svgWire), len(screen.source), "svg")
		}
		if wantRGB && native.data != nil {
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
		for c := range h.clients {
			if c.watch == d.companionID && d.companionID != "" || c.watch == d.key {
				c.enqueueProto(msgFrameSVG, svgWire)
			}
		}
		if d.announcing {
			h.announce(d)
		}
	}
	d.rendering, d.renderNext = false, nil
}

func (h *wsHub) wake(d *wsTerminal, now time.Time) {
	if d.parentReason != "" {
		return
	}
	d.last = now
	h.setSleeping(d, false)
}

func (h *wsHub) setSleeping(d *wsTerminal, asleep bool) {
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

func (d *wsTerminal) brightness() int {
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
				if len(p.offline) > 0 && len(h.terminalsFor(key)) == 0 {
					h.parentRefresh(key, now)
				}
			}
			for _, d := range h.terminals {
				if !h.active(d) {
					continue
				}
				if checkGrade && !d.virtual() && !(d.asleep && d.caps.PanelSleep) &&
					d.svg.grade != h.frameGrade(d, now) {
					h.render(d, now, false)
				}
				if d.companionID != "" && h.parentRefresh(d.companionID, now) != "" {
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
				case d.asleep && d.caps.PanelSleep && !d.virtual():
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
