package main

import (
	imagecolor "image/color"
	"math"
	"testing"
	"time"
)

func TestAtmospherePhasesAndContrast(t *testing.T) {
	t.Parallel()
	at := func(h, m int) time.Time { return time.Date(2026, 1, 1, h, m, 0, 0, time.UTC) }
	for _, tc := range []struct {
		h int
		g gradeKey
	}{{7, gradeDawn}, {12, gradeDay}, {19, gradeDusk}, {23, gradeNight}} {
		if g := gradeAt(at(tc.h, 0), time.UTC); g != tc.g {
			t.Errorf("%d: got %d, want %d", tc.h, g, tc.g)
		}
	}
	var hub terminalHub
	if g := hub.frameGrade(&terminalSession{}, at(23, 0)); g != gradeDay {
		t.Fatalf("unconfigured hub did not keep the identity grade: %d", g)
	}
	hub.atmosphere = true
	if gradeAt(at(13, 0), time.FixedZone("configured", 9*60*60)) != gradeNight {
		t.Fatalf("configured timezone not used")
	}
	if gradeAt(at(5, 1), time.UTC) != gradeAt(at(5, 29), time.UTC) {
		t.Fatal("grade changed within a 30-minute bucket")
	}
	for _, edge := range atmosphereEdges {
		for c := range 3 {
			var previous int
			for j, d := range []int{-61, -45, -15, 15, 45, 61} {
				m := edge.minute + d
				g := gradeAt(at(m/60, m%60), time.UTC)
				x := int(atmosphereLUTs[g][c][160])
				if j > 0 && math.Abs(float64(x-previous)) > 36 {
					t.Errorf("abrupt transition at %d on channel %d: %d -> %d", edge.minute, c, previous, x)
				}
				previous = x
			}
		}
	}
	for _, g := range atmosphereKeys {
		for c := range 3 {
			for i := 1; i < 255; i++ {
				x := atmosphereLUTs[g][c][i]
				if x < atmosphereLUTs[g][c][i-1] || g == gradeDay && int(x) != i {
					t.Fatalf("grade %d/%d/%d mapped to %d", g, c, i, x)
				}
			}
		}
	}
}

func TestAtmosphereGradesDifferInLuminanceAndHue(t *testing.T) {
	t.Parallel()
	// Render a neutral-to-colorful test canvas and verify that dawn, day, dusk,
	// and night produce meaningfully different mean luminance and hue/tint.
	meanStats := func(g gradeKey) (lum, rMean, gMean, bMean float64) {
		c := newPixelCanvas(32, 32)
		for y := range 32 {
			for x := range 32 {
				v := uint8(40 + (x+y)*3)
				c.set(x, y, imagecolor.RGBA{v, v, v, 255})
			}
		}
		c.applyLUT(g)
		var sumL, sumR, sumG, sumB float64
		n := float64(32 * 32)
		for i := 0; i < len(c.img.Pix); i += 4 {
			r := float64(c.img.Pix[i])
			gr := float64(c.img.Pix[i+1])
			b := float64(c.img.Pix[i+2])
			sumR += r
			sumG += gr
			sumB += b
			sumL += 0.2126*r + 0.7152*gr + 0.0722*b
		}
		return sumL / n, sumR / n, sumG / n, sumB / n
	}
	lumDay, rDay, _, bDay := meanStats(gradeDay)
	lumDawn, rDawn, _, bDawn := meanStats(gradeDawn)
	lumDusk, rDusk, _, bDusk := meanStats(gradeDusk)
	lumNight, rNight, _, bNight := meanStats(gradeNight)

	// Night must be substantially darker and blue-shifted relative to Day.
	if lumNight >= lumDay*0.65 {
		t.Fatalf("night luminance %.1f not clearly darker than day %.1f", lumNight, lumDay)
	}
	if bNight-rNight <= bDay-rDay+10 {
		t.Fatalf("night not blue-shifted: R=%.1f B=%.1f (day R=%.1f B=%.1f)", rNight, bNight, rDay, bDay)
	}
	// Dawn and Dusk must be warm (red > blue) and distinct from Day.
	if rDawn-bDawn <= 15 {
		t.Fatalf("dawn not warm enough: R=%.1f B=%.1f", rDawn, bDawn)
	}
	if rDusk-bDusk <= 20 {
		t.Fatalf("dusk not warm enough: R=%.1f B=%.1f", rDusk, bDusk)
	}
	if math.Abs(lumDawn-lumDusk) < 3 && math.Abs((rDawn-bDawn)-(rDusk-bDusk)) < 3 {
		t.Fatalf("dawn and dusk are indistinguishable: dawn=(%.1f, R-B=%.1f) dusk=(%.1f, R-B=%.1f)",
			lumDawn, rDawn-bDawn, lumDusk, rDusk-bDusk)
	}
}

func TestAtmosphereCache(t *testing.T) {
	t.Parallel()
	day := renderPixelHome(pixelSpecBox3, nil, nil, gradeDay, "", "")
	night := renderPixelHome(pixelSpecBox3, nil, nil, gradeNight, "", "")
	if day.Bounds() != night.Bounds() {
		t.Fatalf("bounds mismatch: %v vs %v", day.Bounds(), night.Bounds())
	}
	diff := 0
	for i := 0; i < len(day.Pix); i += 4 {
		if day.Pix[i] != night.Pix[i] || day.Pix[i+1] != night.Pix[i+1] || day.Pix[i+2] != night.Pix[i+2] {
			diff++
		}
	}
	if diff == 0 {
		t.Fatal("night grade did not alter home frame pixels")
	}
}

func BenchmarkRenderAtmosphere(b *testing.B) {
	for _, tc := range []struct {
		name string
		g    gradeKey
	}{{"day", gradeDay}, {"night", gradeNight}} {
		b.Run(tc.name, func(b *testing.B) {
			for range b.N {
				_ = renderPixelHome(pixelSpecVoCat, nil, nil, tc.g, "", "")
			}
		})
	}
}
