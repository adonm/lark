package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	imagecolor "image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const syntheticHEIC = "AAAAHGZ0eXBoZWljAAAAAG1pZjFoZWljbWlhZgAAAX1tZXRhAAAAAAAAACFoZGxyAAAAAAAAAABwaWN0AAAAAAAAAAAAAAAAAAAAACJpbG9jAAAAAERAAAEAAQAAAAABoQABAAAAAAAAAGsAAAAjaWluZgAAAAAAAQAAABVpbmZlAgAAAAABAABodmMxAAAAAA5waXRtAAAAAAABAAAA/WlwcnAAAADdaXBjbwAAAHZodmNDAQNwAAAAkAAAAAAAHvAA/P34+AAADwNgAAEAGEABDAH//wNwAAADAJAAAAMAAAMAHroCQGEAAQAqQgEBA3AAAAMAkAAAAwAAAwAeoCCBBZbq5Ka5uAhoMCAAAAMDIAAAAwAhYgABAAZEAcFzwIkAAAATY29scm5jbHgAAQANAAaAAAAAFGlzcGUAAAAAAAAAQAAAAEAAAAAoY2xhcAAAAEAAAAABAAAAIAAAAAEAAAAAAAAAAv///+AAAAACAAAAEHBpeGkAAAAAAwgICAAAABhpcG1hAAAAAAAAAAEAAQWBAgMFhAAAAHNtZGF0AAAAZygBrwS4bAU5bP/mXf///evfkP6yfbqAv4hWJf8Kbv//uRUnmYNdrB19Bs9CiHUfl+dwowSFXfovTWS1Hvfwm9yOS1SbV4rdYDg9Xn2ya0/Z8/6SBfr8CZdKYAdoPeRKyMAQud/XTYA="

// Both are the same generated 64x32 red/blue bars. The second sets HEIF's
// primary irot in addition to the EXIF tag, so applying both would be visible.
const syntheticHEICExif = "AAAAHGZ0eXBoZWljAAAAAG1pZjFoZWljbWlhZgAAAb5tZXRhAAAAAAAAACFoZGxyAAAAAAAAAABwaWN0AAAAAAAAAAAAAAAAAAAAADRpbG9jAAAAAERAAAIAAQAAAAAB4gABAAAAAAAAAGsAAgAAAAACTQABAAAAAAAAACQAAAA4aWluZgAAAAAAAgAAABVpbmZlAgAAAAABAABodmMxAAAAABVpbmZlAgAAAQACAABFeGlmAAAAAA5waXRtAAAAAAABAAAA/WlwcnAAAADdaXBjbwAAAHZodmNDAQNwAAAAkAAAAAAAHvAA/P34+AAADwNgAAEAGEABDAH//wNwAAADAJAAAAMAAAMAHroCQGEAAQAqQgEBA3AAAAMAkAAAAwAAAwAeoCCBBZbq5Ka5uAhoMCAAAAMDIAAAAwAhYgABAAZEAcFzwIkAAAATY29scm5jbHgAAQANAAaAAAAAFGlzcGUAAAAAAAAAQAAAAEAAAAAoY2xhcAAAAEAAAAABAAAAIAAAAAEAAAAAAAAAAv///+AAAAACAAAAEHBpeGkAAAAAAwgICAAAABhpcG1hAAAAAAAAAAEAAQWBAgMFhAAAABppcmVmAAAAAAAAAA5jZHNjAAIAAQABAAAAl21kYXQAAABnKAGvBLhsBTls/+Zd///969+Q/rJ9uoC/iFYl/wpu//+5FSeZg12sHX0Gz0KIdR+X53CjBIVd+i9NZLUe9/Cb3I5LVJtXit1gOD1efbJrT9nz/pIF+vwJl0pgB2g95ErIwBC539dNgAAAAAZFeGlmAABNTQAqAAAACAABARIAAwAAAAEABgAAAAAAAA=="
const syntheticHEICIRot = "AAAAHGZ0eXBoZWljAAAAAG1pZjFoZWljbWlhZgAAAchtZXRhAAAAAAAAACFoZGxyAAAAAAAAAABwaWN0AAAAAAAAAAAAAAAAAAAAADRpbG9jAAAAAERAAAIAAQAAAAAB7AABAAAAAAAAAGsAAgAAAAACVwABAAAAAAAAACQAAAA4aWluZgAAAAAAAgAAABVpbmZlAgAAAAABAABodmMxAAAAABVpbmZlAgAAAQACAABFeGlmAAAAAA5waXRtAAAAAAABAAABB2lwcnAAAADmaXBjbwAAAHZodmNDAQNwAAAAkAAAAAAAHvAA/P34+AAADwNgAAEAGEABDAH//wNwAAADAJAAAAMAAAMAHroCQGEAAQAqQgEBA3AAAAMAkAAAAwAAAwAeoCCBBZbq5Ka5uAhoMCAAAAMDIAAAAwAhYgABAAZEAcFzwIkAAAATY29scm5jbHgAAQANAAaAAAAAFGlzcGUAAAAAAAAAQAAAAEAAAAAoY2xhcAAAAEAAAAABAAAAIAAAAAEAAAAAAAAAAv///+AAAAACAAAAEHBpeGkAAAAAAwgICAAAAAlpcm90AwAAABlpcG1hAAAAAAAAAAEAAQaBAgMFhAYAAAAaaXJlZgAAAAAAAAAOY2RzYwACAAEAAQAAAJdtZGF0AAAAZygBrwS4bAU5bP/mXf///evfkP6yfbqAv4hWJf8Kbv//uRUnmYNdrB19Bs9CiHUfl+dwowSFXfovTWS1Hvfwm9yOS1SbV4rdYDg9Xn2ya0/Z8/6SBfr8CZdKYAdoPeRKyMAQud/XTYAAAAAGRXhpZgAATU0AKgAAAAgAAQESAAMAAAABAAYAAAAAAAA="

