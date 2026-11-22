package main

import (
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
	board := &wsTerminal{key: "board", companionID: "board", client: &wsClient{role: "device"}}
	hub.atmosphere = true
	hub.parents = map[string]*parentRuntime{"board": {loc: time.FixedZone("configured", 9*60*60), configured: true}}
	if g := hub.frameGrade(board, at(13, 0)); g != gradeNight {
		t.Fatalf("device timezone not used: %d", g)
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
				if j > 0 && math.Abs(float64(x-previous)) > 7 {
					t.Errorf("abrupt transition at %d on channel %d: %d -> %d", edge.minute, c, previous, x)
				}
				previous = x
			}
		}
	}
	luminance := func(g gradeKey, rgb [3]byte) float64 {
		l := &atmosphereLUTs[g]
		lin := func(x uint8) float64 {
			v := float64(x) / 255
			if v <= .04045 {
				return v / 12.92
			}
			return math.Pow((v+.055)/1.055, 2.4)
		}
		return .2126*lin(l[0][rgb[0]]) + .7152*lin(l[1][rgb[1]]) + .0722*lin(l[2][rgb[2]])
	}
	for _, g := range atmosphereKeys {
		for c := range 3 {
			for i := 1; i < 255; i++ {
				x := atmosphereLUTs[g][c][i]
				if x < atmosphereLUTs[g][c][i-1] || x == 0 || x == 255 ||
					g == gradeDay && int(x) != i {
					t.Fatalf("grade %d/%d/%d mapped to %d", g, c, i, x)
				}
			}
		}
		for _, p := range [][2][3]byte{
			{{0xFA, 0xF6, 0xF0}, {0x1A, 0x17, 0x22}}, // cream / ink
			{{0xFA, 0xEF, 0xF6}, {0x24, 0x1F, 0x2D}}, // dark chrome
			{{0xF5, 0xB8, 0x95}, {0x38, 0x22, 0x1E}}, // primary label
		} {
			if ratio := (luminance(g, p[0]) + .05) / (luminance(g, p[1]) + .05); ratio < 4.5 {
				t.Errorf("grade %d lowered chrome contrast to %.2f", g, ratio)
			}
		}
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
	dev := &wsTerminal{screen: shape, client: &wsClient{role: "device"}}
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
