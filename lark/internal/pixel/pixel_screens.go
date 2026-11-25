package pixel

import (
	"encoding/json"
	"fmt"
	"image"
	imagecolor "image/color"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"lark/fonts/dogica"
	"rsc.io/qr"
)

// maxStoryChoices caps the choice buttons the story reader lays out.
const maxStoryChoices = 3

type pixelTextOp struct {
	x, y  int
	text  string
	font  dogica.Font
	scale int
	color imagecolor.RGBA
}

type pixelMenuItem struct {
	ID, Label, Icon string
	Accent          imagecolor.RGBA
}

var HomeMenuItems = []pixelMenuItem{
	{ID: "talk", Label: "Talk to…", Icon: "talk", Accent: Peach},
	{ID: "stories", Label: "Stories", Icon: "books", Accent: Mint},
	{ID: "memories", Label: "Memories", Icon: "memories", Accent: Danger},
	{ID: "voice", Label: "Voice", Icon: "voice", Accent: Mint},
	{ID: "more", Label: "More", Icon: "stats", Accent: Sky},
}

var MoreMenuItems = []pixelMenuItem{
	{ID: "volume", Label: "Volume", Icon: "volume", Accent: Sky},
	{ID: "pair", Label: "Pair", Icon: "pair", Accent: Peach},
	{ID: "unpair", Label: "Unpair", Icon: "unpair", Accent: Danger},
	{ID: "stats", Label: "Stats", Icon: "stats", Accent: Mint},
}

type Story struct {
	ID       string
	Title    string
	Passage  string
	Passages int
	Color    imagecolor.RGBA
}

type MemoryItem struct {
	Section string
	Key     string
	Value   string // raw JSON
}

func finishPixelScreen(c *pixelCanvas, spec Spec, texts []pixelTextOp) *image.RGBA {
	defer releasePixelCanvas(c)
	phys := c.scaleNearest(spec.ArtScale)
	for _, t := range texts {
		drawTextPhysical(phys, t.x, t.y, t.text, t.font, t.scale, t.color)
	}
	return phys
}

func addCenteredText(texts []pixelTextOp, box image.Rectangle, s string, f dogica.Font, scale int, col imagecolor.RGBA) []pixelTextOp {
	tw := measureTextPx(s, f, scale)
	th := f.Ascent * scale
	tx := box.Min.X + (box.Dx()-tw)/2
	ty := box.Min.Y + (box.Dy()-th)/2
	return append(texts, pixelTextOp{x: tx, y: ty, text: s, font: f, scale: scale, color: col})
}

func PhysicalRect(r image.Rectangle, artScale int) image.Rectangle {
	return image.Rect(r.Min.X*artScale, r.Min.Y*artScale, r.Max.X*artScale, r.Max.Y*artScale)
}

func NavHeight(spec Spec) int {
	if !spec.Round {
		return 24 // 48 px physical on BOX-3 (2x)
	}
	return 16 // 48 px physical on VoCat (3x)
}

func drawNavBar(c *pixelCanvas, spec Spec, texts []pixelTextOp, page, totalPages int, border imagecolor.RGBA) []pixelTextOp {
	s := spec.ArtScale
	navH := NavHeight(spec)
	y := spec.Safe.Max.Y - navH
	x0 := spec.Safe.Min.X
	w := spec.Safe.Dx()
	gap := 3
	btnW := (w - 2*gap) / 3

	leftRect := image.Rect(x0, y, x0+btnW, y+navH)
	midRect := image.Rect(x0+btnW+gap, y, x0+2*btnW+gap, y+navH)
	rightRect := image.Rect(x0+2*(btnW+gap), y, x0+w, y+navH)

	if page > 0 {
		c.panel(leftRect.Min.X, leftRect.Min.Y, leftRect.Dx(), leftRect.Dy(), pxTileBG, border)
		texts = addCenteredText(texts, PhysicalRect(leftRect, s), "< Prev", dogica.Regular, spec.BodyTextScale, pxInk)
	} else if totalPages > 1 {
		texts = addCenteredText(texts, PhysicalRect(leftRect, s), fmt.Sprintf("%d/%d", page+1, totalPages), dogica.Regular, spec.BodyTextScale, pxMuted)
	}

	c.panel(midRect.Min.X, midRect.Min.Y, midRect.Dx(), midRect.Dy(), pxTileBG, border)
	texts = addCenteredText(texts, PhysicalRect(midRect, s), "Back", dogica.Regular, spec.BodyTextScale, pxInk)

	if page+1 < totalPages {
		c.panel(rightRect.Min.X, rightRect.Min.Y, rightRect.Dx(), rightRect.Dy(), pxTileBG, border)
		texts = addCenteredText(texts, PhysicalRect(rightRect, s), "Next >", dogica.Regular, spec.BodyTextScale, pxInk)
	} else if page > 0 && totalPages > 1 {
		texts = addCenteredText(texts, PhysicalRect(rightRect, s), fmt.Sprintf("%d/%d", page+1, totalPages), dogica.Regular, spec.BodyTextScale, pxMuted)
	}
	return texts
}

