package main

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

	xdraw "golang.org/x/image/draw"
)

const (
	pixelPortraitHeight = 80
	pixelPortraitMaxW   = 84
	pixelMaxColors      = 32
)

func normalizePortraitSheet(sheet image.Image, targetH int) ([4]*image.RGBA, error) {
	var poses [4]*image.RGBA
	if sheet == nil || sheet.Bounds().Size() != image.Pt(portraitSheetSide, portraitSheetSide) {
		return poses, errors.New("portrait sheet must be 1024×1024")
	}
	if targetH < 16 {
		targetH = pixelPortraitHeight
	}
	rgba := image.NewRGBA(image.Rect(0, 0, portraitSheetSide, portraitSheetSide))
	draw.Draw(rgba, rgba.Bounds(), sheet, sheet.Bounds().Min, draw.Src)

	const side = portraitSheetSide / 2
	var crops [4]image.Rectangle
	var bgs [4]imagecolor.RGBA
	maxCropW, maxCropH := 1, 1
	for i, pose := range portraitPoses {
		cell := image.Rect(0, 0, side, side).Add(image.Pt(i%2*side, i/2*side))
		bgs[i] = portraitCorner(rgba, cell)
		inner := cell.Inset(10)
		b := portraitBounds(rgba, inner, bgs[i])
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
		masked := image.NewRGBA(image.Rect(0, 0, crop.Dx(), crop.Dy()))
		for y := crop.Min.Y; y < crop.Max.Y; y++ {
			for x := crop.Min.X; x < crop.Max.X; x++ {
				c := rgba.RGBAAt(x, y)
				if c.A < 128 || portraitDiff(c, bgs[i]) <= 28*28 {
					continue
				}
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
					c.A = 255
					dst.SetRGBA(x, y, c)
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
	bestDist := portraitDiff(c, best)
	for _, p := range palette[1:] {
		if d := portraitDiff(c, p); d < bestDist {
			best, bestDist = p, d
		}
	}
	return best
}

func pixelPoseFilename(pose string) string {
	return "pixel_" + pose + ".png"
}

func saveNormalizedPoses(dir string, poses [4]*image.RGBA) error {
	for i, pose := range portraitPoses {
		if poses[i] == nil {
			continue
		}
		path := filepath.Join(dir, pixelPoseFilename(pose))
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

func loadOrNormalizePoses(dir string, targetH int) ([4]*image.RGBA, error) {
	var poses [4]*image.RGBA
	allLoaded := true
	for i, pose := range portraitPoses {
		f, err := os.Open(filepath.Join(dir, pixelPoseFilename(pose)))
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
	poses, err = normalizePortraitSheet(sheet, targetH)
	if err != nil {
		return poses, err
	}
	_ = saveNormalizedPoses(dir, poses)
	return poses, nil
}
