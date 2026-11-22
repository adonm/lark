package main

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"image"
	"image/jpeg"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pierrec/lz4/v4"
	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
)

func companionSVG(companion companionState, pose companionPose) string {
	return companionChrome(companion, pose).svg(companionArt(companion, pose))
}

func facePoseSVGs() []string {
	companion := blankCompanion()
	return []string{
		companionSVG(companion, companionPose{mood: "idle", battery: 80}),
		companionSVG(companion, companionPose{mood: "idle", battery: 80, blink: true}),
		companionSVG(companion, companionPose{mood: "speaking", battery: 80}),
		companionSVG(companion, companionPose{mood: "sleepy", battery: 80}),
	}
}

func TestCallScreenTouchTargets(t *testing.T) {
	companion := blankCompanion()
	companion.ID = "mint"
	companion.Name = "Mint & Friends"
	card := intercomCard{sender: companion, text: "A message for you!"}
	for _, tc := range []struct {
		name string
		view sceneView
		ids  []string
	}{
		{"incoming", callScene(callRingingIn, 1, companion, false, 0), []string{"call-answer", "call-decline"}},
		{"outgoing", callScene(callRingingOut, 1, companion, false, 0), []string{"call-hangup"}},
		{"active", callScene(callActive, 1, companion, false, 62*time.Second), []string{"call-mute", "call-hangup"}},
		{"muted", callScene(callActive, 1, companion, true, 62*time.Second), []string{"call-mute", "call-hangup"}},
		{"voice note", card.scene(true), []string{"call", "reply", "done"}},
	} {
		for _, geom := range []screenGeometry{roundScreen, {320, 240, "rect"}} {
			t.Run(tc.name+"/"+strconv.Itoa(geom.width), func(t *testing.T) {
				s, err := buildScreen(tc.view.svg(""), false, geom)
				if err != nil {
					t.Fatal(err)
				}
				if len(s.hits) != len(tc.ids) {
					t.Fatalf("wrong controls: %v", s.hits)
				}
				side := min(geom.width, geom.height)
				px := func(x int) int { return (geom.width-side)/2 + x*side/360 }
				py := func(y int) int { return (geom.height-side)/2 + y*side/360 }
				for i, hit := range s.hits {
					if !slices.Contains(tc.ids, hit.id) {
						t.Fatalf("unexpected control %q", hit.id)
					}
					for _, x := range []int{hit.x1, hit.x2} {
						for _, y := range []int{hit.y1, hit.y2} {
							if !inScreenCircle(x, y) {
								t.Errorf("%s clips the round bezel at %d,%d", hit.id, x, y)
							}
						}
					}
					if px(hit.x2)-px(hit.x1) < 48 || py(hit.y2)-py(hit.y1) < 35 {
						t.Errorf("undersized touch target: %v", hit)
					}
					for _, other := range s.hits[:i] {
						if image.Rect(hit.x1, hit.y1, hit.x2, hit.y2).Overlaps(image.Rect(other.x1, other.y1, other.x2, other.y2)) {
							t.Errorf("overlapping touch targets: %v and %v", hit, other)
						}
					}
					x, y := px((hit.x1+hit.x2)/2), py((hit.y1+hit.y2)/2)
					if sx, sy := geom.inverseTap(x, y); hitID(s.hits, sx, sy) != hit.id {
						t.Errorf("%s cannot be hit at its panel center", hit.id)
					}
					base := s.fb[(y-14)*geom.width+x]
					for yy := y - 6; yy <= y+12; yy++ {
						for xx := x - 20; xx <= x+20; xx++ {
							if s.fb[yy*geom.width+xx] != base {
								goto labelVisible
							}
						}
					}
					t.Errorf("missing label pixels in %s", hit.id)
				labelVisible:
				}
			})
		}
	}
}

func TestMenuScreenTouchTargets(t *testing.T) {
	var games []tile
	var rows []menuRow
	for i := range 10 {
		games = append(games, tile{id: fmt.Sprintf("game-%d", i), label: "Wide WIDE game", icon: "games", detail: "Saved app", colors: navPalette})
		rows = append(rows, menuRow{"A long heading for this detail card", strings.Repeat("Wide W with some text ", 50)})
	}
	for name, scene := range map[string]sceneView{
		"menu-first":     menuHome(heartVoice, true, 55, false, true, 0),
		"menu-last":      menuHome(heartVoice, true, 55, false, true, 1),
		"games":          gamesScene(games, 1, ""),
		"memories":       menuList("Memories", "", rows, 1),
		"history":        menuList("History", "", rows, 1),
		"memory-detail":  menuDetail("Memories", rows[0]),
		"history-detail": menuDetail("History", rows[0]),
		"voice":          voiceScene(heartVoice, 1),
		"call-peers":     peerScene([]menuPeer{{"cat-1", "Friend", "#404040", "#FF0000"}}, 0, true),
	} {
		for _, geom := range []screenGeometry{roundScreen, {320, 240, "rect"}} {
			t.Run(name+"/"+strconv.Itoa(geom.width), func(t *testing.T) {
				s, err := buildScreen(scene.svg(""), false, geom)
				if err != nil {
					t.Fatal(err)
				}
				if len(s.hits) != len(scene.actions) {
					t.Fatalf("wrong controls: %v", s.hits)
				}
				if warnings := layoutWarnings(s.hits, geom.shape == "round"); len(warnings) != 0 {
					t.Error(warnings)
				}
				side := min(geom.width, geom.height)
				px := func(x int) int { return (geom.width-side)/2 + x*side/360 }
				py := func(y int) int { return (geom.height-side)/2 + y*side/360 }
				for i, hit := range s.hits {
					for _, x := range []int{hit.x1, hit.x2} {
						for _, y := range []int{hit.y1, hit.y2} {
							if !inScreenCircle(x, y) || px(x) < 0 || px(x) >= geom.width || py(y) < 0 || py(y) >= geom.height {
								t.Errorf("%s clips the display at %d,%d", hit.id, x, y)
							}
						}
					}
					if px(hit.x2)-px(hit.x1) < 40 || py(hit.y2)-py(hit.y1) < 30 {
						t.Errorf("undersized touch target: %v", hit)
					}
					for _, other := range s.hits[:i] {
						if image.Rect(hit.x1, hit.y1, hit.x2+1, hit.y2+1).Overlaps(image.Rect(other.x1, other.y1, other.x2+1, other.y2+1)) {
							t.Errorf("overlapping touch targets: %v and %v", hit, other)
						}
					}
					x, y := geom.inverseTap(px((hit.x1+hit.x2)/2), py((hit.y1+hit.y2)/2))
					if hitID(s.hits, x, y) != hit.id {
						t.Errorf("%s cannot be hit at its panel center", hit.id)
					}
				}
			})
		}
	}
}

