package main

import (
	imagecolor "image/color"
	"math"
	"slices"
	"testing"
	"time"
)

func TestAtmospherePhasesAndContrast(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 1, 1, h, m, 0, 0, time.UTC) }
	for _, tc := range []struct {
		h int
		g gradeKey
	}{{7, gradeDawn}, {12, gradeDay}, {19, gradeDusk}, {23, gradeNight}} {
		if g := gradeAt(at(tc.h, 0), time.UTC); g != tc.g {
			t.Errorf("%d: got %d, want %d", tc.h, g, tc.g)
		}
	}
	var hub wsHub
	if g := hub.frameGrade(&wsTerminal{}, at(23, 0)); g != gradeDay {
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
	const raw = svgStart + `<rect width="360" height="360" fill="#FAF6F0"/></svg>`
	var cache screenCache
	shape := screenGeometry{320, 240, "rect"}
	day, err := cache.build(raw, false, shape)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		night, err := cache.buildGraded(raw, false, shape, gradeNight)
		if err != nil {
			t.Fatal(err)
		}
		l := &atmosphereLUTs[gradeNight]
		want := pack565(l[0][0xfa], l[1][0xf6], l[2][0xf0])
		if night.fb[0] != want || night.fb[120*320+160] != want || night.source != day.source || night.fb[0] == day.fb[0] {
			t.Fatal("grade did not affect native pixels and side bars only")
		}
		if h, m := cache.stats(); h != uint64(i) || m != 2 {
			t.Fatalf("grade cache: %d hits, %d misses", h, m)
		}
	}
	dev := &wsTerminal{kind: "hardware", screen: shape, client: &wsClient{kind: "hardware"}}
	dev.grade.Store(uint32(gradeNight))
	app, views, err := initApp(savedApp{SVG: raw}, []appPlayer{{id: "board"}}, nil, []*wsTerminal{dev}, nil, buildScreen)
	if err != nil || views[0].grade != gradeNight {
		t.Fatal("app did not use board grade", err)
	}
	app.terminals[0] = nil // A player can disconnect while its render is in flight.
	dev.grade.Store(uint32(gradeDay))
	redrawn, err := app.screens(nil)
	if err != nil || redrawn[0].grade != gradeDay || redrawn[0].raw != views[0].raw || slices.Equal(redrawn[0].fb, views[0].fb) {
		t.Fatal("app reused pixels from the previous grade", err)
	}
}

func BenchmarkRenderAtmosphere(b *testing.B) {
	scene, err := prepareScene(svgStart+`<rect width="360" height="360" fill="#FAF6F0"/>`+
		`<rect x="60" y="105" width="240" height="150" fill="#F5B895"/></svg>`, false)
	if err != nil {
		b.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		g    gradeKey
	}{{"day", gradeDay}, {"night", gradeNight}} {
		b.Run(tc.name, func(b *testing.B) {
			for range b.N {
				if _, err := scene.toRGB565(roundScreen, nil, tc.g); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
