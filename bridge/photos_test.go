package main

import (
	"bytes"
	"fmt"
	"image"
	imagecolor "image/color"
	"image/draw"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

func TestPhotoUploadDisplayAndScope(t *testing.T) {
	const hardware = "echoear-photo"
	virtual := virtualClient{ID: "virt-12345678", Character: "Mint", CreatedAt: time.Now()}
	b, base := wsHarness(t, func(b *bridge, _ string) {
		b.virtual = virtualStore(b.db)
		if err := b.virtual.save(virtual.ID, virtual, false); err != nil {
			t.Fatal(err)
		}
	})
	scope := httptest.NewServer(b.scoped(virtual, "scoped"))
	defer scope.Close()
	board := testConnectWS(t, base, "unit", false)
	board.hello(t, hardware, "Cake")
	web := testConnectWS(t, scope.URL, "scoped", false)
	web.hello(t, virtual.ID, virtual.Character)
	for i := range maxPhotos {
		if _, err := b.db.Exec(`INSERT INTO photos VALUES (?,?,'image/png',x'01','',?)`, fmt.Sprintf("photo-%08x", i), hardware, i); err != nil {
			t.Fatal(err)
		}
	}

	img := image.NewRGBA(image.Rect(0, 0, 800, 400))
	draw.Draw(img, img.Bounds(), image.NewUniform(imagecolor.RGBA{R: 255, A: 255}), image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(200, 0, 600, 400), image.NewUniform(imagecolor.RGBA{G: 255, A: 255}), image.Point{}, draw.Src)
	var raw bytes.Buffer
	if err := png.Encode(&raw, img); err != nil {
		t.Fatal(err)
	}
	do := func(method, url, token, media string, data []byte, status int) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, url, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Lark-Token", token)
		req.Header.Set("Content-Type", media)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != status {
			t.Fatalf("%s %s: got HTTP %d, want %d: %.200s: %v", method, url, res.StatusCode, status, body, err)
		}
		return body
	}
	path := "/api/devices/" + hardware + "/photo"
	do("POST", base+path, "", "image/png", raw.Bytes(), 401)
	do("POST", scope.URL+path, "scoped", "image/png", raw.Bytes(), 403)
	do("POST", base+path, "unit", "image/png", make([]byte, maxPhotoUpload+1), 413)
	saved := testObject(t, do("POST", base+path+"?caption=Hello", "unit", "image/png", raw.Bytes(), 200))
	id := str(saved["id"])
	if !photoID.MatchString(id) || saved["displayed"] != true || saved["caption"] != "Hello" {
		t.Fatal(saved)
	}
	rect, _, pixels := decodeDisplay(t, board.until(t, isDisplay))
	pix := func(x, y int) uint16 {
		t.Helper()
		if !image.Pt(x, y).In(rect) {
			t.Fatalf("%d,%d outside native photo frame %v", x, y, rect)
		}
		return pixels[(y-rect.Min.Y)*rect.Dx()+x-rect.Min.X]
	}
	if pix(90, 180) != 0x07e0 || pix(152, 313) == 0x07e0 {
		t.Fatal("expected green image below visible Cat pill")
	}
	board.json(t, object{"ev": "tap", "x": 180, "y": 316})
	board.until(t, isDisplay)
	eventually(t, func() bool {
		b.ws.mu.Lock()
		defer b.ws.mu.Unlock()
		return b.ws.devices[hardware].view == nil
	})

	download := "/api/photos/" + id
	do("GET", base+download, "", "", nil, 401)
	do("GET", scope.URL+download, "scoped", "", nil, 403)
	stored, err := png.Decode(bytes.NewReader(do("GET", base+download, "unit", "", nil, 200)))
	if err != nil || stored.Bounds().Size() != image.Pt(360, 360) {
		t.Fatalf("invalid normalized upload: %v", err)
	}
	if _, g, _, _ := stored.At(0, 180).RGBA(); g != 65535 {
		t.Fatal("photo was not center cropped")
	}
	history := apiObjects(t, base+"/api/devices/"+hardware+"/messages", "messages")
	if len(history) != 1 {
		t.Fatal(history)
	}
	card := history[0].(object)
	if card["card_title"] != "Photo" || card["text"] != "Hello" || card["role"] != "card" {
		t.Fatal(card)
	}

	var form bytes.Buffer
	multipart := multipart.NewWriter(&form)
	header := textproto.MIMEHeader{"Content-Disposition": {`form-data; name="photo"; filename="cat.png"`}, "Content-Type": {"image/png"}}
	part, err := multipart.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := multipart.WriteField("caption", "From the tunnel"); err != nil {
		t.Fatal(err)
	}
	if err := multipart.Close(); err != nil {
		t.Fatal(err)
	}
	own := testObject(t, do("POST", scope.URL+"/api/devices/"+virtual.ID+"/photo", "scoped", multipart.FormDataContentType(), form.Bytes(), 200))
	ownID := str(own["id"])
	if own["caption"] != "From the tunnel" || own["displayed"] != true {
		t.Fatal(own)
	}
	web.until(t, hasWS("svg", "photo:"+ownID))
	do("GET", scope.URL+"/api/photos/"+ownID, "scoped", "", nil, 200)
	do("GET", base+"/api/photos/photo-00000001", "unit", "", nil, 404)
	var photos int
	if err := b.db.QueryRow(`SELECT count(*) FROM photos`).Scan(&photos); err != nil || photos != maxPhotos {
		t.Fatalf("retained photos: %d, %v", photos, err)
	}
}

func TestSVGPhotosAreLocalAndRespectPainterOrder(t *testing.T) {
	const svg = `<svg><rect width="360" height="360" fill="red"/><g transform="translate(40 50)">
		<image href="photo:photo-12345678" width="200" height="200"/>
		<rect x="50" y="50" width="50" height="50" fill="blue"/>
		<image href="https://example.com/image.png" width="200" height="200"/>
		<image href="javascript:alert(1)" width="200" height="200"/>
	</g></svg>`
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	draw.Draw(img, img.Bounds(), image.NewUniform(imagecolor.RGBA{G: 255, A: 255}), image.Point{}, draw.Src)
	s, err := buildScreenWithPhotos(svg, true, roundScreen, func(id string) (image.Image, error) {
		if id != "photo-12345678" {
			t.Fatalf("nonlocal image lookup: %q", id)
		}
		return img, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s.source, "javascript") || strings.Contains(s.source, "example.com") || strings.Count(s.source, "<image ") != 1 {
		t.Fatal("nonlocal image survived sanitization", s.source)
	}
	for pt, want := range map[image.Point]uint16{{100, 90}: 0x07e0, {120, 125}: 0x001f, {260, 180}: 0xf800} {
		if s.fb[pt.Y*360+pt.X] != want {
			t.Errorf("pixel %v = %04x, want %04x", pt, s.fb[pt.Y*360+pt.X], want)
		}
	}
	if hitID(s.hits, 180, 315) != exitNode {
		t.Fatal("app exit pill missing")
	}
}
