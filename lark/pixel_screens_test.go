package main

import (
	"fmt"
	"image"
	imagecolor "image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lark/fonts/dogica"
)

func TestPixelQRModuleRoundTrip(t *testing.T) {
	const rawInvite = "https://i.delta.chat/#72120f6660ef1a2151b318904197b15e&v=1&n=Greg&i=abcdef1234567890&s=1234567890abcdef&a=greg%40example.org"
	invite := shortenDeltaChatInvite(rawInvite)
	if strings.Contains(invite, "&n=") || strings.Contains(invite, "&v=") {
		t.Fatalf("shortenDeltaChatInvite did not strip optional params: %q", invite)
	}
	pureBlack := imagecolor.RGBA{0, 0, 0, 255}
	pureWhite := imagecolor.RGBA{255, 255, 255, 255}
	for _, spec := range []pixelDisplaySpec{pixelSpecVoCat, pixelSpecBox3} {
		layout, err := computePairQRLayout(spec, invite)
		if err != nil {
			t.Fatalf("%s computePairQRLayout: %v", spec.name, err)
		}
		img, err := renderPixelPair(spec, "Greg", invite)
		if err != nil {
			t.Fatalf("%s renderPixelPair: %v", spec.name, err)
		}
		if img.Bounds().Dx() != spec.logicalW*spec.artScale || img.Bounds().Dy() != spec.logicalH*spec.artScale {
			t.Fatalf("%s bounds = %v", spec.name, img.Bounds())
		}
		for my := range layout.modules {
			for mx := range layout.modules {
				want := pureWhite
				if layout.code.Black(mx, my) {
					want = pureBlack
				}
				for dy := range layout.modPx {
					for dx := range layout.modPx {
						got := img.RGBAAt(
							layout.qrRect.Min.X+(mx+layout.quiet)*layout.modPx+dx,
							layout.qrRect.Min.Y+(my+layout.quiet)*layout.modPx+dy,
						)
						if got != want {
							t.Fatalf("%s module (%d,%d) pixel (%d,%d) = %v, want %v", spec.name, mx, my, dx, dy, got, want)
						}
					}
				}
			}
		}
	}
}

func TestFormatMemoryLinesAndTitles(t *testing.T) {
	if got := formatMemoryLines(`{"text":"Once upon a time."}`); len(got) != 1 || got[0] != "Once upon a time." {
		t.Fatalf("text field: %v", got)
	}
	if got := formatMemoryLines(`{"parts":["Line one.","Line two."]}`); len(got) != 1 || got[0] != "Line one. Line two." {
		t.Fatalf("parts field: %v", got)
	}
	got := formatMemoryLines(`{"Mint":4,"Cake":0}`)
	if len(got) != 2 || got[0] != "Cake: 0" || got[1] != "Mint: 4" {
		t.Fatalf("object fields: %v", got)
	}
	titles := map[string]string{"monsters": "Monsters", "lost_island": "The Lost Island"}
	if got := formatMemoryTitle("Game saves", "monsters", titles); got != "Monsters" {
		t.Fatalf("game save title = %q, want Monsters", got)
	}
	if got := formatMemoryTitle("Reading", "story_lost_island", nil); got != "Lost Island" {
		t.Fatalf("humanized story title = %q, want Lost Island", got)
	}
}

