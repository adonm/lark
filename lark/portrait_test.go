package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	imagecolor "image/color"
	"image/draw"
	"image/png"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lark/internal/pixel"
)

func portraitFixture(t *testing.T) []byte {
	t.Helper()
	bg := parseHexRGBA("#FBEFE6", imagecolor.RGBA{})
	fur := parseHexRGBA("#D4B5DE", imagecolor.RGBA{})
	ink := parseHexRGBA("#583E62", imagecolor.RGBA{})
	img := image.NewRGBA(image.Rect(0, 0, 1024, 1024))
	draw.Draw(img, img.Bounds(), image.NewUniform(bg), image.Point{}, draw.Src)
	for i, offset := range []image.Point{{13, -21}, {-20, 15}, {8, 19}, {-15, -12}} {
		base := image.Pt(i%2*512, i/2*512)
		center := base.Add(image.Pt(256, 256)).Add(offset)
		for y := -160; y <= 160; y++ {
			for x := -165; x <= 165; x++ {
				if x*x*160*160+y*y*165*165 > 165*165*160*160 {
					continue
				}
				img.SetRGBA(center.X+x, center.Y+y, fur)
			}
		}
		body := []image.Rectangle{image.Rect(-30, 43, 30, 51), image.Rect(-36, -28, 36, -21),
			image.Rect(-33, 35, 33, 68), image.Rect(-25, 25, 20, 31)}[i].Add(center)
		draw.Draw(img, body, image.NewUniform(ink), image.Point{}, draw.Src)
		img.SetRGBA(base.X+22, base.Y+20, ink)
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestPortraitAlignsTranslatedSilhouettes(t *testing.T) {
	t.Parallel()
	raw := portraitFixture(t)
	sheet, err := decodePortraitSheet(raw)
	if err != nil {
		t.Fatal(err)
	}
	p := parseHexRGBA("#FFF2E7", imagecolor.RGBA{})
	poses, err := pixel.Slice(sheet, p)
	if err != nil {
		t.Fatal(err)
	}
	var painted image.Rectangle
	for i, im := range poses {
		b := pixel.PoseBounds(im, im.Bounds(), p)
		if i == 0 {
			painted = b
		}
		center := b.Min.Add(b.Max)
		if b != painted || center.X < 359 || center.X > 361 || center.Y < 323 || center.Y > 325 {
			t.Fatalf("%s drifted: got %v, idle %v", pixel.Poses[i], b, painted)
		}
		if i != 0 && bytes.Equal(im.Pix, poses[0].Pix) {
			t.Fatalf("%s did not preserve its expression", pixel.Poses[i])
		}
	}
	if _, err := pixel.Slice(image.NewUniform(imagecolor.White), p); err == nil {
		t.Fatal("accepted wrong sheet dimensions")
	}
	if _, err := pixel.Slice(image.NewRGBA(image.Rect(0, 0, 1024, 1024)), p); err == nil {
		t.Fatal("accepted an empty sheet")
	}
}

func TestPortraitSilhouettesClearMenuAndBezel(t *testing.T) {
	t.Parallel()
	bg := parseHexRGBA("#FFF2E7", imagecolor.RGBA{})
	for _, shape := range []image.Rectangle{image.Rect(100, 50, 410, 460), image.Rect(22, 106, 490, 406)} {
		sheet := image.NewRGBA(image.Rect(0, 0, 1024, 1024))
		draw.Draw(sheet, sheet.Bounds(), image.NewUniform(bg), image.Point{}, draw.Src)
		for i := range pixel.Poses {
			offset := image.Pt(i%2*512+i*3, i/2*512-i*4)
			draw.Draw(sheet, shape.Add(offset), image.NewUniform(imagecolor.Black), image.Point{}, draw.Src)
		}
		poses, err := pixel.Slice(sheet, bg)
		if err != nil {
			t.Fatal(err)
		}
		menu := image.Rect(120, 292, 240, 340)
		for i, pose := range poses {
			b := pixel.PoseBounds(pose, pose.Bounds(), bg)
			if b.Empty() || b.Overlaps(menu) || b.Min.Y < 42 || b.Max.Y > 284 {
				t.Fatalf("%s: portrait %v outside safe vertical framing", pixel.Poses[i], b)
			}
			if scaleX, scaleY := float64(b.Dx())/float64(shape.Dx()), float64(b.Dy())/float64(shape.Dy()); math.Abs(scaleX-scaleY) > 0.01 {
				t.Fatalf("%s: portrait %v distorted from %v", pixel.Poses[i], b, shape)
			}
			for _, x := range []int{b.Min.X, b.Max.X} {
				for _, y := range []int{b.Min.Y, b.Max.Y} {
					if (x-180)*(x-180)+(y-180)*(y-180) >= 174*174 {
						t.Fatalf("%s: portrait %v would be clipped by the bezel", pixel.Poses[i], b)
					}
				}
			}
		}
	}
}

func TestPixelPortraitGridAndAlignment(t *testing.T) {
	t.Parallel()
	fixture := portraitFixture(t)
	store := &portraitStore{dir: t.TempDir(), generate: func(_ context.Context, prompt string, _ []byte) ([]byte, error) {
		if strings.Contains(prompt, "background scene") {
			return fixture, nil
		}
		for _, text := range []string{"indie-game pixel art", "64×64", "hue-shifted colored outlines", "no text, labels, borders or gridlines", "solid flat #FAF0E6"} {
			if !strings.Contains(prompt, text) {
				t.Errorf("missing %q in the pixel prompt", text)
			}
		}
		return fixture, nil
	}}
	const side, grid = 360, 64
	spec := portraitSpec{Prompt: "a hedgehog", Form: "a hedgehog", Style: portraitPixelStyle, BG: "#FAF0E6"}
	id, err := store.create(t.Context(), spec, "")
	if err != nil {
		t.Fatal(err)
	}
	bg := parseHexRGBA("#FAF0E6", imagecolor.RGBA{})
	var idle []byte
	var bounds image.Rectangle
	for i, pose := range pixel.Poses {
		data, err := store.read(id, pose)
		if err != nil {
			t.Fatal(err)
		}
		im, err := png.Decode(bytes.NewReader(data))
		if err != nil || im.Bounds().Size() != image.Pt(side, side) {
			t.Fatalf("%s: expected 360×360 PNG: %v", pose, err)
		}
		cell := func(x int) int { return (2*x + 1) * grid / (2 * side) }
		cells := [grid][grid]imagecolor.RGBA{}
		rgba := image.NewRGBA(im.Bounds())
		draw.Draw(rgba, rgba.Bounds(), im, image.Point{}, draw.Src)
		n, sx, sy := 0, 0, 0
		for y := range side {
			for x := range side {
				k := &cells[cell(y)][cell(x)]
				c := rgba.RGBAAt(x, y)
				if k.A != 0 && *k != c {
					t.Fatalf("%s: color changed inside a pixel block at %d,%d", pose, x, y)
				}
				*k = c
				if pixel.Diff(c, bg) > 25*25 {
					n, sx, sy = n+1, sx+x, sy+y
				}
			}
		}
		b := pixel.PoseBounds(rgba, rgba.Bounds(), bg)
		if i == 0 {
			bounds, idle = b, rgba.Pix
		} else if b != bounds || bytes.Equal(idle, rgba.Pix) {
			t.Fatalf("%s: silhouette shifted or expression lost", pose)
		}
		if n == 0 || math.Abs(float64(sx)/float64(n)-179.5) > 1 || math.Abs(float64(sy)/float64(n)-161.5) > 2 {
			t.Fatalf("%s: silhouette centroid drifted", pose)
		}
	}
	if portraitPrompt(portraitSpec{}) != portraitPrompt(portraitSpec{Style: "storybook"}) {
		t.Fatal("explicit Storybook style altered the default prompt")
	}
}

func TestPortraitConsoleContract(t *testing.T) {
	t.Parallel()
	source := []byte("reference fixture")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req object
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			http.Error(w, "bad fixture request", 400)
			return
		}
		config := req["generationConfig"].(object)
		parts := req["contents"].([]any)[0].(object)["parts"].([]any)
		asset := parts[1].(object)["inlineData"].(object)
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer fixture-only" ||
			config["imageConfig"].(object)["imageSize"] != "1K" ||
			asset["mimeType"] != "image/png" || len(parts) != 2 {
			t.Error("invalid Console contract")
		}
		_ = json.NewEncoder(w).Encode(object{"candidates": []object{{"content": object{
			"parts": []object{{"inlineData": object{"mimeType": "image/png", "data": source}}},
		}}}})
	}))
	defer server.Close()
	got, err := requestPortrait(t.Context(), server.Client(), server.URL, "fixture-only", "A cozy owl", source)
	if err != nil || !bytes.Equal(got, source) {
		t.Fatalf("invalid Console response: %v", err)
	}
}