func blitCenteredBG(c *pixelCanvas, bg *image.RGBA) {
	if bg == nil {
		drawDefaultPixelBackground(c)
		return
	}
	b := bg.Bounds()
	ox := (c.w - b.Dx()) / 2
	oy := (c.h - b.Dy()) / 2
	c.blit(ox, oy, bg)
}

func HomeMenuButtonRect(spec Spec) image.Rectangle {
	btnW, btnH := 24, NavHeight(spec)
	if !spec.Round {
		btnW = 36
	}
	return image.Rect(spec.Safe.Max.X-btnW, spec.Safe.Max.Y-btnH, spec.Safe.Max.X, spec.Safe.Max.Y)
}

func RenderHome(spec Spec, bg, poseImg *image.RGBA, grade GradeKey, status string, caption string) *image.RGBA {
	return RenderHomeListen(spec, bg, poseImg, grade, status, caption, false)
}

func RenderHomeListen(spec Spec, bg, poseImg *image.RGBA, grade GradeKey, status string, caption string, listenWindow bool) *image.RGBA {
	c := newPixelCanvas(spec.LogicalW, spec.LogicalH)
	blitCenteredBG(c, bg)
	if poseImg != nil {
		b := poseImg.Bounds()
		px := (spec.LogicalW - b.Dx()) / 2
		py := spec.Safe.Min.Y + 2
		if caption != "" {
			py = max(4, spec.Safe.Min.Y-4)
		}
		c.blit(px, py, poseImg)
	}

	// Grade applies only to the world + companion, never the UI panels or text.
	c.applyLUT(grade)

	var texts []pixelTextOp
	s := spec.ArtScale

	// Status icon in top-right corner of the safe area (no ring outline).
	if sprite, ok := pxStatusSprites[status]; ok {
		sx := spec.Safe.Max.X - 11
		sy := spec.Safe.Min.Y + 1
		c.panel(sx-1, sy-1, 10, 10, pxPanelBG, pxPanelBorder)
		c.blitSprite(sx, sy, sprite, pxIconPalette, 1)
	}

	if caption != "" {
		boxH := 30
		if !spec.Round {
			boxH = 34
		}
		box := image.Rect(spec.Safe.Min.X, spec.Safe.Max.Y-boxH, spec.Safe.Max.X, spec.Safe.Max.Y)
		c.panel(box.Min.X, box.Min.Y, box.Dx(), box.Dy(), pxPanelBG, pxPanelBorder)
		physBox := PhysicalRect(box, s)
		padX, padY := 8, 8
		maxW := physBox.Dx() - 2*padX
		lineH := dogica.Regular.LineHeight * spec.BodyTextScale
		maxLines := max(1, (physBox.Dy()-2*padY)/lineH)
		lines := wrapTextPx(caption, dogica.Regular, spec.BodyTextScale, maxW)
		if len(lines) > maxLines {
			lines = lines[:maxLines]
			lines[maxLines-1] = truncateEllipsisPx(lines[maxLines-1]+" …", dogica.Regular, spec.BodyTextScale, maxW)
		}
		for i, line := range lines {
			texts = append(texts, pixelTextOp{
				x: physBox.Min.X + padX, y: physBox.Min.Y + padY + i*lineH,
				text: line, font: dogica.Regular, scale: spec.BodyTextScale, color: pxInk,
			})
		}
	} else if status == "listening" && listenWindow {
		btnW, btnH := 56, NavHeight(spec)
		if !spec.Round {
			btnW = 68
		}
		btn := image.Rect((spec.LogicalW-btnW)/2, spec.Safe.Max.Y-btnH, (spec.LogicalW+btnW)/2, spec.Safe.Max.Y)
		c.panel(btn.Min.X, btn.Min.Y, btn.Dx(), btn.Dy(), pxPanelBG, Mint)
		texts = addCenteredText(texts, PhysicalRect(btn, s), "Listening", dogica.Regular, spec.BodyTextScale, Mint)
	} else if status == "listening" {
		btnH := NavHeight(spec)
		y := spec.Safe.Max.Y - btnH
		gap := 4
		bw := (spec.Safe.Dx() - gap) / 2
		left := image.Rect(spec.Safe.Min.X, y, spec.Safe.Min.X+bw, y+btnH)
		right := image.Rect(spec.Safe.Min.X+bw+gap, y, spec.Safe.Max.X, y+btnH)
		c.panel(left.Min.X, left.Min.Y, left.Dx(), left.Dy(), pxTileBG, Danger)
		c.panel(right.Min.X, right.Min.Y, right.Dx(), right.Dy(), pxTileBG, Mint)
		texts = addCenteredText(texts, PhysicalRect(left, s), "Cancel", dogica.Regular, spec.BodyTextScale, Danger)
		texts = addCenteredText(texts, PhysicalRect(right, s), "Send", dogica.Bold, spec.BodyTextScale, Mint)
	} else if status == "thinking" || status == "speaking" {
		btnW, btnH := 42, NavHeight(spec)
		if !spec.Round {
			btnW = 48
		}
		btn := image.Rect((spec.LogicalW-btnW)/2, spec.Safe.Max.Y-btnH, (spec.LogicalW+btnW)/2, spec.Safe.Max.Y)
		c.panel(btn.Min.X, btn.Min.Y, btn.Dx(), btn.Dy(), pxPanelBG, Danger)
		texts = addCenteredText(texts, PhysicalRect(btn, s), "Stop", dogica.Regular, spec.BodyTextScale, Danger)
	} else if status == "" {
		btn := HomeMenuButtonRect(spec)
		c.panel(btn.Min.X, btn.Min.Y, btn.Dx(), btn.Dy(), pxPanelBG, pxPanelBorder)
		texts = addCenteredText(texts, PhysicalRect(btn, s), "Menu", dogica.Regular, spec.BodyTextScale, pxInk)
	}

	return finishPixelScreen(c, spec, texts)
}

