package main

import (
	"fmt"
	"image"
	"log"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

func (h *terminalHub) render(d *terminalSession, now time.Time, full bool) {
	d.syncMode()
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
			for _, other := range h.liveAll() {
				cid := other.companionID.get()
				if cid == "" || cid == d.companionID.get() {
					continue
				}
				if seen[cid] {
					continue
				}
				seen[cid] = true
				choices = append(choices, menuPeer{cid, h.b.companionFor(cid).Name})
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
		r.view.stopControl()
	}
	d.renderGen++
	d.renderNext = &r
	if !d.rendering {
		h.startFrameJob(d)
	}
}

func (h *terminalHub) startFrameJob(d *terminalSession) {
	if d.frames == nil || d.renderNext == nil {
		d.rendering, d.renderNext = false, nil
		return
	}
	job := frameJob{
		req:        *d.renderNext,
		prev:       d.svg,
		gen:        d.renderGen,
		rev:        d.rev.load() + 1,
		wantRGB:    d.displayFormats&dispRGB565 != 0,
		codecs:     d.frameCodecs,
		oldScene:   d.scene,
		pointerRev: d.caps.PointerRev,
		geometry:   d.screen,
	}
	if job.geometry.width == 0 || job.geometry.height == 0 {
		job.geometry = roundScreen
	}
	d.renderNext = nil
	d.rendering = true
	d.frames.jobs <- job
}

// frameJob is everything one render pass needs after the actor lets go.
type frameJob struct {
	req        renderRequest
	prev       svgScreen
	gen        uint64
	rev        uint64
	geometry   screenGeometry
	oldScene   sceneView
	wantRGB    bool
	pointerRev bool
	codecs     uint8
}

// frameWorker owns the frame builder and its scratch buffers for one terminal.
// It never touches session state; results go back to the actor as events.
type frameWorker struct {
	d       *terminalSession
	jobs    chan frameJob
	quit    chan struct{}
	done    chan struct{}
	scratch []uint16
	encoder frameEncoder
}

func newFrameWorker(h *terminalHub, d *terminalSession) *frameWorker {
	w := &frameWorker{d: d, jobs: make(chan frameJob, 1), quit: make(chan struct{}), done: make(chan struct{})}
	go w.run(h)
	return w
}

func (w *frameWorker) run(h *terminalHub) {
	defer close(w.done)
	for {
		select {
		case <-w.quit:
			return
		case job := <-w.jobs:
			screen, view, err := h.buildFrame(w, job)
			native, raw, render, send := encodeFrame(w, job, screen, view)
			w.d.post(terminalEvent{kind: evFrameBuilt, now: time.Now(), frame: terminalFrameBuilt{
				job: job, screen: screen, view: view, err: err, native: native, raw: raw, render: render, send: send,
			}})
		}
	}
}

func (w *frameWorker) close() {
	close(w.quit)
	<-w.done
}

// handleFrameBuilt commits one built frame, then starts the newest pending
// request. The caller is the session actor.
func (h *terminalHub) handleFrameBuilt(d *terminalSession, fb terminalFrameBuilt) {
	if fb.err != nil {
		log.Printf("Lark screen: %v", fb.err)
	} else if h.active(d) && d.renderGen == fb.job.gen {
		h.commitFrame(d, fb)
	}
	d.rendering = false
	h.startFrameJob(d)
}

// buildFrame draws the requested scene and packs it into RGB565. It runs
// without the hub lock; portraits and assets are cached separately.
func (h *terminalHub) buildFrame(w *frameWorker, job frameJob) (svgScreen, sceneView, error) {
	r, geometry := job.req, job.geometry
	spec := pixelSpecForGeometry(geometry)
	bg, poses, th := h.companionAssets(r.companion)
	var img *image.RGBA
	var hits []hitBox
	var err error
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
	var screen svgScreen
	if err != nil || img == nil {
		return screen, r.view, err
	}
	b := img.Bounds()
	dw, dh := b.Dx(), b.Dy()
	var fb []uint16
	if cap(w.scratch) >= dw*dh {
		fb = w.scratch[:dw*dh]
	} else {
		fb = make([]uint16, dw*dh)
	}
	// Reuse the buffer of the frame *before* prev on the next pass. Reusing
	// prev itself would make the diff compare a buffer with itself.
	if len(job.prev.fb) == dw*dh {
		w.scratch = job.prev.fb
	} else {
		w.scratch = nil
	}
	for y := range dh {
		for x := range dw {
			c := img.RGBAAt(b.Min.X+x, b.Min.Y+y)
			fb[y*dw+x] = pack565(c.R, c.G, c.B)
		}
	}
	screen = svgScreen{fb: fb, hits: hits, grade: r.grade}
	releasePixelImage(img)
	if debugScreenText {
		screen.source = screenSource(r, hits)
	}
	return screen, r.view, nil
}

// screenSource is the labelled hit trace DOM tests wait on.
func screenSource(r renderRequest, hits []hitBox) string {
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
			srcParts = append(srcParts, fmt.Sprintf("Level · %d%%", r.vol))
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
	return strings.Join(srcParts, " ")
}

// encodeFrame diffs against the last committed frame and encodes only when the
// rectangle, grade, or interaction targets changed. It runs without the lock.
func encodeFrame(w *frameWorker, job frameJob, screen svgScreen, view sceneView) (protocolFrame, int, time.Duration, bool) {
	var native protocolFrame
	var rawBytes int
	fbChanged := !slices.Equal(job.prev.fb, screen.fb)
	interactionChanged := job.pointerRev && job.wantRGB &&
		(!maps.Equal(view.actions, job.oldScene.actions) || !slices.Equal(screen.hits, job.prev.hits))
	if screen.fb == nil || !job.wantRGB {
		return native, rawBytes, 0, false
	}
	if !(job.req.full || fbChanged || screen.grade != job.prev.grade || interactionChanged) {
		return native, rawBytes, 0, false
	}
	start := time.Now()
	rect := dirtyRect(job.prev.fb, screen.fb, job.geometry)
	if rect.Empty() && (interactionChanged || job.req.full) {
		rect = image.Rect(0, 0, 1, 1)
	}
	if !rect.Empty() {
		frameCodecs := job.codecs
		fullScene := job.prev.fb == nil || job.req.full || screen.grade != job.prev.grade
		if !fullScene || job.req.drawer != "" || view.title != "" || view.heading != "" || view.text != "" {
			frameCodecs &^= codecBitJPEG
		}
		native = w.encoder.encodeWithCodecs(screen.fb, nil, rect, job.rev, job.geometry.width, frameCodecs)
		rawBytes = 12 + rect.Dx()*rect.Dy()*2
	}
	return native, rawBytes, time.Since(start), true
}

// commitFrame publishes the built frame. The caller is the session actor.
func (h *terminalHub) commitFrame(d *terminalSession, fb terminalFrameBuilt) {
	in := fb.job
	d.scene, d.svg = fb.view, fb.screen
	h.sig.notify()
	if fb.native.data() == nil {
		if d.announcing {
			h.announce(d)
		}
		return
	}
	d.rev.store(in.rev)
	d.client.enqueueProto(msgFrame, fb.native.data())
	d.nativeRev = uint16(in.rev)
	hk := pointerKey(fb.screen.hits, fb.view.actions)
	if !d.hasActionRev || hk != d.actionHitsKey {
		d.actionHitsKey = hk
		d.actionRev = d.nativeRev
		d.hasActionRev = true
	}
	in.req.trace.measure(diagRender, fb.render)
	codec := "RGB565"
	switch fb.native.data()[9] {
	case frameCodecRLE565:
		codec = "RLE565"
	case frameCodecJPEG:
		codec = "JPEG"
	case frameCodecLZ4:
		codec = "LZ4"
	}
	h.trackFrame(d, fb.render, len(fb.native.data()), fb.raw, codec)
	if d.announcing {
		h.announce(d)
	}
}

func (h *terminalHub) companionAssets(c *companionState) (*image.RGBA, [4]*image.RGBA, pixelTheme) {
	if c != nil && c.PortraitID != "" && h != nil && h.b != nil && h.b.portraits != nil {
		h.assetsMu.Lock()
		h.assetsClock++
		clock := h.assetsClock
		if cached, ok := h.assets[c.PortraitID]; ok {
			if cached.expires.IsZero() || time.Now().Before(cached.expires) {
				cached.used = clock
				h.assets[c.PortraitID] = cached
				h.assetsMu.Unlock()
				return cached.bg, cached.poses, cached.theme
			}
			delete(h.assets, c.PortraitID)
		}
		epoch := h.assetsEpoch[c.PortraitID]
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
		assets := cachedAssets{bg: bg, poses: poses, theme: th, used: clock}
		if !loaded {
			assets.expires = time.Now().Add(partialAssetTTL)
		}
		h.storeAssets(c.PortraitID, assets, epoch)
		return bg, poses, th
	}
	poses := pixelPlaceholderPoses()
	return nil, poses, placeholderPixelTheme()
}

// storeAssets discards a load that raced an invalidation: its bytes describe
// the replaced portrait and must not become the new cache entry.
func (h *terminalHub) storeAssets(portraitID string, assets cachedAssets, epoch uint64) {
	h.assetsMu.Lock()
	defer h.assetsMu.Unlock()
	if h.assetsEpoch[portraitID] != epoch {
		return
	}
	if h.assets == nil {
		h.assets = make(map[string]cachedAssets)
	}
	h.assets[portraitID] = assets
	h.evictAssetsLocked()
}

const partialAssetTTL = 2 * time.Second

// invalidateAssets drops the decoded background and poses for a portrait so
// the next render reloads them from disk.
func (h *terminalHub) invalidateAssets(portraitID string) {
	if h == nil || portraitID == "" {
		return
	}
	h.assetsMu.Lock()
	delete(h.assets, portraitID)
	if h.assetsEpoch == nil {
		h.assetsEpoch = make(map[string]uint64)
	}
	h.assetsEpoch[portraitID]++
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

func (h *terminalHub) renderCompanion(companionID string, now time.Time, full bool) {
	for _, d := range h.terminalsFor(companionID) {
		h.render(d, now, full)
	}
}

// openStory opens the reader at saved progress. Menu and panel entry points
// are silent; narration belongs to conversational story turns.
func (h *terminalHub) frameGrade(d *terminalSession, now time.Time) gradeKey {
	if !h.atmosphere {
		return gradeDay
	}
	return gradeAt(now, nil)
}

func (h *terminalHub) companionPose(d *terminalSession, now time.Time, mood string) companionPose {
	p := poseFrame(mood, d.pct, d.vol, d.charging, now.Before(d.meter))
	p.stopTaps = d.stopTaps
	p.canContinue = mood == "thinking" && h.bridgeEndpoint(d) && d.queuedVoice == nil
	return p
}

func (h *terminalHub) announce(d *terminalSession) {
	d.announcing = false
	h.sendSnapshotState(d)
	h.broadcastPeers()
}