func TestPortraitOpenCodeContract(t *testing.T) {
	t.Parallel()
	source := []byte("reference fixture")
	sheet := portraitFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model      ocModel
			Prompt     string
			Aspect     string
			Resolution string
			N          int
			Images     []openCodeImage
		}
		user, password, authenticated := r.BasicAuth()
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			http.Error(w, "bad fixture request", 400)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != openCodeImagePath || r.URL.RawQuery != "" ||
			!authenticated || user != "opencode" || password != "fixture-only" ||
			req.Model != (ocModel{Provider: "opencode", ID: "gemini-nano-banana-2.1"}) ||
			req.Aspect != "1:1" || req.Resolution != "1K" || req.N != 1 {
			t.Error("invalid OpenCode image request")
		}
		switch req.Prompt {
		case "draw an owl":
			if req.Images != nil {
				t.Error("create sent edit references")
			}
		case "add a scarf":
			if len(req.Images) != 1 || req.Images[0].MIME != "image/png" || !bytes.Equal(req.Images[0].Data, source) {
				t.Error("edit missed its reference sheet")
			}
		default:
			t.Error("unexpected prompt")
		}
		_ = json.NewEncoder(w).Encode(object{"data": object{"images": []object{
			{"type": "image", "mime": "image/png", "data": sheet},
		}}})
	}))
	defer server.Close()
	oc, err := openCodeUpstream(server.Client(), "", server.URL, "fixture-only")
	if err != nil {
		t.Fatal(err)
	}
	if configuredPortraitGenerator(oc, false, "") != nil {
		t.Fatal("shared-service discovery alone should not enable billed portraits")
	}
	generate := configuredPortraitGenerator(oc, true, "")
	for _, tc := range []struct {
		prompt string
		input  []byte
	}{{"draw an owl", nil}, {"add a scarf", source}} {
		got, err := generate(t.Context(), tc.prompt, tc.input)
		if err != nil || !bytes.Equal(got, sheet) {
			t.Fatalf("%q: invalid OpenCode image response: %v", tc.prompt, err)
		}
	}
}

