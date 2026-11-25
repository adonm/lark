package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"testing"

	"github.com/pierrec/lz4/v4"

	"lark/internal/pixel"
)

// decodeDisplay interprets the display wire with the firmware contract,
// independently of the encoder.
func decodeDisplay(t *testing.T, f protocolFrame) (image.Rectangle, uint16, []uint16) {
	t.Helper()
	p := f.data()
	if f.op() == terminalBinary && len(p) >= protoHeaderSize && p[0] == protoMagic && p[1] == msgFrame {
		_, _, p, _ = unpackProtoPacket(p)
	}
	if f.op() != terminalBinary || len(p) < 12 || p[0] != 0x03 {
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
				out = append(out, pixel.Pack565(uint8(cr>>8), uint8(cg>>8), uint8(cb>>8)))
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

func TestBox3HelloAndRawAppTaps(t *testing.T) {
	t.Parallel()
	_, base := wsHarness(t, nil)
	box := testConnectWS(t, base, "unit", false)
	round := testConnectWS(t, base, "unit", false)
	const boxID, roundID = "box3-test", "vocat-round"
	box.sendProto(t, msgHello, encodeTerminalHello(terminalHello{
		protoVersion: protoVersion1, kind: termKindHardware, displayFormats: dispRGB565, frameCodecs: codecBitAll,
		width: 320, height: 240, shape: shapeRect, terminalID: boxID, board: "esp-box-3", firmware: "0.14.0", token: "unit",
	}))
	box.until(t, hasProto(msgWelcome, ""))
	if r, _, _ := decodeDisplay(t, box.until(t, isDisplay)); r != image.Rect(0, 0, 320, 240) {
		t.Fatalf("BOX-3 initial frame = %v", r)
	}
	if r, _, _ := decodeDisplay(t, round.helloV2(t, roundID, "Cake")); r != image.Rect(0, 0, 360, 360) {
		t.Fatalf("legacy hello initial frame = %v", r)
	}
	terminals := testObject(t, testRequest(t, "GET", base+"/api/terminals", "unit", nil, 200))["terminals"].([]any)
	boxStatus := terminals[0].(object)
	if boxStatus["id"] != boxID || boxStatus["board"] != "esp-box-3" ||
		boxStatus["online"] != true {
		t.Fatal(boxStatus)
	}
}
