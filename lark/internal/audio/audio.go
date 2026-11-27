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
		peak = max(peak, math.Abs(float64(v)*1.8))
		putPCM(pcm, i, v)
	}
	if peak < 1e-5 || math.IsNaN(peak) || math.IsInf(peak, 0) {
		return nil, scratch, errInvalidPCM
	}
	return pcm, scratch, nil
}

const (
	pcmKnee     = 0.75
	pcmHeadroom = 0.8912509381337456 - pcmKnee
)

// putPCM converts one float sample to clipped 16-bit PCM. Both the batch and
// streaming resamplers use it so their output is byte-identical per sample.
func putPCM(pcm []byte, i int, v float32) {
	scaled := float64(v) * 1.8
	mag := math.Abs(scaled)
	if mag > pcmKnee {
		mag = pcmKnee + pcmHeadroom*math.Tanh((mag-pcmKnee)/pcmHeadroom)
	}
	le.PutUint16(pcm[2*i:], uint16(int16(math.RoundToEven(math.Copysign(mag, scaled)*32767))))
}

// Resampler converts 24 kHz float samples to 16 kHz 16-bit PCM incrementally.
// It carries the sinc filter phase and the last input window between Push
// calls, so concatenating chunk outputs is byte-identical to resampling the
// whole utterance at once. Call Flush once at the end to emit the padded tail.
type Resampler struct {
	rate     int
	hist     []float32 // retained input: global samples [base, base+len(hist))
	base     int64
	nextOut  int64
	consumed int64
	started  bool
	done     bool
}

func NewResampler(rate int) *Resampler {
	return &Resampler{rate: rate}
}

// Push appends PCM for every output sample whose filter window is fully known,
// growing dst as needed.
func (r *Resampler) Push(dst []byte, raw []float32) []byte {
	if r.done || len(raw) == 0 {
		return dst
	}
	if r.rate != KokoroRate {
		for _, v := range raw {
			dst = appendPCM(dst, v)
		}
		return dst
	}
	if !r.started {
		r.started = true
		r.base = -sincRadius
		r.hist = append(r.hist[:0], make([]float32, sincRadius)...)
	}
	r.hist = append(r.hist, raw...)
	r.consumed += int64(len(raw))
	dst = r.resample(dst)

	// Keep enough history for the next window, which can start up to 32 raw
	// samples before the samples seen so far.
	const keep = 2 * sincRadius
	if len(r.hist) > keep {
		cut := len(r.hist) - keep
		copy(r.hist, r.hist[cut:])
		r.hist = r.hist[:keep]
		r.base += int64(cut)
	}
	return dst
}

// Flush pads the input with sincRadius zeros like the batch path and emits the
// remaining output samples. It is idempotent.
func (r *Resampler) Flush(dst []byte) []byte {
	if r.done {
		return dst
	}
	r.done = true
	if r.rate != KokoroRate {
		return dst
	}
	if !r.started {
		return dst
	}
	r.hist = append(r.hist, make([]float32, sincRadius)...)
	return r.resample(dst)
}

func (r *Resampler) resample(dst []byte) []byte {
	if r.rate != KokoroRate {
		return dst
	}
	count := (2*r.consumed + 2) / 3
	for ; r.nextOut < count; r.nextOut++ {
		center := 3 * r.nextOut / 2
		local := int(center - r.base)
		if local < sincRadius {
			break
		}
		if local+sincRadius >= len(r.hist) {
			break
		}
		var v float32
		samples := r.hist[local-sincRadius : local+sincRadius+1]
		weights := &sinc[r.nextOut&1]
		for j, sample := range samples {
			v += sample * weights[j]
		}
		dst = appendPCM(dst, v)
	}
	return dst
}

func appendPCM(dst []byte, v float32) []byte {
	var b [2]byte
	putPCM(b[:], 0, v)
	return append(dst, b[0], b[1])
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
