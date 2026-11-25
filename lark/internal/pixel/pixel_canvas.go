package pixel

import (
	"image"
	imagecolor "image/color"
	"strings"
	"sync"

	"lark/fonts/dogica"
)

type Spec struct {
	name           string
	LogicalW       int
	LogicalH       int
	ArtScale       int
	BodyTextScale  int
	TitleTextScale int
	Safe           image.Rectangle
	Round          bool
}

var (
	SpecVoCat = Spec{
		name:           "vocat",
		LogicalW:       120,
		LogicalH:       120,
		ArtScale:       3,
		BodyTextScale:  2,
		TitleTextScale: 3,
		Safe:           image.Rect(18, 18, 102, 102),
		Round:          true,
	}
	SpecBox3 = Spec{
		name:           "box3",
		LogicalW:       160,
		LogicalH:       120,
		ArtScale:       2,
		BodyTextScale:  2,
		TitleTextScale: 2,
		Safe:           image.Rect(4, 4, 156, 116),
		Round:          false,
	}
)

func SpecForGeometry(width, height int, shape string) Spec {
	if shape == "rect" || width != height {
		return SpecBox3
	}
	return SpecVoCat
}

// Layout principles (floristry):
//   - Focal flower: one primary element per screen; secondary chrome stays quieter in size, weight, or contrast.
//   - Greenery: consistent margins and gutters from a 2/4/8 logical-px spacing scale; keep round screens inside the safe square.
//   - Odd numbers: prefer 3 or 5 items per row/page and 3 emphasis tiers; turn the vase and verify every screen on both geometries.
type pixelCanvas struct {
	w, h int
	img  *image.RGBA
}

var pixelCanvasPool sync.Pool

func newPixelCanvas(w, h int) *pixelCanvas {
	if v := pixelCanvasPool.Get(); v != nil {
		c := v.(*pixelCanvas)
		if cap(c.img.Pix) >= w*h*4 {
			c.w, c.h = w, h
			c.img.Rect = image.Rect(0, 0, w, h)
			c.img.Stride = w * 4
			c.img.Pix = c.img.Pix[:w*h*4]
			return c
		}
	}
	return &pixelCanvas{
		w:   w,
		h:   h,
		img: image.NewRGBA(image.Rect(0, 0, w, h)),
	}
}

func releasePixelCanvas(c *pixelCanvas) {
	if c != nil {
		pixelCanvasPool.Put(c)
	}
}

func (c *pixelCanvas) fill(col imagecolor.RGBA) {
	c.rect(0, 0, c.w, c.h, col)
}

func (c *pixelCanvas) set(x, y int, col imagecolor.RGBA) {
	if x < 0 || y < 0 || x >= c.w || y >= c.h || col.A == 0 {
		return
	}
	if col.A == 255 {
		c.img.SetRGBA(x, y, col)
		return
	}
	dst := c.img.RGBAAt(x, y)
	a := int(col.A)
	inv := 255 - a
	c.img.SetRGBA(x, y, imagecolor.RGBA{
		R: uint8((int(col.R)*a + int(dst.R)*inv + 127) / 255),
		G: uint8((int(col.G)*a + int(dst.G)*inv + 127) / 255),
		B: uint8((int(col.B)*a + int(dst.B)*inv + 127) / 255),
		A: 255,
	})
}

func (c *pixelCanvas) rect(x, y, w, h int, col imagecolor.RGBA) {
	if w <= 0 || h <= 0 || col.A == 0 {
		return
	}
	x0, y0 := max(0, x), max(0, y)
	x1, y1 := min(c.w, x+w), min(c.h, y+h)
	for py := y0; py < y1; py++ {
		for px := x0; px < x1; px++ {
			c.set(px, py, col)
		}
	}
}

func (c *pixelCanvas) strokeRect(x, y, w, h int, col imagecolor.RGBA) {
	if w <= 0 || h <= 0 || col.A == 0 {
		return
	}
	c.rect(x, y, w, 1, col)
	c.rect(x, y+h-1, w, 1, col)
	c.rect(x, y, 1, h, col)
	c.rect(x+w-1, y, 1, h, col)
}