func testJPEG(t testing.TB, c imagecolor.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 120, 72))
	draw.Draw(img, img.Bounds(), image.NewUniform(c), image.Point{}, draw.Src)
	var data bytes.Buffer
	if err := jpeg.Encode(&data, img, nil); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestLocalPhotoFormats(t *testing.T) {
	dir := t.TempDir()
	jpg := testJPEG(t, imagecolor.RGBA{R: 220, A: 255})
	exif6 := []byte("\xff\xe1\x00\x22Exif\x00\x00II\x2a\x00\x08\x00\x00\x00\x01\x00\x12\x01\x03\x00\x01\x00\x00\x00\x06\x00\x00\x00\x00\x00\x00\x00")
	rot := append(append(slices.Clone(jpg[:2]), exif6...), jpg[2:]...)
	path := filepath.Join(dir, "jpeg")
	if err := os.WriteFile(path, rot, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := decodePhotoOriginal(path, "image")
	if err != nil || bytes.Contains(out, []byte("Exif")) {
		t.Fatal("orientation derivative failed:", err)
	}
	checkSize := func(out []byte, w, h int) {
		t.Helper()
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(out))
		if err != nil || cfg.Width != w || cfg.Height != h {
			t.Fatalf("JPEG derivative = %dx%d, expected %dx%d: %v", cfg.Width, cfg.Height, w, h, err)
		}
	}
	checkSize(out, 72, 120)
	heic, err := base64.StdEncoding.DecodeString(syntheticHEIC)
	if err != nil || os.WriteFile(path, heic, 0o600) != nil {
		t.Fatal("could not create synthetic HEIC")
	}
	out, err = decodePhotoOriginal(path, "image")
	if err != nil {
		t.Fatal(err)
	}
	checkSize(out, 64, 32)
	for _, fixture := range []string{syntheticHEICExif, syntheticHEICIRot} {
		raw, err := base64.StdEncoding.DecodeString(fixture)
		if err != nil || os.WriteFile(path, raw, 0o600) != nil {
			t.Fatal("could not create rotated HEIC")
		}
		out, err := decodePhotoOriginal(path, "image")
		if err != nil {
			t.Fatal(err)
		}
		checkSize(out, 32, 64)
		oriented, err := jpeg.Decode(bytes.NewReader(out))
		if err != nil {
			t.Fatal(err)
		}
		top, _, blue, _ := oriented.At(12, 12).RGBA()
		if top <= blue {
			t.Fatal("HEIC was oriented in the wrong direction")
		}
	}
	var archive bytes.Buffer
	z := zip.NewWriter(&archive)
	video, _ := z.Create("live.mov")
	video.Write([]byte("ignored"))
	still, _ := z.Create("live.HEIC")
	still.Write(heic)
	if err := z.Close(); err != nil || os.WriteFile(path, archive.Bytes(), 0o600) != nil {
		t.Fatal("could not create synthetic Live Photo:", err)
	}
	out, err = decodePhotoOriginal(path, "livephoto")
	if err != nil {
		t.Fatal(err)
	}
	checkSize(out, 64, 32)
}

