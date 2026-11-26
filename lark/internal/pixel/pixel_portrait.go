package pixel

import (
	"errors"
	"fmt"
	"image"
	imagecolor "image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"

	xdraw "golang.org/x/image/draw"
)

const (
	PortraitHeight    = 80
	pixelPortraitMaxW = 84
	pixelMaxColors    = 32
)

func NormalizePortraitSheet(sheet image.Image, targetH int) ([4]*image.RGBA, error) {
	var poses [4]*image.RGBA
	if sheet == nil || sheet.Bounds().Size() != image.Pt(SheetSide, SheetSide) {
		return poses, errors.New("portrait sheet must be 1024×1024")
	}
	if targetH < 16 {
		targetH = PortraitHeight
	}
	rgba := image.NewRGBA(image.Rect(0, 0, SheetSide, SheetSide))
	draw.Draw(rgba, rgba.Bounds(), sheet, sheet.Bounds().Min, draw.Src)

	const side = SheetSide / 2
	var crops [4]image.Rectangle
	var bgs [4]imagecolor.RGBA
	maxCropW, maxCropH := 1, 1
	for i, pose := range Poses {
		cell := image.Rect(0, 0, side, side).Add(image.Pt(i%2*side, i/2*side))
		bgs[i] = corner(rgba, cell)
		inner := cell.Inset(10)
		b := poseBounds(rgba, inner, bgs[i])
		if b.Empty() {
			return poses, fmt.Errorf("portrait %s has no silhouette", pose)
		}
		b = b.Inset(-2).Intersect(cell)
		crops[i] = b
		maxCropW = max(maxCropW, b.Dx())
		maxCropH = max(maxCropH, b.Dy())
	}

	scale := min(float64(targetH)/float64(maxCropH), float64(pixelPortraitMaxW)/float64(maxCropW))
	for i, crop := range crops {
		cell := image.Rect(0, 0, side, side).Add(image.Pt(i%2*side, i/2*side))
		cw, ch := cell.Dx(), cell.Dy()
		// Build a foreground mask with tighter tolerance (18) and seal 2px outline gaps
		// before flood-filling from the quadrant borders so interior light fur never leaks.
		fg := make([]bool, cw*ch)
		for ly := range ch {
			for lx := range cw {
				c := rgba.RGBAAt(cell.Min.X+lx, cell.Min.Y+ly)
				if c.A >= 128 && Diff(c, bgs[i]) > 18*18 {
					fg[ly*cw+lx] = true
				}
			}
		}
		sealed := make([]bool, cw*ch)
		copy(sealed, fg)
		for ly := 2; ly < ch-2; ly++ {
			for lx := 2; lx < cw-2; lx++ {
				if sealed[ly*cw+lx] {
					continue
				}
				if (fg[ly*cw+lx-1] || fg[ly*cw+lx-2]) && (fg[ly*cw+lx+1] || fg[ly*cw+lx+2]) ||
					(fg[(ly-1)*cw+lx] || fg[(ly-2)*cw+lx]) && (fg[(ly+1)*cw+lx] || fg[(ly+2)*cw+lx]) {
					sealed[ly*cw+lx] = true
				}
			}
		}
		bgMask := make([]bool, cw*ch)
		var q []int
		tryPush := func(lx, ly int) {
			if lx < 0 || ly < 0 || lx >= cw || ly >= ch {
				return
			}
			idx := ly*cw + lx
			if bgMask[idx] || sealed[idx] {
				return
			}
			bgMask[idx] = true
			q = append(q, idx)
		}
		for lx := range cw {
			tryPush(lx, 0)
			tryPush(lx, ch-1)
		}
		for ly := range ch {
			tryPush(0, ly)
			tryPush(cw-1, ly)
		}
		for head := 0; head < len(q); head++ {
			pos := q[head]
			lx, ly := pos%cw, pos/cw
			tryPush(lx-1, ly)
			tryPush(lx+1, ly)
			tryPush(lx, ly-1)
			tryPush(lx, ly+1)
		}

		masked := image.NewRGBA(image.Rect(0, 0, crop.Dx(), crop.Dy()))
		for y := crop.Min.Y; y < crop.Max.Y; y++ {
			for x := crop.Min.X; x < crop.Max.X; x++ {
				lx, ly := x-cell.Min.X, y-cell.Min.Y
				if bgMask[ly*cw+lx] {
					continue
				}
				c := rgba.RGBAAt(x, y)
				masked.SetRGBA(x-crop.Min.X, y-crop.Min.Y, imagecolor.RGBA{c.R, c.G, c.B, 255})
			}
		}
		w := max(1, int(math.Round(float64(crop.Dx())*scale)))
		h := max(1, int(math.Round(float64(crop.Dy())*scale)))
		dst := image.NewRGBA(image.Rect(0, 0, w, h))
		xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), masked, masked.Bounds(), draw.Src, nil)
		// Snap alpha to 0 or 255 and un-premultiply edge pixels so there is no dark halo.
		for y := range h {
			for x := range w {
				c := dst.RGBAAt(x, y)
				if c.A < 128 {
					dst.SetRGBA(x, y, imagecolor.RGBA{})
					continue
				}
				if c.A < 255 {
					c.R = uint8(min(255, int(c.R)*255/int(c.A)))
					c.G = uint8(min(255, int(c.G)*255/int(c.A)))
					c.B = uint8(min(255, int(c.B)*255/int(c.A)))
				}
				c.A = 255
				dst.SetRGBA(x, y, c)
			}
		}
		// Remove isolated 1 px fringe specks after downscaling.
		for y := range h {
			for x := range w {
				if dst.RGBAAt(x, y).A == 0 {
					continue
				}
				n := 0
				if x > 0 && dst.RGBAAt(x-1, y).A > 0 {
					n++
				}
				if x+1 < w && dst.RGBAAt(x+1, y).A > 0 {
					n++
				}
				if y > 0 && dst.RGBAAt(x, y-1).A > 0 {
					n++
				}
				if y+1 < h && dst.RGBAAt(x, y+1).A > 0 {
					n++
				}
				if n <= 1 {
					dst.SetRGBA(x, y, imagecolor.RGBA{})
				}
			}
		}
		poses[i] = dst
	}

	quantizePortraitPoses(poses[:], pixelMaxColors)
	return poses, nil
}