func RenderMenu(spec Spec, page int, th Theme) *image.RGBA {
	return renderPixelMenuGrid(spec, "Menu", HomeMenuItems, page, th, "")
}

func renderPixelMenuGrid(spec Spec, title string, items []pixelMenuItem, page int, th Theme, activeID string) *image.RGBA {
	if th.Border.A == 0 {
		th = DefaultTheme()
	}
	c := newPixelCanvas(spec.LogicalW, spec.LogicalH)
	darkenBackground(c, th.Bg)
	s := spec.ArtScale
	var texts []pixelTextOp

	titleH := 12
	titleRect := image.Rect(spec.Safe.Min.X, spec.Safe.Min.Y, spec.Safe.Max.X, spec.Safe.Min.Y+titleH)
	texts = addCenteredText(texts, PhysicalRect(titleRect, s), title, dogica.Bold, spec.TitleTextScale, th.Accent)

	cols, rows := 1, 3
	if !spec.Round {
		cols, rows = 2, 3
	}
	navH := NavHeight(spec)
	perPage := cols * rows
	totalPages := max(1, (len(items)+perPage-1)/perPage)
	page = max(0, min(page, totalPages-1))

	gridTop := spec.Safe.Min.Y + titleH + 2
	gridBottom := spec.Safe.Max.Y - navH - 2
	gapX, gapY := 4, 2
	if !spec.Round {
		gapX = 2
	}
	tileW := (spec.Safe.Dx() - (cols-1)*gapX) / cols
	tileH := (gridBottom - gridTop - (rows-1)*gapY) / rows

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
		border := th.Border
		ink := pxInk
		if activeID != "" && item.ID == activeID {
			border = Mint
			ink = Mint
		}
		c.panel(tx, ty, tileW, tileH, pxTileBG, border)
		iconX, labelX := tx+4, tx+19
		if !spec.Round {
			iconX, labelX = tx+3, tx+17
		}
		if sprite, ok := pxMenuSprites[item.Icon]; ok {
			c.blitSprite(iconX, ty+(tileH-12)/2, sprite, pxIconPalette, 1)
		}
		labelBox := image.Rect(labelX, ty+1, tx+tileW-2, ty+tileH-1)
		physLabel := PhysicalRect(labelBox, s)
		label := truncateEllipsisPx(item.Label, dogica.Regular, spec.BodyTextScale, physLabel.Dx())
		thPx := dogica.Regular.Ascent * spec.BodyTextScale
		texts = append(texts, pixelTextOp{
			x: physLabel.Min.X, y: physLabel.Min.Y + (physLabel.Dy()-thPx)/2,
			text: label, font: dogica.Regular, scale: spec.BodyTextScale, color: ink,
		})
	}

	texts = drawNavBar(c, spec, texts, page, totalPages, th.Border)
	return finishPixelScreen(c, spec, texts)
}

