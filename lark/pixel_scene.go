package main

import (
	"fmt"
	"image"
	imagecolor "image/color"
	"strconv"
	"strings"
	"time"

	"lark/fonts/dogica"
)

type companionPose struct {
	mood        string
	battery     int
	volume      int
	charging    bool
	showMeter   bool
	stopTaps    int
	canContinue bool
}

func poseFrame(now, moodAt, last time.Time, mood string, pct, vol int, charging, showMeter bool) companionPose {
	_, _, _ = now, moodAt, last
	return companionPose{
		mood:      mood,
		battery:   pct,
		volume:    vol,
		charging:  charging,
		showMeter: showMeter,
	}
}

const (
	drawerMenu       = "menu"
	drawerMore       = "more"
	drawerBooks      = "books"
	drawerBookReader = "book-reader"
	drawerGames      = "games"
	drawerMemories   = "memories"
	drawerHistory    = "history"
	drawerPair       = "pair"
	drawerUnpair     = "unpair"
	drawerVolume     = "volume"
	drawerVoice      = "voice"
	drawerIntercom   = "intercom"
	drawerStats      = "stats"
)

const (
	menuRowsPerPage = 2
	tilesPerPage    = 6
)

type sceneAction struct{ kind, value string }

type intercomCard struct{ companion, heading, text string }

type sceneView struct {
	title, heading, text string
	intercom             *intercomCard
	actions              map[string]sceneAction
	duringTurn           bool
	building             string
	menu                 *menuContent
}

func (s *sceneView) hasIntercom() bool { return s != nil && s.intercom != nil }

func (card intercomCard) scene() sceneView {
	return sceneView{
		title:    card.companion,
		heading:  card.heading,
		text:     card.text,
		intercom: &card,
		actions: map[string]sceneAction{
			"close": {"close-card", ""},
			"reply": {"reply", strings.ToLower(card.companion)},
		},
	}
}

func messageScene(title, heading, text string, _ companionState) sceneView {
	return sceneView{
		title:   title,
		heading: heading,
		text:    text,
		actions: map[string]sceneAction{"close": {"close-view", ""}},
	}
}

func noticeScene(title, message string) sceneView {
	return sceneView{
		title:   title,
		text:    message,
		actions: map[string]sceneAction{"close": {"close-view", ""}},
	}
}

func timerScene(message string) sceneView {
	return sceneView{
		title:   "Timer",
		text:    message,
		actions: map[string]sceneAction{"close": {"close-view", ""}},
	}
}

func (s *sceneView) stopControl(taps int) {
	if s.actions == nil {
		s.actions = map[string]sceneAction{}
	}
	s.actions["stop"] = sceneAction{"stop-turn", ""}
	_ = taps
}

type menuRow struct{ title, text string }

type menuPeer struct{ id, name string }

type tile struct {
	id, label, detail, icon string
	action                  sceneAction
	saved                   bool
}

func clampMenuPage(page, count, pageSize int) int {
	if pageSize <= 0 || count <= 0 {
		return 0
	}
	pages := (count + pageSize - 1) / pageSize
	return max(0, min(page, pages-1))
}

func menuClip(s string, maxRunes int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:max(1, maxRunes-1)]) + "…"
}

func addLogicalHit(hits []hitBox, spec pixelDisplaySpec, id string, r image.Rectangle) []hitBox {
	if id == "" || r.Empty() {
		return hits
	}
	s := spec.artScale
	return append(hits, hitBox{
		id: id,
		x1: r.Min.X * s,
		y1: r.Min.Y * s,
		x2: r.Max.X*s - 1,
		y2: r.Max.Y*s - 1,
	})
}

func navBarHits(spec pixelDisplaySpec, page, totalPages int) ([]hitBox, map[string]sceneAction) {
	navH := pixelNavHeight(spec)
	y := spec.safe.Max.Y - navH
	x0 := spec.safe.Min.X
	w := spec.safe.Dx()
	gap := 3
	btnW := (w - 2*gap) / 3
	leftRect := image.Rect(x0, y, x0+btnW, y+navH)
	midRect := image.Rect(x0+btnW+gap, y, x0+2*btnW+gap, y+navH)
	rightRect := image.Rect(x0+2*(btnW+gap), y, x0+w, y+navH)

	var hits []hitBox
	actions := map[string]sceneAction{}
	hits = addLogicalHit(hits, spec, "menu-back", midRect)
	actions["menu-back"] = sceneAction{"menu-back", ""}
	if page > 0 {
		hits = addLogicalHit(hits, spec, "menu-prev", leftRect)
		actions["menu-prev"] = sceneAction{"menu-page", strconv.Itoa(page - 1)}
	}
	if page+1 < totalPages {
		hits = addLogicalHit(hits, spec, "menu-next", rightRect)
		actions["menu-next"] = sceneAction{"menu-page", strconv.Itoa(page + 1)}
	}
	return hits, actions
}

