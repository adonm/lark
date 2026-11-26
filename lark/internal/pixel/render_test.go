package pixel

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"slices"
	"testing"

	"github.com/pierrec/lz4/v4"
)

// Interpret the wire with the firmware contract, independently of the encoder.
func decodeDisplay(t *testing.T, p []byte) (image.Rectangle, uint16, []uint16) {
	t.Helper()
	if len(p) < 12 || p[0] != 0x03 {
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
				out = append(out, Pack565(uint8(cr>>8), uint8(cg>>8), uint8(cb>>8)))
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
	t.Parallel()
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
	if r := DirtyRect(previous, next, 360, 360); r != rect {
		t.Fatal(r)
	}
	if r := DirtyRect(next, next, 360, 360); !r.Empty() {
		t.Fatal(r)
	}
	if r := DirtyRect(nil, next, 360, 360); r != image.Rect(0, 0, 360, 360) {
		t.Fatal(r)
	}
	var enc FrameEncoder
	frame := enc.EncodeWithCodecs(next, nil, rect, 0x10303, 360, CodecBitAll)
	if got := frame[:12]; !slices.Equal(got, []byte{3, 18, 0, 20, 0, 6, 1, 2, 0, 1, 3, 3}) {
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
	frame = enc.EncodeWithCodecs(next, nil, image.Rect(0, 1, 129, 2), 1, 360, CodecBitAll)
	_, _, p = decodeDisplay(t, frame)
	if frame[9] != 0 || !slices.Equal(p, next[360:489]) || frame[12] != 0 || frame[13] != 1 {
		t.Fatal("incompressible pixels did not use raw big-endian fallback")
	}
	for i := range 300 {
		if i < 130 {
			next[i] = uint16(i)
		} else {
			next[i] = 0x1234
		}
	}
	frame = enc.EncodeWithCodecs(next, nil, image.Rect(0, 0, 300, 1), 19, 360, CodecBitAll)
	_, _, p = decodeDisplay(t, frame)
	if frame[9] != 1 || !slices.Equal(p, next[:300]) {
		t.Fatal("mixed runs did not round-trip")
	}
}

func TestFrameCodecsAndSmartSelector(t *testing.T) {
	t.Parallel()
	var enc FrameEncoder
	const w = 360
	fb := make([]uint16, w*w)

	for i := range 60 {
		fb[i] = uint16(i)*213 + 17
	}
	f := enc.EncodeWithCodecs(fb, nil, image.Rect(0, 0, 60, 1), 1, w, CodecBitAll)
	if f[9] != FrameCodecRaw {
		t.Fatalf("small incompressible rect chose codec %d, want Raw (0)", f[9])
	}
	if _, _, got := decodeDisplay(t, f); !slices.Equal(got, fb[:60]) {
		t.Fatal("Raw round-trip mismatch")
	}

	for y := range 128 {
		for x := range 128 {
			fb[y*w+x] = 0x1234
		}
	}
	f = enc.EncodeWithCodecs(fb, nil, image.Rect(0, 0, 128, 128), 2, w, CodecBitAll)
	if f[9] != FrameCodecRLE565 && f[9] != FrameCodecLZ4 {
		t.Fatalf("uniform rect chose codec %d, want lossless RLE565 or LZ4", f[9])
	}
	if _, _, got := decodeDisplay(t, f); len(got) != 128*128 || got[0] != 0x1234 {
		t.Fatal("uniform lossless round-trip mismatch")
	}

	for y := range 32 {
		for x := range 48 {
			fb[y*w+x] = uint16(x%8)*0x1111 + 0x0101
		}
	}
	f = enc.EncodeWithCodecs(fb, nil, image.Rect(0, 0, 48, 32), 3, w, CodecBitAll)
	if f[9] != FrameCodecLZ4 {
		t.Fatalf("repeating pattern chose codec %d, want LZ4 (3)", f[9])
	}
	_, _, got := decodeDisplay(t, f)
	for y := range 32 {
		if !slices.Equal(got[y*48:(y+1)*48], fb[y*w:y*w+48]) {
			t.Fatalf("LZ4 round-trip mismatch at row %d", y)
		}
	}

	var rng uint32 = 0x12345678
	for y := range 160 {
		for x := range 160 {
			rng = rng*1664525 + 1013904223
			noise := int((rng >> 28) & 0x0f)
			r8 := uint8(min(255, (x*240)/159+noise))
			g8 := uint8(min(255, (y*240)/159+noise))
			b8 := uint8(min(255, ((x+y)*120)/159+noise))
			fb[y*w+x] = Pack565(r8, g8, b8)
		}
	}
	f = enc.EncodeWithCodecs(fb, nil, image.Rect(0, 0, 160, 160), 4, w, CodecBitAll)
	if f[9] != FrameCodecJPEG {
		t.Fatalf("high-entropy 160x160 gradient chose codec %d, want JPEG (2)", f[9])
	}
	r, rev, decoded := decodeDisplay(t, f)
	if r != image.Rect(0, 0, 160, 160) || rev != 4 || len(decoded) != 160*160 {
		t.Fatalf("JPEG metadata mismatch: r=%v rev=%d len=%d", r, rev, len(decoded))
	}
}

func BenchmarkDirtyRect(b *testing.B) {
	prev := make([]uint16, 360*360)
	next := make([]uint16, 360*360)
	next[180*360+180] = 0xf800
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if r := DirtyRect(prev, next, 360, 360); r.Empty() {
			b.Fatal("empty")
		}
	}
}

func BenchmarkFrameEncoderRLE(b *testing.B) {
	var enc FrameEncoder
	fb := make([]uint16, 360*360)
	rect := image.Rect(0, 0, 360, 360)
	enc.EncodeWithCodecs(fb, nil, rect, 1, 360, CodecBitAll)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		_ = enc.EncodeWithCodecs(fb, nil, rect, uint64(i), 360, CodecBitAll)
	}
}

func BenchmarkFrameEncoderLZ4(b *testing.B) {
	var enc FrameEncoder
	fb := make([]uint16, 360*360)
	for y := range 360 {
		for x := range 360 {
			fb[y*360+x] = uint16(x%16) * 0x0841
		}
	}
	rect := image.Rect(0, 0, 360, 360)
	enc.EncodeWithCodecs(fb, nil, rect, 1, 360, CodecBitAll)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		_ = enc.EncodeWithCodecs(fb, nil, rect, uint64(i), 360, CodecBitAll)
	}
}

func BenchmarkFrameEncoderJPEG(b *testing.B) {
	var enc FrameEncoder
	fb := make([]uint16, 360*360)
	var rng uint32 = 0x12345678
	for y := range 160 {
		for x := range 160 {
			rng = rng*1664525 + 1013904223
			noise := int((rng >> 28) & 0x0f)
			fb[y*360+x] = Pack565(uint8(min(255, (x*240)/159+noise)), uint8(min(255, (y*240)/159+noise)), uint8(min(255, ((x+y)*120)/159+noise)))
		}
	}
	rect := image.Rect(0, 0, 160, 160)
	enc.EncodeWithCodecs(fb, nil, rect, 1, 360, CodecBitAll)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		_ = enc.EncodeWithCodecs(fb, nil, rect, uint64(i), 360, CodecBitAll)
	}
}