func RenderStoriesList(spec Spec, stories []Story, page int, th Theme) *image.RGBA {
	if th.Border.A == 0 {
		th = DefaultTheme()
	}
	c := newPixelCanvas(spec.LogicalW, spec.LogicalH)
	darkenBackground(c, th.Bg)
	s := spec.ArtScale
	var texts []pixelTextOp

	titleH := 12
	titleRect := image.Rect(spec.Safe.Min.X, spec.Safe.Min.Y, spec.Safe.Max.X, spec.Safe.Min.Y+titleH)
	texts = addCenteredText(texts, PhysicalRect(titleRect, s), "Stories", dogica.Bold, spec.TitleTextScale, th.Accent2)

	perPage := 2
	if !spec.Round {
		perPage = 3
	}
	navH := NavHeight(spec)
	totalPages := max(1, (len(stories)+perPage-1)/perPage)
	page = max(0, min(page, totalPages-1))

	listTop := spec.Safe.Min.Y + titleH + 2
	listBottom := spec.Safe.Max.Y - navH - 3
	gapY := 3
	rowH := (listBottom - listTop - (perPage-1)*gapY) / perPage

	start := page * perPage
	for i := range perPage {
		idx := start + i
		if idx >= len(stories) {
			break
		}
		st := stories[idx]
		ry := listTop + i*(rowH+gapY)
		rx := spec.Safe.Min.X
		rw := spec.Safe.Dx()
		c.panel(rx, ry, rw, rowH, pxTileBG, th.Border)

		coverCol := st.Color
		if coverCol.A == 0 {
			coverCol = th.Accent
		}
		coverX, coverY := rx+3, ry+(rowH-14)/2
		c.panel(coverX, coverY, 14, 14, coverCol, pxInk)
		c.blitSprite(coverX+1, coverY+1, pxMenuSprites["books"], pxIconPalette, 1)

		physText := PhysicalRect(image.Rect(rx+20, ry+2, rx+rw-3, ry+rowH-2), s)
		title := truncateEllipsisPx(st.Title, dogica.Bold, spec.BodyTextScale, physText.Dx())
		prog := fmt.Sprintf("%d scenes", max(1, st.Passages))
		if st.Passage != "" {
			prog = truncateEllipsisPx(st.Passage, dogica.Regular, spec.BodyTextScale, physText.Dx())
		}
		lineH := dogica.Regular.LineHeight * spec.BodyTextScale
		topY := physText.Min.Y + max(0, (physText.Dy()-2*lineH)/2)
		texts = append(texts,
			pixelTextOp{x: physText.Min.X, y: topY, text: title, font: dogica.Bold, scale: spec.BodyTextScale, color: pxInk},
			pixelTextOp{x: physText.Min.X, y: topY + lineH, text: prog, font: dogica.Regular, scale: spec.BodyTextScale, color: pxMuted},
		)
	}

	texts = drawNavBar(c, spec, texts, page, totalPages, th.Border)
	return finishPixelScreen(c, spec, texts)
}

func wrapTitleLines(title string, f dogica.Font, scale, maxW, maxLines int) []string {
	lines := wrapTextPx(title, f, scale, maxW)
	if len(lines) <= maxLines {
		return lines
	}
	out := slices.Clone(lines[:maxLines])
	out[maxLines-1] = truncateEllipsisPx(out[maxLines-1]+" …", f, scale, maxW)
	return out
}