func quantizePortraitPoses(imgs []*image.RGBA, maxColors int) {
	if maxColors < 2 {
		maxColors = 2
	}
	var samples []imagecolor.RGBA
	for _, img := range imgs {
		if img == nil {
			continue
		}
		b := img.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				c := img.RGBAAt(x, y)
				if c.A >= 128 {
					samples = append(samples, imagecolor.RGBA{c.R, c.G, c.B, 255})
				}
			}
		}
	}
	if len(samples) == 0 {
		return
	}
	palette := medianCutPalette(samples, maxColors)
	for _, img := range imgs {
		if img == nil {
			continue
		}
		b := img.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				c := img.RGBAAt(x, y)
				if c.A < 128 {
					img.SetRGBA(x, y, imagecolor.RGBA{})
					continue
				}
				img.SetRGBA(x, y, nearestColor(c, palette))
			}
		}
	}
}

func medianCutPalette(samples []imagecolor.RGBA, maxColors int) []imagecolor.RGBA {
	type box struct {
		pts []imagecolor.RGBA
	}
	channelRange := func(pts []imagecolor.RGBA) (ch int, span int) {
		minR, maxR := 255, 0
		minG, maxG := 255, 0
		minB, maxB := 255, 0
		for _, p := range pts {
			minR, maxR = min(minR, int(p.R)), max(maxR, int(p.R))
			minG, maxG = min(minG, int(p.G)), max(maxG, int(p.G))
			minB, maxB = min(minB, int(p.B)), max(maxB, int(p.B))
		}
		rSpan, gSpan, bSpan := maxR-minR, maxG-minG, maxB-minB
		if rSpan >= gSpan && rSpan >= bSpan {
			return 0, rSpan
		}
		if gSpan >= bSpan {
			return 1, gSpan
		}
		return 2, bSpan
	}

	boxes := []box{{pts: samples}}
	for len(boxes) < maxColors {
		bestIdx, bestCh, bestSpan := -1, 0, 0
		for i, bx := range boxes {
			if len(bx.pts) < 2 {
				continue
			}
			ch, sp := channelRange(bx.pts)
			if sp > bestSpan {
				bestIdx, bestCh, bestSpan = i, ch, sp
			}
		}
		if bestIdx < 0 || bestSpan == 0 {
			break
		}
		pts := boxes[bestIdx].pts
		slices.SortFunc(pts, func(a, b imagecolor.RGBA) int {
			switch bestCh {
			case 0:
				return int(a.R) - int(b.R)
			case 1:
				return int(a.G) - int(b.G)
			default:
				return int(a.B) - int(b.B)
			}
		})
		mid := len(pts) / 2
		boxes[bestIdx] = box{pts: pts[:mid]}
		boxes = append(boxes, box{pts: pts[mid:]})
	}

	palette := make([]imagecolor.RGBA, 0, len(boxes))
	for _, bx := range boxes {
		if len(bx.pts) == 0 {
			continue
		}
		var sumR, sumG, sumB int
		for _, p := range bx.pts {
			sumR += int(p.R)
			sumG += int(p.G)
			sumB += int(p.B)
		}
		n := len(bx.pts)
		palette = append(palette, imagecolor.RGBA{
			R: uint8((sumR + n/2) / n),
			G: uint8((sumG + n/2) / n),
			B: uint8((sumB + n/2) / n),
			A: 255,
		})
	}
	return palette
}

