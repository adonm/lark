package edge

import (
	"bytes"
	"encoding/binary"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"lark/internal/audio"
)

var le = binary.LittleEndian

const (
	sampleRate            = audio.SampleRate
	endpointFrame         = 20 * time.Millisecond
	endpointBytes         = sampleRate / 50 * 2
	endpointLimit         = 60 * time.Second
	sileroWindow          = 512
	endpointIdle          = 6 * time.Second
	endpointQuiet         = 400 * time.Millisecond
	endpointSpecQuiet     = 180 * time.Millisecond
	endpointCap           = 1200 * time.Millisecond
	endpointHesitationCap = 2200 * time.Millisecond
	endpointProbeEvery    = 250 * time.Millisecond
	endpointMonologue     = 20 * time.Second
	vadTail               = 320 * time.Millisecond

	speechEnergyRatio            = 0.12
	smartTurnThreshold           = float32(0.90)
	smartTurnSpeculateThreshold  = float32(0.40)
	smartTurnHesitationThreshold = float32(0.25)
	gateMargin                   = 5.0
	gateCalibFrames              = 50
	listenPreRollBytes           = sampleRate * 2 * 400 / 1000
)

// sileroVAD consumes fixed 512-sample windows at 16 kHz. Detectors are pooled;
// Reset clears segment history on hand-out.
type sileroVAD struct {
	model  *sherpa.VoiceActivityDetector
	path   string
	window [sileroWindow]float32
	n      int
	voiced bool
}

var sileroPool sync.Pool

func newSileroVAD(path string) VAD {
	if v := sileroPool.Get(); v != nil {
		s := v.(*sileroVAD)
		if s.path == path {
			s.model.Reset()
			s.n, s.voiced = 0, false
			return s
		}
		s.destroy()
	}
	model := sherpa.NewVoiceActivityDetector(&sherpa.VadModelConfig{
		SileroVad: sherpa.SileroVadModelConfig{
			Model: path, Threshold: 0.5, WindowSize: sileroWindow,
			MinSilenceDuration: float32(vadTail.Seconds()), MinSpeechDuration: 0.12, MaxSpeechDuration: 60,
		},
		SampleRate: sampleRate, Provider: "cpu", NumThreads: 1,
	}, 10)
	if model == nil {
		return nil
	}
	return &sileroVAD{model: model, path: path}
}

func (s *sileroVAD) Detect(pcm []byte) bool {
	for i := 0; i+1 < len(pcm); i += 2 {
		s.window[s.n] = float32(int16(le.Uint16(pcm[i:]))) / 32768
		s.n++
		if s.n == len(s.window) {
			s.model.AcceptWaveform(s.window[:])
			s.voiced = s.model.IsSpeech()
			for !s.model.IsEmpty() {
				s.model.Pop()
			}
			s.n = 0
		}
	}
	return s.voiced
}

func (s *sileroVAD) destroy() {
	if s.model != nil {
		sherpa.DeleteVoiceActivityDetector(s.model)
		s.model = nil
	}
}

func (s *sileroVAD) Close() {
	if s == nil || s.model == nil {
		return
	}
	s.model.Clear()
	s.n, s.voiced = 0, false
	sileroPool.Put(s)
}

// energyVAD is the no-model fallback used only when Silero is unavailable.
type energyVAD struct{ speech bool }

func (v *energyVAD) Detect(pcm []byte) bool { v.speech = frameRMS(pcm) >= 350; return v.speech }
func (v *energyVAD) Close()                 {}

// energyGate learns the room noise floor when a listening window opens and
// tracks it during non-speech. Silero still owns the speech decision.
type energyGate struct {
	buf   [gateCalibFrames]float64
	n     int
	next  int
	floor float64
	ready bool
}

