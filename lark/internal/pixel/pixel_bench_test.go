package pixel

import "testing"

func BenchmarkRenderPixelHome(b *testing.B) {
	poses := PlaceholderPoses()
	for _, spec := range []Spec{SpecVoCat, SpecBox3} {
		b.Run(spec.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = RenderHome(spec, nil, poses[0], GradeDay, "listening", "")
			}
		})
	}
}

func BenchmarkRenderPixelMenu(b *testing.B) {
	th := DefaultTheme()
	for _, spec := range []Spec{SpecVoCat, SpecBox3} {
		b.Run(spec.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = RenderMenu(spec, 0, th)
			}
		})
	}
}

func BenchmarkRenderPixelHomeRecycled(b *testing.B) {
	poses := PlaceholderPoses()
	for _, spec := range []Spec{SpecVoCat, SpecBox3} {
		b.Run(spec.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				img := RenderHome(spec, nil, poses[0], GradeDay, "listening", "")
				ReleaseImage(img)
			}
		})
	}
}