func TestCompanionFormChangeRefreshesBackgroundCache(t *testing.T) {
	t.Parallel()
	sheet := portraitFixture(t)
	dir := t.TempDir()
	var backgrounds atomic.Int32
	encode := func(img image.Image) []byte {
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Error(err)
		}
		return buf.Bytes()
	}
	b, base := wsHarness(t, nil)
	b.portraitSync = true
	b.portraits = &portraitStore{dir: dir, generate: func(_ context.Context, prompt string, _ []byte) ([]byte, error) {
		if strings.Contains(prompt, "background scene") {
			n := backgrounds.Add(1)
			img := image.NewRGBA(image.Rect(0, 0, 64, 48))
			draw.Draw(img, img.Bounds(), image.NewUniform(imagecolor.RGBA{R: uint8(20 * n), G: 80, B: 40, A: 255}), image.Point{}, draw.Src)
			return encode(img), nil
		}
		return sheet, nil
	}}
	own := testObject(t, testRequest(t, "POST", base+"/api/companions", "unit", object{"name": "Own", "form": "a cat"}, 201))
	id := str(own["id"])
	created := toolTest(t, base, "patch", object{"companion_id": id, "companion": object{"portrait": object{"prompt": "draw an owl"}}}, false)
	companion, _ := created["companion"].(object)
	pid := str(companion["portrait_id"])
	if !portraitIDPattern.MatchString(pid) {
		t.Fatalf("no portrait id in %+v", created)
	}

	c := b.companionFor(id)
	cached, _, _ := b.hub.companionAssets(&c)
	if cached == nil {
		t.Fatal("portrait background was not cached")
	}
	before := cached.RGBAAt(cached.Bounds().Dx()/2, cached.Bounds().Dy()/2)

	if out := toolTest(t, base, "patch", object{"companion_id": id, "companion": object{"form": "a red fox"}}, false); out["companion"] == nil {
		t.Fatalf("form patch failed: %+v", out)
	}
	// The regeneration runs in the background: wait for the new file, then for
	// the invalidated cache entry.
	deadline := time.Now().Add(testWait(t))
	fileRefreshed := false
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(filepath.Join(dir, pid, "bg.png")); err == nil {
			if img, err := png.Decode(bytes.NewReader(raw)); err == nil && img.At(img.Bounds().Dx()/2, img.Bounds().Dy()/2) != before {
				fileRefreshed = true
				break
			}
		}
		time.Sleep(time.Millisecond)
	}
	if !fileRefreshed {
		t.Fatal("form change did not regenerate the background file")
	}
	for time.Now().Before(deadline) {
		b.hub.assetsMu.RLock()
		_, present := b.hub.assets[pid]
		b.hub.assetsMu.RUnlock()
		if !present {
			break
		}
		time.Sleep(time.Millisecond)
	}
	refreshed, _, _ := b.hub.companionAssets(&c)
	if refreshed == nil {
		t.Fatal("portrait background missing after form change")
	}
	after := refreshed.RGBAAt(refreshed.Bounds().Dx()/2, refreshed.Bounds().Dy()/2)
	if after == before {
		t.Fatalf("cached background survived the form change: %v", after)
	}
}

