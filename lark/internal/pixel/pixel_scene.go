package pixel

import (
	"fmt"
	"image"
	imagecolor "image/color"
	"strconv"

	"lark/fonts/dogica"
)

const DrawerIntercom = "intercom"

type Action struct{ Kind, Value string }

type IntercomCard struct{ Companion, Reply, Heading, Text string }

type Scene struct {
	Title, Heading, Text string
	Intercom             *IntercomCard
	Actions              map[string]Action
	DuringTurn           bool
}

type Row struct{ Title, Text string }

type Tile struct {
	ID, Label, Detail, Icon string
	Action                  Action
	Saved                   bool
}

func AddLabeledHit(hits []HitBox, spec Spec, id, label string, r image.Rectangle) []HitBox {
	if id == "" || r.Empty() {
		return hits
	}
	s := spec.ArtScale
	return append(hits, HitBox{
		ID:    id,
		Label: label,
		X1:    r.Min.X * s,
		Y1:    r.Min.Y * s,
		X2:    r.Max.X*s - 1,
		Y2:    r.Max.Y*s - 1,
	})
}

func AddLogicalHit(hits []HitBox, spec Spec, id string, r image.Rectangle) []HitBox {
	return AddLabeledHit(hits, spec, id, "", r)
}

func NavHits(spec Spec, page, totalPages int) ([]HitBox, map[string]Action) {
	navH := NavHeight(spec)
	y := spec.Safe.Max.Y - navH
	x0 := spec.Safe.Min.X
	w := spec.Safe.Dx()
	gap := 3
	btnW := (w - 2*gap) / 3
	leftRect := image.Rect(x0, y, x0+btnW, y+navH)
	midRect := image.Rect(x0+btnW+gap, y, x0+2*btnW+gap, y+navH)
	rightRect := image.Rect(x0+2*(btnW+gap), y, x0+w, y+navH)

	var hits []HitBox
	actions := map[string]Action{}
	hits = AddLabeledHit(hits, spec, "menu-back", "Back", midRect)
	actions["menu-back"] = Action{"menu-back", ""}
	if page > 0 {
		hits = AddLabeledHit(hits, spec, "menu-prev", "Prev", leftRect)
		actions["menu-prev"] = Action{"menu-page", strconv.Itoa(page - 1)}
	}
	if page+1 < totalPages {
		hits = AddLabeledHit(hits, spec, "menu-next", "Next", rightRect)
		actions["menu-next"] = Action{"menu-page", strconv.Itoa(page + 1)}
	}
	return hits, actions
}

func RenderSceneCard(spec Spec, v Scene, th Theme) (*image.RGBA, []HitBox) {
	if th.Border.A == 0 {
		th = DefaultTheme()
	}
	c := newPixelCanvas(spec.LogicalW, spec.LogicalH)
	darkenBackground(c, th.Bg)
	s := spec.ArtScale
	var texts []pixelTextOp

	titleH := 12
	titleStr := v.Title
	if titleStr == "" {
		titleStr = "Lark"
	}
	titleBox := PhysicalRect(image.Rect(spec.Safe.Min.X, spec.Safe.Min.Y, spec.Safe.Max.X, spec.Safe.Min.Y+titleH), s)
	texts = addCenteredText(texts, titleBox, truncateEllipsisPx(titleStr, dogica.Bold, spec.BodyTextScale, titleBox.Dx()), dogica.Bold, spec.BodyTextScale, th.Accent)

	btnH := NavHeight(spec)
	btnRect := image.Rect(spec.Safe.Min.X, spec.Safe.Max.Y-btnH, spec.Safe.Max.X, spec.Safe.Max.Y)
	panelRect := image.Rect(spec.Safe.Min.X, spec.Safe.Min.Y+titleH+2, spec.Safe.Max.X, btnRect.Min.Y-3)
	c.panel(panelRect.Min.X, panelRect.Min.Y, panelRect.Dx(), panelRect.Dy(), pxTileBG, th.Border)

	body := v.Text
	if v.Heading != "" {
		body = v.Heading + "\n" + body
	}
	physPanel := PhysicalRect(panelRect, s)
	lineH := dogica.Regular.LineHeight * spec.BodyTextScale
	lines := wrapTextPx(body, dogica.Regular, spec.BodyTextScale, physPanel.Dx()-16)
	maxLines := max(1, (physPanel.Dy()-16)/lineH)
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	for i, line := range lines {
		texts = append(texts, pixelTextOp{
			x: physPanel.Min.X + 8, y: physPanel.Min.Y + 8 + i*lineH,
			text: line, font: dogica.Regular, scale: spec.BodyTextScale, color: pxInk,
		})
	}

	var hits []HitBox
	if v.Intercom != nil {
		gap := 4
		bw := (btnRect.Dx() - gap) / 2
		r1 := image.Rect(btnRect.Min.X, btnRect.Min.Y, btnRect.Min.X+bw, btnRect.Max.Y)
		r2 := image.Rect(btnRect.Min.X+bw+gap, btnRect.Min.Y, btnRect.Max.X, btnRect.Max.Y)
		c.panel(r1.Min.X, r1.Min.Y, r1.Dx(), r1.Dy(), pxTileBG, th.Border)
		c.panel(r2.Min.X, r2.Min.Y, r2.Dx(), r2.Dy(), pxTileBG, th.Accent)
		texts = addCenteredText(texts, PhysicalRect(r1, s), "Close", dogica.Regular, spec.BodyTextScale, pxInk)
		texts = addCenteredText(texts, PhysicalRect(r2, s), "Reply", dogica.Bold, spec.BodyTextScale, th.Accent)
		hits = AddLogicalHit(hits, spec, "close", r1)
		hits = AddLogicalHit(hits, spec, "reply", r2)
	} else {
		c.panel(btnRect.Min.X, btnRect.Min.Y, btnRect.Dx(), btnRect.Dy(), pxTileBG, th.Border)
		texts = addCenteredText(texts, PhysicalRect(btnRect, s), "OK", dogica.Bold, spec.BodyTextScale, pxInk)
		hits = AddLogicalHit(hits, spec, "close", image.Rect(0, 0, spec.LogicalW, spec.LogicalH))
	}
	return finishPixelScreen(c, spec, texts), hits
}

