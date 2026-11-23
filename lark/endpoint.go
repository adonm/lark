package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

const (
	endpointFrame = 20 * time.Millisecond
	endpointBytes = sampleRate / 50 * 2
	endpointLimit = 60 * time.Second
	endpointIdle  = 6 * time.Second
	endpointQuiet = 400 * time.Millisecond
	// Speculation starts on an earlier, weaker end-of-turn signal so the LLM
	// call overlaps the remaining quiet window instead of racing the commit.
	endpointSpecQuiet   = 180 * time.Millisecond
	endpointEarlyQuiet  = 400 * time.Millisecond // Must not preempt the 400 ms Smart Turn check that starts speculation.
	endpointCap         = 1200 * time.Millisecond
	endpointProbeEvery  = 250 * time.Millisecond
	endpointProbeWindow = 4 * sampleRate * 2 // last <= 4 s
	// A monologue with no pause still has to become a turn.
	endpointMonologue = 20 * time.Second
	vadTail           = 320 * time.Millisecond
	// Floor-relative gate margins, in RMS counts. The live boards deliver
	// speech around 20-25 RMS over a room floor of 1-4, so the margins are
	// small on purpose; Silero still owns the actual speech decision.
	gateMargin = 5.0
	// Calibration frames: one second of 20 ms frames.
	gateCalibFrames = 50
	// Below this fraction of the running speech level, VAD-positive audio is
	// room noise (its 320 ms tail, a fan, a bump), not a quieter word.
	speechEnergyRatio           = 0.12
	smartTurnThreshold          = float32(0.90)
	smartTurnEarlyThreshold     = float32(0.95)
	smartTurnSpeculateThreshold = float32(0.40)
	sileroModel                 = "silero_vad_v5.onnx"
	sileroSHA256                = "6b99cbfd39246b6706f98ec13c7c50c6b299181f2474fa05cbc8046acc274396"
)

// The native model is configured locally; do not download anything at startup.
func checkEndpointModel(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return errors.New("cannot read Silero v5 model; run scripts/setup-host.sh")
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != sileroSHA256 {
		return errors.New("Silero model does not match the pinned Silero v5 ONNX")
	}
	return nil
}

type endpointVAD interface {
	detect([]byte) bool
	close()
}

type sileroVAD struct {
	model *sherpa.VoiceActivityDetector
	frame [sampleRate / 50]float32
}

func newSileroVAD(path string) endpointVAD {
	model := sherpa.NewVoiceActivityDetector(&sherpa.VadModelConfig{
		SileroVad: sherpa.SileroVadModelConfig{
			Model: path, Threshold: 0.5, WindowSize: 512,
			MinSilenceDuration: float32(vadTail.Seconds()), MinSpeechDuration: 0.12, MaxSpeechDuration: 60,
		},
		SampleRate: sampleRate, Provider: "cpu", NumThreads: 1,
	}, 10)
	if model == nil {
		return nil
	}
	return &sileroVAD{model: model}
}

func (s *sileroVAD) detect(pcm []byte) bool {
	for i := range s.frame {
		s.frame[i] = float32(int16(le.Uint16(pcm[2*i:]))) / 32768
	}
	s.model.AcceptWaveform(s.frame[:])
	voice := s.model.IsSpeech()
	for !s.model.IsEmpty() {
		s.model.Pop()
	}
	return voice
}

func (s *sileroVAD) close() { sherpa.DeleteVoiceActivityDetector(s.model) }

type endpointDecision uint8

const (
	endpointWait endpointDecision = iota
	endpointCommit
	endpointDiscard
)

type endpointPolicy struct {
	voiced     bool
	inVoice    bool
	speechRMS  float64
	hasWords   bool
	incomplete bool
	wordCount  int
	lastWordAt time.Duration
	lastText   string
	// Silence-driven endpointing. lastVoicedAt only advances on frames that
	// are both VAD-positive and above the adaptive noise floor, so transcript
	// churn on room noise cannot hold a turn open.
	lastVoicedAt  time.Duration
	firstVoicedAt time.Duration
	speechFrames  int
	vadFrames     int
	gate          energyGate
}

// energyGate learns the room noise floor when the window opens and keeps
// tracking it during non-speech. A fixed relative-energy floor misses quiet,
// distant speech while steady background noise can hold an absolute gate open.
type energyGate struct {
	buf   [gateCalibFrames]float64
	n     int
	next  int
	floor float64
	ready bool
}