func BenchmarkPhotoDerivative12MP(b *testing.B) {
	img := image.NewRGBA(image.Rect(0, 0, 4000, 3000))
	for y := range 3000 {
		for x := range 4000 {
			img.SetRGBA(x, y, imagecolor.RGBA{R: uint8(x/6 + y/3), G: uint8(x*y/611 + x/19), B: uint8(x/4 - y/13), A: 255})
		}
	}
	path := filepath.Join(b.TempDir(), "synthetic.jpg")
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 90}); err != nil {
		f.Close()
		b.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(fi.Size())
	var output []byte
	for b.Loop() {
		output, err = decodePhotoOriginal(path, "image")
		if err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(output)), "JPEG-B")
}

func testLibrary(t *testing.T) *photoLibrary {
	t.Helper()
	p, err := openPhotoLibrary(t.Context(), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.close)
	return p
}

func fakeEnteSnapshot() enteSnapshot {
	return enteSnapshot{
		Account: "101",
		Albums:  []enteAlbum{{"1", "Beach trip", "album"}, {"2", "Stars", "favorites"}},
		Files: []enteFile{
			{ID: "31", Name: "beach.jpg", Type: "image", CreatedAt: "2024-07-01T14:00:00Z", Hash: "A", AlbumIDs: []string{"1", "2"}, Caption: "50.0001,-20.0002 /home/private/beach.jpg", Location: json.RawMessage(`{"latitude":50.0001,"longitude":-20.0002}`)},
			{ID: "32", Name: "camera.jpg", Type: "image", CreatedAt: "2024-08-01T14:00:00Z", Hash: "B", AlbumIDs: []string{"1"}},
			{ID: "33", Name: "not a still.mov", Type: "video", Hash: "C", AlbumIDs: []string{"1"}},
		},
		Places: json.RawMessage(`[{"name":"Example town","fileCount":2}]`),
	}
}