func RenderReader(spec Spec, title, body string, page, bodyScaleOverride int, th Theme) *image.RGBA {
	if th.Border.A == 0 {
		th = DefaultTheme()
	}
	c := newPixelCanvas(spec.LogicalW, spec.LogicalH)
	darkenBackground(c, th.Bg)
	s := spec.ArtScale
	bodyScale := spec.BodyTextScale
	if bodyScaleOverride > 0 {
		bodyScale = bodyScaleOverride
	}
	var texts []pixelTextOp

	safePhys := PhysicalRect(spec.Safe, s)
	titleLines := wrapTitleLines(title, dogica.Bold, spec.BodyTextScale, safePhys.Dx(), 2)
	titleLineH := dogica.Bold.LineHeight * spec.BodyTextScale
	titlePhysH := max(12*s, len(titleLines)*titleLineH)
	titleH := (titlePhysH + s - 1) / s
	for i, line := range titleLines {
		lineBox := image.Rect(safePhys.Min.X, safePhys.Min.Y+i*titleLineH, safePhys.Max.X, safePhys.Min.Y+(i+1)*titleLineH)
		texts = addCenteredText(texts, lineBox, line, dogica.Bold, spec.BodyTextScale, th.Accent2)
	}

	navH := NavHeight(spec)
	panelRect := image.Rect(spec.Safe.Min.X, spec.Safe.Min.Y+titleH+1, spec.Safe.Max.X, spec.Safe.Max.Y-navH-2)
	c.panel(panelRect.Min.X, panelRect.Min.Y, panelRect.Dx(), panelRect.Dy(), pxTileBG, th.Border)

	physPanel := PhysicalRect(panelRect, s)
	padX, padY := 10, 10
	lineH := dogica.Regular.LineHeight * bodyScale
	lines := wrapTextPx(body, dogica.Regular, bodyScale, physPanel.Dx()-2*padX)
	pages := paginateLines(lines, lineH, physPanel.Dy()-2*padY)
	page = max(0, min(page, len(pages)-1))

	for i, line := range pages[page] {
		texts = append(texts, pixelTextOp{
			x: physPanel.Min.X + padX, y: physPanel.Min.Y + padY + i*lineH,
			text: line, font: dogica.Regular, scale: bodyScale, color: pxInk,
		})
	}

	texts = drawNavBar(c, spec, texts, page, len(pages), th.Border)
	return finishPixelScreen(c, spec, texts)
}

