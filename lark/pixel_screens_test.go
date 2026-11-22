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
	"rsc.io/qr"
)

func TestPixelQRModuleRoundTrip(t *testing.T) {
	const invite = "https://i.delta.chat/#72120f6660ef1a2151b318904197b15e&a=greg%40example.org"
	code, err := qr.Encode(invite, qr.L)
	if err != nil {
		t.Fatalf("qr.Encode: %v", err)
	}
	for _, spec := range []pixelDisplaySpec{pixelSpecVoCat, pixelSpecBox3} {
		img, err := renderPixelPair(spec, "Pair", invite)
		if err != nil {
			t.Fatalf("%s renderPixelPair: %v", spec.name, err)
		}
		if img.Bounds().Dx() != spec.logicalW*spec.artScale || img.Bounds().Dy() != spec.logicalH*spec.artScale {
			t.Fatalf("%s bounds = %v", spec.name, img.Bounds())
		}
		// Verify finder pattern (7x7 black border at top-left of QR) is crisp and un-antialiased.
		safePhys := physicalRect(spec.safe, spec.artScale)
		titleH := 12 * spec.artScale
		availW := safePhys.Dx()
		availH := safePhys.Dy() - titleH - 4
		const quiet = 2
		totalMods := code.Size + 2*quiet
		modPx := max(1, min(availW, availH)/totalMods)
		qrSide := totalMods * modPx
		qx0 := safePhys.Min.X + (availW-qrSide)/2
		qy0 := safePhys.Min.Y + titleH + 4 + (availH-qrSide)/2
		for my := range code.Size {
			for mx := range code.Size {
				want := pxQRWhite
				if code.Black(mx, my) {
					want = pxQRDark
				}
				for dy := range modPx {
					for dx := range modPx {
						got := img.RGBAAt(qx0+(mx+quiet)*modPx+dx, qy0+(my+quiet)*modPx+dy)
						if got != want {
							t.Fatalf("%s module (%d,%d) pixel (%d,%d) = %v, want %v", spec.name, mx, my, dx, dy, got, want)
						}
					}
				}
			}
		}
	}
}

func TestFormatMemoryLines(t *testing.T) {
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
}