func ingestTestPhotos(t *testing.T, p *photoLibrary, s enteSnapshot, stamp time.Time) {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(p.data, "ente-photos-ready.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := p.syncSnapshot(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestPhotoLibraryReconcile(t *testing.T) {
	ctx := t.Context()
	p := testLibrary(t)
	now, s := time.Now(), fakeEnteSnapshot()
	ingestTestPhotos(t, p, s, now)
	out, err := p.search(ctx, "beach", "", 8)
	if err != nil || len(out) != 2 || out[0].id != "31" {
		t.Fatalf("expected title before album match: %d, %v", len(out), err)
	}
	var memberships int
	if err := p.db.QueryRow(`SELECT count(*) FROM library_membership`).Scan(&memberships); err != nil || memberships != 3 {
		t.Fatalf("multi-album membership = %d: %v", memberships, err)
	}
	if err := p.setLabels(ctx, out[1].photoKey, []string{"grandma"}); err != nil {
		t.Fatal(err)
	}
	labels, err := p.search(ctx, "beach", "Grandma", 8)
	if err != nil || len(labels) != 1 || labels[0].id != "32" {
		t.Fatalf("manual people filter: %v", err)
	}
	ids := p.newPhotoHandles(ctx, "lark-test", "", out, now)
	data := testJPEG(t, imagecolor.Black)
	if err := p.storeDerivative(ctx, out[0].photoKey, data); err != nil {
		t.Fatal(err)
	}
	if err := p.storeDerivative(ctx, out[1].photoKey, data); err != nil {
		t.Fatal(err)
	}
	s.Files = s.Files[1:2]
	s.Files[0].Caption = "metadata edited"
	ingestTestPhotos(t, p, s, now.Add(time.Second))
	if _, ok := p.getPhotoHandle(ctx, "lark-test", ids[0]); ok {
		t.Fatal("deleted photo retained its handle")
	}
	if _, ok := p.getPhotoHandle(ctx, "lark-test", ids[1]); !ok {
		t.Fatal("caption edit revoked unchanged photo")
	}
	if gone, err := p.derivative(ctx, out[0].photoKey); err != nil || len(gone) != 0 {
		t.Fatalf("deleted photo retained its derivative: %v", err)
	}
	if kept, err := p.derivative(ctx, out[1].photoKey); err != nil || len(kept) == 0 {
		t.Fatalf("caption edit invalidated unchanged derivative: %v", err)
	}
	s.Files[0].Hash = "new pixels"
	ingestTestPhotos(t, p, s, now.Add(2*time.Second))
	if _, ok := p.getPhotoHandle(ctx, "lark-test", ids[1]); ok {
		t.Fatal("changed original retained its handle")
	}
	ingestTestPhotos(t, p, fakeEnteSnapshot(), now.Add(3*time.Second))
	if _, ok := p.getPhotoHandle(ctx, "lark-test", ids[0]); ok {
		t.Fatal("restored photo revived a revoked handle")
	}
	p.fresh.Store(now.Add(-photoFreshFor - time.Second).UnixNano())
	if p.allowed(ctx, "lark-test") {
		t.Fatal("stale sync served photos")
	}
}

func TestPhotoLibrarySlideshowAndScope(t *testing.T) {
	const hardware = "lark-libtest"
	const virtual = "virt-12345678"
	p := testLibrary(t)
	now := time.Now()
	s := fakeEnteSnapshot()
	ingestTestPhotos(t, p, s, now)
	var fetched atomic.Int32
	jpg := testJPEG(t, imagecolor.RGBA{G: 235, A: 255})
	p.downloadOriginal = func(_ context.Context, _, _, dest string) error {
		fetched.Add(1)
		return os.WriteFile(dest, jpg, 0o600)
	}
	b, base := wsHarness(t, func(b *bridge, _ string) { b.library = p })
	board := testConnectWS(t, base, "unit", false)
	board.hello(t, hardware, "Cake")
	mirror := testConnectWS(t, base, "unit", true)
	mirror.watch(t, hardware)
	access, release := p.newPhotoGrant(t.Context(), hardware)
	defer release()
	search := object{"device_id": hardware, "photos": object{"access": access, "query": "beach"}}
	first := toolTest(t, base, "state", search, false)
	encoded, _ := json.Marshal(first["photos"])
	if strings.Contains(string(encoded), "beach.jpg") || strings.Contains(string(encoded), "50.0001") || strings.Contains(string(encoded), "/home/") {
		t.Fatal("private metadata reached MCP")
	}
	results := first["photos"].(object)["items"].([]any)
	ids := []string{str(results[0].(object)["handle"]), str(results[1].(object)["handle"])}
	if _, err := b.callMCP(withoutPhotoAccess(t.Context()), "state", search); err == nil {
		t.Fatal("scoped context used library grant")
	}
	if access, _ := p.newPhotoGrant(withoutPhotoAccess(t.Context()), hardware); access != "" {
		t.Fatal("scoped turn received a library grant")
	}
	scoped := httptest.NewServer(b.scoped(virtualClient{ID: virtual}, "scoped"))
	defer scoped.Close()
	verifyHTTP := func(method, base, path, token, body string, status int) {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
		req.Header.Set("X-Lark-Token", token)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		_, _ = io.Copy(io.Discard, res.Body)
		if res.StatusCode != status {
			t.Fatalf("%s got %d, want %d", path, res.StatusCode, status)
		}
	}
	thumb := "/api/library/thumbnail/" + ids[0]
	verifyHTTP("GET", scoped.URL, thumb, "scoped", "", 404)
	verifyHTTP("GET", scoped.URL, "/app/photo-library.js", "scoped", "", 404)
	verifyHTTP("GET", scoped.URL, "/api/library?device_id="+virtual, "scoped", "", 404)
	verifyHTTP("POST", scoped.URL, "/mcp", "scoped", "{}", 404)
	args := object{"devices": hardware, "template": "slideshow", "data": object{"handles": ids, "access": access}}
	toolTest(t, base, "app", args, false)
	mirror.until(t, hasWS("svg", "photo:"+ids[0]))
	verifyHTTP("GET", base, thumb, "unit", "", 200)
	if fetched.Load() != 1 {
		t.Fatal("opening one image fetched more than one original")
	}
	if files, _ := os.ReadDir(filepath.Join(p.data, ".photo-downloads")); len(files) != 0 {
		t.Fatal("downloaded original not removed")
	}
	slide, _ := p.getPhotoHandle(t.Context(), hardware, ids[0])
	good := &photoShow{handles: ids}
	v := slideshowScene(good, "")
	for _, geometry := range []screenGeometry{roundScreen, {320, 240, "rect"}} {
		view, err := p.slideScreen(t.Context(), hardware, v.library, v.svg(""), geometry)
		if err != nil || len(view.fb) != geometry.width*geometry.height ||
			len(appLayoutWarnings([]svgScreen{view}, geometry == roundScreen)) != 0 {
			t.Fatalf("invalid slideshow at %dx%d: %v %v", geometry.width, geometry.height, view.hitWarnings, err)
		}
		if view.fb[len(view.fb)/2+geometry.width/2] == 0 {
			t.Fatal("missing slideshow pixels")
		}
	}
	mirror.json(t, object{"ev": "tap", "id": "photo-next"})
	mirror.until(t, hasWS("svg", "photo:"+ids[1]))
	if _, ok := p.getPhotoHandle(t.Context(), virtual, ids[1]); ok {
		t.Fatal("cross-cat handle accepted")
	}
	p.mu.Lock()
	slide.until = now.Add(-time.Second)
	p.handles[ids[0]] = slide
	p.mu.Unlock()
	if _, ok := p.getPhotoHandle(t.Context(), hardware, ids[0]); ok {
		t.Fatal("expired handle accepted")
	}
	// Start a new deck then hide it; the API must clear the photo already on screen.
	requery := toolTest(t, base, "state", search, false)["photos"].(object)["items"].([]any)
	recent := str(requery[0].(object)["handle"])
	toolTest(t, base, "app", object{"devices": hardware, "template": "slideshow",
		"data": object{"handles": []string{recent}, "access": access}}, false)
	mirror.until(t, hasWS("svg", "photo:"+recent))
	verifyHTTP("POST", base, "/api/library?device_id="+hardware, "unit", `{"op":"visibility","hidden":true}`, 200)
	mirror.until(t, hasWS("svg", "companion-ring"))
	verifyHTTP("GET", base, "/api/library/thumbnail/"+recent, "unit", "", 404)
	toolTest(t, base, "state", search, true)
	verifyHTTP("POST", base, "/api/library?device_id="+hardware, "unit", `{"op":"visibility","hidden":false}`, 200)
	verifyHTTP("GET", base, "/api/library/thumbnail/"+recent, "unit", "", 404)
	toolTest(t, base, "state", search, true) // Re-enabling does not revive a prior turn's grant.
	access2, release2 := p.newPhotoGrant(t.Context(), hardware)
	defer release2()
	search["photos"].(object)["access"] = access2
	result := toolTest(t, base, "state", search, false)["photos"].(object)["items"].([]any)
	removed := str(result[0].(object)["handle"])
	toolTest(t, base, "app", object{"devices": hardware, "template": "slideshow",
		"data": object{"handles": []string{removed}, "access": access2}}, false)
	mirror.until(t, hasWS("svg", "photo:"+removed))
	s.Files = s.Files[1:2]
	ingestTestPhotos(t, p, s, now.Add(time.Second))
	b.ws.clearInvalidSlides()
	mirror.until(t, hasWS("svg", "companion-ring"))
	verifyHTTP("GET", base, "/api/library/thumbnail/"+removed, "unit", "", 404)
}

func TestPhotoDerivativeBudget(t *testing.T) {
	p := testLibrary(t)
	ingestTestPhotos(t, p, fakeEnteSnapshot(), time.Now())
	ctx := t.Context()
	files, err := p.search(ctx, "", "", 8)
	if err != nil || len(files) != 2 {
		t.Fatal("missing fixture photos:", err)
	}
	data := testJPEG(t, imagecolor.Black)
	p.cacheBudget = int64(len(data))
	for _, f := range files {
		if err := p.storeDerivative(ctx, f.photoKey, data); err != nil {
			t.Fatal(err)
		}
	}
	if old, err := p.derivative(ctx, files[0].photoKey); err != nil || old != nil {
		t.Fatal("derivative budget did not evict its oldest bytes:", err)
	}
	if recent, err := p.derivative(ctx, files[1].photoKey); err != nil || !bytes.Equal(recent, data) {
		t.Fatal("derivative budget evicted the most recent image:", err)
	}
}

func TestEnteMetadataJobPublishesOnlyCompleteSnapshots(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "synthetic-key")
	cli := filepath.Join(dir, "bin", "ente")
	if err := os.WriteFile(key, []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := `#!/bin/bash
case "$1 $2 $3" in
  'account view --account') printf '{"id":"9","name":"personal","host":"https://api.ente.com","apps":["photos"]}\n' ;;
  'photos album list') printf '[]\n' ;;
  'photos file list') if [ "$STUB_FAIL" = 1 ]; then printf '[]\n'; exit 1; fi; printf '[]\n' ;;
  'photos place list') printf '[]\n' ;;
  'photos export '*) printf '{"output":"%s","complete":true,"files":0,"copies":{"expected":0,"completed":0,"pending":0},"failures":{"total":0,"conflicts":0}}\n' "$3" ;;
  *) exit 2 ;;
esac`
	if err := os.WriteFile(cli, []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(mode string, fail bool) error {
		cmd := exec.CommandContext(t.Context(), "bash", "../scripts/ente-export-job.sh", mode, dir, key)
		if fail {
			cmd.Env = append(os.Environ(), "STUB_FAIL=1")
		}
		return cmd.Run()
	}
	if err := run("sync", false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ente-photos-ready.json")
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatal("snapshot not private and present:", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ente-export")); !os.IsNotExist(err) {
		t.Fatal("metadata-only run made an originals export")
	}
	if err := run("sync", true); err == nil {
		t.Fatal("incomplete list accepted")
	}
	after, err := os.Stat(path)
	if err != nil || !after.ModTime().Equal(fi.ModTime()) {
		t.Fatal("failed run rewrote the ready snapshot")
	}
	if err := run("sync-export", false); err != nil {
		t.Fatal("optional full export failed:", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ente-export-ready.json")); err != nil {
		t.Fatal(err)
	}
}
