package audio

import (
	"encoding/binary"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var scientificPitch = regexp.MustCompile(`^([A-G])([#b]?)([3-7])$`)

func NoteFrequency(pitch string) (float64, bool) {
	if pitch == "R" {
		return 0, true
	}
	if m := scientificPitch.FindStringSubmatch(pitch); m != nil {
		semitone := [...]int{9, 11, 0, 2, 4, 5, 7}[m[1][0]-'A'] + 12*int(m[3][0]-'4')
		switch m[2] {
		case "#":
			semitone++
		case "b":
			semitone--
		}
		if semitone < -12 || semitone > 36 { // C3 through C7, inclusive.
			return 0, false
		}
		return 440 * math.Exp2(float64(semitone-9)/12), true
	}
	hz, err := strconv.ParseFloat(pitch, 64)
	return hz, err == nil && hz >= 100 && hz <= 2500
}

func SynthesizeMelody(notes []string) ([]byte, error) {
	if len(notes) < 1 || len(notes) > 32 {
		return nil, fmt.Errorf("expected 1–32 notes in PITCH:MS format")
	}
	type note struct {
		hz      float64
		samples int
	}
	parsed := make([]note, len(notes))
	total := 0
	for i, token := range notes {
		pitch, duration, split := strings.Cut(token, ":")
		ms, err := strconv.Atoi(duration)
		hz, ok := NoteFrequency(pitch)
		if !split || !ok || err != nil || ms < 30 || ms > 2000 {
			return nil, fmt.Errorf("note %d: use C3–C7, R, or 100–2500 Hz followed by :30–2000 (milliseconds)", i+1)
		}
		total += ms
		if total > 6000 {
			return nil, fmt.Errorf("total duration must not exceed 6000 ms")
		}
		parsed[i] = note{hz, ms * SampleRate / 1000}
	}
	pcm := make([]byte, 2*total*SampleRate/1000)
	offset := 0
	for _, n := range parsed {
		if n.hz != 0 {
			step := 2 * math.Pi * n.hz / SampleRate
			for i := range n.samples {
				phase := step * float64(i)
				fade := float64(min(i, n.samples-1-i, SampleRate/100)) / (SampleRate / 100)
				sample := int16(32767 * 0.45 * fade * (0.85*math.Sin(phase) + 0.15*math.Sin(2*phase)))
				binary.LittleEndian.PutUint16(pcm[offset+2*i:], uint16(sample))
			}
		}
		offset += 2 * n.samples
	}
	return pcm, nil
}

var SFXPresets = map[string]struct {
	ms                        int
	from, to, harmonic, noise float64
}{
	"boing":  {320, 230, 610, .24, 0},
	"pop":    {95, 750, 170, .16, .22},
	"win":    {430, 0, 0, 0, 0},
	"buzz":   {240, 155, 120, .65, .12},
	"ding":   {380, 1175, 1160, .28, 0},
	"whoosh": {360, 220, 720, 0, .85},
}

func SFX(name string) ([]byte, error) {
	p, ok := SFXPresets[name]
	if !ok {
		return nil, fmt.Errorf("unknown SFX name")
	}
	if name == "win" {
		return SynthesizeMelody([]string{"C5:65", "E5:65", "G5:65", "C6:235"})
	}
	n := p.ms * SampleRate / 1000
	pcm := make([]byte, n*2)
	var phase, filtered float64
	rng := uint32(1)
	for i := range n {
		t := float64(i) / float64(n)
		phase += 2 * math.Pi * (p.from + (p.to-p.from)*t) / SampleRate
		rng = rng*1664525 + 1013904223
		filtered += (.15 + .45*t) * (float64(int32(rng>>16)-32768)/32768 - filtered)
		fade := min(1, float64(i)/80, float64(n-1-i)/80) * (1 - t) * (1 - t)
		if name == "whoosh" {
			fade = math.Sin(math.Pi * t)
		}
		tone := (1-p.noise)*math.Sin(phase) + p.harmonic*math.Sin(2*phase) + p.noise*filtered
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(int16(16000*fade*tone)))
	}
	return pcm, nil
}
