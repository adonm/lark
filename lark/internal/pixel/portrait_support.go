package pixel

import (
	"image"
	imagecolor "image/color"
	"slices"
)

const SheetSide = 1024

var Poses = [...]string{"idle", "blink", "talk", "think"}

func corner(img *image.RGBA, r image.Rectangle) imagecolor.RGBA {
	var channels [3][]uint8
	for _, x := range []int{r.Min.X + 8, r.Max.X - 13} {
		for _, y := range []int{r.Min.Y + 8, r.Max.Y - 13} {
			for j := range 5 {
				for i := range 5 {
					c := img.RGBAAt(x+i, y+j)
					channels[0] = append(channels[0], c.R)
					channels[1] = append(channels[1], c.G)
					channels[2] = append(channels[2], c.B)
				}
			}
		}
	}
	for _, values := range channels {
		slices.Sort(values)
	}
	return imagecolor.RGBA{channels[0][50], channels[1][50], channels[2][50], 255}
}

func Diff(a, b imagecolor.RGBA) int {
	sq := func(n int) int { return n * n }
	return sq(int(a.R)-int(b.R)) + sq(int(a.G)-int(b.G)) + sq(int(a.B)-int(b.B))
}

func poseBounds(img *image.RGBA, r image.Rectangle, bg imagecolor.RGBA) image.Rectangle {
	w, h := r.Dx(), r.Dy()
	mask := make([]bool, w*h)
	for y := range h {
		for x := range w {
			mask[y*w+x] = Diff(img.RGBAAt(r.Min.X+x, r.Min.Y+y), bg) > 25*25
		}
	}
	var best image.Rectangle
	var queue []int
	largest := 0
	for i := range mask {
		if !mask[i] {
			continue
		}
		mask[i] = false
		queue = append(queue[:0], i)
		bounds := image.Rectangle{Min: image.Pt(w, h)}
		for head := 0; head < len(queue); head++ {
			pos := queue[head]
			x, y := pos%w, pos/w
			bounds.Min = image.Pt(min(bounds.Min.X, x), min(bounds.Min.Y, y))
			bounds.Max = image.Pt(max(bounds.Max.X, x+1), max(bounds.Max.Y, y+1))
			push := func(p int) {
				if mask[p] {
					mask[p] = false
					queue = append(queue, p)
				}
			}
			if x > 0 {
				push(pos - 1)
			}
			if x+1 < w {
				push(pos + 1)
			}
			if y > 0 {
				push(pos - w)
			}
			if y+1 < h {
				push(pos + w)
			}
		}
		if len(queue) > largest {
			best, largest = bounds.Add(r.Min), len(queue)
		}
	}
	if largest < 1024 || best.Dx() < 70 || best.Dy() < 70 {
		return image.Rectangle{}
	}
	return best
}
