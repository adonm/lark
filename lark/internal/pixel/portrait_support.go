package pixel

import (
	"errors"
	"fmt"
	"image"
	imagecolor "image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"slices"

	xdraw "golang.org/x/image/draw"
)

const (
	SheetSide  = 1024
	ScreenSide = 360
	CenterY    = 162
)

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

func Slice(sheet image.Image, bg imagecolor.RGBA) ([4]*image.RGBA, error) {
	var poses [4]*image.RGBA
	if sheet.Bounds().Size() != image.Pt(SheetSide, SheetSide) {
		return poses, errors.New("portrait sheet must be 1024×1024")
	}
	img := image.NewRGBA(image.Rect(0, 0, SheetSide, SheetSide))
	xdraw.Draw(img, img.Bounds(), image.NewUniform(bg), image.Point{}, xdraw.Src)
	xdraw.Draw(img, img.Bounds(), sheet, sheet.Bounds().Min, xdraw.Over)
	const side = SheetSide / 2
	var crops [4]image.Rectangle
	var backgrounds [4]imagecolor.RGBA
	scale := 1.0
	for i, pose := range Poses {
		cell := image.Rect(0, 0, side, side).Add(image.Pt(i%2*side, i/2*side))
		backgrounds[i] = corner(img, cell)
		inner := cell.Inset(10)
		b := poseBounds(img, inner, backgrounds[i])
		if b.Empty() || b.Min.X == inner.Min.X || b.Min.Y == inner.Min.Y || b.Max.X == inner.Max.X || b.Max.Y == inner.Max.Y {
			return poses, fmt.Errorf("portrait %s needs one silhouette with a clear margin", pose)
		}
		b = b.Inset(-3).Intersect(cell)
		crops[i] = b
		// Fit the lower crop corners in the bezel, reserving space for raster
		// rounding and pixel blocks. Cap the height at y=42..282 above Menu.
		w, h := float64(b.Dx()), float64(b.Dy())
		const offset, radius = ScreenSide/2 - CenterY, 168
		cornerScale := 2 * (math.Sqrt(w*w*(radius*radius-offset*offset)+h*h*radius*radius) - offset*h) / (w*w + h*h)
		scale = min(scale, cornerScale, 240/h)
	}
	for i, crop := range crops {
		src := img.SubImage(crop).(*image.RGBA)
		for y := crop.Min.Y; y < crop.Max.Y; y++ {
			for x := crop.Min.X; x < crop.Max.X; x++ {
				c := src.RGBAAt(x, y)
				// Feather the backdrop normalization around antialiased edges.
				weight := min(1, max(0, (math.Sqrt(float64(Diff(c, backgrounds[i])))-8)/20))
				channel := func(v, base uint8) uint8 { return uint8(math.Round(weight*float64(v) + (1-weight)*float64(base))) }
				src.SetRGBA(x, y, imagecolor.RGBA{channel(c.R, bg.R), channel(c.G, bg.G), channel(c.B, bg.B), 255})
			}
		}
		out := image.NewRGBA(image.Rect(0, 0, ScreenSide, ScreenSide))
		xdraw.Draw(out, out.Bounds(), image.NewUniform(bg), image.Point{}, xdraw.Src)
		w, h := int(math.Round(float64(crop.Dx())*scale)), int(math.Round(float64(crop.Dy())*scale))
		x, y := (ScreenSide-w)/2, CenterY-h/2
		xdraw.CatmullRom.Scale(out, image.Rect(x, y, x+w, y+h), src, crop, xdraw.Src, nil)
		poses[i] = out
	}
	return poses, nil
}

func WritePNG(dir, name string, img image.Image) error {
	f, err := os.OpenFile(filepath.Join(dir, name+".png"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	return errors.Join(png.Encode(f, img), f.Close())
}

// PoseBounds is the painted bounding box of one pose against its background.
func PoseBounds(img *image.RGBA, r image.Rectangle, bg imagecolor.RGBA) image.Rectangle {
	return poseBounds(img, r, bg)
}
