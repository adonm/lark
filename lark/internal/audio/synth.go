package audio

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Metronome and tune synthesis. Both render 16 kHz mono s16le exactly like the
// TTS path, so the edge streams them over the existing TTS_PCM pipeline.
const (
	MetronomeMinBPM     = 30
	MetronomeMaxBPM     = 240
	MetronomeMinBeats   = 1
	MetronomeMaxBeats   = 12
	MetronomeMinSeconds = 5
	MetronomeMaxSeconds = 300

	TuneMinTempo  = 40
	TuneMaxTempo  = 240
	TuneMaxNotes  = 64
	tuneMaxSecond = 30
)

var (
	errMetronome = errors.New("invalid metronome")
	errTune      = errors.New("invalid tune")
)

// MetronomeSpec is a validated metronome request. BPM and beats may be zero
// when the caller wants the defaults (100 BPM, 4/4).
type MetronomeSpec struct {
	BPM       int
	Beats     int
	DurationS int
}

// NewMetronomeSpec applies defaults and rejects out-of-range values.
func NewMetronomeSpec(bpm, beats, durationS int) (MetronomeSpec, error) {
	if bpm == 0 {
		bpm = 100
	}
	if beats == 0 {
		beats = 4
	}
	if durationS == 0 {
		durationS = 120
	}
	if bpm < MetronomeMinBPM || bpm > MetronomeMaxBPM {
		return MetronomeSpec{}, fmt.Errorf("%w: bpm must be %d..%d", errMetronome, MetronomeMinBPM, MetronomeMaxBPM)
	}
	if beats < MetronomeMinBeats || beats > MetronomeMaxBeats {
		return MetronomeSpec{}, fmt.Errorf("%w: beats must be %d..%d", errMetronome, MetronomeMinBeats, MetronomeMaxBeats)
	}
	if durationS < MetronomeMinSeconds || durationS > MetronomeMaxSeconds {
		return MetronomeSpec{}, fmt.Errorf("%w: duration_s must be %d..%d", errMetronome, MetronomeMinSeconds, MetronomeMaxSeconds)
	}
	return MetronomeSpec{BPM: bpm, Beats: beats, DurationS: durationS}, nil
}

// PeriodSamples is the exact integer sample distance between beats. Rounding
// once keeps the whole click track drift-free; no timer or float accumulation
// decides when the next beat lands.
func (s MetronomeSpec) PeriodSamples() int {
	return int(math.Round(float64(SampleRate) * 60.0 / float64(s.BPM)))
}

func metronomeClick(accent bool) []float64 {
	freq, amp := 800.0, 0.52
	if accent {
		freq, amp = 1200.0, 1.0
	}
	n := SampleRate * 48 / 1000 // 48 ms: short, and always shorter than a beat.
	out := make([]float64, n)
	for i := range out {
		t := float64(i) / SampleRate
		env := math.Exp(-t / 0.013)
		if t < 0.0015 {
			env *= t / 0.0015
		}
		// Pitched transient plus a soft second partial for a woodblock snap.
		v := 0.82*math.Sin(2*math.Pi*freq*t) + 0.18*math.Sin(4*math.Pi*freq*t)
		out[i] = amp * env * v
	}
	return out
}

// MetronomeClickPCM renders one accent or light click as s16le PCM.
func MetronomeClickPCM(accent bool) []byte {
	click := metronomeClick(accent)
	pcm := make([]byte, len(click)*2)
	for i, v := range click {
		le.PutUint16(pcm[2*i:], uint16(int16(math.RoundToEven(clampSample(v)*32767))))
	}
	return pcm
}

