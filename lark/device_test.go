package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDeviceActions(t *testing.T) {
	var gotTap, gotMic []byte
	devSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" && r.Header.Get("X-Lark-Token") != "tok" {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch r.URL.Path {
		case "/status":
			w.Write([]byte(`{"board":"esp-box-3","tts_rms":100}`))
		case "/debug/tap":
			gotTap, _ = io.ReadAll(r.Body)
			w.Write([]byte(`{"ok":true}` + "\n"))
		case "/debug/mic":
			gotMic, _ = io.ReadAll(r.Body)
			w.Write([]byte(`{"ok":true}` + "\n"))
		case "/debug/screen":
			w.Header().Set("X-Width", "2")
			w.Header().Set("X-Height", "2")
			w.Write([]byte{0xf8, 0x00, 0x07, 0xe0, 0x00, 0x1f, 0xff, 0xff})
		default:
			http.NotFound(w, r)
		}
	}))
	defer devSrv.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/terminals/box3-b324/hits" {
			json.NewEncoder(w).Encode(object{
				"screen": "home",
				"hits":   []object{{"id": "menu", "label": "Menu", "x1": 10, "y1": 20, "x2": 30, "y2": 40}},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cli := &cliClient{base: srv.URL, token: "tok", http: srv.Client()}
	var out bytes.Buffer
	ctx := context.Background()

	if err := runDeviceAction(ctx, cli, devSrv.Client(), devSrv.URL, "tok", "box3-b324", "status", nil, &out); err != nil {
		t.Fatal(err)
	}
	if err := runDeviceAction(ctx, cli, devSrv.Client(), devSrv.URL, "tok", "box3-b324", "tap-label", []string{"Menu"}, &out); err != nil {
		t.Fatal(err)
	}
	if string(gotTap) != `{"x":20,"y":30}` {
		t.Fatalf("got tap %s", gotTap)
	}

	dir := t.TempDir()
	wavPath := filepath.Join(dir, "in.wav")
	if err := writeWAV16k(wavPath, []byte{1, 2, 3, 4}); err != nil {
		t.Fatal(err)
	}
	if err := runDeviceAction(ctx, cli, devSrv.Client(), devSrv.URL, "tok", "box3-b324", "mic", []string{wavPath}, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotMic, []byte{1, 2, 3, 4}) {
		t.Fatalf("got mic %x", gotMic)
	}

	pngPath := filepath.Join(dir, "screen.png")
	if err := runDeviceAction(ctx, cli, devSrv.Client(), devSrv.URL, "tok", "box3-b324", "screen", []string{pngPath}, &out); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(pngPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil || img.Bounds().Dx() != 2 || img.Bounds().Dy() != 2 {
		t.Fatalf("unexpected PNG: err=%v bounds=%v", err, img.Bounds())
	}
}
