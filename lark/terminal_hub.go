package main

import (
	"context"
	"image"
	"math"
	"net"
	"net/netip"
	"strings"
	"time"

	"lark/internal/stories"

	"lark/internal/pixel"
)

const (
	idleSleep = 120 * time.Second
	// A non-listening capture that the board never ends (or a browser tab that
	// vanished) is force-stopped so the companion cannot stay in "listening".
	captureFailsafe = 90 * time.Second
	// The firmware acks every accepted stream over TCP (retried every 20 ms) and
	// reports consumed samples. Only a socket-session change can lose that ack,
	// so the timer never opens the mic on the same session while audio may
	// still be playing.
	ttsAckFallback = 4 * time.Second
	// Peer cards refresh on this cadence while something is connected.
	peerRefreshEvery = 15 * time.Second
)

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
	actor          *terminalActor
	mode           terminalMode
	client         *terminalClient
	id             string
	key            string
	kind           string // "hardware"
	protoKind      uint8
	companionID    atomicString
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
	pct            int
	charging       bool
	telemetry      terminalTelemetry
	frame          *diagFrame
	svg            svgScreen
	frames         *frameWorker
	renderGen      uint64
	rendering      bool
	renderNext     *renderRequest

	scene         pixel.Scene
	sceneMenu     *menuContent
	view          *pixel.Scene
	pending       *pixel.Scene
	card          *pixel.Scene
	drawer        string
	menuPage      int
	homePage      int
	menuData      *menuContent
	story         *storySession
	storyPage     int
	statsAt       time.Time
	rev           atomicUint64
	nativeRev     uint16
	actionRev     uint16
	hasActionRev  bool
	actionHitsKey string
	pose          companionPose
	moodAt        time.Time
	last          time.Time
	nameFreeUntil time.Time // Follow-ups before this do not need the companion name.
	meter         time.Time
	asleep        bool
	reply         *turnReply // Delta Chat read-out awaiting an in-person reply
	replySpoken   time.Time  // When the read-out was delivered; a capture already open then is stale.
	replyUntil    time.Time

	recording     deviceCapture
	recSession    uint16
	recStop       uint64
	queuedVoice   *endpointCapture
	queuedNotices []queuedNotice
	turn          uint64
	finished      uint64 // Completed turns; waiters use this as a monotonic signal.
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
	ackClient  *terminalClient // connection that owes the playback ack
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

// broadcastPeers refreshes every open intercom list. Each session decides in
// its own actor whether it is showing one.
func (h *terminalHub) broadcastPeers() {
	h.mu.Lock()
	h.broadcastPeersLocked()
	h.mu.Unlock()
}

func (h *terminalHub) broadcastPeersLocked() {
	now := time.Now()
	for _, d := range h.terminals {
		if h.activeLocked(d) {
			d.post(terminalEvent{kind: evPeerRefresh, now: now})
		}
	}
}

// Called under the hub lock, after committing the companion changes.
func (h *terminalHub) companionChanged(id string, companion companionState) {
	h.publishCompanion(id, companion, true)
}