func renderPixelSceneCard(spec pixelDisplaySpec, v sceneView, th pixelTheme) (*image.RGBA, []hitBox) {
	if th.border.A == 0 {
		th = defaultPixelTheme()
	}
	c := newPixelCanvas(spec.logicalW, spec.logicalH)
	darkenBackground(c, th.bg)
	s := spec.artScale
	var texts []pixelTextOp

	titleH := 12
	titleStr := v.title
	if titleStr == "" {
		titleStr = "Lark"
	}
	if v.building != "" {
		titleStr = "Building…"
	}
	titleBox := physicalRect(image.Rect(spec.safe.Min.X, spec.safe.Min.Y, spec.safe.Max.X, spec.safe.Min.Y+titleH), s)
	texts = addCenteredText(texts, titleBox, truncateEllipsisPx(titleStr, dogica.Bold, spec.bodyTextScale, titleBox.Dx()), dogica.Bold, spec.bodyTextScale, th.accent)

	btnH := pixelNavHeight(spec)
	btnRect := image.Rect(spec.safe.Min.X, spec.safe.Max.Y-btnH, spec.safe.Max.X, spec.safe.Max.Y)
	panelRect := image.Rect(spec.safe.Min.X, spec.safe.Min.Y+titleH+2, spec.safe.Max.X, btnRect.Min.Y-3)
	c.panel(panelRect.Min.X, panelRect.Min.Y, panelRect.Dx(), panelRect.Dy(), pxTileBG, th.border)

	body := v.text
	if v.heading != "" {
		body = v.heading + "\n" + body
	}
	if v.building != "" && body == "" {
		body = "Making something new…"
	}
	physPanel := physicalRect(panelRect, s)
	lineH := dogica.Regular.LineHeight * spec.bodyTextScale
	lines := wrapTextPx(body, dogica.Regular, spec.bodyTextScale, physPanel.Dx()-16)
	maxLines := max(1, (physPanel.Dy()-16)/lineH)
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	for i, line := range lines {
		texts = append(texts, pixelTextOp{
			x: physPanel.Min.X + 8, y: physPanel.Min.Y + 8 + i*lineH,
			text: line, font: dogica.Regular, scale: spec.bodyTextScale, color: pxInk,
		})
	}

	var hits []hitBox
	if v.intercom != nil {
		gap := 4
		bw := (btnRect.Dx() - gap) / 2
		r1 := image.Rect(btnRect.Min.X, btnRect.Min.Y, btnRect.Min.X+bw, btnRect.Max.Y)
		r2 := image.Rect(btnRect.Min.X+bw+gap, btnRect.Min.Y, btnRect.Max.X, btnRect.Max.Y)
		c.panel(r1.Min.X, r1.Min.Y, r1.Dx(), r1.Dy(), pxTileBG, th.border)
		c.panel(r2.Min.X, r2.Min.Y, r2.Dx(), r2.Dy(), pxTileBG, th.accent)
		texts = addCenteredText(texts, physicalRect(r1, s), "Close", dogica.Regular, spec.bodyTextScale, pxInk)
		texts = addCenteredText(texts, physicalRect(r2, s), "Reply", dogica.Bold, spec.bodyTextScale, th.accent)
		hits = addLogicalHit(hits, spec, "close", r1)
		hits = addLogicalHit(hits, spec, "reply", r2)
	} else if v.building != "" {
		gap := 4
		bw := (btnRect.Dx() - gap) / 2
		r1 := image.Rect(btnRect.Min.X, btnRect.Min.Y, btnRect.Min.X+bw, btnRect.Max.Y)
		r2 := image.Rect(btnRect.Min.X+bw+gap, btnRect.Min.Y, btnRect.Max.X, btnRect.Max.Y)
		c.panel(r1.Min.X, r1.Min.Y, r1.Dx(), r1.Dy(), pxTileBG, th.accent2)
		c.panel(r2.Min.X, r2.Min.Y, r2.Dx(), r2.Dy(), pxTileBG, pxDanger)
		texts = addCenteredText(texts, physicalRect(r1, s), "Talk", dogica.Bold, spec.bodyTextScale, th.accent2)
		texts = addCenteredText(texts, physicalRect(r2, s), "Cancel build", dogica.Regular, spec.bodyTextScale, pxDanger)
		hits = addLogicalHit(hits, spec, "listen", r1)
		hits = addLogicalHit(hits, spec, "build-cancel", r2)
	} else {
		c.panel(btnRect.Min.X, btnRect.Min.Y, btnRect.Dx(), btnRect.Dy(), pxTileBG, th.border)
		texts = addCenteredText(texts, physicalRect(btnRect, s), "OK", dogica.Bold, spec.bodyTextScale, pxInk)
		hits = addLogicalHit(hits, spec, "close", image.Rect(0, 0, spec.logicalW, spec.logicalH))
	}
	return finishPixelScreen(c, spec, texts), hits
}