// MetronomeStream emits contiguous s16le chunks covering exactly DurationS
// seconds. Chunk boundaries never reset a click. The producer exits promptly
// when ctx is cancelled, so Stop interrupts a long metronome immediately.
func MetronomeStream(ctx context.Context, spec MetronomeSpec, chunkSamples int) (<-chan []byte, error) {
	if spec.BPM < MetronomeMinBPM || spec.BPM > MetronomeMaxBPM ||
		spec.Beats < MetronomeMinBeats || spec.Beats > MetronomeMaxBeats ||
		spec.DurationS < MetronomeMinSeconds || spec.DurationS > MetronomeMaxSeconds {
		return nil, errMetronome
	}
	if chunkSamples <= 0 {
		chunkSamples = 2048
	}
	period := spec.PeriodSamples()
	accent, light := metronomeClick(true), metronomeClick(false)
	total := spec.DurationS * SampleRate
	out := make(chan []byte)
	go func() {
		defer close(out)
		for start := 0; start < total; start += chunkSamples {
			n := min(chunkSamples, total-start)
			chunk := make([]byte, n*2)
			for i := 0; i < n; i++ {
				pos := start + i
				beat := pos / period
				offset := pos - beat*period
				v := 0.0
				if offset < len(accent) {
					if beat%spec.Beats == 0 {
						v = accent[offset]
					} else {
						v = light[offset]
					}
				}
				le.PutUint16(chunk[2*i:], uint16(int16(math.RoundToEven(clampSample(v)*32767))))
			}
			select {
			case out <- chunk:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// TuneNote is one note or rest in a compact tune request.
type TuneNote struct {
	Note  string  `json:"note"`
	Beats float64 `json:"beats"`
}

// TuneSpec is a validated tune request.
type TuneSpec struct {
	Tempo int
	Style string
	Notes []TuneNote
}

// ParseNote converts a compact note name (C4, F#4, Bb3, R, rest) into Hz.
// Rests return 0 and no error.
func ParseNote(name string) (float64, error) {
	raw := strings.TrimSpace(name)
	if raw == "" {
		return 0, fmt.Errorf("%w: empty note", errTune)
	}
	if strings.EqualFold(raw, "r") || strings.EqualFold(raw, "rest") {
		return 0, nil
	}
	if len(raw) < 2 || len(raw) > 4 {
		return 0, fmt.Errorf("%w: bad note %q", errTune, name)
	}
	base := strings.ToUpper(raw[:1])
	semitones := map[string]int{"C": 0, "D": 2, "E": 4, "F": 5, "G": 7, "A": 9, "B": 11}[base]
	if _, ok := map[string]bool{"C": true, "D": true, "E": true, "F": true, "G": true, "A": true, "B": true}[base]; !ok {
		return 0, fmt.Errorf("%w: bad note %q", errTune, name)
	}
	rest := raw[1:]
	if strings.HasPrefix(rest, "#") || strings.HasPrefix(rest, "b") {
		if rest[0] == '#' {
			semitones++
		} else {
			semitones--
		}
		rest = rest[1:]
	}
	octave, err := strconv.Atoi(rest)
	if err != nil || octave < 0 || octave > 8 {
		return 0, fmt.Errorf("%w: bad note %q", errTune, name)
	}
	midi := 12*(octave+1) + semitones
	return 440 * math.Pow(2, float64(midi-69)/12), nil
}

// SynthesizeTune renders a complete tune as s16le PCM. It validates notes,
// tempo, style, count, and the 30-second total before allocating.
func SynthesizeTune(spec TuneSpec) ([]byte, error) {
	tempo := spec.Tempo
	if tempo == 0 {
		tempo = 100
	}
	style := spec.Style
	if style == "" {
		style = "hum"
	}
	if tempo < TuneMinTempo || tempo > TuneMaxTempo {
		return nil, fmt.Errorf("%w: tempo must be %d..%d", errTune, TuneMinTempo, TuneMaxTempo)
	}
	if style != "hum" && style != "chime" && style != "pluck" {
		return nil, fmt.Errorf("%w: style must be hum, chime, or pluck", errTune)
	}
	if len(spec.Notes) == 0 || len(spec.Notes) > TuneMaxNotes {
		return nil, fmt.Errorf("%w: notes must contain 1..%d entries", errTune, TuneMaxNotes)
	}
	secondsPerBeat := 60.0 / float64(tempo)
	type rendered struct {
		freq    float64
		samples int
	}
	notes := make([]rendered, 0, len(spec.Notes))
	total := 0
	for _, note := range spec.Notes {
		if note.Beats < 0.25 || note.Beats > 8 {
			return nil, fmt.Errorf("%w: each note needs beats 0.25..8", errTune)
		}
		freq, err := ParseNote(note.Note)
		if err != nil {
			return nil, err
		}
		samples := int(math.Round(note.Beats * secondsPerBeat * SampleRate))
		if samples < 1 {
			samples = 1
		}
		total += samples
		if total > tuneMaxSecond*SampleRate {
			return nil, fmt.Errorf("%w: tune exceeds %d seconds", errTune, tuneMaxSecond)
		}
		notes = append(notes, rendered{freq: freq, samples: samples})
	}
	buf := make([]float64, total)
	offset := 0
	for _, note := range notes {
		region := buf[offset : offset+note.samples]
		if note.freq > 0 {
			switch style {
			case "hum":
				renderHum(region, note.freq)
			case "chime":
				renderChime(region, note.freq)
			case "pluck":
				renderPluck(region, note.freq)
			}
		}
		offset += note.samples
	}
	var peak float64
	for _, v := range buf {
		peak = math.Max(peak, math.Abs(v))
	}
	if peak < 1e-6 || math.IsNaN(peak) || math.IsInf(peak, 0) {
		return nil, fmt.Errorf("%w: tune is silent", errTune)
	}
	gain := 0.82 / peak
	pcm := make([]byte, len(buf)*2)
	for i, v := range buf {
		le.PutUint16(pcm[2*i:], uint16(int16(math.RoundToEven(clampSample(v*gain)*32767))))
	}
	return pcm, nil
}

// sinTable keeps a warm 30 s tune well under the synthesis budget without
// pulling in a math library dependency; linear interpolation at 4096 points is
// far below the 16 kHz Nyquist accuracy limit.
const sinTableSize = 4096

var sinTable = func() (table [sinTableSize + 1]float64) {
	for i := range table {
		table[i] = math.Sin(2 * math.Pi * float64(i) / sinTableSize)
	}
	return table
}()

func fastSin(phase float64) float64 {
	p := phase * (sinTableSize / (2 * math.Pi))
	idx := int(p) % sinTableSize
	if idx < 0 {
		idx += sinTableSize
	}
	frac := p - math.Floor(p)
	return sinTable[idx] + (sinTable[idx+1]-sinTable[idx])*frac
}

// renderHum is a warm triangle+sine voice with 5.5 Hz vibrato and soft edges.
func renderHum(out []float64, freq float64) {
	n := len(out)
	dur := float64(n) / SampleRate
	attack := math.Min(0.025, dur*0.3)
	release := math.Min(0.04, dur*0.3)
	phase, lfo := 0.0, 0.0
	lp := 0.0
	alpha := math.Min(1, 2*math.Pi*math.Min(4000, freq*3)/SampleRate)
	for i := range out {
		t := float64(i) / SampleRate
		env := 1.0
		if t < attack {
			env = t / attack
		}
		if remain := dur - t; remain < release {
			env = math.Min(env, remain/release)
		}
		lfo += 2 * math.Pi * 5.5 / SampleRate
		phase += 2 * math.Pi * freq * (1 + 0.006*fastSin(lfo)) / SampleRate
		u := phase / (2 * math.Pi)
		tri := 4*math.Abs(u-math.Floor(u+0.75)+0.25) - 1
		v := 0.62*tri + 0.38*fastSin(phase)
		lp += alpha * (v - lp)
		out[i] = 0.7 * env * lp
	}
}

// renderChime is a bright music-box bell with inharmonic partials.
func renderChime(out []float64, freq float64) {
	partials := []struct {
		ratio, amp, step float64
	}{{1, 1, 0.62}, {2, 0.42, 0.34}, {3, 0.2, 0.22}, {4.2, 0.1, 0.14}}
	for i := range partials {
		partials[i].step = math.Exp(-1 / (partials[i].step * SampleRate))
	}
	for i := range out {
		t := float64(i) / SampleRate
		attack := 1.0
		if t < 0.002 {
			attack = t / 0.002
		}
		v := 0.0
		for j := range partials {
			p := &partials[j]
			v += p.amp * fastSin(2*math.Pi*freq*p.ratio*t)
			p.amp *= p.step
		}
		out[i] = 0.58 * attack * v
	}
}

// renderPluck is Karplus-Strong: a short noise burst circulated through a
// tuned delay line with light damping, then faded out to avoid an end click.
func renderPluck(out []float64, freq float64) {
	n := len(out)
	delay := max(2, int(math.Round(float64(SampleRate)/freq)))
	line := make([]float64, delay)
	seed := uint32(int64(freq*1000) & 0x7fffffff)
	for i := range line {
		seed = seed*1664525 + 1013904223
		line[i] = float64(int32(seed)) / float64(math.MaxInt32)
	}
	fade := min(n, SampleRate*10/1000)
	idx := 0
	for i := range out {
		v := line[idx]
		next := line[(idx+1)%delay]
		line[idx] = 0.996 * 0.5 * (v + next)
		idx = (idx + 1) % delay
		env := 1.0
		if i < SampleRate*2/1000 {
			env = float64(i) / (SampleRate * 2 / 1000)
		}
		if i >= n-fade {
			env = math.Min(env, float64(n-i)/float64(fade))
		}
		out[i] = 0.55 * env * v
	}
}

func clampSample(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Max(-0.98, math.Min(0.98, v))
}