func (g *energyGate) observe(rms float64, vad, calibrate bool) {
	if vad {
		return
	}
	if calibrate {
		g.buf[g.next] = rms
		g.next = (g.next + 1) % len(g.buf)
		if g.n < len(g.buf) {
			g.n++
		}
		if !g.ready && g.n == len(g.buf) {
			ordered := slices.Clone(g.buf[:])
			slices.Sort(ordered)
			g.floor = ordered[len(ordered)/10]
			g.ready = true
		}
	}
	if !g.ready {
		return
	}
	if rms < g.floor {
		g.floor = rms
	} else {
		g.floor += (rms - g.floor) * 0.002
	}
}

func (g *energyGate) enter() float64 { return g.floor + max(gateMargin, g.floor) }
func (g *energyGate) stay() float64  { return g.floor + max(gateMargin*0.6, g.floor*0.5) }

func frameRMS(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i+1 < len(pcm); i += 2 {
		s := float64(int16(le.Uint16(pcm[i:])))
		sum += s * s
	}
	return math.Sqrt(sum / float64(n))
}

type endpointPolicy struct {
	voiced        bool
	inVoice       bool
	speechRMS     float64
	hasWords      bool
	incomplete    bool
	wordCount     int
	lastWordAt    time.Duration
	lastText      string
	lastVoicedAt  time.Duration
	firstVoicedAt time.Duration
	speechFrames  int
	vadFrames     int
	gate          energyGate
}

func (p *endpointPolicy) observeEnergy(at time.Duration, vadSpeech bool, rms float64) {
	if !vadSpeech {
		p.gate.observe(rms, false, !p.voiced || at-p.lastVoicedAt >= time.Second)
		p.inVoice = false
		return
	}
	p.vadFrames++
	if p.voiced && rms < p.speechRMS*speechEnergyRatio {
		p.gate.observe(rms, false, false)
		p.inVoice = false
		return
	}
	p.gate.observe(rms, true, false)
	limit := p.gate.stay()
	if !p.voiced || !p.inVoice {
		limit = p.gate.enter()
	}
	if rms < limit {
		p.inVoice = false
		return
	}
	if !p.voiced {
		p.voiced = true
		p.firstVoicedAt = at
		p.speechRMS = rms
	}
	p.inVoice = true
	p.lastVoicedAt = at
	p.speechFrames++
	if rms > p.speechRMS {
		p.speechRMS = p.speechRMS*0.85 + rms*0.15
	} else if rms > 0 {
		p.speechRMS = p.speechRMS*0.95 + rms*0.05
	}
}

func (p *endpointPolicy) observeProgress(probeAt time.Duration, text string) bool {
	words, fillerOnly, incomplete := inspectTranscript(text)
	if fillerOnly || words == 0 {
		return false
	}
	p.incomplete = incomplete
	if !p.hasWords || words >= p.wordCount {
		p.hasWords = true
		p.wordCount = words
		p.lastWordAt = probeAt
		p.lastText = text
		return true
	}
	return false
}

type decision uint8

const (
	decisionContinue decision = iota
	decisionCommitQuiet
	decisionCommitCap
	decisionCommitMonologue
	decisionCommitNoWords
	decisionDiscardIdle
	decisionDiscardNoise
)

func (d decision) stops() bool    { return d != decisionContinue }
func (d decision) discards() bool { return d == decisionDiscardIdle || d == decisionDiscardNoise }

func (p endpointPolicy) classify(at, capDur, limitDur time.Duration, smartTurn float32, listen bool) decision {
	if at >= limitDur {
		if p.speechFrames > 0 {
			return decisionCommitCap
		}
		return decisionDiscardNoise
	}
	if !p.voiced {
		if !listen && at >= endpointIdle {
			return decisionDiscardIdle
		}
		return decisionContinue
	}
	if p.inVoice {
		if p.lastVoicedAt-p.firstVoicedAt >= endpointMonologue {
			return decisionCommitMonologue
		}
		return decisionContinue
	}
	silence := at - p.lastVoicedAt
	if !p.hasWords {
		if silence >= capDur {
			return decisionCommitNoWords
		}
		return decisionContinue
	}
	if silence >= capDur {
		if smartTurn >= 0 && smartTurn < smartTurnHesitationThreshold &&
			(p.wordCount <= 5 || p.incomplete) && silence < endpointHesitationCap {
			return decisionContinue
		}
		return decisionCommitCap
	}
	if silence >= endpointQuiet && smartTurn >= smartTurnThreshold && !p.incomplete {
		return decisionCommitQuiet
	}
	return decisionContinue
}