func TestEveryScreenFitsSafeAreaAndNoTextOverflow(t *testing.T) {
	books := []pixelBook{
		{Title: "The Lost Island", Page: 3, Total: 7, Color: pxMint},
		{Title: "The Clockwork Owl", Page: 1, Total: 5, Color: pxPeach},
	}
	memories := []pixelMemoryItem{
		{Section: "Notes", Key: "bedtime", Value: `{"text":"Greg likes warm chamomile tea before bed."}`},
		{Section: "Game saves", Key: "monsters", Value: `{"Mint":4,"Cake":0}`},
	}
	const invite = "https://i.delta.chat/#72120f6660ef1a2151b318904197b15e&a=greg%40example.org"

	for _, spec := range []pixelDisplaySpec{pixelSpecVoCat, pixelSpecBox3} {
		safePhys := physicalRect(spec.safe, spec.artScale)
		screens := []struct {
			name string
			img  *image.RGBA
		}{
			{"home", renderPixelHome(spec, nil, nil, gradeDay, "listening", 0, "")},
			{"home-caption", renderPixelHome(spec, nil, nil, gradeDay, "speaking", 0, "Message from Mum: dinner's ready")},
			{"menu", renderPixelMenu(spec, 0)},
			{"books", renderPixelBooksList(spec, books, 0)},
			{"reader", renderPixelReader(spec, "The Lost Island", "Once upon a time in a faraway land.", 0, 0)},
			{"memories", renderPixelMemoriesList(spec, memories, 0)},
			{"memory-detail", renderPixelMemoryDetail(spec, memories[0], 0, 0)},
		}
		pairImg, err := renderPixelPair(spec, "Pair", invite)
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
			// On non-home UI screens, no non-background pixel may appear outside the safe area.
			if sc.name != "home" && sc.name != "home-caption" {
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

	books := []pixelBook{
		{Title: "The Lost Island", Page: 3, Total: 7, Color: pxMint},
		{Title: "The Clockwork Owl", Page: 1, Total: 5, Color: pxPeach},
		{Title: "Moonlight Meadow", Page: 4, Total: 4, Color: pxSky},
	}
	lostIslandText := "Once upon a time in a faraway land, there were people who always told tales about this island. All of them thought it was true, so they tried hunting for the Lost Island."
	memories := []pixelMemoryItem{
		{Section: "Notes", Key: "bedtime", Value: `{"text":"Greg likes warm chamomile tea before bed and stories about old sailing ships."}`},
		{Section: "Reading", Key: "story_lost_island", Value: `{"parts":["Once upon a time in a faraway land,","they sailed toward the Lost Island."]}`},
		{Section: "Game saves", Key: "monsters", Value: `{"Mint":4,"Cake":0,"Greg":7}`},
		{Section: "Shared", Key: "household", Value: `{"dinner":"6:30 PM","mum":"paired"}`},
	}
	const invite = "https://i.delta.chat/#72120f6660ef1a2151b318904197b15e&a=greg%40example.org"

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
				renderPixelHome(spec, nil, poses[0], g.grade, "", 0, ""))
			save(fmt.Sprintf("%s-home-%s-talk", spec.name, g.name),
				renderPixelHome(spec, nil, poses[2], g.grade, "speaking", 0, ""))
		}
		save(spec.name+"-home-caption",
			renderPixelHome(spec, nil, poses[0], gradeDay, "listening", 0, "Message from Mum: dinner's ready"))
		save(spec.name+"-menu-p1", renderPixelMenu(spec, 0))
		save(spec.name+"-books-list", renderPixelBooksList(spec, books, 0))
		save(spec.name+"-reader", renderPixelReader(spec, "The Lost Island", lostIslandText, 0, 0))
		save(spec.name+"-memories-list", renderPixelMemoriesList(spec, memories, 0))
		save(spec.name+"-memories-detail", renderPixelMemoryDetail(spec, memories[2], 0, 0))
		pairImg, err := renderPixelPair(spec, "Pair", invite)
		if err != nil {
			t.Fatal(err)
		}
		save(spec.name+"-pair-qr", pairImg)
	}

	// Side-by-side 2x vs 3x body text comparisons on VoCat.
	sideBySide := func(name string, left, right *image.RGBA, labelL, labelR string) {
		w := left.Bounds().Dx() + right.Bounds().Dx() + 16
		h := max(left.Bounds().Dy(), right.Bounds().Dy()) + 28
		comb := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(comb, comb.Bounds(), image.NewUniform(imagecolor.RGBA{18, 16, 26, 255}), image.Point{}, draw.Src)
		draw.Draw(comb, left.Bounds().Add(image.Pt(0, 24)), left, image.Point{}, draw.Src)
		draw.Draw(comb, right.Bounds().Add(image.Pt(left.Bounds().Dx()+16, 24)), right, image.Point{}, draw.Src)
		drawTextPhysical(comb, 8, 4, labelL, dogica.Bold, 2, pxPeach)
		drawTextPhysical(comb, left.Bounds().Dx()+24, 4, labelR, dogica.Bold, 2, pxMint)
		save(name, comb)
	}
	sideBySide("vocat-reader-2x-vs-3x",
		renderPixelReader(pixelSpecVoCat, "The Lost Island", lostIslandText, 0, 2),
		renderPixelReader(pixelSpecVoCat, "The Lost Island", lostIslandText, 0, 3),
		"VoCat Reader 2x (16px)", "VoCat Reader 3x (24px)")
	sideBySide("vocat-memory-2x-vs-3x",
		renderPixelMemoryDetail(pixelSpecVoCat, memories[0], 0, 2),
		renderPixelMemoryDetail(pixelSpecVoCat, memories[0], 0, 3),
		"VoCat Memory 2x (16px)", "VoCat Memory 3x (24px)")

	// Contact sheet combining all samples in a 4-column grid.
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
			// Half-size nearest-neighbor fit for wide side-by-side comparisons.
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
