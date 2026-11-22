package main

import (
	"encoding/json"
	"fmt"
	"image"
	imagecolor "image/color"
	"math"
	"slices"
	"strings"
	"unicode"

	"lark/fonts/dogica"
	"rsc.io/qr"
)

type pixelTextOp struct {
	x, y  int
	text  string
	font  dogica.Font
	scale int
	color imagecolor.RGBA
}

type pixelMenuItem struct {
	id, label, icon string
	accent          imagecolor.RGBA
}

var pixelHomeMenuItems = []pixelMenuItem{
	{id: "talk", label: "Talk to…", icon: "talk", accent: pxPeach},
	{id: "books", label: "Books", icon: "books", accent: pxMint},
	{id: "games", label: "Games", icon: "games", accent: pxSky},
	{id: "memories", label: "Memories", icon: "memories", accent: pxDanger},
	{id: "history", label: "History", icon: "history", accent: pxWarn},
	{id: "voice", label: "Voice", icon: "voice", accent: pxMint},
	{id: "volume", label: "Volume", icon: "volume", accent: pxSky},
	{id: "pair", label: "Pair", icon: "pair", accent: pxPeach},
	{id: "unpair", label: "Unpair", icon: "unpair", accent: pxDanger},
	{id: "stats", label: "Stats", icon: "stats", accent: pxMint},
}

type pixelBook struct {
	Name  string
	Title string
	Page  int
	Total int
	Color imagecolor.RGBA
}

type pixelGame struct {
	Name  string
	Title string
	Kind  string
	Saved bool
}

type pixelMemoryItem struct {
	Section string
	Key     string
	Value   string // raw JSON
}

