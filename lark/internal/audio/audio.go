package audio

import (
	"encoding/binary"
	"errors"
	"math"
	"os"
)

const (
	SampleRate = 16000
	MaxPCM     = 4 << 20
)

var (
	le            = binary.LittleEndian
	errInvalidPCM = errors.New("invalid synthesized audio")
)

func WavHeader(size int) (h [44]byte) {
	copy(h[:], "RIFF")
	le.PutUint32(h[4:], uint32(36+size))
	copy(h[8:], "WAVEfmt ")
	le.PutUint32(h[16:], 16)
	le.PutUint16(h[20:], 1)
	le.PutUint16(h[22:], 1)
	le.PutUint32(h[24:], SampleRate)
	le.PutUint32(h[28:], SampleRate*2)
	le.PutUint16(h[32:], 2)
	le.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	le.PutUint32(h[40:], uint32(size))
	return h
}

// At an output position 3*n/2, the fractional input position is either 0 or 1/2.
const sincRadius = 16

var sinc = func() (phases [2][2*sincRadius + 1]float32) {
	for p := range phases {
		var sum float32
		for i := range phases[p] {
			x := float64(i-sincRadius) - float64(p)*0.5
			y := math.Pi * x / (sincRadius + 1)
			window := 0.42 + 0.5*math.Cos(y) + 0.08*math.Cos(2*y)
			v := 0.62
			if x != 0 {
				v = math.Sin(math.Pi*v*x) / (math.Pi * x)
			}
			phases[p][i] = float32(v * window)
			sum += phases[p][i]
		}
		for i := range phases[p] {
			phases[p][i] /= sum
		}
	}
	return phases
}()

func SynthPCM(raw []float32, rate int) ([]byte, error) {
	pcm, _, err := SynthPCMWithScratch(raw, rate, nil)
	return pcm, err
}

func SynthPCMWithScratch(raw []float32, rate int, scratch []float32) ([]byte, []float32, error) {
	if len(raw) == 0 || (rate != KokoroRate && rate != SampleRate) {
		return nil, scratch, errInvalidPCM
	}
	n := len(raw)
	count := n
	if rate == KokoroRate {
		count = (n*2 + 2) / 3
	}
	if count > MaxPCM/2 {
		return nil, scratch, errInvalidPCM
	}
	var input []float32
	if rate == KokoroRate {
		need := n + 2*sincRadius + 1
		if cap(scratch) < need {
			scratch = make([]float32, need)
		} else {
			scratch = scratch[:need]
		}
		clear(scratch[:sincRadius])
		copy(scratch[sincRadius:], raw)
		clear(scratch[sincRadius+n : need])
		input = scratch
	}
	pcm := make([]byte, count*2)
	var peak float64
	const knee, headroom = 0.75, 0.8912509381337456 - 0.75
	for i := range count {
		var v float32
		if rate == KokoroRate {
			center := 3 * i / 2
			samples := input[center : center+2*sincRadius+1]
			weights := &sinc[i&1]
			for j, sample := range samples {
				v += sample * weights[j]
			}
		} else {
			v = raw[i]
		}
		scaled := float64(v) * 1.8
		mag := math.Abs(scaled)
		peak = max(peak, mag)
		if mag > knee {
			mag = knee + headroom*math.Tanh((mag-knee)/headroom)
		}
		le.PutUint16(pcm[2*i:], uint16(int16(math.RoundToEven(math.Copysign(mag, scaled)*32767))))
	}
	if peak < 1e-5 || math.IsNaN(peak) || math.IsInf(peak, 0) {
		return nil, scratch, errInvalidPCM
	}
	return pcm, scratch, nil
}

// ReadWAVPCM decodes a RIFF/WAVE file into 16 kHz mono little-endian PCM.
// It accepts 16-bit PCM at 16 kHz (the edge's capture format) and rejects
// anything else rather than silently feeding the wrong rate to STT.
func ReadWAVPCM(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < 44 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		return nil, errors.New("not a WAV file")
	}
	var (
		channels  uint16
		rate      uint32
		bits      uint16
		data      []byte
		foundFmt  bool
		foundData bool
	)
	for off := 12; off+8 <= len(raw); {
		id := string(raw[off : off+4])
		size := int(le.Uint32(raw[off+4 : off+8]))
		body := off + 8
		if body+size > len(raw) {
			size = len(raw) - body
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return nil, errors.New("short WAV fmt chunk")
			}
			format := le.Uint16(raw[body:])
			channels = le.Uint16(raw[body+2:])
			rate = le.Uint32(raw[body+4:])
			bits = le.Uint16(raw[body+14:])
			if format != 1 || bits != 16 {
				return nil, errors.New("only 16-bit PCM WAV is supported")
			}
			foundFmt = true
		case "data":
			data = raw[body : body+size]
			foundData = true
		}
		off = body + size
		if size&1 == 1 {
			off++
		}
	}
	if !foundFmt || !foundData {
		return nil, errors.New("WAV missing fmt or data chunk")
	}
	if rate != SampleRate {
		return nil, errors.New("WAV must be 16 kHz")
	}
	if channels == 1 {
		return data, nil
	}
	if channels != 2 {
		return nil, errors.New("WAV must be mono or stereo")
	}
	mono := make([]byte, len(data)/2)
	for i := range len(mono) / 2 {
		l := int16(le.Uint16(data[4*i:]))
		r := int16(le.Uint16(data[4*i+2:]))
		le.PutUint16(mono[2*i:], uint16(int16((int32(l)+int32(r))/2)))
	}
	return mono, nil
}