func (g *energyGate) observe(rms float64, vad, calibrate bool) {
	if !vad {
		if calibrate {
			// Only a stretch of non-speech may teach the floor. A window that
			// opens mid-sentence keeps the conservative zero floor until the
			// room is actually quiet.
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
		// Fast downward tracking keeps a new quiet room from being mistaken
		// for speech; slow upward tracking follows drifting noise.
		if rms < g.floor {
			g.floor = rms
		} else {
			g.floor += (rms - g.floor) * 0.002
		}
	}
}

func (g *energyGate) enter() float64 {
	return g.floor + max(gateMargin, g.floor)
}

func (g *energyGate) stay() float64 {
	return g.floor + max(gateMargin*0.6, g.floor*0.5)
}

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

func (p *endpointPolicy) observeEnergy(at time.Duration, vadSpeech bool, rms float64) {
	if !vadSpeech {
		// Learn the floor only before the first word or after a full second of
		// quiet, so a VAD miss on loud speech cannot teach it speech level.
		p.gate.observe(rms, false, !p.voiced || at-p.lastVoicedAt >= time.Second)
		p.inVoice = false
		return
	}
	p.vadFrames++
	if p.voiced && rms < p.speechRMS*speechEnergyRatio {
		// Let the floor learn noise that the VAD keeps calling speech; the
		// ratio also closes the gate on a loud room after a loud word.
		p.gate.observe(rms, false, false)
		p.inVoice = false
		return
	}
	p.gate.observe(rms, true, false)
	// Enter above floor+margin, then keep a lower sustain threshold so brief
	// dips inside a word or a quiet breath do not end the segment.
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

// A transcript ending in one of these is still mid-thought. Committing the
// 400 ms quiet path there is how "can you put his..." reached the model.
func isIncompleteWord(w string) bool {
	switch w {
	case // conjunctions and subordinators
		"and", "but", "or", "nor", "so", "because", "if", "when", "while", "until",
		"although", "though", "unless", "than", "that", "then",
		// articles, possessives, determiners
		"a", "an", "the", "this", "these", "those", "my", "your", "his", "her",
		"its", "our", "their", "some", "any", "every", "each", "no",
		// prepositions
		"to", "of", "in", "on", "at", "for", "from", "with", "by", "as", "into",
		"about", "over", "under", "after", "before", "between", "through",
		// auxiliaries and copulas
		"is", "am", "are", "was", "were", "be", "been", "being", "do", "does",
		"did", "have", "has", "had", "will", "would", "can", "could", "should",
		"shall", "may", "might", "must",
		// fillers
		"um", "umm", "uh", "uhh", "er", "erm", "ah", "eh":
		return true
	}
	return false
}

func inspectTranscript(s string) (words int, fillerOnly, incomplete bool) {
	end := len(s)
	for end > 0 && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	if end == 0 {
		return 0, true, false
	}
	incompleteComma := s[end-1] == ','
	var buf [32]byte
	var wlen int
	var lastLen int
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
	return words, false, incompleteComma || isIncompleteWord(lastWord)
}

func (p *endpointPolicy) observeProgress(probeAt time.Duration, text string, windowed bool) bool {
	words, fillerOnly, incomplete := inspectTranscript(text)
	if fillerOnly || words == 0 {
		return false
	}
	p.incomplete = incomplete
	if !p.hasWords {
		p.hasWords = true
		p.wordCount = words
		p.lastWordAt = probeAt
		p.lastText = text
		return true
	}
	// A probe that re-hears the same clip can replace an earlier guess with a
	// same-length transcript. That is still progress for completeness (and a
	// reason to abort speculation); only shrinkage is ignored.
	if !windowed {
		if words >= p.wordCount && text != p.lastText {
			p.wordCount = words
			p.lastWordAt = probeAt
			p.lastText = text
			return true
		}
		return false
	}
	if words != p.wordCount || text != p.lastText {
		p.wordCount = words
		p.lastWordAt = probeAt
		p.lastText = text
		return true
	}
	return false
}

// terminalPunctuation reports whether the transcript closes a sentence. Smart
// Turn alone can fire on a plausible clause break, so the early commit also
// requires Parakeet to have heard the end of one.
func terminalPunctuation(text string) bool {
	text = strings.TrimSpace(text)
	text = strings.TrimRight(text, `\"'”’`)
	if text == "" {
		return false
	}
	switch text[len(text)-1] {
	case '.', '?', '!':
		return true
	}
	return false
}

func (p endpointPolicy) decision(at, capDur, limitDur time.Duration, smartTurnReady, listen bool) endpointDecision {
	var prob float32
	if smartTurnReady {
		prob = smartTurnThreshold
	}
	return p.decide(at, capDur, limitDur, prob, false, listen)
}

func (p endpointPolicy) decide(at, capDur, limitDur time.Duration, smartTurn float32, early bool, listen bool) endpointDecision {
	if at >= limitDur {
		if p.speechFrames > 0 {
			return endpointCommit
		}
		// Noise that never crossed the speech gate must not become a turn.
		return endpointDiscard
	}
	if !p.voiced {
		if !listen && at >= endpointIdle {
			return endpointDiscard
		}
		return endpointWait
	}
	if p.inVoice {
		// A monologue should not wait for silence past this point.
		if p.lastVoicedAt-p.firstVoicedAt >= endpointMonologue {
			return endpointCommit
		}
		return endpointWait
	}
	// The segment is over acoustically. Transcript growth on quiet audio is
	// not allowed to extend this: only new voiced frames can.
	silence := at - p.lastVoicedAt
	if !p.hasWords {
		// Speech was heard but no usable transcript arrived; commit so the
		// caller can say "didn't catch that" instead of dropping it silently.
		if silence >= capDur {
			return endpointCommit
		}
		return endpointWait
	}
	if silence >= capDur {
		return endpointCommit
	}
	if silence >= endpointQuiet && smartTurn >= smartTurnThreshold && !p.incomplete {
		return endpointCommit
	}
	if silence >= endpointEarlyQuiet && early && !p.incomplete {
		return endpointCommit
	}
	return endpointWait
}

// speechSpan is how much speech-like audio this segment contained. It decides
// whether an unusable transcript deserves feedback ("didn't catch that") or
// was just a cough, door, or sub-threshold noise.
func (p endpointPolicy) speechSpan() time.Duration {
	return pcmDuration(p.speechFrames * endpointBytes)
}

// One capture worker owns this accumulator. Keep the original clip for the
// same whole-clip STT path even if VAD is unavailable.
type utterance struct {
	pcm     []byte
	limit   int // byte cap for this capture; zero uses the single-utterance cap
	scan    int
	policy  endpointPolicy
	preRoll *[listenPreRollBytes]byte
	preHead int
	preLen  int
	vadBuf  [endpointBytes]byte
	vadLen  int
	preScan int
}

func utteranceBytes(span time.Duration) int {
	return sampleRate * 2 * int(span/time.Second)
}

func (u *utterance) add(pcm []byte, vad endpointVAD) {
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
		u.policy.observeEnergy(pcmDuration(u.scan), vad.detect(frame), frameRMS(frame))
	}
}

const listenPreRollBytes = sampleRate * 2 * 400 / 1000

func (u *utterance) pushPreRoll(pcm []byte) {
	if u.preRoll == nil {
		u.preRoll = new([listenPreRollBytes]byte)
	}
	if len(pcm) >= listenPreRollBytes {
		copy(u.preRoll[:], pcm[len(pcm)-listenPreRollBytes:])
		u.preHead = 0
		u.preLen = listenPreRollBytes
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
	u.pcm = u.pcm[:0]
	first := min(u.preLen, listenPreRollBytes-u.preHead)
	u.pcm = append(u.pcm, u.preRoll[u.preHead:u.preHead+first]...)
	if first < u.preLen {
		u.pcm = append(u.pcm, u.preRoll[:u.preLen-first]...)
	}
	u.preHead = 0
	u.preLen = 0
	u.scan = len(u.pcm)
}

func (u *utterance) addFor(pcm []byte, vad endpointVAD, listen bool) {
	if !listen || u.policy.voiced {
		u.add(pcm, vad)
		return
	}
	u.pushPreRoll(pcm)
	if vad == nil {
		return
	}
	pos := 0
	for pos < len(pcm) {
		take := min(len(pcm)-pos, endpointBytes-u.vadLen)
		copy(u.vadBuf[u.vadLen:], pcm[pos:pos+take])
		u.vadLen += take
		pos += take
		if u.vadLen == endpointBytes {
			u.preScan += endpointBytes
			u.vadLen = 0
			u.policy.observeEnergy(pcmDuration(u.preScan), vad.detect(u.vadBuf[:]), frameRMS(u.vadBuf[:]))
			if u.policy.voiced {
				u.flushPreRoll()
				if pos < len(pcm) {
					u.add(pcm[pos:], vad)
				}
				return
			}
		}
	}
}

var fillerWords = map[string]bool{
	"uh": true, "um": true, "umm": true, "er": true, "erm": true,
	"ah": true, "eh": true, "oh": true, "hm": true, "hmm": true,
	"mm": true, "mmm": true, "mhm": true, "huh": true,
	"uh-huh": true, "mm-hmm": true,
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

func pcmDuration(n int) time.Duration {
	return time.Duration(n/2) * time.Second / sampleRate
}

func (u *utterance) clip() []byte {
	if len(bytes.Trim(u.pcm, "\x00")) == 0 {
		return nil
	}
	return u.pcm
}