func TestEveryScreenFitsSafeAreaAndNoTextOverflow(t *testing.T) {
	books := []pixelBook{
		{Name: "lost_island", Title: "The Lost Island", Page: 3, Total: 7, Color: pxMint},
		{Name: "clockwork_owl", Title: "The Clockwork Owl", Page: 1, Total: 5, Color: pxPeach},
	}
	games := []pixelGame{
		{Name: "monsters", Title: "Monsters", Kind: "game", Saved: true},
		{Name: "fireworks", Title: "Fireworks", Kind: "game", Saved: false},
	}
	memories := []pixelMemoryItem{
		{Section: "Notes", Key: "bedtime", Value: `{"text":"Greg likes warm chamomile tea before bed."}`},
		{Section: "Game saves", Key: "monsters", Value: `{"Mint":4,"Cake":0}`},
	}
	titles := map[string]string{"monsters": "Monsters"}
	invite := shortenDeltaChatInvite("https://i.delta.chat/#72120f6660ef1a2151b318904197b15e&i=abcdef1234567890&s=1234567890abcdef&a=greg%40example.org")

	for _, spec := range []pixelDisplaySpec{pixelSpecVoCat, pixelSpecBox3} {
		safePhys := physicalRect(spec.safe, spec.artScale)
		th := defaultPixelTheme()
		voiceP1, v1Hits, _ := renderPixelVoiceWithHits(spec, lilyVoice, 0, th)
		voiceP2, _, _ := renderPixelVoiceWithHits(spec, lilyVoice, 1, th)
		moreImg, _, _ := renderPixelMoreWithHits(spec, 0, th)
		volImg, _, _ := renderPixelVolumeWithHits(spec, false, 60, 0, th)
		statsImg, _, _ := renderPixelStatsWithHits(spec, []menuRow{{"Companion", "Greg"}, {"Voice", "Heart"}}, th)
		wantVoiceP1 := 3
		if !spec.round {
			wantVoiceP1 = 6
		}
		voiceCount := 0
		for _, hb := range v1Hits {
			if strings.HasPrefix(hb.id, "voice-") {
				voiceCount++
			}
		}
		if voiceCount != wantVoiceP1 {
			t.Fatalf("%s voice page 1 items = %d, want %d", spec.name, voiceCount, wantVoiceP1)
		}
		screens := []struct {
			name string
			img  *image.RGBA
		}{
			{"home", renderPixelHome(spec, nil, nil, gradeDay, "listening", "")},
			{"home-listen-win", renderPixelHomeListen(spec, nil, nil, gradeDay, "listening", "", true)},
			{"home-hint", renderPixelHome(spec, nil, nil, gradeDay, "", "")},
			{"home-caption", renderPixelHome(spec, nil, nil, gradeDay, "speaking", "Message from Mum: dinner's ready")},
			{"menu", renderPixelMenu(spec, 0, th)},
			{"more", moreImg},
			{"voice-p1", voiceP1},
			{"voice-p2", voiceP2},
			{"volume", volImg},
			{"stats", statsImg},
			{"books", renderPixelBooksList(spec, books, 0, th)},
			{"games", renderPixelGamesList(spec, games, 0, th)},
			{"reader", renderPixelReader(spec, "The Lost Island", "Once upon a time in a faraway land.", 0, 0, th)},
			{"memories", renderPixelMemoriesList(spec, memories, titles, 0, th)},
			{"memory-detail", renderPixelMemoryDetail(spec, memories[1], titles, 0, 0, th)},
		}
		pairImg, err := renderPixelPair(spec, "Greg", invite)
		if err != nil {
			t.Fatalf("%s pair: %v", spec.name, err)
		}
		screens = append(screens, struct {
			name string
			img  *image.RGBA
		}{"pair", pairImg})

		wantW, wantH := spec.logicalW*spec.artScale, spec.logicalH*spec.artScale
		for _, sc := range screens {
			if sc.img.Bounds() != image.Rect(0, 0, wantW, wantH) {
				t.Fatalf("%s %s bounds = %v, want %dx%d", spec.name, sc.name, sc.img.Bounds(), wantW, wantH)
			}
			// On non-home, non-pair UI screens without a backdrop, no non-background pixel may appear outside the safe area.
			if sc.name != "home" && sc.name != "home-listen-win" && sc.name != "home-hint" && sc.name != "home-caption" && sc.name != "pair" {
				for y := range wantH {
					for x := range wantW {
						if image.Pt(x, y).In(safePhys) {
							continue
						}
						if got := sc.img.RGBAAt(x, y); got != pxPanelBG {
							t.Fatalf("%s %s drew %v outside safe area at (%d,%d)", spec.name, sc.name, got, x, y)
						}
					}
				}
			}
		}
	}
}

