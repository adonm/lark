package main

import (
	"encoding/json"
	"fmt"
	"image"
	imagecolor "image/color"
	"slices"
	"strings"

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
	Title string
	Page  int
	Total int
	Color imagecolor.RGBA
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

func drawNavBar(c *pixelCanvas, spec pixelDisplaySpec, texts []pixelTextOp, page, totalPages int, centerLabel string) []pixelTextOp {
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
	c.panel(leftRect.Min.X, leftRect.Min.Y, leftRect.Dx(), leftRect.Dy(), pxTileBG, pxTileBorder)
	texts = addCenteredText(texts, physicalRect(leftRect, s), leftLabel, dogica.Regular, spec.bodyTextScale, pxInk)

	if centerLabel == "" && totalPages > 1 {
		centerLabel = fmt.Sprintf("%d/%d", page+1, totalPages)
	}
	if centerLabel != "" {
		if page > 0 {
			c.panel(midRect.Min.X, midRect.Min.Y, midRect.Dx(), midRect.Dy(), pxTileBG, pxTileBorder)
			texts = addCenteredText(texts, physicalRect(midRect, s), "Back", dogica.Regular, spec.bodyTextScale, pxMuted)
		} else {
			texts = addCenteredText(texts, physicalRect(midRect, s), centerLabel, dogica.Regular, spec.bodyTextScale, pxMuted)
		}
	}

	nextCol := pxInk
	nextBorder := pxTileBorder
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

func renderPixelHome(spec pixelDisplaySpec, bg, poseImg *image.RGBA, grade gradeKey, status string, bobY int, caption string) *image.RGBA {
	c := newPixelCanvas(spec.logicalW, spec.logicalH)
	if bg != nil {
		c.blit(0, 0, bg)
	} else {
		drawDefaultPixelBackground(c)
	}
	if poseImg != nil {
		b := poseImg.Bounds()
		px := (spec.logicalW - b.Dx()) / 2
		py := spec.safe.Min.Y + 2 + bobY
		if caption != "" {
			py = max(4, spec.safe.Min.Y-4) + bobY
		}
		c.blit(px, py, poseImg)
	}

	// Grade only the world + companion, never the UI panels or text.
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
	} else {
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

func renderPixelMenu(spec pixelDisplaySpec, page int) *image.RGBA {
	c := newPixelCanvas(spec.logicalW, spec.logicalH)
	c.fill(pxPanelBG)
	s := spec.artScale
	var texts []pixelTextOp

	titleH := 12
	titleRect := image.Rect(spec.safe.Min.X, spec.safe.Min.Y, spec.safe.Max.X, spec.safe.Min.Y+titleH)
	texts = addCenteredText(texts, physicalRect(titleRect, s), "Menu", dogica.Bold, spec.titleTextScale, pxPeach)

	cols, rows := 1, 3
	navH := 14
	if !spec.round {
		cols, rows = 3, 2
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
		c.panel(tx, ty, tileW, tileH, pxTileBG, pxTileBorder)
		if spec.round {
			if sprite, ok := pxMenuSprites[item.icon]; ok {
				c.blitSprite(tx+4, ty+(tileH-12)/2, sprite, pxIconPalette, 1)
			}
			labelBox := image.Rect(tx+20, ty+1, tx+tileW-4, ty+tileH-1)
			physLabel := physicalRect(labelBox, s)
			label := truncateEllipsisPx(item.label, dogica.Regular, spec.bodyTextScale, physLabel.Dx())
			th := dogica.Regular.Ascent * spec.bodyTextScale
			texts = append(texts, pixelTextOp{
				x: physLabel.Min.X, y: physLabel.Min.Y + (physLabel.Dy()-th)/2,
				text: label, font: dogica.Regular, scale: spec.bodyTextScale, color: pxInk,
			})
		} else {
			if sprite, ok := pxMenuSprites[item.icon]; ok {
				c.blitSprite(tx+(tileW-12)/2, ty+4, sprite, pxIconPalette, 1)
			}
			labelBox := image.Rect(tx+2, ty+17, tx+tileW-2, ty+tileH-2)
			physLabel := physicalRect(labelBox, s)
			label := truncateEllipsisPx(item.label, dogica.Regular, spec.bodyTextScale, physLabel.Dx()-2)
			texts = addCenteredText(texts, physLabel, label, dogica.Regular, spec.bodyTextScale, pxInk)
		}
	}

	texts = drawNavBar(c, spec, texts, page, totalPages, fmt.Sprintf("%d/%d", page+1, totalPages))
	return finishPixelScreen(c, spec, texts)
}

func renderPixelBooksList(spec pixelDisplaySpec, books []pixelBook, page int) *image.RGBA {
	c := newPixelCanvas(spec.logicalW, spec.logicalH)
	c.fill(pxPanelBG)
	s := spec.artScale
	var texts []pixelTextOp

	titleH := 12
	titleRect := image.Rect(spec.safe.Min.X, spec.safe.Min.Y, spec.safe.Max.X, spec.safe.Min.Y+titleH)
	texts = addCenteredText(texts, physicalRect(titleRect, s), "Books", dogica.Bold, spec.titleTextScale, pxMint)

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
		c.panel(rx, ry, rw, rowH, pxTileBG, pxTileBorder)

		// Cover tile on left.
		coverX, coverY := rx+3, ry+(rowH-14)/2
		c.panel(coverX, coverY, 14, 14, bk.Color, pxInk)
		c.blitSprite(coverX+1, coverY+1, pxMenuSprites["books"], pxIconPalette, 1)

		physText := physicalRect(image.Rect(rx+20, ry+2, rx+rw-3, ry+rowH-2), s)
		title := truncateEllipsisPx(bk.Title, dogica.Bold, spec.bodyTextScale, physText.Dx())
		prog := fmt.Sprintf("p %d/%d", bk.Page, bk.Total)
		lineH := dogica.Regular.LineHeight * spec.bodyTextScale
		topY := physText.Min.Y + max(0, (physText.Dy()-2*lineH)/2)
		texts = append(texts,
			pixelTextOp{x: physText.Min.X, y: topY, text: title, font: dogica.Bold, scale: spec.bodyTextScale, color: pxInk},
			pixelTextOp{x: physText.Min.X, y: topY + lineH, text: prog, font: dogica.Regular, scale: spec.bodyTextScale, color: pxMuted},
		)
	}

	texts = drawNavBar(c, spec, texts, page, totalPages, fmt.Sprintf("%d/%d", page+1, totalPages))
	return finishPixelScreen(c, spec, texts)
}

func renderPixelReader(spec pixelDisplaySpec, title, body string, page, bodyScaleOverride int) *image.RGBA {
	c := newPixelCanvas(spec.logicalW, spec.logicalH)
	c.fill(pxPanelBG)
	s := spec.artScale
	bodyScale := spec.bodyTextScale
	if bodyScaleOverride > 0 {
		bodyScale = bodyScaleOverride
	}
	var texts []pixelTextOp

	titleH := 12
	titleBox := physicalRect(image.Rect(spec.safe.Min.X, spec.safe.Min.Y, spec.safe.Max.X, spec.safe.Min.Y+titleH), s)
	titleStr := truncateEllipsisPx(title, dogica.Bold, spec.bodyTextScale, titleBox.Dx())
	texts = addCenteredText(texts, titleBox, titleStr, dogica.Bold, spec.bodyTextScale, pxMint)

	navH := 14
	if !spec.round {
		navH = 20
	}
	panelRect := image.Rect(spec.safe.Min.X, spec.safe.Min.Y+titleH+1, spec.safe.Max.X, spec.safe.Max.Y-navH-2)
	c.panel(panelRect.Min.X, panelRect.Min.Y, panelRect.Dx(), panelRect.Dy(), pxTileBG, pxPanelBorder)

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

	texts = drawNavBar(c, spec, texts, page, len(pages), fmt.Sprintf("%d/%d", page+1, len(pages)))
	return finishPixelScreen(c, spec, texts)
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

func renderPixelMemoriesList(spec pixelDisplaySpec, items []pixelMemoryItem, page int) *image.RGBA {
	c := newPixelCanvas(spec.logicalW, spec.logicalH)
	c.fill(pxPanelBG)
	s := spec.artScale
	var texts []pixelTextOp

	titleH := 12
	titleRect := image.Rect(spec.safe.Min.X, spec.safe.Min.Y, spec.safe.Max.X, spec.safe.Min.Y+titleH)
	texts = addCenteredText(texts, physicalRect(titleRect, s), "Memories", dogica.Bold, spec.titleTextScale, pxDanger)

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
		c.panel(rx, ry, rw, rowH, pxTileBG, pxTileBorder)

		physText := physicalRect(image.Rect(rx+3, ry+2, rx+rw-3, ry+rowH-2), s)
		header := truncateEllipsisPx(it.Section+" · "+it.Key, dogica.Bold, spec.bodyTextScale, physText.Dx())
		preview := strings.Join(formatMemoryLines(it.Value), " ")
		preview = truncateEllipsisPx(preview, dogica.Regular, spec.bodyTextScale, physText.Dx())

		lineH := dogica.Regular.LineHeight * spec.bodyTextScale
		topY := physText.Min.Y + max(0, (physText.Dy()-2*lineH)/2)
		texts = append(texts,
			pixelTextOp{x: physText.Min.X, y: topY, text: header, font: dogica.Bold, scale: spec.bodyTextScale, color: pxPeach},
			pixelTextOp{x: physText.Min.X, y: topY + lineH, text: preview, font: dogica.Regular, scale: spec.bodyTextScale, color: pxInk},
		)
	}

	texts = drawNavBar(c, spec, texts, page, totalPages, fmt.Sprintf("%d/%d", page+1, totalPages))
	return finishPixelScreen(c, spec, texts)
}

func renderPixelMemoryDetail(spec pixelDisplaySpec, item pixelMemoryItem, page, bodyScaleOverride int) *image.RGBA {
	body := strings.Join(formatMemoryLines(item.Value), "\n")
	return renderPixelReader(spec, item.Section+": "+item.Key, body, page, bodyScaleOverride)
}

func renderPixelPair(spec pixelDisplaySpec, title, invite string) (*image.RGBA, error) {
	code, err := qr.Encode(invite, qr.L)
	if err != nil {
		return nil, err
	}
	c := newPixelCanvas(spec.logicalW, spec.logicalH)
	c.fill(pxPanelBG)
	s := spec.artScale
	var texts []pixelTextOp

	titleH := 12
	titleBox := physicalRect(image.Rect(spec.safe.Min.X, spec.safe.Min.Y, spec.safe.Max.X, spec.safe.Min.Y+titleH), s)
	titleStr := truncateEllipsisPx(title, dogica.Bold, spec.titleTextScale, titleBox.Dx())
	texts = addCenteredText(texts, titleBox, titleStr, dogica.Bold, spec.titleTextScale, pxPeach)

	// Draw QR at an integer physical module size with >= 2 modules quiet zone.
	phys := c.scaleNearest(s)
	safePhys := physicalRect(spec.safe, s)
	availW := safePhys.Dx()
	availH := safePhys.Dy() - titleBox.Dy() - 4
	const quiet = 2
	totalMods := code.Size + 2*quiet
	modPx := max(1, min(availW, availH)/totalMods)
	qrSide := totalMods * modPx
	qx0 := safePhys.Min.X + (availW-qrSide)/2
	qy0 := titleBox.Max.Y + 4 + (availH-qrSide)/2

	// White quiet zone background.
	for y := qy0; y < qy0+qrSide; y++ {
		for x := qx0; x < qx0+qrSide; x++ {
			phys.SetRGBA(x, y, pxQRWhite)
		}
	}
	// Black QR modules.
	for my := range code.Size {
		for mx := range code.Size {
			if !code.Black(mx, my) {
				continue
			}
			px0 := qx0 + (mx+quiet)*modPx
			py0 := qy0 + (my+quiet)*modPx
			for dy := range modPx {
				for dx := range modPx {
					phys.SetRGBA(px0+dx, py0+dy, pxQRDark)
				}
			}
		}
	}

	for _, t := range texts {
		drawTextPhysical(phys, t.x, t.y, t.text, t.font, t.scale, t.color)
	}
	return phys, nil
}
