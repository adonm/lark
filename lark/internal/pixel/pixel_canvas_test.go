package pixel

import (
	"image"
	imagecolor "image/color"
	"strings"
	"testing"

	"lark/fonts/dogica"
)

func TestPixelCanvasExactIntegerScaling(t *testing.T) {
	t.Parallel()
	c := newPixelCanvas(4, 3)
	colors := []imagecolor.RGBA{
		{255, 0, 0, 255},
		{0, 255, 0, 255},
		{0, 0, 255, 255},
		{255, 255, 0, 255},
	}
	for y := range 3 {
		for x := range 4 {
			c.set(x, y, colors[(x+y)%len(colors)])
		}
	}
	for _, scale := range []int{2, 3} {
		out := c.scaleNearest(scale)
		if out.Bounds() != image.Rect(0, 0, 4*scale, 3*scale) {
			t.Fatalf("scale %d bounds = %v", scale, out.Bounds())
		}
		for y := range 3 {
			for x := range 4 {
				want := c.img.RGBAAt(x, y)
				for dy := range scale {
					for dx := range scale {
						got := out.RGBAAt(x*scale+dx, y*scale+dy)
						if got != want {
							t.Fatalf("scale %d at (%d,%d) block (%d,%d): got %v, want %v", scale, x, y, dx, dy, got, want)
						}
					}
				}
			}
		}
	}
}

func TestPixelTextWrapAndEllipsis(t *testing.T) {
	t.Parallel()
	// At scale 2, VoCat safe width is 84*3 = 252 px (with 8px padding each side -> 236 px).
	lines := wrapTextPx("Message from Mum: dinner's ready in five minutes!", dogica.Regular, 2, 160)
	if len(lines) < 2 {
		t.Fatalf("expected wrapped lines, got %v", lines)
	}
	for _, line := range lines {
		if w := measureTextPx(line, dogica.Regular, 2); w > 160 {
			t.Fatalf("line %q width %d > 160", line, w)
		}
	}
	short := truncateEllipsisPx("The Clockwork Owl Adventure", dogica.Bold, 2, 120)
	if !strings.HasSuffix(short, "…") {
		t.Fatalf("expected ellipsis suffix, got %q", short)
	}
	if w := measureTextPx(short, dogica.Bold, 2); w > 120 {
		t.Fatalf("truncated %q width %d > 120", short, w)
	}
	unchanged := truncateEllipsisPx("Books", dogica.Bold, 2, 120)
	if unchanged != "Books" {
		t.Fatalf("expected unchanged 'Books', got %q", unchanged)
	}
	pages := paginateLines([]string{"a", "b", "c", "d", "e"}, 20, 45)
	if len(pages) != 3 || len(pages[0]) != 2 || len(pages[2]) != 1 {
		t.Fatalf("unexpected pages: %v", pages)
	}
}

func TestPortraitQuantizationMax32Colors(t *testing.T) {
	t.Parallel()
	sheet := image.NewRGBA(image.Rect(0, 0, SheetSide, SheetSide))
	bg := imagecolor.RGBA{240, 235, 250, 255}
	for y := range SheetSide {
		for x := range SheetSide {
			sheet.SetRGBA(x, y, bg)
		}
	}
	// Draw 4 distinct-gradient silhouettes in the 4 quadrants (> 1000 unique colors).
	const side = SheetSide / 2
	for q := range 4 {
		qx, qy := (q%2)*side, (q/2)*side
		for y := 100; y < 400; y++ {
			for x := 100; x < 400; x++ {
				dx, dy := x-250, y-250
				if dx*dx+dy*dy <= 120*120 {
					sheet.SetRGBA(qx+x, qy+y, imagecolor.RGBA{
						R: uint8((x * 255) / 400),
						G: uint8((y * 255) / 400),
						B: uint8(((x + y + q*30) * 255) / 800),
						A: 255,
					})
				}
			}
		}
	}
	poses, err := NormalizePortraitSheet(sheet, PortraitHeight)
	if err != nil {
		t.Fatalf("NormalizePortraitSheet: %v", err)
	}
	unique := map[imagecolor.RGBA]struct{}{}
	for i, p := range poses {
		if p == nil {
			t.Fatalf("pose %d is nil", i)
		}
		b := p.Bounds()
		if b.Dy() > PortraitHeight {
			t.Fatalf("pose %d height %d > %d", i, b.Dy(), PortraitHeight)
		}
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				c := p.RGBAAt(x, y)
				if c.A == 0 {
					continue
				}
				if c.A != 255 {
					t.Fatalf("pose %d has semi-transparent pixel %v", i, c)
				}
				unique[c] = struct{}{}
			}
		}
	}
	if len(unique) == 0 || len(unique) > pixelMaxColors {
		t.Fatalf("expected 1..%d unique opaque colors, got %d", pixelMaxColors, len(unique))
	}
}