func TestGeneratePixelRenderSamples(t *testing.T) {
	if os.Getenv("LARK_RENDER_SAMPLES") == "" {
		t.Skip("set LARK_RENDER_SAMPLES=1 to write sample PNGs to /tmp/opencode/render-samples/")
	}
	outDir := "/tmp/opencode/render-samples"
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	gregDir := filepath.Join(home, ".local", "share", "lark", "portraits", "72120f6660ef1a2151b318904197b15e")
	poses, err := loadOrNormalizePoses(gregDir, pixelPortraitHeight)
	if err != nil {
		t.Fatalf("loadOrNormalizePoses(%s): %v", gregDir, err)
	}
	gregBG, _ := loadBackgroundPNG(gregDir)
	th := derivePixelTheme(gregBG, poses[:])

	books := []pixelBook{
		{Name: "lost_island", Title: "The Lost Island", Page: 3, Total: 7, Color: pxMint},
		{Name: "clockwork_owl", Title: "The Clockwork Owl", Page: 1, Total: 5, Color: pxPeach},
		{Name: "moonlight_meadow", Title: "Moonlight Meadow", Page: 4, Total: 4, Color: pxSky},
	}
	games := []pixelGame{
		{Name: "monsters", Title: "Monsters", Kind: "game", Saved: true},
		{Name: "fireworks", Title: "Fireworks", Kind: "game", Saved: false},
		{Name: "picture_math", Title: "Picture Math", Kind: "game", Saved: true},
	}
	lostIslandText := "Once upon a time in a faraway land, there were people who always told tales about this island. All of them thought it was true, so they tried hunting for the Lost Island."
	memories := []pixelMemoryItem{
		{Section: "Notes", Key: "bedtime", Value: `{"text":"Greg likes warm chamomile tea before bed and stories about old sailing ships."}`},
		{Section: "Reading", Key: "story_lost_island", Value: `{"parts":["Once upon a time in a faraway land,","they sailed toward the Lost Island."]}`},
		{Section: "Game saves", Key: "monsters", Value: `{"Mint":4,"Cake":0,"Greg":7}`},
		{Section: "Shared", Key: "household", Value: `{"dinner":"6:30 PM","mum":"paired"}`},
	}
	titles := map[string]string{
		"story_lost_island": "The Lost Island",
		"monsters":          "Monsters",
	}
	invite := shortenDeltaChatInvite("https://i.delta.chat/#72120f6660ef1a2151b318904197b15e&i=abcdef1234567890&s=1234567890abcdef&a=greg%40example.org")

	type sampleEntry struct {
		name string
		img  *image.RGBA
	}
	var samples []sampleEntry
	save := func(name string, img *image.RGBA) {
		t.Helper()
		f, err := os.Create(filepath.Join(outDir, name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(f, img); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		samples = append(samples, sampleEntry{name: name, img: img})
	}

	grades := []struct {
		name  string
		grade gradeKey
	}{
		{"dawn", gradeDawn},
		{"day", gradeDay},
		{"dusk", gradeDusk},
		{"night", gradeNight},
	}

	for _, spec := range []pixelDisplaySpec{pixelSpecVoCat, pixelSpecBox3} {
		for _, g := range grades {
			save(fmt.Sprintf("%s-home-%s-idle", spec.name, g.name),
				renderPixelHome(spec, gregBG, poses[0], g.grade, "", ""))
			save(fmt.Sprintf("%s-home-%s-talk", spec.name, g.name),
				renderPixelHome(spec, gregBG, poses[2], g.grade, "speaking", ""))
		}
		save(spec.name+"-home-hint",
			renderPixelHome(spec, gregBG, poses[0], gradeDay, "", ""))
		save(spec.name+"-home-listening",
			renderPixelHome(spec, gregBG, poses[0], gradeDay, "listening", ""))
		save(spec.name+"-home-listen-win",
			renderPixelHomeListen(spec, gregBG, poses[0], gradeDay, "listening", "", true))
		save(spec.name+"-home-caption",
			renderPixelHome(spec, nil, poses[0], gradeDay, "listening", "Message from Mum: dinner's ready"))
		save(spec.name+"-menu-p1", renderPixelMenu(spec, 0, th))
		if spec.round {
			save(spec.name+"-menu-p2", renderPixelMenu(spec, 1, th))
		}
		moreImg, _, _ := renderPixelMoreWithHits(spec, 0, th)
		save(spec.name+"-more", moreImg)
		voiceP1, _, _ := renderPixelVoiceWithHits(spec, heartVoice, 0, th)
		voiceP2, _, _ := renderPixelVoiceWithHits(spec, heartVoice, 1, th)
		save(spec.name+"-voice-p1", voiceP1)
		save(spec.name+"-voice-p2", voiceP2)
		volImg, _, _ := renderPixelVolumeWithHits(spec, false, 60, 0, th)
		save(spec.name+"-volume", volImg)
		statsImg, _, _ := renderPixelStatsWithHits(spec, []menuRow{{"Companion", "Greg"}, {"Battery", "88%"}, {"Voice", "Heart"}}, th)
		save(spec.name+"-stats", statsImg)
		save(spec.name+"-books-list", renderPixelBooksList(spec, books, 0, th))
		save(spec.name+"-games-list", renderPixelGamesList(spec, games, 0, th))
		save(spec.name+"-reader", renderPixelReader(spec, "The Lost Island", lostIslandText, 0, 0, th))
		save(spec.name+"-memories-list", renderPixelMemoriesList(spec, memories, titles, 0, th))
		save(spec.name+"-memories-detail", renderPixelMemoryDetail(spec, memories[2], titles, 0, 0, th))
		pairImg, err := renderPixelPair(spec, "Greg", invite)
		if err != nil {
			t.Fatal(err)
		}
		if layout, err := computePairQRLayout(spec, invite); err == nil {
			t.Logf("%s Pair QR: version=%d modules=%dx%d quiet=%d modPx=%dpx",
				spec.name, layout.version, layout.modules, layout.modules, layout.quiet, layout.modPx)
		}
		save(spec.name+"-pair-qr", pairImg)

		// One sample per app template.
		geom := roundScreen
		if !spec.round {
			geom = screenGeometry{320, 240, "rect"}
		}
		d := &wsTerminal{kind: "hardware", screen: geom, client: &wsClient{kind: "hardware"}}
		for _, tpl := range templateNames() {
			var appSpec savedApp
			_ = appSpec.setTemplate(tpl, map[string]any{})
			if _, views, err := initApp(appSpec, []appPlayer{{id: "sample"}}, nil, []*wsTerminal{d}, nil, nil); err == nil && len(views) > 0 {
				var enc frameEncoder
				save(spec.name+"-tpl-"+tpl, enc.rgbaFromFB(views[0].fb, image.Rect(0, 0, geom.width, geom.height), geom.width))
			}
		}
		// Offline sample.
		offImg := renderPixelListWithHitsImg(spec, "Offline", th)
		save(spec.name+"-offline", offImg)
	}

	// Regenerate and sample all local companion portraits (cake, mint, george) plus mint before/after.
	for _, comp := range []struct{ name, id string }{
		{"cake", "0ba6d163e84af09a85e8501c06fd17ee"},
		{"mint", "97f7705ad6ecd5f87f7ee43ef2ccac9e"},
		{"george", "888c1435c752965e68fde60d14edb409"},
	} {
		dir := filepath.Join(home, ".local", "share", "lark", "portraits", comp.id)
		cPoses, err := loadOrNormalizePoses(dir, pixelPortraitHeight)
		if err != nil {
			continue
		}
		cBG, _ := loadBackgroundPNG(dir)
		for _, spec := range []pixelDisplaySpec{pixelSpecVoCat, pixelSpecBox3} {
			save(fmt.Sprintf("%s-home-%s-day", spec.name, comp.name),
				renderPixelHome(spec, cBG, cPoses[0], gradeDay, "", ""))
		}
		if comp.name == "mint" {
			save("vocat-mint-after", renderPixelHome(pixelSpecVoCat, cBG, cPoses[0], gradeDay, "", ""))
			if bf, err := os.Open(filepath.Join(outDir, "mint-pose-before.png")); err == nil {
				if bImg, err := png.Decode(bf); err == nil {
					bRGBA := image.NewRGBA(bImg.Bounds())
					draw.Draw(bRGBA, bRGBA.Bounds(), bImg, bImg.Bounds().Min, draw.Src)
					save("vocat-mint-before", renderPixelHome(pixelSpecVoCat, cBG, bRGBA, gradeDay, "", ""))
				}
				_ = bf.Close()
			}
		}
	}

	const cols = 4
	const cellW, cellH = 380, 400
	rows := (len(samples) + cols - 1) / cols
	sheet := image.NewRGBA(image.Rect(0, 0, cols*cellW, rows*cellH))
	draw.Draw(sheet, sheet.Bounds(), image.NewUniform(imagecolor.RGBA{14, 12, 20, 255}), image.Point{}, draw.Src)
	for i, smp := range samples {
		col, row := i%cols, i/cols
		x0, y0 := col*cellW+10, row*cellH+28
		drawTextPhysical(sheet, x0, row*cellH+6, strings.TrimSuffix(smp.name, ".png"), dogica.Bold, 2, pxInk)
		b := smp.img.Bounds()
		if b.Dx() > cellW-20 {
			hw, hh := b.Dx()/2, b.Dy()/2
			for y := range hh {
				for x := range hw {
					sheet.SetRGBA(x0+x, y0+y, smp.img.RGBAAt(x*2, y*2))
				}
			}
		} else {
			draw.Draw(sheet, image.Rect(x0, y0, x0+b.Dx(), y0+b.Dy()), smp.img, image.Point{}, draw.Src)
		}
	}
	f, err := os.Create(filepath.Join(outDir, "contact-sheet.png"))
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, sheet); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func renderPixelListWithHitsImg(spec pixelDisplaySpec, title string, th pixelTheme) *image.RGBA {
	c := newPixelCanvas(spec.logicalW, spec.logicalH)
	c.fill(pxPanelBG)
	c.blitSprite((spec.logicalW-12*2)/2, (spec.logicalH-12*2)/2-10, pxMenuSprites["voice"], pxIconPalette, 2)
	var texts []pixelTextOp
	texts = addCenteredText(texts, physicalRect(image.Rect(spec.safe.Min.X, spec.logicalH/2+6, spec.safe.Max.X, spec.logicalH/2+20), spec.artScale), title, dogica.Bold, spec.bodyTextScale, th.accent)
	return finishPixelScreen(c, spec, texts)
}