func nearestColor(c imagecolor.RGBA, palette []imagecolor.RGBA) imagecolor.RGBA {
	best := palette[0]
	bestDist := Diff(c, best)
	for _, p := range palette[1:] {
		if d := Diff(c, p); d < bestDist {
			best, bestDist = p, d
		}
	}
	return best
}

func PoseFilename(pose string) string {
	return "pixel_" + pose + ".png"
}

func SaveNormalizedPoses(dir string, poses [4]*image.RGBA) error {
	for i, pose := range Poses {
		if poses[i] == nil {
			continue
		}
		path := filepath.Join(dir, PoseFilename(pose))
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		if err := errors.Join(png.Encode(f, poses[i]), f.Close()); err != nil {
			return err
		}
	}
	return nil
}

func LoadOrNormalizePoses(dir string, targetH int) ([4]*image.RGBA, error) {
	var poses [4]*image.RGBA
	allLoaded := true
	for i, pose := range Poses {
		f, err := os.Open(filepath.Join(dir, PoseFilename(pose)))
		if err != nil {
			allLoaded = false
			break
		}
		img, err := png.Decode(f)
		_ = f.Close()
		if err != nil {
			allLoaded = false
			break
		}
		rgba := image.NewRGBA(image.Rect(0, 0, img.Bounds().Dx(), img.Bounds().Dy()))
		draw.Draw(rgba, rgba.Bounds(), img, img.Bounds().Min, draw.Src)
		poses[i] = rgba
	}
	if allLoaded {
		return poses, nil
	}
	f, err := os.Open(filepath.Join(dir, "sheet.png"))
	if err != nil {
		return poses, err
	}
	defer f.Close()
	sheet, _, err := image.Decode(f)
	if err != nil {
		return poses, err
	}
	poses, err = NormalizePortraitSheet(sheet, targetH)
	if err != nil {
		return poses, err
	}
	_ = SaveNormalizedPoses(dir, poses)
	return poses, nil
}

type Theme struct {
	Bg      *image.RGBA
	Accent  imagecolor.RGBA
	Accent2 imagecolor.RGBA
	Border  imagecolor.RGBA
}