func (c *pixelCanvas) panel(x, y, w, h int, bg, border imagecolor.RGBA) {
	if w < 2 || h < 2 {
		return
	}
	c.rect(x+1, y+1, w-2, h-2, bg)
	c.rect(x+1, y, w-2, 1, border)
	c.rect(x+1, y+h-1, w-2, 1, border)
	c.rect(x, y+1, 1, h-2, border)
	c.rect(x+w-1, y+1, 1, h-2, border)
}

func (c *pixelCanvas) line(x0, y0, x1, y1 int, col imagecolor.RGBA) {
	dx := x1 - x0
	if dx < 0 {
		dx = -dx
	}
	dy := y1 - y0
	if dy < 0 {
		dy = -dy
	}
	sx := -1
	if x0 < x1 {
		sx = 1
	}
	sy := -1
	if y0 < y1 {
		sy = 1
	}
	err := dx - dy
	for {
		c.set(x0, y0, col)
		if x0 == x1 && y0 == y1 {
			break
		}
		e2 := 2 * err
		if e2 > -dy {
			err -= dy
			x0 += sx
		}
		if e2 < dx {
			err += dx
			y0 += sy
		}
	}
}

func (c *pixelCanvas) blit(x, y int, src *image.RGBA) {
	if src == nil {
		return
	}
	b := src.Bounds()
	for sy := b.Min.Y; sy < b.Max.Y; sy++ {
		dy := y + (sy - b.Min.Y)
		if dy < 0 || dy >= c.h {
			continue
		}
		for sx := b.Min.X; sx < b.Max.X; sx++ {
			dx := x + (sx - b.Min.X)
			if dx < 0 || dx >= c.w {
				continue
			}
			col := src.RGBAAt(sx, sy)
			if col.A > 0 {
				c.set(dx, dy, col)
			}
		}
	}
}

func (c *pixelCanvas) blitSprite(x, y int, rows []string, pal map[string]imagecolor.RGBA, scale int) {
	if scale < 1 {
		scale = 1
	}
	for j, row := range rows {
		for i := range len(row) {
			col, ok := pal[row[i:i+1]]
			if !ok || col.A == 0 {
				continue
			}
			c.rect(x+i*scale, y+j*scale, scale, scale, col)
		}
	}
}

func (c *pixelCanvas) applyLUT(grade GradeKey) {
	if grade == GradeDay {
		return
	}
	lut := &atmosphereLUTs[grade]
	pix := c.img.Pix
	for i := 0; i < len(pix); i += 4 {
		pix[i] = lut[0][pix[i]]
		pix[i+1] = lut[1][pix[i+1]]
		pix[i+2] = lut[2][pix[i+2]]
	}
}

// Rendered screens are recycled by the terminal renderer once converted to
// RGB565. Callers that keep an image (tests, one-off avatars) simply never
// release it, so it is not reused while they hold it.
var pixelImagePool sync.Pool

func acquirePixelImage(w, h int) *image.RGBA {
	if v := pixelImagePool.Get(); v != nil {
		img := v.(*image.RGBA)
		if cap(img.Pix) >= w*h*4 {
			img.Rect = image.Rect(0, 0, w, h)
			img.Stride = w * 4
			img.Pix = img.Pix[:w*h*4]
			return img
		}
	}
	return image.NewRGBA(image.Rect(0, 0, w, h))
}

func ReleaseImage(img *image.RGBA) {
	if img != nil {
		pixelImagePool.Put(img)
	}
}

func (c *pixelCanvas) scaleNearest(scale int) *image.RGBA {
	if scale < 1 {
		scale = 1
	}
	out := acquirePixelImage(c.w*scale, c.h*scale)
	if scale == 1 {
		copy(out.Pix, c.img.Pix)
		return out
	}
	src := c.img.Pix
	dst := out.Pix
	srcStride := c.img.Stride
	dstStride := out.Stride
	for y := range c.h {
		sRow := src[y*srcStride : y*srcStride+c.w*4]
		for sy := range scale {
			dRow := dst[(y*scale+sy)*dstStride:]
			for x := range c.w {
				si := x * 4
				r, g, b, a := sRow[si], sRow[si+1], sRow[si+2], sRow[si+3]
				di := x * scale * 4
				for sx := range scale {
					dRow[di+sx*4] = r
					dRow[di+sx*4+1] = g
					dRow[di+sx*4+2] = b
					dRow[di+sx*4+3] = a
				}
			}
		}
	}
	return out
}

