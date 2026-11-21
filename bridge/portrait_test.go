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
	"testing"
)

func portraitFixture(t *testing.T) []byte {
	t.Helper()
	bg, _ := directPaint("#FBEFE6")
	fur, _ := directPaint("#D4B5DE")
	ink, _ := directPaint("#583E62")
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
	raw := portraitFixture(t)
	sheet, err := decodePortraitSheet(raw)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := directPaint("#FFF2E7")
	poses, err := slicePortrait(sheet, p)
	if err != nil {
		t.Fatal(err)
	}
	var painted image.Rectangle
	for i, im := range poses {
		b := portraitBounds(im, im.Bounds(), p)
		if i == 0 {
			painted = b
		}
		center := b.Min.Add(b.Max)
		if b != painted || center.X < 359 || center.X > 361 || center.Y < 323 || center.Y > 325 {
			t.Fatalf("%s drifted: got %v, idle %v", portraitPoses[i], b, painted)
		}
		if i != 0 && bytes.Equal(im.Pix, poses[0].Pix) {
			t.Fatalf("%s did not preserve its expression", portraitPoses[i])
		}
	}
	if _, err := slicePortrait(image.NewUniform(imagecolor.White), p); err == nil {
		t.Fatal("accepted wrong sheet dimensions")
	}
	if _, err := slicePortrait(image.NewRGBA(image.Rect(0, 0, 1024, 1024)), p); err == nil {
		t.Fatal("accepted an empty sheet")
	}
}

func TestPortraitSilhouettesClearMenuAndBezel(t *testing.T) {
	bg, _ := directPaint("#FFF2E7")
	for _, shape := range []image.Rectangle{image.Rect(100, 50, 410, 460), image.Rect(22, 106, 490, 406)} {
		sheet := image.NewRGBA(image.Rect(0, 0, 1024, 1024))
		draw.Draw(sheet, sheet.Bounds(), image.NewUniform(bg), image.Point{}, draw.Src)
		for i := range portraitPoses {
			offset := image.Pt(i%2*512+i*3, i/2*512-i*4)
			draw.Draw(sheet, shape.Add(offset), image.NewUniform(imagecolor.Black), image.Point{}, draw.Src)
		}
		poses, err := slicePortrait(sheet, bg)
		if err != nil {
			t.Fatal(err)
		}
		menu := image.Rect(120, 292, 240, 340)
		for i, pose := range poses {
			b := portraitBounds(pose, pose.Bounds(), bg)
			if b.Empty() || b.Overlaps(menu) || b.Min.Y < 42 || b.Max.Y > 284 {
				t.Fatalf("%s: portrait %v outside safe vertical framing", portraitPoses[i], b)
			}
			if scaleX, scaleY := float64(b.Dx())/float64(shape.Dx()), float64(b.Dy())/float64(shape.Dy()); math.Abs(scaleX-scaleY) > 0.01 {
				t.Fatalf("%s: portrait %v distorted from %v", portraitPoses[i], b, shape)
			}
			for _, x := range []int{b.Min.X, b.Max.X} {
				for _, y := range []int{b.Min.Y, b.Max.Y} {
					if (x-180)*(x-180)+(y-180)*(y-180) >= 174*174 {
						t.Fatalf("%s: portrait %v would be clipped by the bezel", portraitPoses[i], b)
					}
				}
			}
		}
	}
}