func DefaultTheme() Theme {
	return Theme{
		Accent:  Peach,
		Accent2: Mint,
		Border:  pxTileBorder,
	}
}

func ensureBrightAccent(c imagecolor.RGBA) imagecolor.RGBA {
	// Ensure high contrast against dark panel/Tile backgrounds (~pxPanelBG 30,26,43).
	lum := 0.2126*float64(c.R) + 0.7152*float64(c.G) + 0.0722*float64(c.B)
	if lum < 145 {
		mix := (165 - lum) / max(1, 255-lum)
		c.R = uint8(min(255, int(float64(c.R)+(255-float64(c.R))*mix)))
		c.G = uint8(min(255, int(float64(c.G)+(255-float64(c.G))*mix)))
		c.B = uint8(min(255, int(float64(c.B)+(255-float64(c.B))*mix)))
	}
	c.A = 255
	return c
}

func DeriveTheme(bg *image.RGBA, poses []*image.RGBA) Theme {
	th := DefaultTheme()
	th.Bg = bg
	type bucket struct {
		r, g, b int
		count   int
		score   float64
	}
	buckets := map[uint32]*bucket{}
	for _, img := range poses {
		if img == nil {
			continue
		}
		b := img.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				c := img.RGBAAt(x, y)
				if c.A < 128 {
					continue
				}
				key := uint32(c.R>>4)<<8 | uint32(c.G>>4)<<4 | uint32(c.B>>4)
				bk := buckets[key]
				if bk == nil {
					bk = &bucket{}
					buckets[key] = bk
				}
				bk.r += int(c.R)
				bk.g += int(c.G)
				bk.b += int(c.B)
				bk.count++
			}
		}
	}
	if len(buckets) == 0 {
		return th
	}
	var list []*bucket
	for _, bk := range buckets {
		r := float64(bk.r) / float64(bk.count)
		g := float64(bk.g) / float64(bk.count)
		bl := float64(bk.b) / float64(bk.count)
		maxC := max(r, max(g, bl))
		minC := min(r, min(g, bl))
		sat := maxC - minC
		lum := 0.2126*r + 0.7152*g + 0.0722*bl
		if lum < 35 || lum > 245 {
			continue
		}
		bk.score = sat*2.0 + math.Sqrt(float64(bk.count))
		list = append(list, bk)
	}
	if len(list) == 0 {
		return th
	}
	slices.SortFunc(list, func(a, b *bucket) int {
		if b.score > a.score {
			return 1
		}
		if b.score < a.score {
			return -1
		}
		return 0
	})
	avgColor := func(bk *bucket) imagecolor.RGBA {
		return imagecolor.RGBA{
			R: uint8(bk.r / bk.count),
			G: uint8(bk.g / bk.count),
			B: uint8(bk.b / bk.count),
			A: 255,
		}
	}
	c1 := ensureBrightAccent(avgColor(list[0]))
	th.Accent = c1
	th.Border = imagecolor.RGBA{
		R: uint8((int(c1.R)*3 + int(pxPanelBG.R)*5) / 8),
		G: uint8((int(c1.G)*3 + int(pxPanelBG.G)*5) / 8),
		B: uint8((int(c1.B)*3 + int(pxPanelBG.B)*5) / 8),
		A: 255,
	}
	for _, bk := range list[1:] {
		c2 := ensureBrightAccent(avgColor(bk))
		if Diff(c1, c2) > 55*55 {
			th.Accent2 = c2
			break
		}
	}
	return th
}

var (
	placeholderOnce       sync.Once
	placeholderCache      [4]*image.RGBA
	placeholderThemeOnce  sync.Once
	placeholderThemeCache Theme
)

// PlaceholderPoses returns one shared immutable set: every terminal
// without a portrait draws the same creature.
func PlaceholderPoses() [4]*image.RGBA {
	placeholderOnce.Do(func() { placeholderCache = buildPlaceholderPoses() })
	return placeholderCache
}

