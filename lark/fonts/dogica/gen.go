//go:build ignore

package main

import (
	"bytes"
	"fmt"
	"go/format"
	"image"
	"image/draw"
	"log"
	"os"
	"sort"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

func requiredRunes() []rune {
	var runes []rune
	for r := rune(0x20); r <= 0x7E; r++ {
		runes = append(runes, r)
	}
	for r := rune(0xA0); r <= 0xFF; r++ {
		runes = append(runes, r)
	}
	runes = append(runes,
		'\u2013', // – en dash
		'\u2014', // — em dash
		'\u2018', // ‘ left single quote
		'\u2019', // ’ right single quote
		'\u201C', // “ left double quote
		'\u201D', // ” right double quote
		'\u2026', // … ellipsis
	)
	sort.Slice(runes, func(i, j int) bool { return runes[i] < runes[j] })
	return runes
}

type bakedGlyph struct {
	r       rune
	advance uint8
	width   uint8
	height  uint8
	xOff    int8
	yOff    int8
	rows    []uint16 // 1 bit per pixel, MSB at left (bit width-1-x)
}

func bakeFont(path string, runes []rune) (int, []bakedGlyph, []rune) {
	raw, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read %s: %v", path, err)
	}
	f, err := opentype.Parse(raw)
	if err != nil {
		log.Fatalf("parse %s: %v", path, err)
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{
		Size:    8,
		DPI:     72,
		Hinting: font.HintingNone,
	})
	if err != nil {
		log.Fatalf("face %s: %v", path, err)
	}
	defer face.Close()

	metrics := face.Metrics()
	ascent := metrics.Ascent.Round()
	var buf sfnt.Buffer
	var glyphs []bakedGlyph
	var missing []rune

	img := image.NewAlpha(image.Rect(0, 0, 24, 24))
	const originX, originY = 4, 16

	for _, r := range runes {
		idx, err := f.GlyphIndex(&buf, r)
		if err != nil || idx == 0 {
			missing = append(missing, r)
			continue
		}
		dr, mask, maskp, adv, ok := face.Glyph(fixed.P(originX, originY), r)
		if !ok {
			missing = append(missing, r)
			continue
		}
		advPx := adv.Round()
		if advPx < 0 {
			advPx = 0
		}
		// Clear scratch image and draw mask.
		draw.Draw(img, img.Bounds(), image.Transparent, image.Point{}, draw.Src)
		if !dr.Empty() {
			draw.DrawMask(img, dr, image.Opaque, image.Point{}, mask, maskp, draw.Src)
		}

		// Find tight bounding box of non-zero pixels (>= 128 alpha).
		minX, minY, maxX, maxY := 24, 24, 0, 0
		for y := 0; y < 24; y++ {
			for x := 0; x < 24; x++ {
				if img.AlphaAt(x, y).A >= 128 {
					if x < minX {
						minX = x
					}
					if y < minY {
						minY = y
					}
					if x+1 > maxX {
						maxX = x + 1
					}
					if y+1 > maxY {
						maxY = y + 1
					}
				}
			}
		}
		if maxX <= minX || maxY <= minY {
			glyphs = append(glyphs, bakedGlyph{r: r, advance: uint8(advPx)})
			continue
		}
		w, h := maxX-minX, maxY-minY
		if w > 16 {
			log.Fatalf("%s rune %U width %d > 16", path, r, w)
		}
		rows := make([]uint16, h)
		for y := 0; y < h; y++ {
			var bits uint16
			for x := 0; x < w; x++ {
				if img.AlphaAt(minX+x, minY+y).A >= 128 {
					bits |= 1 << (w - 1 - x)
				}
			}
			rows[y] = bits
		}
		glyphs = append(glyphs, bakedGlyph{
			r:       r,
			advance: uint8(advPx),
			width:   uint8(w),
			height:  uint8(h),
			xOff:    int8(minX - originX),
			yOff:    int8(minY - (originY - ascent)),
			rows:    rows,
		})
	}
	return ascent, glyphs, missing
}

