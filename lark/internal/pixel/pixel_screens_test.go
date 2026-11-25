package pixel

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

func TestFormatMemoryLinesAndTitles(t *testing.T) {
	t.Parallel()
	if got := FormatMemoryLines(`{"text":"Once upon a time."}`); len(got) != 1 || got[0] != "Once upon a time." {
		t.Fatalf("text field: %v", got)
	}
	if got := FormatMemoryLines(`{"parts":["Line one.","Line two."]}`); len(got) != 1 || got[0] != "Line one. Line two." {
		t.Fatalf("parts field: %v", got)
	}
	got := FormatMemoryLines(`{"Mint":4,"Cake":0}`)
	if len(got) != 2 || got[0] != "Cake: 0" || got[1] != "Mint: 4" {
		t.Fatalf("object fields: %v", got)
	}
	titles := map[string]string{"monsters": "Monsters", "lost_island": "The Lost Island"}
	if got := FormatMemoryTitle("Game saves", "monsters", titles); got != "Monsters" {
		t.Fatalf("game save title = %q, want Monsters", got)
	}
	if got := FormatMemoryTitle("Reading", "story_lost_island", nil); got != "Lost Island" {
		t.Fatalf("humanized story title = %q, want Lost Island", got)
	}
}

func TestEveryScreenFitsSafeAreaAndNoTextOverflow(t *testing.T) {
	t.Parallel()
	stories := []Story{
		{ID: "lost_island", Title: "The Lost Island", Passage: "Harbor", Passages: 7, Color: Mint},
		{ID: "clockwork_owl", Title: "The Clockwork Owl", Passage: "Start", Passages: 5, Color: Peach},
	}
	storyChoices := []string{"Take the mossy path", "Follow the lantern", "Wait here"}
	memories := []MemoryItem{
		{Section: "Notes", Key: "bedtime", Value: `{"text":"Greg likes warm chamomile tea before bed."}`},
		{Section: "Game saves", Key: "monsters", Value: `{"Mint":4,"Cake":0}`},
	}
	titles := map[string]string{"monsters": "Monsters"}
	invite := "https://i.delta.chat/#72120f6660ef1a2151b318904197b15e&i=abcdef1234567890&s=1234567890abcdef&a=greg%40example.org"

	voices := []Tile{
		{ID: "bf_lily", Label: "Lily"}, {ID: "bm_fable", Label: "Fable"}, {ID: "af_heart", Label: "Heart"},
		{ID: "af_bella", Label: "Bella"}, {ID: "am_puck", Label: "Puck"}, {ID: "bf_alice", Label: "Alice"},
		{ID: "bm_george", Label: "George"}, {ID: "bf_isabella", Label: "Isabella"},
	}
	for _, spec := range []Spec{SpecVoCat, SpecBox3} {
		safePhys := PhysicalRect(spec.Safe, spec.ArtScale)
		th := DefaultTheme()
		voiceP1, v1Hits, _ := RenderVoiceWithHits(spec, voices, "bf_lily", 0, th)
		voiceP2, _, _ := RenderVoiceWithHits(spec, voices, "bf_lily", 1, th)
		moreImg, _, _ := RenderMoreWithHits(spec, 0, th)
		volImg, _, _ := RenderVolumeWithHits(spec, 60, 0, th)
		statsImg, _, _ := RenderStatsWithHits(spec, []Row{{"Companion", "Greg"}, {"Voice", "Heart"}}, th)
		wantVoiceP1 := 3
		if !spec.Round {
			wantVoiceP1 = 6
		}
		voiceCount := 0
		for _, hb := range v1Hits {
			if strings.HasPrefix(hb.ID, "voice-") {
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
			{"home", RenderHome(spec, nil, nil, GradeDay, "listening", "")},
			{"home-listen-win", RenderHomeListen(spec, nil, nil, GradeDay, "listening", "", true)},
			{"home-hint", RenderHome(spec, nil, nil, GradeDay, "", "")},
			{"home-caption", RenderHome(spec, nil, nil, GradeDay, "speaking", "Message from Mum: dinner's ready")},
			{"menu", RenderMenu(spec, 0, th)},
			{"more", moreImg},
			{"voice-p1", voiceP1},
			{"voice-p2", voiceP2},
			{"volume", volImg},
			{"stats", statsImg},
			{"stories", RenderStoriesList(spec, stories, 0, th)},
			{"reader", RenderReader(spec, "The Lost Island", "Once upon a time in a faraway land.", 0, 0, th)},
			{"story-bg", func() *image.RGBA {
				img, _, _, _ := RenderStoryReaderWithHits(spec, "Woods", "Fork in the trail.", storyChoices, false, 0, nil, nil, th)
				return img
			}()},
			{"story-choices", func() *image.RGBA {
				img, hits, _, _ := RenderStoryReaderWithHits(spec, "Woods", "Fork in the trail.", storyChoices, false, 1, nil, nil, th)
				for _, h := range hits {
					if strings.HasPrefix(h.ID, "choice-") && (h.Y2-h.Y1+1) < 48 {
						t.Fatalf("%s choice %s height %dpx < 48px", spec.name, h.ID, h.Y2-h.Y1+1)
					}
				}
				return img
			}()},
			{"memories", RenderMemoriesList(spec, memories, titles, 0, th)},
			{"memory-detail", RenderMemoryDetail(spec, memories[1], titles, 0, 0, th)},
		}
		pairImg, err := RenderPair(spec, "Greg", invite)
		if err != nil {
			t.Fatalf("%s pair: %v", spec.name, err)
		}
		screens = append(screens, struct {
			name string
			img  *image.RGBA
		}{"pair", pairImg})

		wantW, wantH := spec.LogicalW*spec.ArtScale, spec.LogicalH*spec.ArtScale
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
	t.Parallel()
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
	poses, err := LoadOrNormalizePoses(gregDir, PortraitHeight)
	if err != nil {
		t.Fatalf("LoadOrNormalizePoses(%s): %v", gregDir, err)
	}
	gregBG, _ := LoadBackgroundPNG(gregDir)
	th := DeriveTheme(gregBG, poses[:])

	stories := []Story{
		{ID: "lost_island", Title: "The Lost Island", Passage: "Harbor", Passages: 7, Color: Mint},
		{ID: "clockwork_owl", Title: "The Clockwork Owl", Passage: "Start", Passages: 5, Color: Peach},
		{ID: "moonlight_meadow", Title: "Moonlight Meadow", Passage: "Clearing", Passages: 4, Color: Sky},
	}
	storyChoices := []string{"Take the mossy path", "Follow the lantern", "Wait here"}
	lostIslandText := "Once upon a time in a faraway land, there were people who always told tales about this island. All of them thought it was true, so they tried hunting for the Lost Island."
	memories := []MemoryItem{
		{Section: "Notes", Key: "bedtime", Value: `{"text":"Greg likes warm chamomile tea before bed and stories about old sailing ships."}`},
		{Section: "Reading", Key: "story_lost_island", Value: `{"parts":["Once upon a time in a faraway land,","they sailed toward the Lost Island."]}`},
		{Section: "Game saves", Key: "monsters", Value: `{"Mint":4,"Cake":0,"Greg":7}`},
		{Section: "Shared", Key: "household", Value: `{"dinner":"6:30 PM","mum":"paired"}`},
	}
	titles := map[string]string{
		"story_lost_island": "The Lost Island",
		"monsters":          "Monsters",
	}
	invite := "https://i.delta.chat/#72120f6660ef1a2151b318904197b15e&i=abcdef1234567890&s=1234567890abcdef&a=greg%40example.org"

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
		grade GradeKey
	}{
		{"dawn", GradeDawn},
		{"day", GradeDay},
		{"dusk", GradeDusk},
		{"night", GradeNight},
	}

	for _, spec := range []Spec{SpecVoCat, SpecBox3} {
		for _, g := range grades {
			save(fmt.Sprintf("%s-home-%s-idle", spec.name, g.name),
				RenderHome(spec, gregBG, poses[0], g.grade, "", ""))
			save(fmt.Sprintf("%s-home-%s-talk", spec.name, g.name),
				RenderHome(spec, gregBG, poses[2], g.grade, "speaking", ""))
		}
		save(spec.name+"-home-hint",
			RenderHome(spec, gregBG, poses[0], GradeDay, "", ""))
		save(spec.name+"-home-listening",
			RenderHome(spec, gregBG, poses[0], GradeDay, "listening", ""))
		save(spec.name+"-home-listen-win",
			RenderHomeListen(spec, gregBG, poses[0], GradeDay, "listening", "", true))
		save(spec.name+"-home-caption",
			RenderHome(spec, nil, poses[0], GradeDay, "listening", "Message from Mum: dinner's ready"))
		voices := []Tile{
			{ID: "bf_lily", Label: "Lily"}, {ID: "bm_fable", Label: "Fable"}, {ID: "af_heart", Label: "Heart"},
			{ID: "af_bella", Label: "Bella"}, {ID: "am_puck", Label: "Puck"}, {ID: "bf_alice", Label: "Alice"},
			{ID: "bm_george", Label: "George"}, {ID: "bf_isabella", Label: "Isabella"},
		}
		save(spec.name+"-menu-p1", RenderMenu(spec, 0, th))
		if spec.Round {
			save(spec.name+"-menu-p2", RenderMenu(spec, 1, th))
		}
		moreImg, _, _ := RenderMoreWithHits(spec, 0, th)
		save(spec.name+"-more", moreImg)
		voiceP1, _, _ := RenderVoiceWithHits(spec, voices, "bf_lily", 0, th)
		voiceP2, _, _ := RenderVoiceWithHits(spec, voices, "bf_lily", 1, th)
		save(spec.name+"-voice-p1", voiceP1)
		save(spec.name+"-voice-p2", voiceP2)
		volImg, _, _ := RenderVolumeWithHits(spec, 60, 0, th)
		save(spec.name+"-volume", volImg)
		statsImg, _, _ := RenderStatsWithHits(spec, []Row{{"Companion", "Greg"}, {"Battery", "88%"}, {"Voice", "Heart"}}, th)
		save(spec.name+"-stats", statsImg)
		save(spec.name+"-stories-list", RenderStoriesList(spec, stories, 0, th))
		save(spec.name+"-reader", RenderReader(spec, "The Lost Island", lostIslandText, 0, 0, th))
		storyTextImg, _, _, _ := RenderStoryReaderWithHits(spec, "The Whispering Woods", "You stand at a fork in the trail.", storyChoices, false, 0, gregBG, poses[0], th)
		save(spec.name+"-story-bg", storyTextImg)
		storyChoicesImg, choiceHits, _, _ := RenderStoryReaderWithHits(spec, "The Whispering Woods", "You stand at a fork in the trail.", storyChoices, false, 1, gregBG, poses[0], th)
		save(spec.name+"-story-choices", storyChoicesImg)
		for _, ch := range choiceHits {
			if strings.HasPrefix(ch.ID, "choice-") && (ch.Y2-ch.Y1+1) < 48 {
				t.Fatalf("%s choice %s height %dpx < 48px", spec.name, ch.ID, ch.Y2-ch.Y1+1)
			}
		}
		save(spec.name+"-memories-list", RenderMemoriesList(spec, memories, titles, 0, th))
		save(spec.name+"-memories-detail", RenderMemoryDetail(spec, memories[2], titles, 0, 0, th))
		pairImg, err := RenderPair(spec, "Greg", invite)
		if err != nil {
			t.Fatal(err)
		}
		if layout, err := computePairQRLayout(spec, invite); err == nil {
			t.Logf("%s Pair QR: version=%d modules=%dx%d quiet=%d modPx=%dpx",
				spec.name, layout.version, layout.modules, layout.modules, layout.quiet, layout.modPx)
		}
		save(spec.name+"-pair-qr", pairImg)

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
		cPoses, err := LoadOrNormalizePoses(dir, PortraitHeight)
		if err != nil {
			continue
		}
		cBG, _ := LoadBackgroundPNG(dir)
		for _, spec := range []Spec{SpecVoCat, SpecBox3} {
			save(fmt.Sprintf("%s-home-%s-day", spec.name, comp.name),
				RenderHome(spec, cBG, cPoses[0], GradeDay, "", ""))
		}
		if comp.name == "mint" {
			save("vocat-mint-after", RenderHome(SpecVoCat, cBG, cPoses[0], GradeDay, "", ""))
			if bf, err := os.Open(filepath.Join(outDir, "mint-pose-before.png")); err == nil {
				if bImg, err := png.Decode(bf); err == nil {
					bRGBA := image.NewRGBA(bImg.Bounds())
					draw.Draw(bRGBA, bRGBA.Bounds(), bImg, bImg.Bounds().Min, draw.Src)
					save("vocat-mint-before", RenderHome(SpecVoCat, cBG, bRGBA, GradeDay, "", ""))
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

func renderPixelListWithHitsImg(spec Spec, title string, th Theme) *image.RGBA {
	c := newPixelCanvas(spec.LogicalW, spec.LogicalH)
	c.fill(pxPanelBG)
	c.blitSprite((spec.LogicalW-12*2)/2, (spec.LogicalH-12*2)/2-10, pxMenuSprites["voice"], pxIconPalette, 2)
	var texts []pixelTextOp
	texts = addCenteredText(texts, PhysicalRect(image.Rect(spec.Safe.Min.X, spec.LogicalH/2+6, spec.Safe.Max.X, spec.LogicalH/2+20), spec.ArtScale), title, dogica.Bold, spec.BodyTextScale, th.Accent)
	return finishPixelScreen(c, spec, texts)
}

func TestRenderPixelListDrawsNavBar(t *testing.T) {
	t.Parallel()
	spec := SpecForGeometry(360, 360, "round")
	th := DefaultTheme()
	for _, items := range [][]Tile{nil, {{ID: "a", Label: "Alpha"}}} {
		img, hits, _ := RenderListWithHits(spec, "Memories", items, "No memories", 0, th, th.Accent)
		var back HitBox
		for _, h := range hits {
			if h.ID == "menu-back" {
				back = h
			}
		}
		if back.X2 <= back.X1 {
			t.Fatalf("missing menu-back hit")
		}
		// Verify the Back button border is actually drawn (differs from background).
		bg := img.RGBAAt(0, 0)
		drawn := false
		for y := back.Y1; y <= back.Y2 && !drawn; y++ {
			for x := back.X1; x <= back.X2; x++ {
				if img.RGBAAt(x, y) != bg {
					drawn = true
					break
				}
			}
		}
		if !drawn {
			t.Fatalf("menu-back button was not drawn for items=%v", items)
		}
	}
}

func TestPixelQRModuleRoundTrip(t *testing.T) {
	t.Parallel()
	const invite = "https://i.delta.chat/#72120f6660ef1a2151b318904197b15e&i=abcdef1234567890&s=1234567890abcdef&a=greg%40example.org"
	pureBlack := imagecolor.RGBA{0, 0, 0, 255}
	pureWhite := imagecolor.RGBA{255, 255, 255, 255}
	for _, spec := range []Spec{SpecVoCat, SpecBox3} {
		layout, err := computePairQRLayout(spec, invite)
		if err != nil {
			t.Fatalf("%s computePairQRLayout: %v", spec.name, err)
		}
		img, err := RenderPair(spec, "Greg", invite)
		if err != nil {
			t.Fatalf("%s RenderPair: %v", spec.name, err)
		}
		if img.Bounds().Dx() != spec.LogicalW*spec.ArtScale || img.Bounds().Dy() != spec.LogicalH*spec.ArtScale {
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