func finishPixelScreen(c *pixelCanvas, spec pixelDisplaySpec, texts []pixelTextOp) *image.RGBA {
	phys := c.scaleNearest(spec.artScale)
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

func physicalRect(r image.Rectangle, artScale int) image.Rectangle {
	return image.Rect(r.Min.X*artScale, r.Min.Y*artScale, r.Max.X*artScale, r.Max.Y*artScale)
}

func drawNavBar(c *pixelCanvas, spec pixelDisplaySpec, texts []pixelTextOp, page, totalPages int, centerLabel string, border imagecolor.RGBA) []pixelTextOp {
	s := spec.artScale
	navH := 14
	if !spec.round {
		navH = 20 // 40 px physical on BOX-3 (2x)
	}
	y := spec.safe.Max.Y - navH
	x0 := spec.safe.Min.X
	w := spec.safe.Dx()
	gap := 3
	btnW := (w - 2*gap) / 3

	leftRect := image.Rect(x0, y, x0+btnW, y+navH)
	midRect := image.Rect(x0+btnW+gap, y, x0+2*btnW+gap, y+navH)
	rightRect := image.Rect(x0+2*(btnW+gap), y, x0+w, y+navH)

	leftLabel := "Back"
	if page > 0 {
		leftLabel = "< Prev"
	}
	c.panel(leftRect.Min.X, leftRect.Min.Y, leftRect.Dx(), leftRect.Dy(), pxTileBG, border)
	texts = addCenteredText(texts, physicalRect(leftRect, s), leftLabel, dogica.Regular, spec.bodyTextScale, pxInk)

	if centerLabel == "" && totalPages > 1 {
		centerLabel = fmt.Sprintf("%d/%d", page+1, totalPages)
	}
	if centerLabel != "" {
		if page > 0 {
			c.panel(midRect.Min.X, midRect.Min.Y, midRect.Dx(), midRect.Dy(), pxTileBG, border)
			texts = addCenteredText(texts, physicalRect(midRect, s), "Back", dogica.Regular, spec.bodyTextScale, pxMuted)
		} else {
			texts = addCenteredText(texts, physicalRect(midRect, s), centerLabel, dogica.Regular, spec.bodyTextScale, pxMuted)
		}
	}

	nextCol := pxInk
	nextBorder := border
	if page+1 >= totalPages {
		nextCol = pxMuted
		nextBorder = pxPanelBG
	}
	c.panel(rightRect.Min.X, rightRect.Min.Y, rightRect.Dx(), rightRect.Dy(), pxTileBG, nextBorder)
	rightLabel := "Next >"
	if page+1 >= totalPages {
		rightLabel = "End"
	}
	texts = addCenteredText(texts, physicalRect(rightRect, s), rightLabel, dogica.Regular, spec.bodyTextScale, nextCol)
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

func renderPixelHome(spec pixelDisplaySpec, bg, poseImg *image.RGBA, grade gradeKey, status string, bobY int, caption string, showMenuHint bool) *image.RGBA {
	return renderPixelHomeWeather(spec, bg, poseImg, grade, weatherClear, 0, status, bobY, caption, showMenuHint)
}

func renderPixelHomeWeather(spec pixelDisplaySpec, bg, poseImg *image.RGBA, grade gradeKey, weather weatherKind, tick int, status string, bobY int, caption string, showMenuHint bool) *image.RGBA {
	c := newPixelCanvas(spec.logicalW, spec.logicalH)
	blitCenteredBG(c, bg)
	if poseImg != nil {
		b := poseImg.Bounds()
		px := (spec.logicalW - b.Dx()) / 2
		py := spec.safe.Min.Y + 2 + bobY
		if caption != "" {
			py = max(4, spec.safe.Min.Y-4) + bobY
		}
		c.blit(px, py, poseImg)
	}

	// Grade and weather apply only to the world + companion, never the UI panels or text.
	applyWeatherEffects(c, weather, tick)
	c.applyLUT(grade)

	var texts []pixelTextOp
	s := spec.artScale

	// Status icon in top-right corner of the safe area (no ring outline).
	if sprite, ok := pxStatusSprites[status]; ok {
		sx := spec.safe.Max.X - 11
		sy := spec.safe.Min.Y + 1
		c.panel(sx-1, sy-1, 10, 10, pxPanelBG, pxPanelBorder)
		c.blitSprite(sx, sy, sprite, pxIconPalette, 1)
	}

	if caption != "" {
		boxH := 30
		if !spec.round {
			boxH = 34
		}
		box := image.Rect(spec.safe.Min.X, spec.safe.Max.Y-boxH, spec.safe.Max.X, spec.safe.Max.Y)
		c.panel(box.Min.X, box.Min.Y, box.Dx(), box.Dy(), pxPanelBG, pxPanelBorder)
		physBox := physicalRect(box, s)
		padX, padY := 8, 8
		maxW := physBox.Dx() - 2*padX
		lineH := dogica.Regular.LineHeight * spec.bodyTextScale
		maxLines := max(1, (physBox.Dy()-2*padY)/lineH)
		lines := wrapTextPx(caption, dogica.Regular, spec.bodyTextScale, maxW)
		if len(lines) > maxLines {
			lines = lines[:maxLines]
			lines[maxLines-1] = truncateEllipsisPx(lines[maxLines-1]+" …", dogica.Regular, spec.bodyTextScale, maxW)
		}
		for i, line := range lines {
			texts = append(texts, pixelTextOp{
				x: physBox.Min.X + padX, y: physBox.Min.Y + padY + i*lineH,
				text: line, font: dogica.Regular, scale: spec.bodyTextScale, color: pxInk,
			})
		}
	} else if status == "listening" {
		btnH := 14
		if !spec.round {
			btnH = 18
		}
		y := spec.safe.Max.Y - btnH
		gap := 4
		bw := (spec.safe.Dx() - gap) / 2
		left := image.Rect(spec.safe.Min.X, y, spec.safe.Min.X+bw, y+btnH)
		right := image.Rect(spec.safe.Min.X+bw+gap, y, spec.safe.Max.X, y+btnH)
		c.panel(left.Min.X, left.Min.Y, left.Dx(), left.Dy(), pxTileBG, pxDanger)
		c.panel(right.Min.X, right.Min.Y, right.Dx(), right.Dy(), pxTileBG, pxMint)
		texts = addCenteredText(texts, physicalRect(left, s), "Cancel", dogica.Regular, spec.bodyTextScale, pxDanger)
		texts = addCenteredText(texts, physicalRect(right, s), "Send", dogica.Bold, spec.bodyTextScale, pxMint)
	} else if status == "thinking" || status == "speaking" {
		btnW, btnH := 38, 14
		if !spec.round {
			btnW, btnH = 44, 18
		}
		btn := image.Rect((spec.logicalW-btnW)/2, spec.safe.Max.Y-btnH, (spec.logicalW+btnW)/2, spec.safe.Max.Y)
		c.panel(btn.Min.X, btn.Min.Y, btn.Dx(), btn.Dy(), pxPanelBG, pxDanger)
		texts = addCenteredText(texts, physicalRect(btn, s), "Stop", dogica.Regular, spec.bodyTextScale, pxDanger)
	} else if showMenuHint {
		btnW, btnH := 36, 14
		if !spec.round {
			btnW, btnH = 44, 20
		}
		btn := image.Rect((spec.logicalW-btnW)/2, spec.safe.Max.Y-btnH, (spec.logicalW+btnW)/2, spec.safe.Max.Y)
		c.panel(btn.Min.X, btn.Min.Y, btn.Dx(), btn.Dy(), pxPanelBG, pxPanelBorder)
		texts = addCenteredText(texts, physicalRect(btn, s), "Menu", dogica.Regular, spec.bodyTextScale, pxInk)
	}

	return finishPixelScreen(c, spec, texts)
}

func renderPixelMenu(spec pixelDisplaySpec, page int, th pixelTheme) *image.RGBA {
	if th.border.A == 0 {
		th = defaultPixelTheme()
	}
	c := newPixelCanvas(spec.logicalW, spec.logicalH)
	darkenBackground(c, th.bg)
	s := spec.artScale
	var texts []pixelTextOp

	titleH := 12
	titleRect := image.Rect(spec.safe.Min.X, spec.safe.Min.Y, spec.safe.Max.X, spec.safe.Min.Y+titleH)
	texts = addCenteredText(texts, physicalRect(titleRect, s), "Menu", dogica.Bold, spec.titleTextScale, th.accent)

	cols, rows := 1, 3
	navH := 14
	if !spec.round {
		cols, rows = 2, 3
		navH = 20
	}
	perPage := cols * rows
	totalPages := (len(pixelHomeMenuItems) + perPage - 1) / perPage
	page = max(0, min(page, totalPages-1))

	gridTop := spec.safe.Min.Y + titleH + 2
	gridBottom := spec.safe.Max.Y - navH - 2
	gapX, gapY := 4, 2
	tileW := (spec.safe.Dx() - (cols-1)*gapX) / cols
	tileH := (gridBottom - gridTop - (rows-1)*gapY) / rows

	start := page * perPage
	for idx := range perPage {
		itemIdx := start + idx
		if itemIdx >= len(pixelHomeMenuItems) {
			break
		}
		item := pixelHomeMenuItems[itemIdx]
		col := idx % cols
		row := idx / cols
		tx := spec.safe.Min.X + col*(tileW+gapX)
		ty := gridTop + row*(tileH+gapY)
		c.panel(tx, ty, tileW, tileH, pxTileBG, th.border)
		if sprite, ok := pxMenuSprites[item.icon]; ok {
			c.blitSprite(tx+4, ty+(tileH-12)/2, sprite, pxIconPalette, 1)
		}
		labelBox := image.Rect(tx+19, ty+1, tx+tileW-3, ty+tileH-1)
		physLabel := physicalRect(labelBox, s)
		label := truncateEllipsisPx(item.label, dogica.Regular, spec.bodyTextScale, physLabel.Dx())
		thPx := dogica.Regular.Ascent * spec.bodyTextScale
		texts = append(texts, pixelTextOp{
			x: physLabel.Min.X, y: physLabel.Min.Y + (physLabel.Dy()-thPx)/2,
			text: label, font: dogica.Regular, scale: spec.bodyTextScale, color: pxInk,
		})
	}

	texts = drawNavBar(c, spec, texts, page, totalPages, fmt.Sprintf("%d/%d", page+1, totalPages), th.border)
	return finishPixelScreen(c, spec, texts)
}

func renderPixelBooksList(spec pixelDisplaySpec, books []pixelBook, page int, th pixelTheme) *image.RGBA {
	if th.border.A == 0 {
		th = defaultPixelTheme()
	}
	c := newPixelCanvas(spec.logicalW, spec.logicalH)
	darkenBackground(c, th.bg)
	s := spec.artScale
	var texts []pixelTextOp

	titleH := 12
	titleRect := image.Rect(spec.safe.Min.X, spec.safe.Min.Y, spec.safe.Max.X, spec.safe.Min.Y+titleH)
	texts = addCenteredText(texts, physicalRect(titleRect, s), "Books", dogica.Bold, spec.titleTextScale, th.accent2)

	perPage := 2
	navH := 14
	if !spec.round {
		perPage = 3
		navH = 20
	}
	totalPages := max(1, (len(books)+perPage-1)/perPage)
	page = max(0, min(page, totalPages-1))

	listTop := spec.safe.Min.Y + titleH + 2
	listBottom := spec.safe.Max.Y - navH - 3
	gapY := 3
	rowH := (listBottom - listTop - (perPage-1)*gapY) / perPage

	start := page * perPage
	for i := range perPage {
		idx := start + i
		if idx >= len(books) {
			break
		}
		bk := books[idx]
		ry := listTop + i*(rowH+gapY)
		rx := spec.safe.Min.X
		rw := spec.safe.Dx()
		c.panel(rx, ry, rw, rowH, pxTileBG, th.border)

		coverCol := bk.Color
		if coverCol.A == 0 {
			coverCol = th.accent
		}
		coverX, coverY := rx+3, ry+(rowH-14)/2
		c.panel(coverX, coverY, 14, 14, coverCol, pxInk)
		c.blitSprite(coverX+1, coverY+1, pxMenuSprites["books"], pxIconPalette, 1)

		physText := physicalRect(image.Rect(rx+20, ry+2, rx+rw-3, ry+rowH-2), s)
		title := truncateEllipsisPx(bk.Title, dogica.Bold, spec.bodyTextScale, physText.Dx())
		prog := fmt.Sprintf("p %d/%d", max(1, bk.Page), max(1, bk.Total))
		lineH := dogica.Regular.LineHeight * spec.bodyTextScale
		topY := physText.Min.Y + max(0, (physText.Dy()-2*lineH)/2)
		texts = append(texts,
			pixelTextOp{x: physText.Min.X, y: topY, text: title, font: dogica.Bold, scale: spec.bodyTextScale, color: pxInk},
			pixelTextOp{x: physText.Min.X, y: topY + lineH, text: prog, font: dogica.Regular, scale: spec.bodyTextScale, color: pxMuted},
		)
	}

	texts = drawNavBar(c, spec, texts, page, totalPages, fmt.Sprintf("%d/%d", page+1, totalPages), th.border)
	return finishPixelScreen(c, spec, texts)
}

func renderPixelGamesList(spec pixelDisplaySpec, games []pixelGame, page int, th pixelTheme) *image.RGBA {
	if th.border.A == 0 {
		th = defaultPixelTheme()
	}
	c := newPixelCanvas(spec.logicalW, spec.logicalH)
	darkenBackground(c, th.bg)
	s := spec.artScale
	var texts []pixelTextOp

	titleH := 12
	titleRect := image.Rect(spec.safe.Min.X, spec.safe.Min.Y, spec.safe.Max.X, spec.safe.Min.Y+titleH)
	texts = addCenteredText(texts, physicalRect(titleRect, s), "Games", dogica.Bold, spec.titleTextScale, th.accent)

	perPage := 2
	navH := 14
	if !spec.round {
		perPage = 3
		navH = 20
	}
	totalPages := max(1, (len(games)+perPage-1)/perPage)
	page = max(0, min(page, totalPages-1))

	listTop := spec.safe.Min.Y + titleH + 2
	listBottom := spec.safe.Max.Y - navH - 3
	gapY := 3
	rowH := (listBottom - listTop - (perPage-1)*gapY) / perPage

	start := page * perPage
	for i := range perPage {
		idx := start + i
		if idx >= len(games) {
			break
		}
		gm := games[idx]
		ry := listTop + i*(rowH+gapY)
		rx := spec.safe.Min.X
		rw := spec.safe.Dx()
		c.panel(rx, ry, rw, rowH, pxTileBG, th.border)

		c.blitSprite(rx+4, ry+(rowH-12)/2, pxMenuSprites["games"], pxIconPalette, 1)

		badgeW := 0
		if gm.Saved {
			badgeW = 24
			bx, by := rx+rw-badgeW-3, ry+(rowH-10)/2
			c.panel(bx, by, badgeW, 10, th.accent2, th.accent2)
			texts = addCenteredText(texts, physicalRect(image.Rect(bx, by, bx+badgeW, by+10), s), "Saved", dogica.Bold, 1, pxPanelBG)
		}
		physText := physicalRect(image.Rect(rx+19, ry+2, rx+rw-4-badgeW, ry+rowH-2), s)
		title := truncateEllipsisPx(gm.Title, dogica.Bold, spec.bodyTextScale, physText.Dx())
		thPx := dogica.Bold.Ascent * spec.bodyTextScale
		texts = append(texts, pixelTextOp{
			x: physText.Min.X, y: physText.Min.Y + (physText.Dy()-thPx)/2,
			text: title, font: dogica.Bold, scale: spec.bodyTextScale, color: pxInk,
		})
	}

	texts = drawNavBar(c, spec, texts, page, totalPages, fmt.Sprintf("%d/%d", page+1, totalPages), th.border)
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

func renderPixelReader(spec pixelDisplaySpec, title, body string, page, bodyScaleOverride int, th pixelTheme) *image.RGBA {
	if th.border.A == 0 {
		th = defaultPixelTheme()
	}
	c := newPixelCanvas(spec.logicalW, spec.logicalH)
	darkenBackground(c, th.bg)
	s := spec.artScale
	bodyScale := spec.bodyTextScale
	if bodyScaleOverride > 0 {
		bodyScale = bodyScaleOverride
	}
	var texts []pixelTextOp

	safePhys := physicalRect(spec.safe, s)
	titleLines := wrapTitleLines(title, dogica.Bold, spec.bodyTextScale, safePhys.Dx(), 2)
	titleLineH := dogica.Bold.LineHeight * spec.bodyTextScale
	titlePhysH := max(12*s, len(titleLines)*titleLineH)
	titleH := (titlePhysH + s - 1) / s
	for i, line := range titleLines {
		lineBox := image.Rect(safePhys.Min.X, safePhys.Min.Y+i*titleLineH, safePhys.Max.X, safePhys.Min.Y+(i+1)*titleLineH)
		texts = addCenteredText(texts, lineBox, line, dogica.Bold, spec.bodyTextScale, th.accent2)
	}

	navH := 14
	if !spec.round {
		navH = 20
	}
	panelRect := image.Rect(spec.safe.Min.X, spec.safe.Min.Y+titleH+1, spec.safe.Max.X, spec.safe.Max.Y-navH-2)
	c.panel(panelRect.Min.X, panelRect.Min.Y, panelRect.Dx(), panelRect.Dy(), pxTileBG, th.border)

	physPanel := physicalRect(panelRect, s)
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

	texts = drawNavBar(c, spec, texts, page, len(pages), fmt.Sprintf("%d/%d", page+1, len(pages)), th.border)
	return finishPixelScreen(c, spec, texts)
}

func humanizeKey(key string) string {
	k := strings.TrimSpace(key)
	for _, p := range []string{"story_", "book_", "reading:", "reading_", "game_", "app:"} {
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

func formatMemoryTitle(section, key string, titles map[string]string) string {
	if titles != nil {
		if t := strings.TrimSpace(titles[key]); t != "" {
			return t
		}
		trimmed := strings.TrimPrefix(strings.TrimPrefix(key, "reading:"), "app:")
		if t := strings.TrimSpace(titles[trimmed]); t != "" {
			return t
		}
	}
	return humanizeKey(key)
}

// formatMemoryLines converts structured memory JSON into readable prose or key: value lines.
func formatMemoryLines(rawJSON string) []string {
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

func renderPixelMemoriesList(spec pixelDisplaySpec, items []pixelMemoryItem, titles map[string]string, page int, th pixelTheme) *image.RGBA {
	if th.border.A == 0 {
		th = defaultPixelTheme()
	}
	c := newPixelCanvas(spec.logicalW, spec.logicalH)
	darkenBackground(c, th.bg)
	s := spec.artScale
	var texts []pixelTextOp

	titleH := 12
	titleRect := image.Rect(spec.safe.Min.X, spec.safe.Min.Y, spec.safe.Max.X, spec.safe.Min.Y+titleH)
	texts = addCenteredText(texts, physicalRect(titleRect, s), "Memories", dogica.Bold, spec.titleTextScale, th.accent)

	perPage := 2
	navH := 14
	if !spec.round {
		perPage = 3
		navH = 20
	}
	totalPages := max(1, (len(items)+perPage-1)/perPage)
	page = max(0, min(page, totalPages-1))

	listTop := spec.safe.Min.Y + titleH + 2
	listBottom := spec.safe.Max.Y - navH - 3
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
		rx := spec.safe.Min.X
		rw := spec.safe.Dx()
		c.panel(rx, ry, rw, rowH, pxTileBG, th.border)

		physText := physicalRect(image.Rect(rx+3, ry+2, rx+rw-3, ry+rowH-2), s)
		readableTitle := formatMemoryTitle(it.Section, it.Key, titles)
		header := truncateEllipsisPx(it.Section+": "+readableTitle, dogica.Bold, spec.bodyTextScale, physText.Dx())
		preview := strings.Join(formatMemoryLines(it.Value), " ")
		preview = truncateEllipsisPx(preview, dogica.Regular, spec.bodyTextScale, physText.Dx())

		lineH := dogica.Regular.LineHeight * spec.bodyTextScale
		topY := physText.Min.Y + max(0, (physText.Dy()-2*lineH)/2)
		texts = append(texts,
			pixelTextOp{x: physText.Min.X, y: topY, text: header, font: dogica.Bold, scale: spec.bodyTextScale, color: th.accent},
			pixelTextOp{x: physText.Min.X, y: topY + lineH, text: preview, font: dogica.Regular, scale: spec.bodyTextScale, color: pxInk},
		)
	}

	texts = drawNavBar(c, spec, texts, page, totalPages, fmt.Sprintf("%d/%d", page+1, totalPages), th.border)
	return finishPixelScreen(c, spec, texts)
}

func renderPixelMemoryDetail(spec pixelDisplaySpec, item pixelMemoryItem, titles map[string]string, page, bodyScaleOverride int, th pixelTheme) *image.RGBA {
	body := strings.Join(formatMemoryLines(item.Value), "\n")
	title := item.Section + ": " + formatMemoryTitle(item.Section, item.Key, titles)
	return renderPixelReader(spec, title, body, page, bodyScaleOverride, th)
}

type pairQRLayout struct {
	version int
	modules int
	quiet   int
	modPx   int
	qrRect  image.Rectangle // physical pixels including quiet zone
	code    *qr.Code
}

func computePairQRLayout(spec pixelDisplaySpec, invite string) (pairQRLayout, error) {
	code, err := qr.Encode(invite, qr.L)
	if err != nil {
		return pairQRLayout{}, err
	}
	physW := spec.logicalW * spec.artScale
	physH := spec.logicalH * spec.artScale
	var availSide int
	if spec.round {
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

func renderPixelPair(spec pixelDisplaySpec, companionName, invite string) (*image.RGBA, error) {
	layout, err := computePairQRLayout(spec, invite)
	if err != nil {
		return nil, err
	}
	c := newPixelCanvas(spec.logicalW, spec.logicalH)
	c.fill(pxPanelBG)
	phys := c.scaleNearest(spec.artScale)

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
	if !spec.round {
		sideW := r.Min.X
		if sideW >= 24 {
			drawTextPhysical(phys, 6, 6, "<", dogica.Bold, 2, pxMuted)
		}
		if companionName != "" && sideW >= 20 {
			nameW := measureTextPx(companionName, dogica.Regular, 2)
			if nameW <= sideW-6 {
				ny := (phys.Bounds().Dy() - dogica.Regular.Ascent*2) / 2
				drawTextPhysical(phys, (sideW-nameW)/2, ny, companionName, dogica.Regular, 2, pxInk)
			} else if nameW := measureTextPx(companionName, dogica.Regular, 1); nameW <= sideW-4 {
				ny := (phys.Bounds().Dy() - dogica.Regular.Ascent) / 2
				drawTextPhysical(phys, (sideW-nameW)/2, ny, companionName, dogica.Regular, 1, pxInk)
			}
		}
	}
	return phys, nil
}