func RenderListWithHits(spec Spec, title string, items []Tile, empty string, page int, th Theme, accent imagecolor.RGBA) (*image.RGBA, []HitBox, map[string]Action) {
	if th.Border.A == 0 {
		th = DefaultTheme()
	}
	c := newPixelCanvas(spec.LogicalW, spec.LogicalH)
	darkenBackground(c, th.Bg)
	s := spec.ArtScale
	var texts []pixelTextOp

	titleH := 12
	titleRect := image.Rect(spec.Safe.Min.X, spec.Safe.Min.Y, spec.Safe.Max.X, spec.Safe.Min.Y+titleH)
	texts = addCenteredText(texts, PhysicalRect(titleRect, s), title, dogica.Bold, spec.TitleTextScale, accent)

	perPage := 2
	if !spec.Round {
		perPage = 3
	}
	navH := NavHeight(spec)
	totalPages := max(1, (len(items)+perPage-1)/perPage)
	page = max(0, min(page, totalPages-1))

	listTop := spec.Safe.Min.Y + titleH + 2
	listBottom := spec.Safe.Max.Y - navH - 3
	gapY := 3
	rowH := (listBottom - listTop - (perPage-1)*gapY) / perPage

	hits, actions := NavHits(spec, page, totalPages)
	if len(items) == 0 {
		panel := image.Rect(spec.Safe.Min.X, listTop, spec.Safe.Max.X, listBottom)
		c.panel(panel.Min.X, panel.Min.Y, panel.Dx(), panel.Dy(), pxTileBG, th.Border)
		texts = addCenteredText(texts, PhysicalRect(panel, s), empty, dogica.Regular, spec.BodyTextScale, pxMuted)
		texts = drawNavBar(c, spec, texts, page, totalPages, th.Border)
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
		rx := spec.Safe.Min.X
		rw := spec.Safe.Dx()
		rowRect := image.Rect(rx, ry, rx+rw, ry+rowH)
		c.panel(rx, ry, rw, rowH, pxTileBG, th.Border)
		if it.ID != "" {
			hits = AddLabeledHit(hits, spec, it.ID, it.Label, rowRect)
			actions[it.ID] = it.Action
		}
		if sprite, ok := pxMenuSprites[it.Icon]; ok {
			c.blitSprite(rx+4, ry+(rowH-12)/2, sprite, pxIconPalette, 1)
		}
		textX := rx + 4
		if it.Icon != "" {
			textX = rx + 19
		}
		physText := PhysicalRect(image.Rect(textX, ry+2, rx+rw-3, ry+rowH-2), s)
		lbl := truncateEllipsisPx(it.Label, dogica.Bold, spec.BodyTextScale, physText.Dx())
		if it.Detail != "" {
			det := truncateEllipsisPx(it.Detail, dogica.Regular, spec.BodyTextScale, physText.Dx())
			lineH := dogica.Regular.LineHeight * spec.BodyTextScale
			topY := physText.Min.Y + max(0, (physText.Dy()-2*lineH)/2)
			texts = append(texts,
				pixelTextOp{x: physText.Min.X, y: topY, text: lbl, font: dogica.Bold, scale: spec.BodyTextScale, color: pxInk},
				pixelTextOp{x: physText.Min.X, y: topY + lineH, text: det, font: dogica.Regular, scale: spec.BodyTextScale, color: pxMuted},
			)
		} else {
			thPx := dogica.Bold.Ascent * spec.BodyTextScale
			texts = append(texts, pixelTextOp{
				x: physText.Min.X, y: physText.Min.Y + (physText.Dy()-thPx)/2,
				text: lbl, font: dogica.Bold, scale: spec.BodyTextScale, color: pxInk,
			})
		}
	}

	texts = drawNavBar(c, spec, texts, page, totalPages, th.Border)
	return finishPixelScreen(c, spec, texts), hits, actions
}