func RenderStoryReaderWithHits(spec Spec, title, body string, choices []string, end bool, page int, bg, narrator *image.RGBA, th Theme) (*image.RGBA, []HitBox, map[string]Action, int) {
	if th.Border.A == 0 {
		th = DefaultTheme()
	}
	if bg == nil {
		bg = th.Bg
	}
	c := newPixelCanvas(spec.LogicalW, spec.LogicalH)
	darkenBackground(c, bg)
	if narrator != nil {
		nb := narrator.Bounds()
		nx := spec.Safe.Max.X - nb.Dx()/2 - 4
		ny := max(2, spec.Safe.Min.Y-2)
		c.blit(nx, ny, narrator)
	}

	s := spec.ArtScale
	bodyScale := spec.BodyTextScale
	var texts []pixelTextOp

	safePhys := PhysicalRect(spec.Safe, s)
	titleLines := wrapTitleLines(title, dogica.Bold, spec.BodyTextScale, safePhys.Dx(), 1)
	titleLineH := dogica.Bold.LineHeight * spec.BodyTextScale
	titleH := max(10, (titleLineH+s-1)/s)

	navH := NavHeight(spec)
	panelRect := image.Rect(spec.Safe.Min.X, spec.Safe.Min.Y+titleH+1, spec.Safe.Max.X, spec.Safe.Max.Y-navH-2)
	physPanel := PhysicalRect(panelRect, s)
	padX, padY := 10, 8
	lineH := dogica.Regular.LineHeight * bodyScale
	lines := wrapTextPx(body, dogica.Regular, bodyScale, physPanel.Dx()-2*padX)
	pages := paginateLines(lines, lineH, physPanel.Dy()-2*padY)
	textPages := len(pages)
	totalPages := textPages
	if len(choices) > 0 {
		totalPages++
	}
	page = max(0, min(page, totalPages-1))

	if len(choices) > 0 && page == totalPages-1 {
		// Choice page: up to 3 choices, each >= 48 physical px tall, plus Prev/Back.
		choiceH := max(16, (48+s-1)/s)
		topY := spec.Safe.Min.Y
		bottomY := spec.Safe.Max.Y
		tH := titleH
		gapY := 2
		btnNavH := max(navH, choiceH)
		if topY+tH+2+len(choices)*choiceH+(len(choices)-1)*gapY+2+btnNavH > bottomY {
			tH = 7
			gapY = 0
			btnNavH = choiceH
		}
		tBox := PhysicalRect(image.Rect(spec.Safe.Min.X, topY, spec.Safe.Max.X, topY+tH), s)
		texts = addCenteredText(texts, tBox, truncateEllipsisPx(title, dogica.Bold, spec.BodyTextScale, tBox.Dx()), dogica.Bold, spec.BodyTextScale, th.Accent2)

		listTop := topY + tH + 1
		if gapY > 0 {
			listTop = topY + tH + 2
		}
		hits := make([]HitBox, 0, len(choices)+2)
		acts := make(map[string]Action, len(choices)+2)
		for i, ch := range choices {
			if i >= maxStoryChoices {
				break
			}
			ry := listTop + i*(choiceH+gapY)
			r := image.Rect(spec.Safe.Min.X, ry, spec.Safe.Max.X, ry+choiceH)
			c.panel(r.Min.X, r.Min.Y, r.Dx(), r.Dy(), pxTileBG, th.Accent)
			pBox := PhysicalRect(image.Rect(r.Min.X+3, r.Min.Y+1, r.Max.X-3, r.Max.Y-1), s)
			label := truncateEllipsisPx(ch, dogica.Bold, bodyScale, pBox.Dx())
			texts = addCenteredText(texts, pBox, label, dogica.Bold, bodyScale, pxInk)
			id := fmt.Sprintf("choice-%d", i)
			hits = AddLabeledHit(hits, spec, id, ch, r)
			acts[id] = Action{"story-choice", strconv.Itoa(i)}
		}
		navY := bottomY - btnNavH
		gap := 4
		bw := (spec.Safe.Dx() - gap) / 2
		prevR := image.Rect(spec.Safe.Min.X, navY, spec.Safe.Min.X+bw, bottomY)
		backR := image.Rect(spec.Safe.Min.X+bw+gap, navY, spec.Safe.Max.X, bottomY)
		c.panel(prevR.Min.X, prevR.Min.Y, prevR.Dx(), prevR.Dy(), pxTileBG, th.Border)
		c.panel(backR.Min.X, backR.Min.Y, backR.Dx(), backR.Dy(), pxTileBG, th.Border)
		texts = addCenteredText(texts, PhysicalRect(prevR, s), "Prev", dogica.Bold, bodyScale, pxInk)
		texts = addCenteredText(texts, PhysicalRect(backR, s), "Back", dogica.Bold, bodyScale, pxInk)
		hits = AddLabeledHit(hits, spec, "menu-prev", "Prev", prevR)
		hits = AddLabeledHit(hits, spec, "menu-back", "Back", backR)
		acts["menu-prev"] = Action{"menu-page", strconv.Itoa(page - 1)}
		acts["menu-back"] = Action{"menu-back", ""}
		return finishPixelScreen(c, spec, texts), hits, acts, totalPages
	}

	// Text page.
	for i, line := range titleLines {
		lineBox := image.Rect(safePhys.Min.X, safePhys.Min.Y+i*titleLineH, safePhys.Max.X, safePhys.Min.Y+(i+1)*titleLineH)
		texts = addCenteredText(texts, lineBox, line, dogica.Bold, spec.BodyTextScale, th.Accent2)
	}
	c.panel(panelRect.Min.X, panelRect.Min.Y, panelRect.Dx(), panelRect.Dy(), pxTileBG, th.Border)
	for i, line := range pages[page] {
		texts = append(texts, pixelTextOp{
			x: physPanel.Min.X + padX, y: physPanel.Min.Y + padY + i*lineH,
			text: line, font: dogica.Regular, scale: bodyScale, color: pxInk,
		})
	}
	if end && page == textPages-1 {
		endBox := PhysicalRect(image.Rect(panelRect.Min.X+4, panelRect.Max.Y-12, panelRect.Max.X-4, panelRect.Max.Y-2), s)
		texts = addCenteredText(texts, endBox, "— The End —", dogica.Bold, bodyScale, th.Accent2)
	}
	texts = drawNavBar(c, spec, texts, page, totalPages, th.Border)
	hits, acts := NavHits(spec, page, totalPages)
	if page == 0 {
		acts["menu-back"] = Action{"menu-back", ""}
	}
	return finishPixelScreen(c, spec, texts), hits, acts, totalPages
}