func PlaceholderTheme() Theme {
	placeholderThemeOnce.Do(func() {
		poses := PlaceholderPoses()
		placeholderThemeCache = DeriveTheme(nil, poses[:])
	})
	return placeholderThemeCache
}

func buildPlaceholderPoses() [4]*image.RGBA {
	// Cute 16x16 creature scaled 4x -> 64x64 for idle, blink, talk, think.
	body := []string{
		"....kkkkkkkk....",
		"..kkmmmmmmmmkk..",
		".kmmmmmmmmmmmmk.",
		".kmmwwwwmmmmmmk.",
		"kmmwmmmmmmmmmmmk",
		"kmmmkmmmmmmkmmmk",
		"kmmmkmmmmmmkmmmk",
		"kmmmmmmmmmmmmmmk",
		"kmmmmppmmppmmmmk",
		"kmmmmmmkkmmmmmmk",
		".kmmmmmmmmmmmmk.",
		".kmmmmmmmmmmmmk.",
		"..kkmmmmmmmmkk..",
		"...kmmk..kmmk...",
		"...kkk....kkk...",
		"................",
	}
	blink := slices.Clone(body)
	blink[5] = "kmmkkmmmmmmkkmmk"
	blink[6] = "kmmmmmmmmmmmmmmk"
	talk := slices.Clone(body)
	talk[9] = "kmmmmmkkkkmmmmmk"
	talk[10] = ".kmmmmkrrkmmmmk."
	think := slices.Clone(body)
	think[5] = "kmmmmkmmmmmmkmmk"
	think[6] = "kmmmmkmmmmmmkmmk"
	think[9] = "kmmmmmmkmmmmmmmk"

	pal := map[string]imagecolor.RGBA{
		".": {},
		"k": {52, 40, 58, 255},
		"m": {154, 229, 195, 255},
		"w": {235, 252, 245, 255},
		"p": {255, 193, 168, 255},
		"r": {235, 130, 145, 255},
	}
	var poses [4]*image.RGBA
	for i, rows := range [4][]string{body, blink, talk, think} {
		c := newPixelCanvas(64, 64)
		c.blitSprite(0, 0, rows, pal, 4)
		poses[i] = c.img
	}
	return poses
}

func LoadBackgroundPNG(dir string) (*image.RGBA, error) {
	f, err := os.Open(filepath.Join(dir, "bg.png"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, err
	}
	rgba := image.NewRGBA(image.Rect(0, 0, img.Bounds().Dx(), img.Bounds().Dy()))
	draw.Draw(rgba, rgba.Bounds(), img, img.Bounds().Min, draw.Src)
	return rgba, nil
}

// darkenBackground scales a companion background to the canvas size (center-cropped)
// and darkens it toward pxPanelBG in pixel space so UI chrome stays legible.
func darkenBackground(c *pixelCanvas, bg *image.RGBA) {
	if bg == nil {
		c.fill(pxPanelBG)
		return
	}
	b := bg.Bounds()
	ox := (b.Dx() - c.w) / 2
	oy := (b.Dy() - c.h) / 2
	for y := range c.h {
		sy := b.Min.Y + oy + y
		if sy < b.Min.Y {
			sy = b.Min.Y
		} else if sy >= b.Max.Y {
			sy = b.Max.Y - 1
		}
		for x := range c.w {
			sx := b.Min.X + ox + x
			if sx < b.Min.X {
				sx = b.Min.X
			} else if sx >= b.Max.X {
				sx = b.Max.X - 1
			}
			src := bg.RGBAAt(sx, sy)
			// 26% scene + 74% dark panel base for readable contrast.
			c.set(x, y, imagecolor.RGBA{
				R: uint8((int(src.R)*26 + int(pxPanelBG.R)*74) / 100),
				G: uint8((int(src.G)*26 + int(pxPanelBG.G)*74) / 100),
				B: uint8((int(src.B)*26 + int(pxPanelBG.B)*74) / 100),
				A: 255,
			})
		}
	}
}