func renderPixelListWithHits(spec pixelDisplaySpec, title string, items []tile, empty string, page int, th pixelTheme, accent imagecolor.RGBA) (*image.RGBA, []hitBox, map[string]sceneAction) {
	if th.border.A == 0 {
		th = defaultPixelTheme()
	}
	c := newPixelCanvas(spec.logicalW, spec.logicalH)
	darkenBackground(c, th.bg)
	s := spec.artScale
	var texts []pixelTextOp

	titleH := 12
	titleRect := image.Rect(spec.safe.Min.X, spec.safe.Min.Y, spec.safe.Max.X, spec.safe.Min.Y+titleH)
	texts = addCenteredText(texts, physicalRect(titleRect, s), title, dogica.Bold, spec.titleTextScale, accent)

	perPage := 2
	if !spec.round {
		perPage = 3
	}
	navH := pixelNavHeight(spec)
	totalPages := max(1, (len(items)+perPage-1)/perPage)
	page = max(0, min(page, totalPages-1))

	listTop := spec.safe.Min.Y + titleH + 2
	listBottom := spec.safe.Max.Y - navH - 3
	gapY := 3
	rowH := (listBottom - listTop - (perPage-1)*gapY) / perPage

	hits, actions := navBarHits(spec, page, totalPages)
	if len(items) == 0 {
		panel := image.Rect(spec.safe.Min.X, listTop, spec.safe.Max.X, listBottom)
		c.panel(panel.Min.X, panel.Min.Y, panel.Dx(), panel.Dy(), pxTileBG, th.border)
		texts = addCenteredText(texts, physicalRect(panel, s), empty, dogica.Regular, spec.bodyTextScale, pxMuted)
		return finishPixelScreen(c, spec, texts), hits, actions
	}

	start := page * perPage
	for i := range perPage {
		idx := start + i
		if idx >= len(items) {
			break
		}
		it := items[idx]
		ry := listTop + i*(rowH+gapY)
		rx := spec.safe.Min.X
		rw := spec.safe.Dx()
		rowRect := image.Rect(rx, ry, rx+rw, ry+rowH)
		c.panel(rx, ry, rw, rowH, pxTileBG, th.border)
		if it.id != "" {
			hits = addLogicalHit(hits, spec, it.id, rowRect)
			actions[it.id] = it.action
		}
		if sprite, ok := pxMenuSprites[it.icon]; ok {
			c.blitSprite(rx+4, ry+(rowH-12)/2, sprite, pxIconPalette, 1)
		}
		textX := rx + 4
		if it.icon != "" {
			textX = rx + 19
		}
		physText := physicalRect(image.Rect(textX, ry+2, rx+rw-3, ry+rowH-2), s)
		lbl := truncateEllipsisPx(it.label, dogica.Bold, spec.bodyTextScale, physText.Dx())
		if it.detail != "" {
			det := truncateEllipsisPx(it.detail, dogica.Regular, spec.bodyTextScale, physText.Dx())
			lineH := dogica.Regular.LineHeight * spec.bodyTextScale
			topY := physText.Min.Y + max(0, (physText.Dy()-2*lineH)/2)
			texts = append(texts,
				pixelTextOp{x: physText.Min.X, y: topY, text: lbl, font: dogica.Bold, scale: spec.bodyTextScale, color: pxInk},
				pixelTextOp{x: physText.Min.X, y: topY + lineH, text: det, font: dogica.Regular, scale: spec.bodyTextScale, color: pxMuted},
			)
		} else {
			thPx := dogica.Bold.Ascent * spec.bodyTextScale
			texts = append(texts, pixelTextOp{
				x: physText.Min.X, y: physText.Min.Y + (physText.Dy()-thPx)/2,
				text: lbl, font: dogica.Bold, scale: spec.bodyTextScale, color: pxInk,
			})
		}
	}

	return finishPixelScreen(c, spec, texts), hits, actions
}