func TestPixelPortraitGridAndAlignment(t *testing.T) {
	fixture := portraitFixture(t)
	store := &portraitStore{dir: t.TempDir(), generate: func(_ context.Context, prompt string, _ []byte) ([]byte, error) {
		for _, text := range []string{"indie-game pixel art", "64×64", "hue-shifted colored outlines", "no text, labels, borders or gridlines", "solid flat #FFF2E7"} {
			if !strings.Contains(prompt, text) {
				t.Errorf("missing %q in the pixel prompt", text)
			}
		}
		return fixture, nil
	}}
	const side, grid = 360, 64
	pending, err := store.prepare(t.Context(), companionState{}, object{"prompt": "a hedgehog", "bg": "#FFF2E7", "style": "pixel"})
	if err != nil {
		t.Fatal(err)
	}
	bg, _ := directPaint(pending.bg)
	var idle []byte
	var bounds image.Rectangle
	for i, pose := range portraitPoses {
		data, err := store.read(pending.id, pose)
		if err != nil {
			t.Fatal(err)
		}
		im, err := png.Decode(bytes.NewReader(data))
		if err != nil || im.Bounds().Size() != image.Pt(side, side) {
			t.Fatalf("%s: expected 360×360 PNG: %v", pose, err)
		}
		// Nearest-neighbor picks the source block at each destination pixel center.
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
				if portraitDiff(c, bg) > 25*25 {
					n, sx, sy = n+1, sx+x, sy+y
				}
			}
		}
		b := portraitBounds(rgba, rgba.Bounds(), bg)
		if i == 0 {
			bounds, idle = b, rgba.Pix
			scene, err := prepareScene(pending.svg(), false)
			if err != nil || !strings.Contains(scene.svg, `image-rendering="pixelated"`) {
				t.Fatalf("missing pixelated SVG hint: %v", err)
			}
			box, err := scene.raster(320, 240, 40, 0, 240, bg, func(string) (image.Image, error) { return rgba, nil })
			if err != nil {
				t.Fatal(err)
			}
			colors := map[imagecolor.RGBA]bool{}
			for _, row := range cells {
				for _, c := range row {
					colors[c] = true
				}
			}
			for y := range 240 {
				for x := 40; x < 280; x++ {
					if !colors[box.RGBAAt(x, y)] {
						t.Fatalf("native renderer smoothed pixel at %d,%d", x, y)
					}
				}
			}
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

func TestPortraitPatchScopeAndNativeFrames(t *testing.T) {
	sheet := portraitFixture(t)
	dir := t.TempDir()
	b, base := wsHarness(t, func(b *bridge, _ string) {
		b.virtual = virtualStore(b.db)
	})
	own := testObject(t, testRequest(t, "POST", base+"/api/virtual-clients", "unit", object{}, 201))
	other := testObject(t, testRequest(t, "POST", base+"/api/virtual-clients", "unit", object{}, 201))
	id := str(own["id"])
	patch := func(spec object, failed bool) object {
		t.Helper()
		out := toolTest(t, base, "patch", object{"device_id": id, "companion": object{"portrait": spec, "save_profile": false}}, failed)
		companion, _ := out["companion"].(object)
		return companion
	}
	patch(object{"prompt": "draw an owl"}, true) // No configured generator or paid fallback.
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
	badStyle := toolTest(t, base, "patch", object{"device_id": id, "companion": object{
		"portrait": object{"prompt": "an owl", "style": "vector"},
	}}, true)
	if got := str(badStyle["error"]); got != "companion.portrait.style: expected one of [storybook pixel]" {
		t.Fatalf("invalid MCP style: %q", got)
	}
	c := patch(object{"prompt": "draw an owl", "bg": "#eaf3fc"}, false)
	raw := str(c["face_svg"])
	asset := activePortrait(raw)
	if !portraitIDPattern.MatchString(asset) || len(raw) > 1600 || c["face_bg"] != "#EAF3FC" {
		t.Fatal(c)
	}
	for _, pose := range append(slices.Clone(portraitPoses[:]), "sheet") {
		fi, err := os.Stat(filepath.Join(dir, asset, pose+".png"))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s not private: %v", pose, err)
		}
	}
	s1 := httptest.NewServer(b.scoped(virtualClient{ID: id}, "on-device"))
	defer s1.Close()
	s2 := httptest.NewServer(b.scoped(virtualClient{ID: str(other["id"])}, "other-device"))
	defer s2.Close()
	path := "/api/portraits/" + asset + "/idle.png"
	testRequest(t, "GET", s1.URL+path, "", nil, 401)
	testRequest(t, "GET", s2.URL+path, "other-device", nil, 404)
	testRequest(t, "GET", base+"/api/portraits/"+asset+"/sheet.png", "unit", nil, 404)
	out := testRequest(t, "GET", s1.URL+path, "on-device", nil, 200)
	if config, err := png.DecodeConfig(bytes.NewReader(out)); err != nil || config.Width != 360 || config.Height != 360 {
		t.Fatalf("expected aligned pose PNG: %v", err)
	}
	for _, pair := range []struct {
		mood, pose string
		blink      bool
	}{{"idle", "idle", false}, {"idle", "blink", true}, {"speaking", "talk", false}, {"thinking", "think", false},
		{"listening", "idle", false}, {"sleepy", "blink", false}} {
		scene := companionSVG(companionState{FaceSVG: raw, companionLook: companionLook{FaceBG: "#EAF3FC"}}, companionPose{mood: pair.mood, blink: pair.blink, battery: -1})
		ref := `href="portrait:` + asset + ":" + pair.pose + `"`
		if strings.Count(scene, `href="portrait:`) != 1 || !strings.Contains(scene, ref) {
			t.Fatalf("wrong asset for %q", pair.mood)
		}
		day, err := b.portraits.frames.buildGradedWithAssets(scene, false, roundScreen, gradeDay, b.portraits.image)
		if err != nil || day.fb[180*360+180] == day.fb[180*360+50] {
			t.Fatalf("portrait did not render: %v", err)
		}
		night, err := b.portraits.frames.buildGradedWithAssets(scene, false, roundScreen, gradeNight, b.portraits.image)
		if err != nil || night.grade != gradeNight || night.fb[180*360+180] == day.fb[180*360+180] {
			t.Fatalf("portrait was not graded: %v", err)
		}
	}
	edited := patch(object{"prompt": "add a scarf", "edit": true}, false)
	if newAsset := activePortrait(str(edited["face_svg"])); newAsset == "" || asset == newAsset || edited["face_bg"] != c["face_bg"] {
		t.Fatal(edited)
	}
	testRequest(t, "GET", s1.URL+path, "on-device", nil, 404)
	toolTest(t, base, "patch", object{"device_id": str(other["id"]), "companion": object{"face_svg": raw}}, true)
}
