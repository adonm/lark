package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"lark/internal/pixel"
)

func TestSimDecodeFrames(t *testing.T) {
	t.Parallel()
	const w, h = 64, 64
	src := make([]uint16, w*h)
	for y := range h {
		for x := range w {
			src[y*w+x] = pixel.Pack565(uint8(x*4), uint8(y*4), 128)
		}
	}
	r := image.Rect(0, 0, w, h)
	var enc pixel.FrameEncoder
	for _, tc := range []struct {
		name  string
		codec uint8
	}{
		{"raw", codecBitRaw},
		{"rle565", codecBitRLE565},
		{"lz4", codecBitLZ4},
		{"jpeg", codecBitJPEG},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := enc.EncodeWithCodecs(src, nil, r, 7, w, tc.codec)
			dst := make([]uint16, w*h)
			rev, ok := decodeFrameInto(dst, w, h, f)
			if !ok || rev != 7 {
				t.Fatalf("decodeFrameInto(%s) failed: ok=%v rev=%d", tc.name, ok, rev)
			}
			if tc.codec != codecBitJPEG {
				for i := range src {
					if dst[i] != src[i] {
						t.Fatalf("pixel %d mismatch: got %#x want %#x", i, dst[i], src[i])
					}
				}
			}
		})
	}
}

func TestSimPNGAndWAV(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fb := []uint16{pixel.Pack565(255, 0, 0), pixel.Pack565(0, 255, 0), pixel.Pack565(0, 0, 255), pixel.Pack565(255, 255, 255)}
	pngPath := filepath.Join(dir, "out.png")
	if err := writeFBToPNG(fb, 2, 2, pngPath); err != nil {
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

	wavPath := filepath.Join(dir, "out.wav")
	pcm := []byte{1, 2, 3, 4, 5, 6}
	if err := writeWAV16k(wavPath, pcm); err != nil {
		t.Fatal(err)
	}
	got, err := readWAVPCM16k(wavPath)
	if err != nil || !bytes.Equal(got, pcm) {
		t.Fatalf("WAV round-trip mismatch: err=%v got=%x", err, got)
	}
}

func TestResolveHitLabel(t *testing.T) {
	t.Parallel()
	res := object{
		"screen": "home",
		"hits": []any{
			object{"id": "menu", "label": "Menu", "x1": json.Number("10"), "y1": json.Number("20"), "x2": json.Number("30"), "y2": json.Number("40")},
		},
	}
	x, y, matched, err := resolveHitLabel(res, "menu")
	if err != nil || x != 20 || y != 30 || matched != "Menu" {
		t.Fatalf("got (%d,%d,%q,%v)", x, y, matched, err)
	}
	if _, _, _, err := resolveHitLabel(res, "missing"); err == nil {
		t.Fatal("expected error for missing label")
	}
}