func (h *terminalHub) publishCompanion(id string, companion companionState, clearView bool) {
	key := strings.ToLower(id)
	voice := companion.Voice
	if !h.b.voices.allowed(voice) {
		voice = h.b.voices.defaultFor(companionTheme(companion.Name))
	}
	for _, d := range h.liveFor(key) {
		d.post(terminalEvent{kind: evCompanion, now: time.Now(),
			companion: terminalCompanion{companion: companion, voice: voice, clearView: clearView}})
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

// sendSnapshotState runs in the session actor.
func (h *terminalHub) sendSnapshotState(d *terminalSession) {
	c := d.client
	if d.companionID.get() != "" {
		st, ok := h.statusState(d.companionID.get())
		if !ok {
			st.state = statusIdle
		}
		c.enqueueProto(msgStatus, encodeStatus(st.state))
		if owner := h.ownerOf(d.companionID.get()); owner != nil {
			c.enqueueProto(msgVoiceOwner, encodeVoiceOwner(d.companionID.get(), owner.id))
		}
	}
	h.pushVolume(d, false, true)
	h.pushPower(d, d.brightness(), true)
}

// ownerOf reads the current voice owner for a companion.
func (h *terminalHub) ownerOf(companionID string) *terminalSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.voiceOwner(companionID)
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
	if !terminalIDRE.MatchString(termID) {
		c.enqueueProto(msgReject, encodeReject(rejectUnknownTerminal, "invalid hardware terminal ID"))
		return false
	}
	screen, board, dispFmt, frameCodecs, ok := helloDisplay(ev)
	if !ok {
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
		var reason uint8 = rejectBadCaps
		if api, ok := err.(*apiError); ok && api.status == 404 {
			reason = rejectUnknownTerminal
		}
		c.enqueueProto(msgReject, encodeReject(reason, err.Error()))
		return false
	}

	d := h.newHardwareSession(c, rec, ev, screen, board, dispFmt, frameCodecs, now)
	d.startActor(h)
	h.mu.Lock()
	old, ok := h.attachHardwareLocked(c, d, now)
	h.mu.Unlock()
	if old != nil {
		old.closeWith("reconnect")
	}
	return ok
}

// helloDisplay resolves and validates the advertised display capabilities.
func helloDisplay(ev terminalHello) (screen screenGeometry, board string, dispFmt, frameCodecs uint8, ok bool) {
	screen, board = roundScreen, "esp-vocat"
	dispFmt = ev.displayFormats
	if dispFmt == 0 {
		dispFmt = dispRGB565
	}
	frameCodecs = ev.frameCodecs
	if frameCodecs == 0 && dispFmt&dispRGB565 != 0 {
		frameCodecs = codecBitAll
	}
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
	ok = screen.width >= 1 && screen.width <= screenSize && screen.height >= 1 && screen.height <= screenSize &&
		(screen.shape == "round" || screen.shape == "rect") &&
		(board == "esp-vocat" || board == "esp-box-3") &&
		dispFmt&dispRGB565 != 0
	return screen, board, dispFmt, frameCodecs, ok
}

func (h *terminalHub) newHardwareSession(c *terminalClient, rec terminalRecord, ev terminalHello, screen screenGeometry, board string, dispFmt, frameCodecs uint8, now time.Time) *terminalSession {
	d := &terminalSession{
		client:         c,
		id:             rec.ID,
		key:            rec.ID,
		kind:           "hardware",
		protoKind:      ev.kind,
		companionID:    atomicStr(rec.CompanionID),
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
	if d.companionID.get() != "" {
		d.companion = h.b.companionFor(d.companionID.get())
	} else {
		d.companion = blankCompanion()
	}
	d.voice = h.b.voices.resolve(d.companion.Voice, d.voiceTheme())
	d.vol = h.b.volumePref(d.key)
	d.telemetry.HasBattery = new(d.caps.Battery)
	return d
}

// attachHardwareLocked installs a registered session, replacing a reconnect.
// The caller holds the hub lock; the replaced session is returned so its actor
// can be stopped after the lock is released.
func (h *terminalHub) attachHardwareLocked(c *terminalClient, d *terminalSession, now time.Time) (*terminalSession, bool) {
	if h.closed || c.ctx.Err() != nil {
		return nil, false
	}
	var old *terminalSession
	if old = h.terminals[d.key]; old != nil {
		old.client.cancel()
		d.rev.store(old.rev.load())
	}
	h.terminals[d.key] = d
	c.terminal.Store(d)
	if d.companionID.get() != "" && h.voiceOwners[d.companionID.get()] == "" {
		h.voiceOwners[d.companionID.get()] = d.id
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
	c.enqueueProto(msgWelcome, encodeWelcome(wFlags, udpPort, ssrc, protoMaxPayload, resumeWindowMS, d.resumeToken, d.id, d.companionID.get()))
	companion := d.companion
	d.post(terminalEvent{kind: evRender, now: now, render: terminalRender{full: true}})
	if d.companionID.get() != "" && h.b.oc != nil {
		go h.b.oc.prewarm(c.ctx, companion)
	}
	return old, true
}

func (h *terminalHub) handleResume(c *terminalClient, r terminalResume, now time.Time) bool {
	if r.protoVersion != protoVersion1 {
		c.enqueueProto(msgReject, encodeReject(rejectBadVersion, "protocol version mismatch"))
		return false
	}
	h.mu.Lock()
	if h.closed || c.ctx.Err() != nil {
		h.mu.Unlock()
		return false
	}
	h.pruneResumes(now)
	entry, ok := h.resumes[r.resumeToken]
	if !ok || !strings.EqualFold(entry.terminalID, strings.TrimSpace(r.terminalID)) {
		h.mu.Unlock()
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
		companionID:    atomicStr(compID),
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
	if d.companionID.get() != "" {
		d.companion = h.b.companionFor(d.companionID.get())
	} else {
		d.companion = blankCompanion()
	}
	d.voice = h.b.voices.resolve(d.companion.Voice, d.voiceTheme())
	d.vol = h.b.volumePref(d.key)
	d.telemetry.HasBattery = new(d.caps.Battery)
	d.startActor(h)

	old := h.terminals[d.key]
	if old != nil {
		old.client.cancel()
		d.rev.store(old.rev.load())
	}
	h.terminals[d.key] = d
	c.terminal.Store(d)
	if d.companionID.get() != "" {
		if entry.hadVoiceOwner {
			h.voiceOwners[d.companionID.get()] = d.id
		} else if h.voiceOwners[d.companionID.get()] == "" {
			h.voiceOwners[d.companionID.get()] = d.id
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
	c.enqueueProto(msgWelcome, encodeWelcome(wFlags, udpPort, ssrc, protoMaxPayload, resumeWindowMS, d.resumeToken, d.id, d.companionID.get()))
	companion := d.companion
	d.post(terminalEvent{kind: evRender, now: now, render: terminalRender{full: true}})
	h.mu.Unlock()
	if old != nil {
		old.closeWith("reconnect")
	}
	if d.companionID.get() != "" && h.b.oc != nil {
		go h.b.oc.prewarm(c.ctx, companion)
	}
	return true
}

// speakingCompanion names the only companion with a turn in flight, so an MCP
// control call can default companion_id when the model leaves it out.
func (h *terminalHub) speakingCompanion() string {
	if h == nil {
		return ""
	}
	h.mu.Lock()
	terms := make([]*terminalSession, 0, len(h.terminals))
	for _, d := range h.terminals {
		if h.activeLocked(d) {
			terms = append(terms, d)
		}
	}
	h.mu.Unlock()
	found := ""
	for _, d := range terms {
		st := d.state()
		if d.companionID.get() == "" || !st.busy && !st.recording.active() {
			continue
		}
		if found != "" && !strings.EqualFold(found, d.companionID.get()) {
			return ""
		}
		found = d.companionID.get()
	}
	return found
}

// pendingReply returns the armed read-out context when a capture starts after
// the read-out finished and before its short TTL expires. A capture already
// open when the notice played is not an answer to it.
func (d *terminalSession) pendingReply(now time.Time) *turnReply {
	if d.reply == nil {
		return nil
	}
	// A read-out still playing has no TTL yet; a capture that starts now cannot
	// be its answer.
	if d.replyUntil.IsZero() {
		return nil
	}
	if now.Before(d.replyUntil) && !now.Before(d.replySpoken) {
		return d.reply
	}
	if !now.Before(d.replyUntil) {
		d.clearReply()
	}
	return nil
}

// armReplyTTL starts the read-out answer window when its playback finishes.
// replySpoken keeps the delivery time so a capture opened before the notice
// is still recognised as stale.
func (d *terminalSession) armReplyTTL(now time.Time) {
	if d.reply != nil && !d.replySpoken.IsZero() && d.replyUntil.IsZero() {
		d.replyUntil = now.Add(deviceReplyTTL)
	}
}

func (d *terminalSession) clearReply() {
	d.reply, d.replySpoken, d.replyUntil = nil, time.Time{}, time.Time{}
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
	view         pixel.Scene
	menu         *menuContent
	companion    *companionState
	pose         companionPose
	grade        pixel.GradeKey
	drawer       string
	menuPage     int
	vol          int
	voice        string
	peers        []pixel.Peer
	statsRows    []pixel.Row
	storyTitle   string
	storyText    string
	storyChoices []stories.Choice
	storyEnd     bool
	storyBG      *image.RGBA
	storyPage    int
	full         bool
	listenWindow bool
	trace        *turnTrace
}

// openStory starts opening a story from outside the actor. Twee parsing, saved
// progress, and the background load run in the caller goroutine; the session
// commit runs in its actor.
func (h *terminalHub) openStory(d *terminalSession, st stories.Saved, narrate bool) {
	d.post(terminalEvent{kind: evStoryOpen, now: time.Now(), story: terminalStory{st: st, narrate: narrate}})
}

func (h *terminalHub) openStoryEvent(d *terminalSession, st stories.Saved, narrate bool) {
	go func() {
		cs, err := stories.Parse(st.Twee)
		if err != nil {
			return
		}
		sess := &storySession{
			id:         st.ID,
			title:      cs.Title,
			compiled:   cs,
			passageIdx: -1,
			flags:      cs.FlagsSlice(nil),
		}
		page := 0
		entered := false
		if h.b != nil && h.b.db != nil && d.companionID.get() != "" {
			if prog, ok, perr := h.b.getStoryProgress(d.client.ctx, d.companionID.get(), st.ID); perr == nil && ok {
				if idx, exists := cs.ByName[prog.Passage]; exists {
					sess.flags = cs.FlagsSlice(prog.Flags)
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
		d.update(func(d *terminalSession) {
			if !h.active(d) {
				return
			}
			d.drawer = pixel.DrawerStoryReader
			d.story = sess
			d.storyPage = page
			now := time.Now()
			h.stopPlayback(d, true)
			if narrate && sess.text != "" {
				h.speakLocal(d, sess.text)
			}
			h.render(d, now, false)
		})
	}()
}

func (h *terminalHub) postStoryBG(d *terminalSession, storyID, passage string, img *image.RGBA) {
	d.post(terminalEvent{kind: evStoryBG, now: time.Now(),
		story: terminalStory{storyID: storyID, passage: passage, bg: img}})
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
	if h.b != nil && h.b.db != nil && d.companionID.get() != "" {
		prog := stories.Progress{
			CompanionID: d.companionID.get(),
			StoryID:     sess.id,
			Passage:     sess.currentPassageName(),
			Page:        0,
			Flags:       sess.compiled.FlagsMap(sess.flags),
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

func (h *terminalHub) wake(d *terminalSession, now time.Time) {
	d.last = now
	h.setSleeping(d, false)
}

func (h *terminalHub) setSleeping(d *terminalSession, asleep bool) {
	if d.asleep != asleep {
		d.asleep = asleep
		h.pushPower(d, d.brightness(), true)
	}
	d.syncMode()
}

func (d *terminalSession) brightness() int {
	if d.drawer == pixel.DrawerPair {
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
			now := time.Now()
			h.mu.Lock()
			if h.closed {
				h.mu.Unlock()
				return
			}
			checkGrade := h.atmosphere && !now.Before(nextGrade)
			if checkGrade {
				nextGrade = now.Truncate(30 * time.Minute).Add(30 * time.Minute)
			}
			targets := make([]*terminalSession, 0, len(h.terminals))
			for _, d := range h.terminals {
				if h.activeLocked(d) {
					targets = append(targets, d)
				}
			}
			if now.Sub(sincePeers) > peerRefreshEvery {
				h.broadcastPeersLocked()
				sincePeers = now
			}
			h.mu.Unlock()
			for _, d := range targets {
				d.post(terminalEvent{kind: evTick, now: now, tick: terminalTick{checkGrade}})
			}
		}
	}
}

// losslessTTSAcks reports whether the firmware retries one ack per accepted
// stream. Before 0.21.0 an ack could be lost, so the timer still rescues the
// same connection; newer firmware only needs it after a session change.
func losslessTTSAcks(firmware string) bool {
	parts := strings.Split(strings.TrimSpace(firmware), ".")
	want := [...]int{0, 21, 0}
	for i := range want {
		n := 0
		if i < len(parts) {
			for _, r := range parts[i] {
				if r < '0' || r > '9' {
					break
				}
				n = n*10 + int(r-'0')
			}
		}
		if n != want[i] {
			return n > want[i]
		}
	}
	return true
}

func (h *terminalHub) companionTerminals(companionID string) []object {
	if h == nil {
		return nil
	}
	var out []object
	for _, d := range h.liveFor(companionID) {
		st := d.state()
		entry := object{
			"id":     d.id,
			"name":   d.id,
			"online": true,
			"volume": st.vol,
		}
		if d.caps.Battery && st.pct >= 0 {
			entry["battery_pct"] = st.pct
			entry["charging"] = st.charging
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
	d := h.live(terminalID)
	if d == nil || d.companionID.get() != companionID {
		return problem(404, "Terminal not attached or offline")
	}
	return h.setVolume(ctx, d, level, "")
}

func (h *terminalHub) saveVoice(id, voice string) error {
	h.b.companionMu.Lock()
	defer h.b.companionMu.Unlock()
	if err := h.b.voices.save(id, voice); err != nil {
		return err
	}
	now := time.Now()
	for _, d := range h.liveFor(id) {
		d.post(terminalEvent{kind: evVoice, now: now, voice: voice})
	}
	return nil
}

// Takes and returns with h.mu held. Serialize writes without blocking the hub.
// setVolume resolves the requested level and persists it. The preference write
// runs here, outside the actor; the session actor applies the value and renders.
func (h *terminalHub) setVolume(ctx context.Context, d *terminalSession, level int, action string) error {
	h.settingsMu.Lock()
	defer h.settingsMu.Unlock()
	st := d.state()
	if !h.active(d) {
		return problem(409, "Terminal is not connected")
	}
	switch action {
	case "vol-minus":
		level = max(0, st.vol-10)
	case "vol-plus":
		level = min(100, st.vol+10)
	}
	if level < 0 {
		level = st.vol
	}
	if err := h.b.saveVolumePref(ctx, d.key, level); err != nil {
		return err
	}
	applied := false
	d.update(func(d *terminalSession) {
		if !h.active(d) {
			return
		}
		applied = true
		d.vol = level
		h.pushVolume(d, true, true)
		now := time.Now()
		d.meter = now.Add(1800 * time.Millisecond)
		h.render(d, now, false)
	})
	if !applied {
		return problem(409, "Device disconnected while saving volume")
	}
	return nil
}