func menuGridHits(spec pixelDisplaySpec, items []pixelMenuItem, page int, actionFn func(pixelMenuItem) (string, sceneAction)) ([]hitBox, map[string]sceneAction) {
	cols, rows := 1, 3
	if !spec.round {
		cols, rows = 2, 3
	}
	navH := pixelNavHeight(spec)
	perPage := cols * rows
	totalPages := max(1, (len(items)+perPage-1)/perPage)
	page = max(0, min(page, totalPages-1))

	gridTop := spec.safe.Min.Y + 12 + 2
	gridBottom := spec.safe.Max.Y - navH - 2
	gapX, gapY := 4, 2
	if !spec.round {
		gapX = 2
	}
	tileW := (spec.safe.Dx() - (cols-1)*gapX) / cols
	tileH := (gridBottom - gridTop - (rows-1)*gapY) / rows

	hits, actions := navBarHits(spec, page, totalPages)
	start := page * perPage
	for idx := range perPage {
		itemIdx := start + idx
		if itemIdx >= len(items) {
			break
		}
		item := items[itemIdx]
		col := idx % cols
		row := idx / cols
		tx := spec.safe.Min.X + col*(tileW+gapX)
		ty := gridTop + row*(tileH+gapY)
		id, act := actionFn(item)
		hits = addLogicalHit(hits, spec, id, image.Rect(tx, ty, tx+tileW, ty+tileH))
		actions[id] = act
	}
	return hits, actions
}

func renderPixelMenuWithHits(spec pixelDisplaySpec, page int, th pixelTheme) (*image.RGBA, []hitBox, map[string]sceneAction) {
	if th.border.A == 0 {
		th = defaultPixelTheme()
	}
	hits, actions := menuGridHits(spec, pixelHomeMenuItems, page, func(item pixelMenuItem) (string, sceneAction) {
		if item.id == "talk" {
			return "menu-intercom", sceneAction{"menu-open", drawerIntercom}
		}
		return "menu-" + item.id, sceneAction{"menu-open", item.id}
	})
	return renderPixelMenu(spec, page, th), hits, actions
}

func renderPixelMoreWithHits(spec pixelDisplaySpec, page int, th pixelTheme) (*image.RGBA, []hitBox, map[string]sceneAction) {
	if th.border.A == 0 {
		th = defaultPixelTheme()
	}
	hits, actions := menuGridHits(spec, pixelMoreMenuItems, page, func(item pixelMenuItem) (string, sceneAction) {
		return "menu-" + item.id, sceneAction{"menu-open", item.id}
	})
	return renderPixelMenuGrid(spec, "More", pixelMoreMenuItems, page, th, ""), hits, actions
}

func renderPixelVolumeWithHits(spec pixelDisplaySpec, auto bool, vol, page int, th pixelTheme) (*image.RGBA, []hitBox, map[string]sceneAction) {
	mode := "Manual"
	if auto {
		mode = "Auto"
	}
	items := []tile{
		{id: "vol-mode", label: "Mode: " + mode, detail: "Tap to switch", icon: "volume", action: sceneAction{"volume-auto", ""}},
		{id: "vol-down", label: "Quieter (-10)", detail: fmt.Sprintf("Level %d%%", vol), icon: "volume", action: sceneAction{"volume-step", "-10"}},
		{id: "vol-up", label: "Louder (+10)", detail: fmt.Sprintf("Level %d%%", vol), icon: "volume", action: sceneAction{"volume-step", "10"}},
	}
	return renderPixelListWithHits(spec, "Volume", items, "", page, th, th.accent2)
}

func renderPixelVoiceWithHits(spec pixelDisplaySpec, current string, page int, th pixelTheme) (*image.RGBA, []hitBox, map[string]sceneAction) {
	if th.border.A == 0 {
		th = defaultPixelTheme()
	}
	items := make([]pixelMenuItem, 0, len(voiceCatalog))
	for _, v := range voiceCatalog {
		items = append(items, pixelMenuItem{
			id:     v.ID,
			label:  v.Name,
			icon:   "voice",
			accent: th.accent2,
		})
	}
	hits, actions := menuGridHits(spec, items, page, func(item pixelMenuItem) (string, sceneAction) {
		return "voice-" + item.id, sceneAction{"voice-set", item.id}
	})
	return renderPixelMenuGrid(spec, "Voice", items, page, th, current), hits, actions
}

func renderPixelStatsWithHits(spec pixelDisplaySpec, rows []menuRow, th pixelTheme) (*image.RGBA, []hitBox, map[string]sceneAction) {
	items := make([]tile, 0, len(rows))
	for i, r := range rows {
		items = append(items, tile{id: fmt.Sprintf("stat-%d", i), label: r.title, detail: r.text, icon: "stats"})
	}
	return renderPixelListWithHits(spec, "Stats", items, "No stats", 0, th, th.accent2)
}

func renderPixelPairWithHits(spec pixelDisplaySpec, companionName, invite string) (*image.RGBA, []hitBox, map[string]sceneAction, error) {
	img, err := renderPixelPair(spec, companionName, invite)
	if err != nil {
		return nil, nil, nil, err
	}
	// Tapping anywhere outside the QR closes Pair.
	hits := []hitBox{{id: "pair-close", x1: 0, y1: 0, x2: spec.logicalW*spec.artScale - 1, y2: spec.logicalH*spec.artScale - 1}}
	actions := map[string]sceneAction{"pair-close": {"menu-back", ""}}
	return img, hits, actions, nil
}