func writeFontVar(buf *bytes.Buffer, name string, ascent int, glyphs []bakedGlyph) {
	sort.Slice(glyphs, func(i, j int) bool { return glyphs[i].r < glyphs[j].r })
	lower := strings.ToLower(name)

	type entry struct{ off, n int }
	entries := make([]entry, len(glyphs))
	index := make(map[rune]int, len(glyphs))
	var rows []uint16
	for i, g := range glyphs {
		if len(g.rows) > 255 {
			log.Fatalf("%s rune %U has %d rows", name, g.r, len(g.rows))
		}
		entries[i] = entry{off: len(rows), n: len(g.rows)}
		index[g.r] = i
		rows = append(rows, g.rows...)
	}
	if len(rows) > 1<<16 {
		log.Fatalf("%s row arena has %d entries", name, len(rows))
	}

	fmt.Fprintf(buf, "var %sRows = []uint16{\n", lower)
	for i := 0; i < len(rows); i += 16 {
		buf.WriteByte('\t')
		for j := i; j < min(i+16, len(rows)); j++ {
			fmt.Fprintf(buf, "0x%02x, ", rows[j])
		}
		buf.WriteByte('\n')
	}
	buf.WriteString("}\n\n")

	fmt.Fprintf(buf, "var %s = Font{\n\tAscent: %d,\n\tLineHeight: 10,\n\tascii: [95]uint16{\n", name, ascent)
	for r := rune(0x20); r <= 0x7e; r++ {
		if r == 0x20 || (r-0x20)%16 == 0 {
			buf.WriteByte('\t')
		}
		idx := 0xffff
		if i, ok := index[r]; ok {
			idx = i
		}
		fmt.Fprintf(buf, "0x%04x, ", idx)
		if (r-0x20)%16 == 15 || r == 0x7e {
			buf.WriteByte('\n')
		}
	}
	buf.WriteString("\t},\n\tglyphs: []glyphEntry{\n")
	for i, g := range glyphs {
		fmt.Fprintf(buf, "\t\t{0x%02x, %d, %d, %d, %d, %d, %d, %d},\n",
			g.r, g.advance, g.width, g.height, g.xOff, g.yOff, entries[i].off, entries[i].n)
	}
	fmt.Fprintf(buf, "\t},\n\trows: %sRows,\n}\n\n", lower)
}

func main() {
	runes := requiredRunes()
	regAsc, regGlyphs, regMissing := bakeFont("dogicapixel.ttf", runes)
	boldAsc, boldGlyphs, boldMissing := bakeFont("dogicapixelbold.ttf", runes)
	if len(regMissing) > 0 {
		log.Printf("regular missing %d runes: %U", len(regMissing), regMissing)
	}
	if len(boldMissing) > 0 {
		log.Printf("bold missing %d runes: %U", len(boldMissing), boldMissing)
	}

	var buf bytes.Buffer
	buf.WriteString("// Code generated by gen.go; DO NOT EDIT.\n\npackage dogica\n\n")
	buf.WriteString("// Glyph holds a 1-bit pixel glyph rasterised at 8 px with no antialiasing.\n")
	buf.WriteString("type Glyph struct {\n\tAdvance uint8\n\tWidth   uint8\n\tHeight  uint8\n\tXOff    int8\n\tYOff    int8\n\tRows    []uint16\n}\n\n")
	buf.WriteString("// glyphEntry is one sorted rune-table row. rowOff/rowLen index Font.rows.\n")
	buf.WriteString("type glyphEntry struct {\n\tr       rune\n\tadvance uint8\n\twidth   uint8\n\theight  uint8\n\txOff    int8\n\tyOff    int8\n\trowOff  uint16\n\trowLen  uint8\n}\n\n")
	buf.WriteString("// Font holds a baked pixel font as a sorted rune table and a flat row arena.\n")
	buf.WriteString("type Font struct {\n\tAscent     int\n\tLineHeight int\n\tascii      [95]uint16 // glyph index for runes 0x20..0x7e, 0xffff if absent\n\tglyphs     []glyphEntry\n\trows       []uint16\n}\n\n")
	buf.WriteString("const (\n\tasciiFirst = 0x20\n\tasciiLast  = 0x7e\n)\n\n")
	buf.WriteString(`// Lookup returns the glyph for r. ASCII uses a direct table; other runes use
// a binary search over the sorted table. The returned Rows slice aliases the
// font's row arena and stays valid for the lifetime of the program.
func (f Font) Lookup(r rune) (Glyph, bool) {
	i := -1
	if r >= asciiFirst && r <= asciiLast {
		i = int(f.ascii[r-asciiFirst])
	} else {
		lo, hi := 0, len(f.glyphs)
		for lo < hi {
			mid := int(uint(lo+hi) >> 1)
			if f.glyphs[mid].r < r {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		if lo < len(f.glyphs) && f.glyphs[lo].r == r {
			i = lo
		}
	}
	if i < 0 || i >= len(f.glyphs) {
		return Glyph{}, false
	}
	m := f.glyphs[i]
	off, n := int(m.rowOff), int(m.rowLen)
	return Glyph{
		Advance: m.advance, Width: m.width, Height: m.height,
		XOff: m.xOff, YOff: m.yOff, Rows: f.rows[off : off+n],
	}, true
}

// GlyphOrFallback returns r's glyph, else '?', else a space, matching the old
// map-based lookup contract.
func (f Font) GlyphOrFallback(r rune) Glyph {
	if g, ok := f.Lookup(r); ok {
		return g
	}
	if g, ok := f.Lookup('?'); ok {
		return g
	}
	g, _ := f.Lookup(' ')
	return g
}

`)
	writeFontVar(&buf, "Regular", regAsc, regGlyphs)
	writeFontVar(&buf, "Bold", boldAsc, boldGlyphs)

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		log.Fatalf("gofmt: %v", err)
	}
	if err := os.WriteFile("dogica.go", formatted, 0o644); err != nil {
		log.Fatalf("write dogica.go: %v", err)
	}
	fmt.Printf("Generated dogica.go: regular=%d glyphs (ascent=%d), bold=%d glyphs (ascent=%d)\n",
		len(regGlyphs), regAsc, len(boldGlyphs), boldAsc)
}