func TestPortraitPatchScopeAndNativeFrames(t *testing.T) {
	t.Parallel()
	sheet := portraitFixture(t)
	dir := t.TempDir()
	b, base := wsHarness(t, nil)
	own := testObject(t, testRequest(t, "POST", base+"/api/companions", "unit", object{"name": "Own", "form": "a cat"}, 201))
	other := testObject(t, testRequest(t, "POST", base+"/api/companions", "unit", object{"name": "Other", "form": "a cat"}, 201))
	id := str(own["id"])
	patch := func(spec object, failed bool) object {
		t.Helper()
		out := toolTest(t, base, "patch", object{"companion_id": id, "companion": object{"portrait": spec}}, failed)
		companion, _ := out["companion"].(object)
		return companion
	}
	patch(object{"prompt": "draw an owl"}, true) // No configured generator or paid fallback.
	b.portraitSync = true
	b.portraits = &portraitStore{dir: dir, generate: func(_ context.Context, prompt string, input []byte) ([]byte, error) {
		if strings.Contains(prompt, "add a scarf") != (len(input) > 0) {
			t.Error("edit source does not match the requested operation")
		}
		if len(input) > 0 {
			if _, err := png.Decode(bytes.NewReader(input)); err != nil {
				t.Error("sent an invalid reference sheet")
			}
		}
		return sheet, nil
	}}
	patch(object{"prompt": " ", "edit": true}, true)
	patch(object{"prompt": "invalid", "typo": true}, true)
	c := patch(object{"prompt": "draw an owl", "form": "a snowy owl"}, false)
	asset := str(c["portrait_id"])
	if !portraitIDPattern.MatchString(asset) || c["form"] != "a snowy owl" {
		t.Fatal(c)
	}
	for _, pose := range append(slices.Clone(pixel.Poses[:]), "sheet") {
		fi, err := os.Stat(filepath.Join(dir, asset, pose+".png"))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s not private: %v", pose, err)
		}
	}
	_ = other
	poses, err := pixel.LoadOrNormalizePoses(filepath.Join(dir, asset), pixel.PortraitHeight)
	if err != nil || poses[0] == nil {
		t.Fatalf("pixel.LoadOrNormalizePoses: %v", err)
	}
	day := pixel.RenderHome(pixel.SpecVoCat, nil, poses[0], pixel.GradeDay, "", "")
	night := pixel.RenderHome(pixel.SpecVoCat, nil, poses[0], pixel.GradeNight, "", "")
	if day.RGBAAt(180, 180) == night.RGBAAt(180, 180) {
		t.Fatal("portrait home was not graded")
	}
	edited := patch(object{"prompt": "add a scarf", "edit": true}, false)
	if newAsset := str(edited["portrait_id"]); newAsset == "" || asset == newAsset || edited["form"] != "a snowy owl" {
		t.Fatal(edited)
	}
	if _, err := os.Stat(filepath.Join(dir, asset)); !os.IsNotExist(err) {
		t.Fatalf("replaced portrait directory survived: %v", err)
	}
}