func TestSystemScreenPixelRegions(t *testing.T) {
	type region struct {
		name       string
		box        image.Rectangle
		background string
		wide, tall int
	}
	type scene struct {
		name, svg string
		regions   []region
	}
	reg := func(name, bg string, x, y, w, h, wide, tall int) region {
		return region{name, image.Rect(x, y, x+w, y+h), bg, wide, tall}
	}
	var scenes []scene
	companion := blankCompanion()
	for i, pose := range []companionPose{{mood: "idle"}, {mood: "idle", blink: true}, {mood: "speaking"}, {mood: "sleepy"}} {
		pose.battery = 80
		eyes := []int{20, 4, 9, 10}
		r := []region{
			reg("left eye", companion.FaceBG, 89, 118, 46, 47, 22, eyes[i]),
			reg("right eye", companion.FaceBG, 225, 118, 46, 47, 22, eyes[i]),
			reg("mouth", companion.FaceBG, 139, 188, 82, 44, 34, 8),
			reg("battery", companion.FaceBG, 226, 39, 40, 21, 23, 10),
		}
		if i < 3 {
			bg, icon := navPalette.bg, "menu"
			if i == 2 {
				bg, icon = dangerPalette.bg, "stop"
			}
			r = append(r, reg(icon+" icon", bg, 148, 307, 22, 17, 8, 7),
				reg(icon+" label", bg, 172, 306, 45, 22, 22, 8))
		} else {
			r = append(r, reg("sleep z", companion.FaceBG, 212, 79, 39, 36, 20, 17))
		}
		scenes = append(scenes, scene{[]string{"idle", "blink", "talk", "sleep"}[i], companionSVG(companion, pose), r})
	}

	menu := func(name string, s sceneView, regions ...region) {
		regions = append(regions, reg("title", menuBG, 95, 51, 170, 29, 42, 14))
		if s.actions["menu-prev"].kind != "" || s.actions["menu-next"].kind != "" {
			regions = append(regions, reg("back label", navPalette.bg, 149, 287, 62, 22, 23, 8))
		} else {
			regions = append(regions, reg("back icon", navPalette.bg, 148, 306, 22, 18, 8, 8),
				reg("back label", navPalette.bg, 171, 305, 49, 22, 23, 8))
		}
		scenes = append(scenes, scene{name, s.svg(""), regions})
	}
	menu("home page 1", menuHome(heartVoice, true, 55, false, false, 0),
		reg("intercom icon", accentPalette.bg, 103, 98, 22, 21, 13, 13),
		reg("intercom label", accentPalette.bg, 67, 126, 94, 22, 44, 9),
		reg("games icon", warningPalette.bg, 235, 98, 22, 21, 13, 8),
		reg("games label", warningPalette.bg, 199, 126, 94, 22, 40, 9),
		reg("history label", volumePalette.bg, 67, 221, 94, 22, 40, 9),
		reg("memories label", accentPalette.bg, 199, 221, 94, 22, 50, 9))
	menu("home page 2", menuHome(heartVoice, true, 55, false, false, 1),
		reg("volume icon", volumePalette.bg, 235, 94, 22, 21, 13, 10),
		reg("volume label", volumePalette.bg, 199, 118, 94, 18, 45, 9),
		reg("volume subtitle", volumePalette.bg, 199, 140, 94, 15, 32, 6),
		reg("voice icon", voicePalette.bg, 104, 94, 22, 21, 13, 8),
		reg("voice label", voicePalette.bg, 68, 118, 94, 18, 34, 9),
		reg("voice subtitle", voicePalette.bg, 68, 140, 94, 15, 22, 6),
		reg("stats icon", successPalette.bg, 102, 193, 22, 21, 13, 10),
		reg("stats label", successPalette.bg, 66, 221, 94, 22, 32, 9))
	game := tile{id: "game-0", label: "Puzzle", detail: "Saved app", icon: "games", colors: navPalette}
	menu("games", gamesScene([]tile{game}, 0, ""),
		reg("game icon", navPalette.bg, 103, 94, 22, 21, 13, 8),
		reg("game title", navPalette.bg, 67, 118, 94, 18, 35, 9),
		reg("game subtitle", navPalette.bg, 67, 140, 94, 15, 45, 6))
	listRows := []menuRow{{"favorite-toy", "A surprisingly tiny blue kite"}, {"tricks", "Ring the bell"}, {"name", "Cake"}}
	for _, title := range []string{"Memories", "History"} {
		menu(title, menuList(title, "", listRows, 0),
			reg("row title", navPalette.bg, 105, 91, 150, 18, 60, 8),
			reg("row preview", navPalette.bg, 65, 111, 230, 18, 170, 8),
			reg("last row title", navPalette.bg, 120, 211, 120, 18, 30, 8))
	}
	detail := menuDetail("Memories", listRows[0])
	scenes = append(scenes, scene{"detail", detail.svg(""), []region{
		reg("detail heading", navPalette.bg, 85, 92, 190, 24, 94, 12),
		reg("detail body", navPalette.bg, 65, 121, 230, 34, 140, 19),
		reg("detail back", navPalette.bg, 96, 287, 63, 28, 42, 8),
		reg("detail cat", navPalette.bg, 204, 287, 56, 28, 18, 10),
	}})
	menu("volume", volumeScene(true, 55),
		reg("level", menuBG, 76, 98, 208, 31, 105, 17),
		reg("volume track", menuBG, 70, 144, 220, 15, 185, 7),
		reg("minus", volumePalette.bg, 110, 194, 30, 16, 15, 2),
		reg("plus", volumePalette.bg, 220, 188, 30, 28, 15, 13),
		reg("auto icon", volumePalette.bg, 148, 252, 22, 18, 10, 8),
		reg("auto label", volumePalette.bg, 171, 251, 48, 22, 24, 8))
	var choiceRegions []region
	for i := range 4 {
		x, y := 57+132*(i%2), 86+95*(i/2)
		choiceRegions = append(choiceRegions,
			reg(voiceCatalog[i].Name+" icon", voicePalette.bg, x+46, y+12, 22, 23, 11, 8),
			reg(voiceCatalog[i].Name+" label", voicePalette.bg, x+10, y+36, 94, 24, 26, 9))
	}
	menu("voice", voiceScene(heartVoice, 0), choiceRegions...)
	var choices []menuPeer
	choiceRegions = nil
	for i := range 9 {
		choices = append(choices, menuPeer{strconv.Itoa(i), "Cat " + strconv.Itoa(i), "#372530", "#FFADAC"})
		if i < 4 {
			x, y := 57+132*(i%2), 86+95*(i/2)
			choiceRegions = append(choiceRegions, reg("peer "+strconv.Itoa(i)+" icon", "#372530", x+41, y+13, 32, 18, 16, 6),
				reg("peer "+strconv.Itoa(i)+" label", "#372530", x+22, y+42, 70, 18, 24, 7))
		}
	}
	menu("intercom", peerScene(choices, 1, false), append(choiceRegions,
		reg("previous label", navPalette.bg, 89, 286, 40, 20, 23, 7),
		reg("next label", navPalette.bg, 231, 286, 40, 20, 23, 7))...)
	var rows []region
	for i := range 6 {
		rows = append(rows, reg("stats row "+strconv.Itoa(i+1), menuCard, 55, 106+27*i, 250, 18, 90, 6))
	}
	hub := &wsHub{b: &larkServer{}}
	menu("stats", hub.statsScene(&wsTerminal{client: &wsClient{role: "device"}, companion: companion, board: "esp-vocat", firmware: "0.9.1", vol: 55}), rows...)

	for _, geometry := range []screenGeometry{roundScreen, {320, 240, "rect"}} {
		for _, s := range scenes {
			t.Run(s.name+"/"+strconv.Itoa(geometry.width), func(t *testing.T) {
				screen, err := buildScreen(s.svg, false, geometry)
				if err != nil {
					t.Fatal(err)
				}
				side := min(geometry.width, geometry.height)
				offsetX, offsetY := (geometry.width-side)/2, (geometry.height-side)/2
				for _, r := range s.regions {
					n, _ := strconv.ParseUint(r.background[1:], 16, 24)
					bg := uint16(n>>19)<<11 | uint16((n>>10)&63)<<5 | uint16((n>>3)&31)
					extent, lit := image.Rectangle{}, 0
					for y := offsetY + r.box.Min.Y*side/360; y < offsetY+r.box.Max.Y*side/360; y++ {
						for x := offsetX + r.box.Min.X*side/360; x < offsetX+r.box.Max.X*side/360; x++ {
							if screen.fb[y*geometry.width+x] != bg {
								extent = extent.Union(image.Rect(x, y, x+1, y+1))
								lit++
							}
						}
					}
					if extent.Dx() < max(2, r.wide*side/360) || extent.Dy() < max(2, r.tall*side/360) || lit < 8 {
						t.Errorf("%s: only %d lit pixels spanning %dx%d in %v", r.name, lit, extent.Dx(), extent.Dy(), r.box)
					}
				}
			})
		}
	}
}

