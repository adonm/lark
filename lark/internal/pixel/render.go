package pixel

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"

	"github.com/pierrec/lz4/v4"
)

type HitBox struct {
	ID, Label      string
	X1, Y1, X2, Y2 int
}

func HitID(hits []HitBox, x, y int) string {
	for i := len(hits) - 1; i >= 0; i-- {
		h := hits[i]
		if x >= h.X1 && x <= h.X2 && y >= h.Y1 && y <= h.Y2 {
			return h.ID
		}
	}
	return ""
}

func Pack565(r, g, b uint8) uint16 {
	return uint16(r&0xf8)<<8 | uint16(g&0xfc)<<3 | uint16(b)>>3
}

const (
	FrameCodecRaw    byte = 0
	FrameCodecRLE565 byte = 1
	FrameCodecJPEG   byte = 2
	FrameCodecLZ4    byte = 3
)

const (
	codecBitRLE565 = 1 << 1
	codecBitLZ4    = 1 << 2
	codecBitJPEG   = 1 << 3
	CodecBitAll    = 15
)

func DirtyRect(prev, next []uint16, w, h int) image.Rectangle {
	if prev == nil || len(prev) != len(next) {
		return image.Rect(0, 0, w, h)
	}
	minX, minY, maxX, maxY := w, h, 0, 0
	for y := range h {
		off := y * w
		prevRow := prev[off : off+w]
		nextRow := next[off : off+w]
		for x := range w {
			if nextRow[x] != prevRow[x] {
				minX = min(minX, x)
				minY = min(minY, y)
				maxX = max(maxX, x+1)
				maxY = max(maxY, y+1)
			}
		}
	}
	if minX >= maxX {
		return image.Rectangle{}
	}
	return image.Rect(minX, minY, maxX, maxY)
}

func encodeRLE565FromBE(out []byte, rawBE []byte, pixels int) []byte {
	for i := 0; i < pixels; {
		start := i
		i++
		sOff := start * 2
		hi, lo := rawBE[sOff], rawBE[sOff+1]
		if i < pixels && rawBE[i*2] == hi && rawBE[i*2+1] == lo {
			for i < pixels && i-start < 128 && rawBE[i*2] == hi && rawBE[i*2+1] == lo {
				i++
			}
			out = append(out, 0x80|byte(i-start-1), hi, lo)
		} else {
			for i < pixels && i-start < 128 && (i+1 == pixels || rawBE[i*2] != rawBE[(i+1)*2] || rawBE[i*2+1] != rawBE[(i+1)*2+1]) {
				i++
			}
			out = append(out, byte(i-start-1))
			out = append(out, rawBE[sOff:i*2]...)
		}
	}
	return out
}

type FrameEncoder struct {
	rawBE   []byte
	rleBuf  []byte
	lz4Buf  []byte
	lz4Comp lz4.Compressor
	jpegBuf bytes.Buffer
	jpegImg *image.RGBA
}

func (e *FrameEncoder) rgbaFromFB(fb []uint16, r image.Rectangle, width int) *image.RGBA {
	rw, rh := r.Dx(), r.Dy()
	need := rw * rh * 4
	if e.jpegImg == nil || cap(e.jpegImg.Pix) < need {
		e.jpegImg = image.NewRGBA(image.Rect(0, 0, rw, rh))
	} else {
		e.jpegImg.Rect = image.Rect(0, 0, rw, rh)
		e.jpegImg.Stride = rw * 4
		e.jpegImg.Pix = e.jpegImg.Pix[:need]
	}
	pix := e.jpegImg.Pix
	off := 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for _, v := range fb[y*width+r.Min.X : y*width+r.Max.X] {
			r5 := uint8(v >> 11)
			g6 := uint8((v >> 5) & 0x3f)
			b5 := uint8(v & 0x1f)
			pix[off] = (r5 << 3) | (r5 >> 2)
			pix[off+1] = (g6 << 2) | (g6 >> 4)
			pix[off+2] = (b5 << 3) | (b5 >> 2)
			pix[off+3] = 255
			off += 4
		}
	}
	return e.jpegImg
}

func (e *FrameEncoder) EncodeWithCodecs(fb []uint16, rgba *image.RGBA, r image.Rectangle, rev uint64, width int, allowed uint8) []byte {
	if allowed == 0 {
		allowed = CodecBitAll
	}
	rw, rh := r.Dx(), r.Dy()
	pixels := rw * rh
	rawLen := pixels * 2
	if cap(e.rawBE) < rawLen {
		e.rawBE = make([]byte, rawLen)
	}
	rawBE := e.rawBE[:rawLen]
	off := 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for _, v := range fb[y*width+r.Min.X : y*width+r.Max.X] {
			rawBE[off] = byte(v >> 8)
			rawBE[off+1] = byte(v)
			off += 2
		}
	}

	bestCodec := FrameCodecRaw
	bestPayload := rawBE
	if allowed&codecBitRLE565 != 0 {
		e.rleBuf = encodeRLE565FromBE(e.rleBuf[:0], rawBE, pixels)
		if len(e.rleBuf) <= len(bestPayload) {
			bestCodec = FrameCodecRLE565
			bestPayload = e.rleBuf
		}
	}

	if allowed&codecBitLZ4 != 0 {
		bound := lz4.CompressBlockBound(rawLen)
		if cap(e.lz4Buf) < bound {
			e.lz4Buf = make([]byte, bound)
		}
		lz4Dst := e.lz4Buf[:bound]
		if n, err := e.lz4Comp.CompressBlock(rawBE, lz4Dst); err == nil && n > 0 && n < len(bestPayload) {
			bestCodec = FrameCodecLZ4
			bestPayload = lz4Dst[:n]
		}
	}

	if allowed&codecBitJPEG != 0 && rw >= 64 && rh >= 64 && len(bestPayload) > rawLen/3 {
		var src image.Image
		if rgba != nil && r.In(rgba.Bounds()) {
			src = rgba.SubImage(r)
		} else {
			src = e.rgbaFromFB(fb, r, width)
		}
		e.jpegBuf.Reset()
		if err := jpeg.Encode(&e.jpegBuf, src, &jpeg.Options{Quality: 82}); err == nil {
			if e.jpegBuf.Len() < len(bestPayload)*6/10 {
				bestCodec = FrameCodecJPEG
				bestPayload = e.jpegBuf.Bytes()
			}
		}
	}

	out := make([]byte, 12+len(bestPayload))
	out[0] = 0x03
	binary.LittleEndian.PutUint16(out[1:], uint16(r.Min.X))
	binary.LittleEndian.PutUint16(out[3:], uint16(r.Min.Y))
	binary.LittleEndian.PutUint16(out[5:], uint16(rw))
	binary.LittleEndian.PutUint16(out[7:], uint16(rh))
	out[9] = bestCodec
	binary.LittleEndian.PutUint16(out[10:], uint16(rev))
	copy(out[12:], bestPayload)
	return out
}