func TestPortraitInteriorMatchesBGStaysOpaque(t *testing.T) {
	t.Parallel()
	bg := imagecolor.RGBA{240, 244, 240, 255}
	outline := imagecolor.RGBA{45, 70, 55, 255}
	sheet := image.NewRGBA(image.Rect(0, 0, pixel.SheetSide, pixel.SheetSide))
	draw.Draw(sheet, sheet.Bounds(), image.NewUniform(bg), image.Point{}, draw.Src)
	// Draw a closed outline ring in each quadrant whose interior has the exact background colour
	// (and even transparent pixels) — the flood-fill must keep the entire interior opaque.
	for i := range pixel.Poses {
		cx, cy := i%2*512+256, i/2*512+256
		for y := -120; y <= 120; y++ {
			for x := -120; x <= 120; x++ {
				d2 := x*x + y*y
				if d2 <= 120*120 && d2 >= 96*96 {
					sheet.SetRGBA(cx+x, cy+y, outline)
				} else if d2 < 96*96 && (x+y)%5 == 0 {
					// Interior transparent pixel must also become opaque.
					sheet.SetRGBA(cx+x, cy+y, imagecolor.RGBA{})
				}
			}
		}
	}
	poses, err := pixel.NormalizePortraitSheet(sheet, pixel.PortraitHeight)
	if err != nil {
		t.Fatalf("pixel.NormalizePortraitSheet: %v", err)
	}
	for i, pose := range poses {
		b := pose.Bounds()
		cx, cy := b.Min.X+b.Dx()/2, b.Min.Y+b.Dy()/2
		for dy := -5; dy <= 5; dy++ {
			for dx := -5; dx <= 5; dx++ {
				if got := pose.RGBAAt(cx+dx, cy+dy); got.A != 255 {
					t.Fatalf("pose %d interior pixel (%d,%d) became transparent: %v", i, cx+dx, cy+dy, got)
				}
			}
		}
		// Binary alpha only.
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				a := pose.RGBAAt(x, y).A
				if a != 0 && a != 255 {
					t.Fatalf("pose %d has partial alpha %d at (%d,%d)", i, a, x, y)
				}
			}
		}
	}

	// Also verify against mint's real sheet if present locally.
	home, err := os.UserHomeDir()
	if err == nil {
		mintSheetPath := filepath.Join(home, ".local", "share", "lark", "portraits", "97f7705ad6ecd5f87f7ee43ef2ccac9e", "sheet.png")
		if f, err := os.Open(mintSheetPath); err == nil {
			defer f.Close()
			if img, err := png.Decode(f); err == nil {
				mPoses, err := pixel.NormalizePortraitSheet(img, pixel.PortraitHeight)
				if err != nil {
					t.Fatalf("mint pixel.NormalizePortraitSheet: %v", err)
				}
				mb := mPoses[0].Bounds()
				if got := mPoses[0].RGBAAt(mb.Min.X+mb.Dx()/2, mb.Min.Y+mb.Dy()/2); got.A != 255 {
					t.Fatalf("mint center face pixel is transparent: %v", got)
				}
			}
		}
	}
}