func TestFacePoseCacheReusesFramesAndHitboxes(t *testing.T) {
	var cache screenCache
	poses := facePoseSVGs()
	prior := make([]svgScreen, len(poses))
	for i, svg := range poses {
		screen, err := cache.build(svg, false, roundScreen)
		if err != nil || len(screen.fb) != screenSize*screenSize || len(screen.hits) == 0 {
			t.Fatalf("pose %d did not build: %v", i, err)
		}
		prior[i] = screen
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for i, svg := range poses {
				screen, err := cache.build(svg, false, roundScreen)
				if err != nil || &screen.fb[0] != &prior[i].fb[0] || &screen.hits[0] != &prior[i].hits[0] {
					t.Errorf("pose %d did not reuse cached pixels and hits: %v", i, err)
				}
			}
		})
	}
	wg.Wait()
	if hits, misses := cache.stats(); hits != 4*uint64(len(poses)) || misses != uint64(len(poses)) {
		t.Fatalf("cache stats: hits=%d misses=%d", hits, misses)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		_, _ = cache.build(poses[0], false, roundScreen)
	}); allocs != 0 {
		t.Fatalf("cache hit allocated %.2f times", allocs)
	}
}

func TestThinkingFramesAndStopLabels(t *testing.T) {
	for _, geom := range []screenGeometry{roundScreen, {320, 240, "rect"}} {
		t.Run(strconv.Itoa(geom.width), func(t *testing.T) {
			side := min(geom.width, geom.height)
			top := image.Rect(0, 0, geom.width, 70*side/360)
			bottom := image.Rect(0, 292*side/360, geom.width, geom.height)
			var prevLabel []uint16
			for taps, label := range []string{"Stop", "Tap 2×", "Tap 1×"} {
				var prevDot []uint16
				for dot := range 3 {
					raw := companionSVG(blankCompanion(), companionPose{mood: "thinking", dot: dot, stopTaps: taps, battery: 80})
					screen, err := buildScreen(raw, false, geom)
					if err != nil {
						t.Fatal(err)
					}
					if warnings := layoutWarnings(screen.hits, geom.shape == "round"); len(warnings) != 0 {
						t.Error(warnings)
					}
					if len(screen.hits) != 1 || hitID(screen.hits, 180, 315) != "stop" ||
						!strings.Contains(screen.source, `aria-label="`+label+`"`) ||
						!strings.Contains(screen.source, "Thinking") {
						t.Fatalf("missing Thinking status or %q Stop control: %v", label, screen.hits)
					}
					if dot == 0 {
						if prevLabel != nil {
							rect := dirtyRect(prevLabel, screen.fb, geom)
							if rect.Empty() || !rect.In(bottom) {
								t.Errorf("%s: expected a visible label update at the bottom, got %v", label, rect)
							}
						}
						prevLabel = screen.fb
					} else {
						rect := dirtyRect(prevDot, screen.fb, geom)
						if rect.Empty() || !rect.In(top) || rect.Min.Y >= 30*side/360 || rect.Max.Y <= 50*side/360 {
							t.Errorf("%s dot %d: ring and dots should animate only at the top, got %v", label, dot, rect)
						}
					}
					prevDot = screen.fb
				}
			}
		})
	}
}