// utterance accumulates the clip and keeps the policy clock continuous over
// discarded listening pre-roll.
type utterance struct {
	pcm          []byte
	limit        int
	scan         int
	policy       endpointPolicy
	preRoll      *[listenPreRollBytes]byte
	preHead      int
	preLen       int
	vadBuf       [endpointBytes]byte
	vadLen       int
	preScan      int
	speechOffset time.Duration
}

func utteranceBytes(span time.Duration) int { return sampleRate * 2 * int(span/time.Second) }

func pcmDuration(n int) time.Duration {
	return time.Duration(n/2) * time.Second / sampleRate
}

func (u *utterance) at(n int) time.Duration { return pcmDuration(n) + u.speechOffset }

func (u *utterance) add(pcm []byte, vad VAD) {
	limit := u.limit
	if limit <= 0 {
		limit = utteranceBytes(endpointLimit)
	}
	if room := limit - len(u.pcm); room > 0 {
		u.pcm = append(u.pcm, pcm[:min(len(pcm), room)]...)
	}
	for vad != nil && u.scan+endpointBytes <= len(u.pcm) {
		frame := u.pcm[u.scan : u.scan+endpointBytes]
		u.scan += endpointBytes
		u.policy.observeEnergy(u.at(u.scan), vad.Detect(frame), frameRMS(frame))
	}
}

func (u *utterance) pushPreRoll(pcm []byte) {
	if u.preRoll == nil {
		u.preRoll = new([listenPreRollBytes]byte)
	}
	if len(pcm) >= listenPreRollBytes {
		copy(u.preRoll[:], pcm[len(pcm)-listenPreRollBytes:])
		u.preHead, u.preLen = 0, listenPreRollBytes
		return
	}
	start := (u.preHead + u.preLen) % listenPreRollBytes
	first := min(len(pcm), listenPreRollBytes-start)
	copy(u.preRoll[start:], pcm[:first])
	if first < len(pcm) {
		copy(u.preRoll[:], pcm[first:])
	}
	if u.preLen+len(pcm) <= listenPreRollBytes {
		u.preLen += len(pcm)
	} else {
		drop := u.preLen + len(pcm) - listenPreRollBytes
		u.preHead = (u.preHead + drop) % listenPreRollBytes
		u.preLen = listenPreRollBytes
	}
}

func (u *utterance) flushPreRoll() {
	if u.preRoll == nil || u.preLen == 0 {
		return
	}
	u.speechOffset += pcmDuration(u.preScan) - pcmDuration(u.preLen)
	u.pcm = u.pcm[:0]
	first := min(u.preLen, listenPreRollBytes-u.preHead)
	u.pcm = append(u.pcm, u.preRoll[u.preHead:u.preHead+first]...)
	if first < u.preLen {
		u.pcm = append(u.pcm, u.preRoll[:u.preLen-first]...)
	}
	u.preHead, u.preLen, u.scan, u.preScan = 0, 0, len(u.pcm), 0
}

func (u *utterance) appendClip(pcm []byte) {
	limit := u.limit
	if limit <= 0 {
		limit = utteranceBytes(endpointLimit)
	}
	if room := limit - len(u.pcm); room > 0 {
		u.pcm = append(u.pcm, pcm[:min(len(pcm), room)]...)
	}
}

