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
	p := poseFrame(now, d.moodAt, d.last, mood, d.pct, d.vol, d.charging, now.Before(d.meter))
	p.stopTaps = d.stopTaps
	p.canContinue = mood == "thinking" && h.bridgeEndpoint(d) && d.queuedVoice == nil
	return p
}

func (h *terminalHub) announce(d *terminalSession) {
	d.announcing = false
	h.sendSnapshotState(d)
	h.broadcastPeers()
}