func (c *pixelCanvas) text(x, y int, s string, f dogica.Font, col imagecolor.RGBA) {
	drawTextPhysical(c.img, x, y, s, f, 1, col)
}

func lookupGlyph(f dogica.Font, r rune) dogica.Glyph {
	if g, ok := f.Glyphs[r]; ok {
		return g
	}
	if g, ok := f.Glyphs['?']; ok {
		return g
	}
	return f.Glyphs[' ']
}

func drawTextPhysical(dst *image.RGBA, x, y int, s string, f dogica.Font, scale int, col imagecolor.RGBA) {
	if dst == nil || col.A == 0 {
		return
	}
	if scale < 1 {
		scale = 1
	}
	b := dst.Bounds()
	cx := x
	for _, r := range s {
		g := lookupGlyph(f, r)
		gx0 := cx + int(g.XOff)*scale
		gy0 := y + int(g.YOff)*scale
		w := int(g.Width)
		for rowIdx, rowBits := range g.Rows {
			py := gy0 + rowIdx*scale
			for bit := range w {
				if rowBits&(1<<(w-1-bit)) == 0 {
					continue
				}
				px := gx0 + bit*scale
				for dy := range scale {
					yy := py + dy
					if yy < b.Min.Y || yy >= b.Max.Y {
						continue
					}
					for dx := range scale {
						xx := px + dx
						if xx < b.Min.X || xx >= b.Max.X {
							continue
						}
						dst.SetRGBA(xx, yy, col)
					}
				}
			}
		}
		cx += int(g.Advance) * scale
	}
}

func measureTextPx(s string, f dogica.Font, scale int) int {
	if scale < 1 {
		scale = 1
	}
	w := 0
	for _, r := range s {
		w += int(lookupGlyph(f, r).Advance) * scale
	}
	return w
}

func truncateEllipsisPx(s string, f dogica.Font, scale, maxW int) string {
	s = strings.TrimSpace(s)
	if maxW <= 0 {
		return ""
	}
	if measureTextPx(s, f, scale) <= maxW {
		return s
	}
	const ell = "…"
	ellW := measureTextPx(ell, f, scale)
	if ellW > maxW {
		return ""
	}
	runes := []rune(s)
	for len(runes) > 0 {
		cand := strings.TrimRight(string(runes), " ")
		if measureTextPx(cand, f, scale)+ellW <= maxW {
			return cand + ell
		}
		runes = runes[:len(runes)-1]
	}
	return ell
}

func wrapTextPx(s string, f dogica.Font, scale, maxW int) []string {
	if maxW <= 0 {
		return nil
	}
	var lines []string
	for _, para := range strings.Split(s, "\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			lines = append(lines, "")
			continue
		}
		words := strings.Fields(para)
		cur := ""
		for _, word := range words {
			cand := word
			if cur != "" {
				cand = cur + " " + word
			}
			if measureTextPx(cand, f, scale) <= maxW {
				cur = cand
				continue
			}
			if cur != "" {
				lines = append(lines, cur)
				cur = ""
			}
			if measureTextPx(word, f, scale) <= maxW {
				cur = word
				continue
			}
			// Hard-break a single word wider than maxW.
			var chunk []rune
			for _, r := range word {
				next := append(chunk, r)
				if measureTextPx(string(next), f, scale) > maxW && len(chunk) > 0 {
					lines = append(lines, string(chunk))
					chunk = []rune{r}
				} else {
					chunk = next
				}
			}
			cur = string(chunk)
		}
		if cur != "" {
			lines = append(lines, cur)
		}
	}
	return lines
}

func paginateLines(lines []string, lineH, maxH int) [][]string {
	if lineH <= 0 || maxH < lineH {
		return nil
	}
	perPage := maxH / lineH
	if perPage < 1 {
		perPage = 1
	}
	if len(lines) == 0 {
		return [][]string{{""}}
	}
	var pages [][]string
	for i := 0; i < len(lines); i += perPage {
		end := min(len(lines), i+perPage)
		pages = append(pages, lines[i:end])
	}
	return pages
}