func TestScreenCacheSeparatesGeometryAndEvicts(t *testing.T) {
	var cache screenCache
	const raw = `<svg><rect width="360" height="360" fill="red"/></svg>`
	for _, geometry := range []screenGeometry{{}, {320, 240, "round"}, {320, 240, "rect"}, roundScreen} {
		screen, err := cache.build(raw, false, geometry)
		if err != nil || len(screen.fb) != geometry.width*geometry.height {
			t.Fatalf("geometry %+v: %v, %d pixels", geometry, err, len(screen.fb))
		}
		if geometry.width != 0 && (screen.fb[0] != 0) != (geometry.shape == "rect") {
			t.Fatalf("cache mixed screen shapes: %+v", geometry)
		}
	}
	withExit, err := cache.build(raw, true, screenGeometry{})
	if err != nil || hitID(withExit.hits, 180, 318) != exitNode || withExit.fb != nil {
		t.Fatal("web-only drawing did not retain its exit hitbox", err)
	}
	for i := range screenCacheSize {
		if _, err := cache.build(`<svg><text>`+strconv.Itoa(i)+`</text></svg>`, false, screenGeometry{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cache.build(raw, false, screenGeometry{}); err != nil {
		t.Fatal(err)
	}
	if hits, misses := cache.stats(); hits != 0 || misses != screenCacheSize+6 || len(cache.entries) != screenCacheSize {
		t.Fatalf("cache did not bound its FIFO: hits=%d misses=%d size=%d", hits, misses, len(cache.entries))
	}
}

func TestSVGSingleArgumentScalePreservesRasterHeight(t *testing.T) {
	const svg = `<svg><defs><linearGradient id="shade" gradientTransform="scale(.8333)"><stop stop-color="#FFFFFF"/></linearGradient></defs>
		<path d="M12 3v16" stroke="#FFFFFF" fill="none" stroke-width="2.6" transform="translate(100 100) scale(0.8333)"/></svg>`
	screen, err := buildScreenUncached(svg, false, roundScreen)
	if err != nil {
		t.Fatal(err)
	}
	height := 0
	for y := 100; y < 120; y++ {
		if slices.ContainsFunc(screen.fb[y*360+106:y*360+114], func(px uint16) bool { return px != 0 }) {
			height++
		}
	}
	if height <= 8 || !strings.Contains(screen.source, `gradientTransform="matrix(0.8333 0 0 0.8333 0 0)"`) {
		t.Fatalf("scaled icon occupies %d rows; gradient transform not normalized: %t", height, !strings.Contains(screen.source, `gradientTransform="matrix(0.8333 0 0 0.8333 0 0)"`))
	}
}

func TestScreenScannerPreservesPixels(t *testing.T) {
	const art = `<svg><defs><linearGradient id="paint"><stop stop-color="red"/><stop offset="1" stop-color="blue"/></linearGradient></defs>
		<rect x="-10" y="-4" width="145.2" height="40.6" fill="url(#paint)"/>
		<circle cx="12.5" cy="18" r="10.8" fill="lime" opacity=".5"/>
		<path d="M310.5 130.2 L330.3 340.4 L312 315 Z" stroke="blue" stroke-width="3" fill="red"/>
		<rect x="400" y="400" width="20" height="20" fill="red"/></svg>`
	scene, err := prepareScene(art, false)
	if err != nil {
		t.Fatal(err)
	}
	icon, err := oksvg.ReadIconStream(strings.NewReader(scene.toBrowserSVG()), oksvg.StrictErrorMode)
	if err != nil {
		t.Fatal(err)
	}
	ref, got := image.NewRGBA(image.Rect(0, 0, 320, 240)), image.NewRGBA(image.Rect(0, 0, 320, 240))
	icon.SetTarget(40, 0, 240, 240)
	icon.Draw(rasterx.NewDasher(320, 240, rasterx.NewScannerGV(320, 240, ref, ref.Bounds())), 1)
	icon.Draw(rasterx.NewDasher(320, 240, screenScanner{rasterx.NewScannerGV(320, 240, got, got.Bounds()), got}), 1)
	if !bytes.Equal(got.Pix, ref.Pix) {
		t.Fatal("bounded compositing changed pixels")
	}
}

func TestPreparedSceneBrowserControlsMatchHits(t *testing.T) {
	const raw = `<svg transform="translate(20 10)">
		<defs><linearGradient id="paint" data-id="hidden" data-hit="0 0 200 200"/></defs>
		<g data-id="no-bounds"><path data-id="no-hit" d="M0 0L3 4"/></g>
		<g data-id="back" data-hit="15 20 30 35" data-lark-hit="0 0 359 359">
			<circle data-id="front" cx="25" cy="30" r="5"/>
		</g>
		<rect data-id="lark-exit" x="700" y="700" width="5" height="5"/>
	</svg>`
	scene, err := prepareScene(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	dec := xml.NewDecoder(strings.NewReader(scene.toBrowserSVG()))
	var browser []hitBox
	for token, err := dec.Token(); err == nil; token, err = dec.Token() {
		if el, ok := token.(xml.StartElement); ok {
			var id, bounds string
			for _, attr := range el.Attr {
				switch attr.Name.Local {
				case "data-id":
					id = attr.Value
				case "data-lark-hit":
					bounds = attr.Value
				}
			}
			if id != "" {
				h := hitBox{id: id}
				if _, err := fmt.Sscan(bounds, &h.x1, &h.y1, &h.x2, &h.y2); err != nil {
					t.Fatal(id, err)
				}
				browser = append(browser, h)
			}
		}
	}
	slices.Reverse(browser)
	sameHit := func(wire, hit hitBox) bool {
		hit.bounds = svgBounds{}
		return wire == hit
	}
	if len(browser) != 3 || !slices.EqualFunc(browser, scene.hits, sameHit) || hitID(scene.hits, 45, 40) != "front" || hitID(scene.hits, 180, 318) != exitNode {
		t.Fatalf("browser %v; hit targets %v", browser, scene.hits)
	}
}

func TestAppLayoutWarnings(t *testing.T) {
	for _, tc := range []struct {
		name, controls string
		want           []string
	}{
		{"safe", `<rect data-id="top" x="110" y="18" width="140" height="32"/>
			<rect data-id="next" x="150" y="270" width="60" height="21"/>
			<rect data-id="only-padded-corner-clips" x="76" y="36" width="70" height="66"/>`, nil},
		{"padded overlap", `<rect data-id="next" x="150" y="270" width="60" height="25"/>`,
			[]string{`control "next" overlaps bottom Home button (keep controls above y=296)`}},
		{"one corner", `<g data-id="prev" data-hit="74,20,64,40"/>`,
			[]string{`control "prev" clips outside the round display bezel`}},
		{"two corners", `<rect data-id="wide" x="80" y="12" width="200" height="24"/>`,
			[]string{`control "wide" clips outside the round display bezel`}},
		{"center margin", `<rect data-id="high" x="171" y="2" width="18" height="6"/>`,
			[]string{`control "high" clips outside the round display bezel`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			screen, err := buildScreen("<svg>"+tc.controls+"</svg>", true, roundScreen)
			if err != nil {
				t.Fatal(err)
			}
			if got := layoutWarnings(screen.hits, true); !slices.Equal(got, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
			exit := screen.hits[0]
			if exit.id != exitNode || exit.x1 != 133 || exit.y1 != 300 || exit.x2 != 227 || exit.y2 != 340 ||
				!inScreenCircle(exit.x1, exit.y2) || !inScreenCircle(exit.x2, exit.y2) {
				t.Fatalf("exit target drifted or clipped: %+v", exit)
			}
		})
	}
}

func TestSVGTransparentPaint(t *testing.T) {
	const raw = `<svg><rect width="360" height="360" fill="#224466"/>
		<g fill="TrAnSpArEnT"><rect x="90" y="90" width="90" height="90" stroke="#FFFFFF"/></g>
		<rect x="190" y="190" width="50" height="50" fill="#FF0000" stroke="transparent" stroke-width="5"/></svg>`
	want, err := buildScreen(strings.NewReplacer("TrAnSpArEnT", "none", "transparent", "none").Replace(raw), false, roundScreen)
	if err != nil {
		t.Fatal(err)
	}
	got, err := buildScreen(raw, false, roundScreen)
	if err != nil {
		t.Fatal(err)
	}
	if got.source != want.source || !slices.Equal(got.fb, want.fb) {
		t.Fatal("transparent paint differs from none")
	}
}

func TestFaceLayersCacheByArtAndPose(t *testing.T) {
	var cache faceLayerCache
	const raw = `<svg><g data-pose="idle"><rect fill="red"/></g><g data-pose="blink"><rect fill="blue"/></g></svg>`
	for i, tc := range []struct{ raw, pose, color string }{
		{raw, "idle", "red"}, {raw, "blink", "blue"}, {strings.ReplaceAll(raw, "red", "green"), "idle", "green"}, {raw, "idle", "red"},
	} {
		art, err := cache.layer(tc.raw, tc.pose)
		if err != nil || !strings.Contains(art, `fill="`+tc.color+`"`) || strings.Count(art, "<rect") != 1 || len(cache.entries) != min(3, i+1) {
			t.Fatalf("%+v: %q, %v, %d cached entries", tc, art, err, len(cache.entries))
		}
	}
	for i := range faceLayerCacheSize {
		if _, err := cache.layer(raw+"<!--"+strconv.Itoa(i)+"-->", "idle"); err != nil {
			t.Fatal(err)
		}
	}
	if _, present := cache.entries[faceLayerKey{svgHash(raw), "idle"}]; present || len(cache.entries) != faceLayerCacheSize {
		t.Fatal("face layer cache failed to evict old entries")
	}
}

func BenchmarkRenderSVG(b *testing.B) {
	companion := blankCompanion()
	poses := [...]companionPose{{mood: "idle", battery: 80}, {mood: "idle", battery: 80, blink: true},
		{mood: "speaking", battery: 80}, {mood: "sleepy", battery: 80}}
	b.Run("uncached", func(b *testing.B) {
		for i := range b.N {
			if _, err := buildScreenUncached(companionSVG(companion, poses[i%len(poses)]), false, roundScreen); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("cached", func(b *testing.B) {
		var cache screenCache
		for _, pose := range poses {
			if _, err := cache.build(companionSVG(companion, pose), false, roundScreen); err != nil {
				b.Fatal(err)
			}
		}
		b.ResetTimer()
		for i := range b.N {
			if _, err := cache.build(companionSVG(companion, poses[i%len(poses)]), false, roundScreen); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkFacePoseCache(b *testing.B) {
	poses := facePoseSVGs()
	for _, display := range []struct {
		name     string
		geometry screenGeometry
	}{{"360x360", roundScreen}, {"320x240", screenGeometry{320, 240, "rect"}}} {
		b.Run(display.name+"/uncached", func(b *testing.B) {
			b.ReportAllocs()
			for i := range b.N {
				if _, err := buildScreenUncached(poses[i%len(poses)], false, display.geometry); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(display.name+"/hit", func(b *testing.B) {
			var cache screenCache
			for _, pose := range poses {
				if _, err := cache.build(pose, false, display.geometry); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				if _, err := cache.build(poses[i%len(poses)], false, display.geometry); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestRasterSVGShapesTextAndHitOrder(t *testing.T) {
	raw := svgStart + `<rect width="360" height="360" fill="#101820"/>
<defs><linearGradient id="rainbow"><stop offset="0" stop-color="red"/><stop offset="1" stop-color="blue"/></linearGradient></defs>
<rect data-id="behind" x="110" y="80" width="140" height="100" fill="url(#rainbow)"/>
<circle data-id="orb" cx="180" cy="120" r="20" fill="lime"/>
<ellipse data-id="ellipse" cx="180" cy="192" rx="18" ry="10" fill="red"/>
<polygon data-id="triangle" points="78,166 102,166 90,190" fill="#ff0"/>
<line x1="90" y1="220" x2="120" y2="220" stroke="#fff" stroke-width="4"/>
<polyline points="100,240 120,250 140,240" fill="none" stroke="#fff" stroke-width="3"/>
<path d="M200 198 Q225 235 250 198 T300 198 L300 245 L200 245 Z" fill="#00f"/>
<g transform="translate(100,0)" data-id="group" data-hit="31,262,115,25">
<text x="80" y="281" fill="#fff" font-size="22" text-anchor="middle" font-weight="bold" data-id="label">Go Font</text>
</g></svg>`
	fb, hits, err := renderSVG(raw)
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[image.Point]uint16{
		{0, 0}: 0, {180, 120}: 0x07e0, {180, 192}: 0xf800, {90, 173}: 0xffe0, {96, 220}: 0xffff, {230, 236}: 0x001f,
	} {
		if got := fb[p.Y*360+p.X]; got != want {
			t.Errorf("pixel %v = %04x, want %04x", p, got, want)
		}
	}
	if fb[100*360+118] == fb[100*360+242] {
		t.Fatal("gradient painted flat")
	}
	if hitID(hits, 157, 120) != "orb" || hitID(hits, 130, 105) != "behind" || hitID(hits, 180, 190) != "ellipse" ||
		hitID(hits, 90, 172) != "triangle" || hitID(hits, 180, 274) != "label" || hitID(hits, 245, 291) != "group" {
		t.Fatal("wrong z-order, padding or hit geometry:", hits)
	}
	var textPixels int
	for y := 260; y < 283; y++ {
		for x := 130; x < 229; x++ {
			if fb[y*360+x] == 0xffff {
				textPixels++
			}
		}
	}
	if textPixels < 30 {
		t.Fatal("font outline is missing")
	}
}

func TestSVGSanitization(t *testing.T) {
	input := `<?xml version="1.0"?><!DOCTYPE svg><svg xmlns:xlink="https://evil.example" viewBox="0 0 360 360" onload="bad()">
<script>bad()</script><foreignObject><b>bad</b></foreignObject><image href="https://evil.example"/>
<a href="javascript:bad()"><text x="180" y="180">bad</text></a><style>bad</style>
<rect x="150" y="150" width="60" height="60" data-id="ok" onclick="bad()" style="bad" fill="url(https://evil.example)"/>
</svg>`
	scene, err := prepareScene(input, true)
	if err != nil {
		t.Fatal(err)
	}
	got := scene.toBrowserSVG()
	for _, bad := range []string{"onload", "onclick", "style", "DOCTYPE", "href", "evil", "foreignObject", "<script", "javascript:"} {
		if strings.Contains(got, bad) {
			t.Errorf("unsafe %q survived: %s", bad, got)
		}
	}
	if hitID(scene.hits, 180, 180) != "ok" || hitID(scene.hits, 180, 318) != exitNode {
		t.Fatal(scene.hits)
	}
	for _, raw := range []string{`<svg><rect></svg>`, `<svg/><svg/>`, `<svg viewBox="0 0 10 10"/>`} {
		if _, _, err := renderSVG(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, _, err := renderSVG(got); err != nil {
		t.Fatal(err)
	}
}

// Interpret the wire with the firmware contract, independently of the encoder.
func decodeDisplay(t *testing.T, f wsFrame) (image.Rectangle, uint16, []uint16) {
	t.Helper()
	p := f.data
	if f.op != wsBinary || len(p) < 12 || p[0] != 0x03 {
		t.Fatalf("not a display packet: %x", p[:min(len(p), 12)])
	}
	u := func(i int) int { return int(binary.LittleEndian.Uint16(p[i:])) }
	x, y, w, h := u(1), u(3), u(5), u(7)
	if w < 1 || h < 1 || x+w > 360 || y+h > 360 {
		t.Fatalf("invalid rect %d %d %d %d", x, y, w, h)
	}
	c, rev, raw := p[9], uint16(u(10)), p[12:]
	out := make([]uint16, 0, w*h)
	switch c {
	case 0:
		for len(raw) >= 2 {
			out = append(out, binary.BigEndian.Uint16(raw))
			raw = raw[2:]
		}
	case 1:
		for len(raw) > 0 {
			tag := raw[0]
			raw = raw[1:]
			n := int(tag&127) + 1
			if tag&0x80 != 0 {
				if len(raw) < 2 {
					t.Fatal("short repeat run")
				}
				for range n {
					out = append(out, binary.BigEndian.Uint16(raw))
				}
				raw = raw[2:]
			} else {
				if len(raw) < n*2 {
					t.Fatal("short literal run")
				}
				for range n {
					out = append(out, binary.BigEndian.Uint16(raw))
					raw = raw[2:]
				}
			}
			if len(out) > w*h {
				t.Fatal("run overflows dirty rectangle")
			}
		}
	case 2:
		img, err := jpeg.Decode(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("jpeg decode: %v", err)
		}
		b := img.Bounds()
		if b.Dx() != w || b.Dy() != h {
			t.Fatalf("jpeg dimensions %dx%d, want %dx%d", b.Dx(), b.Dy(), w, h)
		}
		for py := b.Min.Y; py < b.Max.Y; py++ {
			for px := b.Min.X; px < b.Max.X; px++ {
				cr, cg, cb, _ := img.At(px, py).RGBA()
				out = append(out, pack565(uint8(cr>>8), uint8(cg>>8), uint8(cb>>8)))
			}
		}
		raw = nil
	case 3:
		dst := make([]byte, w*h*2)
		n, err := lz4.UncompressBlock(raw, dst)
		if err != nil || n != len(dst) {
			t.Fatalf("lz4 decode: n=%d want=%d err=%v", n, len(dst), err)
		}
		for i := 0; i < len(dst); i += 2 {
			out = append(out, binary.BigEndian.Uint16(dst[i:i+2]))
		}
		raw = nil
	default:
		t.Fatalf("unknown codec %d", c)
	}
	if len(out) != w*h || len(raw) != 0 {
		t.Fatal("malformed display payload")
	}
	return image.Rect(x, y, x+w, y+h), rev, out
}

func TestDirtyRectAndRLE565(t *testing.T) {
	previous, next := make([]uint16, 360*360), make([]uint16, 360*360)
	rect := image.Rect(18, 20, 280, 22)
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			p := uint16(0xf800)
			if y == 21 && x > 140 {
				p = 0x001f
			}
			next[y*360+x] = p
		}
	}
	if r := dirtyRect(previous, next, roundScreen); r != rect {
		t.Fatal(r)
	}
	if r := dirtyRect(next, next, roundScreen); !r.Empty() {
		t.Fatal(r)
	}
	if r := dirtyRect(nil, next, roundScreen); r != image.Rect(0, 0, 360, 360) {
		t.Fatal(r)
	}
	frame := displayFrame(next, rect, 0x10303, roundScreen.width)
	if got := frame.data[:12]; !slices.Equal(got, []byte{3, 18, 0, 20, 0, 6, 1, 2, 0, 1, 3, 3}) {
		t.Fatalf("header %x", got)
	}
	r, rev, p := decodeDisplay(t, frame)
	if r != rect || rev != 0x303 || !slices.Equal(p[:262], next[20*360+18:20*360+280]) ||
		!slices.Equal(p[262:], next[21*360+18:21*360+280]) {
		t.Fatal("RLE round trip changed pixels")
	}
	for i := range 129 {
		next[360+i] = uint16(i)*213 + 1
	}
	frame = displayFrame(next, image.Rect(0, 1, 129, 2), 1, roundScreen.width)
	_, _, p = decodeDisplay(t, frame)
	if frame.data[9] != 0 || !slices.Equal(p, next[360:489]) || frame.data[12] != 0 || frame.data[13] != 1 {
		t.Fatal("incompressible pixels did not use raw big-endian fallback")
	}
	// Mix literals and repeats long enough to require multiple run tags.
	for i := range 300 {
		if i < 130 {
			next[i] = uint16(i)
		} else {
			next[i] = 0x1234
		}
	}
	frame = displayFrame(next, image.Rect(0, 0, 300, 1), 19, roundScreen.width)
	_, _, p = decodeDisplay(t, frame)
	if frame.data[9] != 1 || !slices.Equal(p, next[:300]) {
		t.Fatal("mixed runs did not round-trip")
	}
}

func TestRectangularScreen(t *testing.T) {
	geometry := screenGeometry{320, 240, "rect"}
	const bg = svgStart + `<rect width="360" height="360" fill="#123456"/>`
	blank, err := buildScreen(bg+`</svg>`, false, geometry)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range blank.fb {
		if p != 0x11aa {
			t.Fatalf("background pixel %d = %04x", i, p)
		}
	}
	art, err := buildScreen(bg+`<rect x="120" y="90" width="120" height="180" fill="red"/>
		<rect width="30" height="30" fill="lime" data-id="corner"/></svg>`, false, geometry)
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[image.Point]uint16{
		{0, 0}: 0x11aa, {319, 239}: 0x11aa, {40, 0}: 0x07e0,
		{120, 60}: 0xf800, {199, 179}: 0xf800, {200, 179}: 0x11aa,
	} {
		if got := art.fb[p.Y*geometry.width+p.X]; got != want {
			t.Errorf("pixel %v = %04x, want %04x", p, got, want)
		}
	}
	if rect := dirtyRect(blank.fb, art.fb, geometry); rect != image.Rect(40, 0, 200, 180) {
		t.Fatalf("dirty rectangle = %v", rect)
	}
	full := dirtyRect(nil, art.fb, geometry)
	f := displayFrame(art.fb, full, 7, geometry.width)
	if got := f.data[:12]; (got[9] != frameCodecRLE565 && got[9] != frameCodecLZ4) || !slices.Equal(got[:9], []byte{3, 0, 0, 0, 0, 64, 1, 240, 0}) || got[10] != 7 || got[11] != 0 {
		t.Fatalf("header %x", got)
	}
	if r, _, pixels := decodeDisplay(t, f); r != full || !slices.Equal(pixels, art.fb) {
		t.Fatal("native frame round trip changed pixels")
	}
	for p, want := range map[image.Point]image.Point{
		{0, 0}: {0, 0}, {40, 0}: {0, 0}, {42, 2}: {3, 3}, {160, 120}: {180, 180},
		{200, 150}: {240, 225}, {279, 239}: {358, 358}, {319, 239}: {359, 358},
	} {
		x, y := geometry.inverseTap(p.X, p.Y)
		if image.Pt(x, y) != want {
			t.Errorf("tap %v mapped to %d,%d, want %v", p, x, y, want)
		}
	}
}

func TestBox3HelloAndRawAppTaps(t *testing.T) {
	_, base := wsHarness(t, nil)
	box := testConnectWS(t, base, "unit", false)
	round := testConnectWS(t, base, "unit", false)
	web := testConnectWS(t, base, "unit", true)
	const boxID, roundID = "box3-test", "lark-round"
	box.json(t, object{"ev": "hello", "role": "device", "device_id": boxID, "proto": 2, "firmware": "0.8.0",
		"width": 320, "height": 240, "shape": "rect", "board": "esp-box-3"})
	if r, _, _ := decodeDisplay(t, box.until(t, isDisplay)); r != image.Rect(0, 0, 320, 240) {
		t.Fatalf("BOX-3 initial frame = %v", r)
	}
	if r, _, _ := decodeDisplay(t, round.helloV2(t, roundID, "Cake")); r != image.Rect(0, 0, 360, 360) {
		t.Fatalf("legacy hello initial frame = %v", r)
	}
	terminals := testObject(t, testRequest(t, "GET", base+"/api/terminals", "unit", nil, 200))["terminals"].([]any)
	boxStatus := terminals[0].(object)
	if boxStatus["id"] != boxID || boxStatus["board"] != "esp-box-3" ||
		boxStatus["ws_connected"] != true {
		t.Fatal(boxStatus)
	}

	web.json(t, object{"ev": "hello", "role": "web"})
	web.json(t, object{"ev": "watch", "terminal_id": boxID})
	boxFrame := web.until(t, hasWS("svg", "companion-ring"))
	if got := testObject(t, boxFrame.data)["terminal_id"]; got != boxID {
		t.Fatalf("BOX-3 mirror tag = %v", got)
	}
	code := `function init(){ return {hits:"untouched"} }
	function render(s){ return '<svg><rect width="360" height="360" fill="#123456"/>'+
		'<rect width="30" height="30" fill="red" data-id="corner"/><text x="100" y="180">'+s.hits+'</text></svg>' }
	function onTap(s,p,id,x,y){ s.hits=id+":"+x+","+y; }`
	toolTest(t, base, "app", object{"companions": []string{"new-companion", "cake"}, "code": code}, false)
	web.until(t, hasWS("svg", "untouched"))
	if r, _, _ := decodeDisplay(t, box.until(t, isDisplay)); !r.In(image.Rect(0, 0, 320, 240)) {
		t.Fatalf("BOX-3 app sent an out-of-bounds frame: %v", r)
	}
	box.json(t, object{"ev": "tap", "x": 48, "y": 10})
	web.until(t, hasWS("svg", "corner:12,15"))
	web.json(t, object{"ev": "tap", "x": 20, "y": 18})
	web.until(t, hasWS("svg", "corner:20,18"))
	web.json(t, object{"ev": "watch", "terminal_id": roundID})
	web.until(t, hasWS("svg", `"terminal_id":"`+roundID+`"`))
}

func TestFrameCodecsAndSmartSelector(t *testing.T) {
	var enc frameEncoder
	const w = 360
	fb := make([]uint16, w*w)

	// 1. Small incompressible rect -> Raw (0)
	for i := range 60 {
		fb[i] = uint16(i)*213 + 17
	}
	f := enc.encode(fb, nil, image.Rect(0, 0, 60, 1), 1, w)
	if f.data[9] != frameCodecRaw {
		t.Fatalf("small incompressible rect chose codec %d, want Raw (0)", f.data[9])
	}
	if _, _, got := decodeDisplay(t, f); !slices.Equal(got, fb[:60]) {
		t.Fatal("Raw round-trip mismatch")
	}

	// 2. Uniform 128x128 rect -> RLE565 (1) or LZ4 (3), never JPEG (2)
	for y := range 128 {
		for x := range 128 {
			fb[y*w+x] = 0x1234
		}
	}
	f = enc.encode(fb, nil, image.Rect(0, 0, 128, 128), 2, w)
	if f.data[9] != frameCodecRLE565 && f.data[9] != frameCodecLZ4 {
		t.Fatalf("uniform rect chose codec %d, want lossless RLE565 or LZ4", f.data[9])
	}
	if _, _, got := decodeDisplay(t, f); len(got) != 128*128 || got[0] != 0x1234 {
		t.Fatal("uniform lossless round-trip mismatch")
	}

	// 3. Repeating 16-byte pattern across rows (< 64x64) -> LZ4 (3) beats RLE565
	for y := range 32 {
		for x := range 48 {
			fb[y*w+x] = uint16(x%8)*0x1111 + 0x0101
		}
	}
	f = enc.encode(fb, nil, image.Rect(0, 0, 48, 32), 3, w)
	if f.data[9] != frameCodecLZ4 {
		t.Fatalf("repeating pattern chose codec %d, want LZ4 (3)", f.data[9])
	}
	_, _, got := decodeDisplay(t, f)
	for y := range 32 {
		if !slices.Equal(got[y*48:(y+1)*48], fb[y*w:y*w+48]) {
			t.Fatalf("LZ4 round-trip mismatch at row %d", y)
		}
	}

	// 4. High-entropy 160x160 photographic-like gradient with subtle noise -> JPEG (2)
	var rng uint32 = 0x12345678
	for y := range 160 {
		for x := range 160 {
			rng = rng*1664525 + 1013904223
			noise := int((rng >> 28) & 0x0f)
			r8 := uint8(min(255, (x*240)/159+noise))
			g8 := uint8(min(255, (y*240)/159+noise))
			b8 := uint8(min(255, ((x+y)*120)/159+noise))
			fb[y*w+x] = pack565(r8, g8, b8)
		}
	}
	f = enc.encode(fb, nil, image.Rect(0, 0, 160, 160), 4, w)
	if f.data[9] != frameCodecJPEG {
		t.Fatalf("high-entropy 160x160 gradient chose codec %d, want JPEG (2)", f.data[9])
	}
	r, rev, decoded := decodeDisplay(t, f)
	if r != image.Rect(0, 0, 160, 160) || rev != 4 || len(decoded) != 160*160 {
		t.Fatalf("JPEG metadata mismatch: r=%v rev=%d len=%d", r, rev, len(decoded))
	}
}

func BenchmarkToRGB565(b *testing.B) {
	svg := companionSVG(blankCompanion(), companionPose{mood: "idle", battery: 80})
	scene, err := prepareScene(svg, false)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := scene.toRGB565(roundScreen, nil, gradeDay); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDirtyRect(b *testing.B) {
	prev := make([]uint16, 360*360)
	next := make([]uint16, 360*360)
	next[180*360+180] = 0xf800
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if r := dirtyRect(prev, next, roundScreen); r.Empty() {
			b.Fatal("empty")
		}
	}
}

func BenchmarkFrameEncoderRLE(b *testing.B) {
	var enc frameEncoder
	fb := make([]uint16, 360*360)
	rect := image.Rect(0, 0, 360, 360)
	// Warm up scratch buffers.
	enc.encode(fb, nil, rect, 1, 360)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		_ = enc.encode(fb, nil, rect, uint64(i), 360)
	}
}

func BenchmarkFrameEncoderLZ4(b *testing.B) {
	var enc frameEncoder
	fb := make([]uint16, 360*360)
	for y := range 360 {
		for x := range 360 {
			fb[y*360+x] = uint16(x%16) * 0x0841
		}
	}
	rect := image.Rect(0, 0, 360, 360)
	enc.encode(fb, nil, rect, 1, 360)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		_ = enc.encode(fb, nil, rect, uint64(i), 360)
	}
}

func BenchmarkFrameEncoderJPEG(b *testing.B) {
	var enc frameEncoder
	fb := make([]uint16, 360*360)
	var rng uint32 = 0x12345678
	for y := range 160 {
		for x := range 160 {
			rng = rng*1664525 + 1013904223
			noise := int((rng >> 28) & 0x0f)
			fb[y*360+x] = pack565(uint8(min(255, (x*240)/159+noise)), uint8(min(255, (y*240)/159+noise)), uint8(min(255, ((x+y)*120)/159+noise)))
		}
	}
	rect := image.Rect(0, 0, 160, 160)
	enc.encode(fb, nil, rect, 1, 360)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		_ = enc.encode(fb, nil, rect, uint64(i), 360)
	}
}
