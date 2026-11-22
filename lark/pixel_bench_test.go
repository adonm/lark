package main

import "testing"

func BenchmarkRenderPixelHome(b *testing.B) {
	poses := pixelPlaceholderPoses()
	for _, spec := range []pixelDisplaySpec{pixelSpecVoCat, pixelSpecBox3} {
		b.Run(spec.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = renderPixelHome(spec, nil, poses[0], gradeDay, "listening", 0, "", false)
			}
		})
	}
}

func BenchmarkRenderPixelMenu(b *testing.B) {
	th := defaultPixelTheme()
	for _, spec := range []pixelDisplaySpec{pixelSpecVoCat, pixelSpecBox3} {
		b.Run(spec.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = renderPixelMenu(spec, 0, th)
			}
		})
	}
}
