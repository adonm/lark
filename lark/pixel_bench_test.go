package main

import (
	"fmt"
	"image"
	imagecolor "image/color"
	"image/draw"
	"strings"
	"testing"

	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
)

func BenchmarkRenderPixelScene(b *testing.B) {
	var svg strings.Builder
	svg.WriteString(svgStart + `<rect width="360" height="360" fill="#FAF6F0"/>`)
	for y := range 16 {
		for x := range 16 {
			if (x+y)%3 != 0 {
				fmt.Fprintf(&svg, `<rect x="%d" y="%d" width="12" height="12" fill="#%s"/>`, 84+x*12, 72+y*12,
					[...]string{"F5B895", "1A1722", "5EEAD4"}[(x+y/2)%3])
			}
		}
	}
	svg.WriteString(`<circle cx="180" cy="180" r="9" fill="#FDE047"/></svg>`)
	raw := svg.String()
	b.ResetTimer()
	for range b.N {
		if _, err := buildScreenUncached(raw, true, roundScreen); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRenderDenseSprite(b *testing.B) {
	for _, size := range []int{32, 64} {
		scale := 9
		if size == 64 {
			scale = 3
		}
		origin := (360 - size*scale) / 2
		art, err := compileSprite(denseSpriteRows(size), nil, origin, origin, scale)
		if err != nil {
			b.Fatal(err)
		}
		scene, err := prepareScene(svgStart+art+"</svg>", false)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(fmt.Sprintf("%dx%d/direct", size, size), func(b *testing.B) {
			for range b.N {
				if _, err := scene.raster(360, 360, 0, 0, 360, imagecolor.Black, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("%dx%d/vector", size, size), func(b *testing.B) {
			for range b.N {
				img := image.NewRGBA(image.Rect(0, 0, 360, 360))
				draw.Draw(img, img.Bounds(), image.NewUniform(imagecolor.Black), image.Point{}, draw.Src)
				scan := screenScanner{rasterx.NewScannerGV(360, 360, img, img.Bounds()), img}
				icon, err := oksvg.ReadIconStream(strings.NewReader(scene.svg), oksvg.StrictErrorMode)
				if err != nil {
					b.Fatal(err)
				}
				icon.SetTarget(0, 0, 360, 360)
				icon.Draw(rasterx.NewDasher(360, 360, scan), 1)
			}
		})
	}
}