func humanizeKey(key string) string {
	k := strings.TrimSpace(key)
	for _, p := range []string{"story_", "reading:", "reading_"} {
		if strings.HasPrefix(k, p) {
			k = strings.TrimPrefix(k, p)
			break
		}
	}
	k = strings.ReplaceAll(k, "_", " ")
	k = strings.ReplaceAll(k, "-", " ")
	words := strings.Fields(k)
	for i, w := range words {
		runes := []rune(w)
		if len(runes) > 0 {
			runes[0] = unicode.ToUpper(runes[0])
			words[i] = string(runes)
		}
	}
	if len(words) == 0 {
		return key
	}
	return strings.Join(words, " ")
}

func FormatMemoryTitle(section, key string, titles map[string]string) string {
	if titles != nil {
		if t := strings.TrimSpace(titles[key]); t != "" {
			return t
		}
		trimmed := strings.TrimPrefix(key, "reading:")
		if t := strings.TrimSpace(titles[trimmed]); t != "" {
			return t
		}
	}
	return humanizeKey(key)
}

// FormatMemoryLines converts structured memory JSON into readable prose or key: value lines.
func FormatMemoryLines(rawJSON string) []string {
	var val any
	if err := json.Unmarshal([]byte(rawJSON), &val); err != nil {
		return []string{rawJSON}
	}
	return formatMemoryValue(val)
}