func (u *utterance) addFor(pcm []byte, vad VAD, listen bool) {
	if !listen || u.policy.voiced {
		u.add(pcm, vad)
		return
	}
	if vad == nil {
		u.pushPreRoll(pcm)
		return
	}
	pos := 0
	for pos < len(pcm) {
		take := min(len(pcm)-pos, endpointBytes-u.vadLen)
		copy(u.vadBuf[u.vadLen:], pcm[pos:pos+take])
		u.vadLen += take
		pos += take
		if u.vadLen < endpointBytes {
			continue
		}
		u.preScan += endpointBytes
		u.vadLen = 0
		u.policy.observeEnergy(u.at(u.preScan), vad.Detect(u.vadBuf[:]), frameRMS(u.vadBuf[:]))
		if !u.policy.voiced {
			u.pushPreRoll(u.vadBuf[:])
			continue
		}
		u.flushPreRoll()
		u.appendClip(u.vadBuf[:])
		u.scan = len(u.pcm)
		if pos < len(pcm) {
			u.add(pcm[pos:], vad)
		}
		return
	}
}

func (u *utterance) clip() []byte {
	if len(bytes.Trim(u.pcm, "\x00")) == 0 {
		return nil
	}
	return u.pcm
}

var fillerWords = map[string]bool{
	"uh": true, "um": true, "umm": true, "er": true, "erm": true,
	"ah": true, "eh": true, "oh": true, "hm": true, "hmm": true,
	"mm": true, "mmm": true, "mhm": true, "huh": true,
	"uh-huh": true, "mm-hmm": true,
}

func isIncompleteWord(w string) bool {
	switch w {
	case "and", "but", "or", "nor", "so", "because", "if", "when", "while", "until",
		"although", "though", "unless", "than", "that", "then",
		"a", "an", "the", "this", "these", "those", "my", "your", "his", "her",
		"its", "our", "their", "some", "any", "every", "each", "no",
		"to", "of", "in", "on", "at", "for", "from", "with", "by", "as", "into",
		"about", "over", "under", "after", "before", "between", "through",
		"is", "am", "are", "was", "were", "be", "been", "being", "do", "does",
		"did", "have", "has", "had", "will", "would", "can", "could", "should",
		"shall", "may", "might", "must",
		"um", "umm", "uh", "uhh", "er", "erm", "ah", "eh":
		return true
	}
	return false
}

func inspectTranscript(s string) (words int, fillerOnly, incomplete bool) {
	end := len(s)
	for end > 0 && strings.ContainsRune(" \t\n\r", rune(s[end-1])) {
		end--
	}
	if end == 0 {
		return 0, true, false
	}
	incompleteComma := s[end-1] == ','
	terminated := strings.ContainsRune(".,?!", rune(s[end-1]))
	var buf [32]byte
	var wlen, lastLen int
	var lastBuf [32]byte
	allFiller := true
	flush := func() {
		if wlen == 0 {
			return
		}
		words++
		w := string(buf[:wlen])
		if !fillerWords[w] {
			allFiller = false
		}
		copy(lastBuf[:], buf[:wlen])
		lastLen = wlen
		wlen = 0
	}
	for _, r := range s[:end] {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\'' || r == '-' {
			if r >= 'A' && r <= 'Z' {
				r += 'a' - 'A'
			}
			if wlen < len(buf) {
				if r < 128 {
					buf[wlen] = byte(r)
					wlen++
				} else {
					wlen += copy(buf[wlen:], string(unicode.ToLower(r)))
				}
			}
		} else {
			flush()
		}
	}
	flush()
	if words == 0 || (words <= 2 && allFiller) {
		return 0, true, false
	}
	lastWord := string(lastBuf[:lastLen])
	return words, false, incompleteComma || fillerWords[lastWord] || (!terminated && isIncompleteWord(lastWord))
}

func isFillerTranscript(s string) bool {
	words := strings.FieldsFunc(strings.ToLower(strings.TrimSpace(s)), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '\''
	})
	if len(words) == 0 {
		return true
	}
	if len(words) > 2 {
		return false
	}
	for _, w := range words {
		if !fillerWords[w] {
			return false
		}
	}
	return true
}