func menuGridHits(spec Spec, items []pixelMenuItem, page int, actionFn func(pixelMenuItem) (string, Action)) ([]HitBox, map[string]Action) {
	cols, rows := 1, 3
	if !spec.Round {
		cols, rows = 2, 3
	}
	navH := NavHeight(spec)
	perPage := cols * rows
	totalPages := max(1, (len(items)+perPage-1)/perPage)
	page = max(0, min(page, totalPages-1))

	gridTop := spec.Safe.Min.Y + 12 + 2
	gridBottom := spec.Safe.Max.Y - navH - 2
	gapX, gapY := 4, 2
	if !spec.Round {
		gapX = 2
	}
	tileW := (spec.Safe.Dx() - (cols-1)*gapX) / cols
	tileH := (gridBottom - gridTop - (rows-1)*gapY) / rows

	hits, actions := NavHits(spec, page, totalPages)
	start := page * perPage
	for idx := range perPage {
		itemIdx := start + idx
		if itemIdx >= len(items) {
			break
		}
		item := items[itemIdx]
		col := idx % cols
		row := idx / cols
		tx := spec.Safe.Min.X + col*(tileW+gapX)
		ty := gridTop + row*(tileH+gapY)
		id, act := actionFn(item)
		hits = AddLabeledHit(hits, spec, id, item.Label, image.Rect(tx, ty, tx+tileW, ty+tileH))
		actions[id] = act
	}
	return hits, actions
}

func RenderMenuWithHits(spec Spec, page int, th Theme) (*image.RGBA, []HitBox, map[string]Action) {
	if th.Border.A == 0 {
		th = DefaultTheme()
	}
	hits, actions := menuGridHits(spec, HomeMenuItems, page, func(item pixelMenuItem) (string, Action) {
		if item.ID == "talk" {
			return "menu-intercom", Action{"menu-open", DrawerIntercom}
		}
		return "menu-" + item.ID, Action{"menu-open", item.ID}
	})
	return RenderMenu(spec, page, th), hits, actions
}

func RenderVolumeWithHits(spec Spec, vol, page int, th Theme) (*image.RGBA, []HitBox, map[string]Action) {
	items := []Tile{
		{ID: "vol-down", Label: "Quieter (-10)", Detail: fmt.Sprintf("Level %d%%", vol), Icon: "volume", Action: Action{"volume-step", "-10"}},
		{ID: "vol-up", Label: "Louder (+10)", Detail: fmt.Sprintf("Level %d%%", vol), Icon: "volume", Action: Action{"volume-step", "10"}},
	}
	return RenderListWithHits(spec, "Volume", items, "", page, th, th.Accent2)
}

func RenderVoiceWithHits(spec Spec, voices []Tile, current string, page int, th Theme) (*image.RGBA, []HitBox, map[string]Action) {
	if th.Border.A == 0 {
		th = DefaultTheme()
	}
	items := make([]pixelMenuItem, 0, len(voices))
	for _, v := range voices {
		items = append(items, pixelMenuItem{
			ID:     v.ID,
			Label:  v.Label,
			Icon:   "voice",
			Accent: th.Accent2,
		})
	}
	hits, actions := menuGridHits(spec, items, page, func(item pixelMenuItem) (string, Action) {
		return "voice-" + item.ID, Action{"voice-set", item.ID}
	})
	return renderPixelMenuGrid(spec, "Voice", items, page, th, current), hits, actions
}

func RenderStatsWithHits(spec Spec, rows []Row, th Theme) (*image.RGBA, []HitBox, map[string]Action) {
	items := make([]Tile, 0, len(rows))
	for i, r := range rows {
		items = append(items, Tile{ID: fmt.Sprintf("stat-%d", i), Label: r.Title, Detail: r.Text, Icon: "stats"})
	}
	return RenderListWithHits(spec, "Stats", items, "No stats", 0, th, th.Accent2)
}

func RenderPairWithHits(spec Spec, companionName, invite string) (*image.RGBA, []HitBox, map[string]Action, error) {
	img, err := RenderPair(spec, companionName, invite)
	if err != nil {
		return nil, nil, nil, err
	}
	// Tapping anywhere outside the QR closes Pair.
	hits := []HitBox{{ID: "pair-close", X1: 0, Y1: 0, X2: spec.LogicalW*spec.ArtScale - 1, Y2: spec.LogicalH*spec.ArtScale - 1}}
	actions := map[string]Action{"pair-close": {"menu-back", ""}}
	return img, hits, actions, nil
}
