package main

import (
	"encoding/binary"
	"errors"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	sampleRate = 16000
	maxPCM     = 4 << 20
	maxText    = 640
)

var (
	le            = binary.LittleEndian
	errInvalidPCM = errors.New("invalid synthesized audio")
)

func ambientRMS(pcm []byte) (float64, bool) {
	const block = sampleRate / 100 * 2
	var energy [20]int64
	if len(pcm) < len(energy)*block {
		return 0, false
	}
	for i := range energy {
		for offset := i * block; offset < (i+1)*block; offset += 2 {
			s := int64(int16(le.Uint16(pcm[offset:])))
			energy[i] += s * s
		}
	}
	slices.Sort(energy[:])
	return math.Sqrt(float64(energy[0]+energy[1]+energy[2]+energy[3]) / (4 * (block / 2))), true
}

func ambientVolume(rms float64) int {
	curve := [...]struct{ rms, level float64 }{{80, 40}, {300, 58}, {800, 78}, {1800, 100}}
	for i, p := range curve {
		if rms <= p.rms {
			if i == 0 {
				return int(p.level)
			}
			prev := curve[i-1]
			return int(math.Round(prev.level + (p.level-prev.level)*(rms-prev.rms)/(p.rms-prev.rms)))
		}
	}
	return 100
}

func wavHeader(size int) (h [44]byte) {
	copy(h[:], "RIFF")
	le.PutUint32(h[4:], uint32(36+size))
	copy(h[8:], "WAVEfmt ")
	le.PutUint32(h[16:], 16)
	le.PutUint16(h[20:], 1)
	le.PutUint16(h[22:], 1)
	le.PutUint32(h[24:], sampleRate)
	le.PutUint32(h[28:], sampleRate*2)
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

func synthPCM(raw []float32, rate int) ([]byte, error) {
	if len(raw) == 0 || (rate != kokoroRate && rate != sampleRate) {
		return nil, errInvalidPCM
	}
	n := len(raw)
	count := n
	if rate == kokoroRate {
		count = (n*2 + 2) / 3
	}
	if count > maxPCM/2 {
		return nil, errInvalidPCM
	}
	var input []float32
	if rate == kokoroRate {
		input = make([]float32, n+2*sincRadius+1)
		copy(input[sincRadius:], raw)
	}
	pcm := make([]byte, count*2)
	var peak float64
	const knee, headroom = 0.75, 0.8912509381337456 - 0.75
	for i := range count {
		var v float32
		if rate == kokoroRate {
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
		return nil, errInvalidPCM
	}
	return pcm, nil
}

func NormalizeVoicePCM(pcm16k []byte) []byte {
	if len(pcm16k) < 2 || len(pcm16k)%2 != 0 {
		return pcm16k
	}
	var peak int
	var power int64
	for i := 0; i < len(pcm16k); i += 2 {
		v := int(int16(le.Uint16(pcm16k[i:])))
		peak = max(peak, v, -v)
		power += int64(v * v)
	}
	if peak < 1100 || power < int64(len(pcm16k)/2)*400*400 {
		return pcm16k
	}
	gain := min(3, 26027.2982/float64(peak))
	out := make([]byte, len(pcm16k))
	for i := 0; i < len(pcm16k); i += 2 {
		v := float64(int16(le.Uint16(pcm16k[i:])))
		le.PutUint16(out[i:], uint16(int16(math.RoundToEven(v*gain))))
	}
	return out
}

func alphaNum(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }

func plainText(s string) string {
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune("*_`#", r) {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

var (
	sentenceEnd = regexp.MustCompile(`[.!?]+["'”’)\]]*`)
	abbrev      = regexp.MustCompile(`(?i)\b(?:Mr|Mrs|Ms|Dr|Prof|St|vs|etc|e\.g|i\.e)\.$`)
	initials    = regexp.MustCompile(`\b(?:[A-Z]\.)+$`)
)

func sentences(text string, final, boundary bool) (out []string) {
	start, clause := 0, 0
	for _, loc := range sentenceEnd.FindAllStringIndex(text, -1) {
		a, b := loc[0], loc[1]
		if b < len(text) && text[b] != ' ' || b == len(text) && !(final || boundary) {
			continue
		}
		s := text[clause:b]
		if text[a] == '.' && (abbrev.MatchString(s) || initials.MatchString(s)) {
			continue
		}
		clause = b
		if text[a] != '.' {
			count := 0
			for _, r := range s {
				if alphaNum(r) {
					count++
				}
			}
			if count == 0 || start != 0 && count < 20 {
				continue
			}
		}
		out = append(out, strings.TrimSpace(text[start:b]))
		start = b
	}
	if s := strings.TrimSpace(text[start:]); final && s != "" {
		out = append(out, s)
	}
	return out
}

func SplitSentences(text string) []string { return sentences(plainText(text), true, true) }

type sentenceExtractor struct {
	text              strings.Builder
	start, clause     int
	punct             byte
	closers, space    bool
	ticks, seen       int
	utf8Tail          string
	hasLetter, halted bool
}

func (s *sentenceExtractor) boundary(out []string) []string {
	if s.punct == 0 {
		return out
	}
	punct := s.punct
	s.punct, s.closers = 0, false
	text := s.text.String()
	clause := text[s.clause:]
	if punct == '.' {
		// Only the final abbreviation or initial and its word boundary matter.
		tail := clause[max(0, len(clause)-6):]
		if abbrev.MatchString(tail) || initials.MatchString(tail) {
			return out
		}
	}
	s.clause = len(text)
	if punct != '.' {
		n := 0
		for _, r := range clause {
			if alphaNum(r) {
				n++
			}
		}
		// Start speaking on a short opening question or exclamation;
		// coalesce short later clauses to avoid tiny synthesis jobs.
		if n == 0 || s.start != 0 && n < 20 {
			return out
		}
	}
	out = append(out, strings.TrimSpace(text[s.start:]))
	s.start = len(text)
	return out
}

func (s *sentenceExtractor) add(delta string) (out []string) {
	if s.halted || delta == "" {
		return nil
	}
	// Process only new bytes. An unfinished UTF-8 rune is the only input
	// carried over; punctuation stays pending until its right boundary.
	const capBytes = 8 * 1024
	used := min(len(delta), capBytes-s.seen)
	s.seen += used
	text := s.utf8Tail + delta[:used]
	s.utf8Tail = ""
	for len(text) != 0 {
		if !utf8.FullRuneInString(text) {
			s.utf8Tail = text
			break
		}
		r, n := utf8.DecodeRuneInString(text)
		text = text[n:]
		if r == '`' {
			s.ticks++
			if s.ticks == 3 {
				s.halted = true
				return s.boundary(out)
			}
		} else {
			s.ticks = 0
		}
		if strings.ContainsRune("*_`#", r) {
			continue
		}
		if unicode.IsSpace(r) {
			out = s.boundary(out)
			s.space = s.text.Len() != 0
			continue
		}
		if s.space {
			s.text.WriteByte(' ')
			s.space = false
		}
		switch {
		case strings.ContainsRune(".!?", r):
			if s.punct == 0 || s.closers {
				s.punct, s.closers = byte(r), false
			}
		case s.punct != 0 && strings.ContainsRune("\"'”’)]", r):
			s.closers = true
		default:
			s.punct, s.closers = 0, false
		}
		s.text.WriteRune(r)
		s.hasLetter = s.hasLetter || alphaNum(r)
	}
	s.halted = s.seen == capBytes
	return out
}