func formatMemoryValue(val any) []string {
	switch v := val.(type) {
	case string:
		return []string{v}
	case map[string]any:
		if txt, ok := v["text"].(string); ok && strings.TrimSpace(txt) != "" {
			return []string{txt}
		}
		if parts, ok := v["parts"].([]any); ok {
			var strs []string
			for _, p := range parts {
				if s, ok := p.(string); ok && strings.TrimSpace(s) != "" {
					strs = append(strs, strings.TrimSpace(s))
				}
			}
			if len(strs) > 0 {
				return []string{strings.Join(strs, " ")}
			}
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		var lines []string
		for _, k := range keys {
			lines = append(lines, fmt.Sprintf("%s: %s", k, formatMemoryScalar(v[k])))
		}
		return lines
	case []any:
		var lines []string
		for _, item := range v {
			lines = append(lines, "- "+formatMemoryScalar(item))
		}
		return lines
	default:
		return []string{formatMemoryScalar(v)}
	}
}

func formatMemoryScalar(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	case bool:
		if x {
			return "yes"
		}
		return "no"
	case nil:
		return "none"
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func RenderMemoriesList(spec Spec, items []MemoryItem, titles map[string]string, page int, th Theme) *image.RGBA {
	if th.Border.A == 0 {
		th = DefaultTheme()
	}
	c := newPixelCanvas(spec.LogicalW, spec.LogicalH)
	darkenBackground(c, th.Bg)
	s := spec.ArtScale
	var texts []pixelTextOp

	titleH := 12
	titleRect := image.Rect(spec.Safe.Min.X, spec.Safe.Min.Y, spec.Safe.Max.X, spec.Safe.Min.Y+titleH)
	texts = addCenteredText(texts, PhysicalRect(titleRect, s), "Memories", dogica.Bold, spec.TitleTextScale, th.Accent)

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
		c.panel(rx, ry, rw, rowH, pxTileBG, th.Border)

		physText := PhysicalRect(image.Rect(rx+3, ry+2, rx+rw-3, ry+rowH-2), s)
		readableTitle := FormatMemoryTitle(it.Section, it.Key, titles)
		header := truncateEllipsisPx(it.Section+": "+readableTitle, dogica.Bold, spec.BodyTextScale, physText.Dx())
		preview := strings.Join(FormatMemoryLines(it.Value), " ")
		preview = truncateEllipsisPx(preview, dogica.Regular, spec.BodyTextScale, physText.Dx())

		lineH := dogica.Regular.LineHeight * spec.BodyTextScale
		topY := physText.Min.Y + max(0, (physText.Dy()-2*lineH)/2)
		texts = append(texts,
			pixelTextOp{x: physText.Min.X, y: topY, text: header, font: dogica.Bold, scale: spec.BodyTextScale, color: th.Accent},
			pixelTextOp{x: physText.Min.X, y: topY + lineH, text: preview, font: dogica.Regular, scale: spec.BodyTextScale, color: pxInk},
		)
	}

	texts = drawNavBar(c, spec, texts, page, totalPages, th.Border)
	return finishPixelScreen(c, spec, texts)
}

func RenderMemoryDetail(spec Spec, item MemoryItem, titles map[string]string, page, bodyScaleOverride int, th Theme) *image.RGBA {
	body := strings.Join(FormatMemoryLines(item.Value), "\n")
	title := item.Section + ": " + FormatMemoryTitle(item.Section, item.Key, titles)
	return RenderReader(spec, title, body, page, bodyScaleOverride, th)
}

type pairQRLayout struct {
	version int
	modules int
	quiet   int
	modPx   int
	qrRect  image.Rectangle // physical pixels including quiet zone
	code    *qr.Code
}

func computePairQRLayout(spec Spec, invite string) (pairQRLayout, error) {
	code, err := qr.Encode(invite, qr.L)
	if err != nil {
		return pairQRLayout{}, err
	}
	physW := spec.LogicalW * spec.ArtScale
	physH := spec.LogicalH * spec.ArtScale
	var availSide int
	if spec.Round {
		// Largest square inscribed in circle of diameter physW.
		availSide = int(math.Floor(float64(physW) / math.Sqrt2))
	} else {
		// Full height on BOX-3.
		availSide = physH
	}
	quiet := 2
	modPx2 := max(1, availSide/(code.Size+2*2))
	modPx4 := max(1, availSide/(code.Size+2*4))
	modPx := modPx2
	if modPx4 == modPx2 {
		quiet = 4
	}
	qrSide := (code.Size + 2*quiet) * modPx
	qx0 := (physW - qrSide) / 2
	qy0 := (physH - qrSide) / 2
	version := (code.Size - 17) / 4
	return pairQRLayout{
		version: version,
		modules: code.Size,
		quiet:   quiet,
		modPx:   modPx,
		qrRect:  image.Rect(qx0, qy0, qx0+qrSide, qy0+qrSide),
		code:    code,
	}, nil
}

func RenderPair(spec Spec, companionName, invite string) (*image.RGBA, error) {
	layout, err := computePairQRLayout(spec, invite)
	if err != nil {
		return nil, err
	}
	c := newPixelCanvas(spec.LogicalW, spec.LogicalH)
	c.fill(pxPanelBG)
	phys := c.scaleNearest(spec.ArtScale)

	// Pure white quiet zone and pure black modules; never graded, tinted, or weathered.
	pureBlack := imagecolor.RGBA{0, 0, 0, 255}
	pureWhite := imagecolor.RGBA{255, 255, 255, 255}
	r := layout.qrRect
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			phys.SetRGBA(x, y, pureWhite)
		}
	}
	for my := range layout.modules {
		for mx := range layout.modules {
			if !layout.code.Black(mx, my) {
				continue
			}
			px0 := r.Min.X + (mx+layout.quiet)*layout.modPx
			py0 := r.Min.Y + (my+layout.quiet)*layout.modPx
			for dy := range layout.modPx {
				for dx := range layout.modPx {
					phys.SetRGBA(px0+dx, py0+dy, pureBlack)
				}
			}
		}
	}

	// Only draw a small back indicator and companion name on BOX-3 side space if it fits outside the QR.
	if !spec.Round {
		sideW := r.Min.X
		if sideW >= 24 {
			drawTextPhysical(phys, 6, 6, "<", dogica.Bold, 2, pxMuted)
		}
		if companionName != "" && sideW >= 20 {
			nameW := measureTextPx(companionName, dogica.Regular, 2)
			if nameW <= sideW-8 {
				ny := (phys.Bounds().Dy() - dogica.Regular.Ascent*2) / 2
				drawTextPhysical(phys, (sideW-nameW)/2, ny, companionName, dogica.Regular, 2, pxInk)
			} else if nameW := measureTextPx(companionName, dogica.Regular, 1); nameW <= sideW-6 {
				ny := (phys.Bounds().Dy() - dogica.Regular.Ascent) / 2
				drawTextPhysical(phys, (sideW-nameW)/2, ny, companionName, dogica.Regular, 1, pxInk)
			}
		}
	}
	return phys, nil
}