// An unfinished portrait must not decode (and fail) on every frame, and a load
// that raced an invalidation must not repopulate the old bytes.
func TestCompanionAssetsCachePartialsAndRespectEpoch(t *testing.T) {
	t.Parallel()
	h := &terminalHub{b: &larkServer{portraits: &portraitStore{dir: t.TempDir()}}}
	c := companionState{ID: "p1", PortraitID: "missing"}
	h.companionAssets(&c)
	h.assetsMu.RLock()
	entry, ok := h.assets["missing"]
	h.assetsMu.RUnlock()
	if !ok {
		t.Fatal("placeholder portrait was not cached")
	}
	if entry.expires.IsZero() || time.Until(entry.expires) > partialAssetTTL {
		t.Fatalf("partial entry expiry = %v, want within %v", entry.expires, partialAssetTTL)
	}

	epoch := h.assetsEpoch["p2"] // the epoch a load reads before decoding
	h.invalidateAssets("p2")
	h.storeAssets("p2", cachedAssets{theme: pixel.PlaceholderTheme()}, epoch)
	h.assetsMu.RLock()
	_, stale := h.assets["p2"]
	h.assetsMu.RUnlock()
	if stale {
		t.Fatal("a load from the pre-invalidation epoch was cached")
	}
	h.storeAssets("p2", cachedAssets{theme: pixel.PlaceholderTheme()}, h.assetsEpoch["p2"])
	h.assetsMu.RLock()
	_, fresh := h.assets["p2"]
	h.assetsMu.RUnlock()
	if !fresh {
		t.Fatal("a load from the current epoch was not cached")
	}
}

// A portrait patch returns immediately, persists text fields, then applies the
// generated portrait and announces it when the worker finishes.
func TestPortraitPatchGeneratesAsynchronously(t *testing.T) {
	t.Parallel()
	sheet := portraitFixture(t)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	encode := func(img image.Image) []byte {
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Error(err)
		}
		return buf.Bytes()
	}
	dir := t.TempDir()
	b, base := wsHarness(t, nil)
	b.portraits = &portraitStore{dir: dir, generate: func(_ context.Context, prompt string, _ []byte) ([]byte, error) {
		if strings.Contains(prompt, "background scene") {
			img := image.NewRGBA(image.Rect(0, 0, 64, 48))
			draw.Draw(img, img.Bounds(), image.NewUniform(imagecolor.RGBA{R: 30, G: 80, B: 50, A: 255}), image.Point{}, draw.Src)
			return encode(img), nil
		}
		started <- struct{}{}
		<-release
		return sheet, nil
	}}
	id := str(testObject(t, testRequest(t, "POST", base+"/api/companions", "unit", object{"name": "Own", "form": "a cat"}, 201))["id"])

	out := toolTest(t, base, "patch", object{"companion_id": id, "companion": object{
		"personality": "curious", "portrait": object{"prompt": "draw an owl", "form": "a snowy owl"},
	}}, false)
	companion, _ := out["companion"].(object)
	if companion["status"] != "generating" || companion["form"] != "a snowy owl" {
		t.Fatalf("patch did not return a generating result: %+v", out)
	}
	if c := b.companionFor(id); c.Form != "a snowy owl" || c.Personality != "curious" || c.PortraitID != "" {
		t.Fatalf("text fields were not persisted immediately: %+v", c)
	}
	<-started
	close(release)
	if err := b.waitPortrait(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	c := b.companionFor(id)
	if !portraitIDPattern.MatchString(c.PortraitID) {
		t.Fatalf("portrait was not applied: %+v", c)
	}
	entries, err := b.history.list(t.Context(), id)
	if err != nil || len(entries) == 0 || entries[len(entries)-1].Text != portraitReadyNotice {
		t.Fatalf("ready notice missing: %+v, %v", entries, err)
	}
}
