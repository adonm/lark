package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"time"
	"unicode"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

const (
	endpointFrame = 20 * time.Millisecond
	endpointBytes = sampleRate / 50 * 2
	endpointLimit = 60 * time.Second
	endpointIdle  = 7 * time.Second
	vadTail       = 320 * time.Millisecond
	sileroModel   = "silero_vad_v5.onnx"
	sileroSHA256  = "6b99cbfd39246b6706f98ec13c7c50c6b299181f2474fa05cbc8046acc274396"
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
	voiced  bool
	inVoice bool
	first   time.Duration
	last    time.Duration // Estimated acoustic end (Silero includes a tail).
}

func (p *endpointPolicy) observe(at time.Duration, speech bool) {
	p.inVoice = speech
	if speech {
		if !p.voiced {
			p.first = at
		}
		p.voiced = true
		p.last = max(0, at-vadTail)
	}
}

func (p endpointPolicy) decision(at, silence time.Duration) endpointDecision {
	if at >= endpointLimit || !p.voiced && at >= endpointIdle {
		if p.voiced {
			return endpointCommit
		}
		return endpointDiscard
	}
	if p.paused(at, silence) {
		return endpointCommit
	}
	return endpointWait
}

func (p endpointPolicy) paused(at, silence time.Duration) bool {
	return p.voiced && !p.inVoice && at-p.last >= silence
}

type endpointThresholds struct {
	probe, quick, complete, uncertain, patient, storySpan time.Duration
	minWords, storyWords                                  int
}

var tunedEndpoint = endpointThresholds{
	probe: 450 * time.Millisecond, quick: 650 * time.Millisecond,
	complete: 1100 * time.Millisecond, uncertain: 1700 * time.Millisecond, patient: 4500 * time.Millisecond,
	storySpan: 8 * time.Second, minWords: 3, storyWords: 23,
}

func (c endpointThresholds) silence(p endpointPolicy, text string) time.Duration {
	text = strings.TrimSpace(text)
	body := strings.TrimRight(text, `"'’”)]`)
	if p.last-p.first >= c.storySpan || strings.HasSuffix(body, "..") || strings.HasSuffix(body, "…") {
		return c.patient
	}
	words := strings.FieldsFunc(strings.ReplaceAll(strings.ToLower(body), "’", "'"), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	})
	if len(words) < 2 || len(words) >= c.storyWords {
		return c.patient
	}
	wordCount := len(words)
	switch words[0] {
	case "what's", "how's", "where's", "who's", "when's":
		wordCount++ // The leading contraction includes the finite verb.
	}
	if wordCount < c.minWords {
		return c.patient
	}
	normalized := strings.Join(words, " ")
	for _, start := range []string{"once upon a time", "one day", "this is a story", "let me tell you", "here's a story",
		"i have a story", "so there was", "there was", "there were", "and then", "yesterday", "last night",
		"i found", "i saw", "i went", "i was", "if", "while", "because", "what if", "first", "the first"} {
		if normalized == start || strings.HasPrefix(normalized, start+" ") {
			return c.patient
		}
	}
	switch words[len(words)-1] {
	case "a", "an", "the", "and", "then", "or", "but", "so", "because", "when", "if",
		"to", "of", "in", "on", "with", "for", "about", "at", "from", "by", "into", "that",
		"who", "which", "named", "called", "like", "uh", "um", "er", "erm", "ah", "well", "hmm":
		return c.patient
	}
	for _, tail := range []string{"you know", "i mean", "let's see", "let me think", "what was it", "sort of", "kind of"} {
		if strings.HasSuffix(normalized, " "+tail) {
			return c.patient
		}
	}
	if words[0] == "when" {
		switch words[1] {
		case "is", "are", "was", "were", "will", "can", "do", "did", "does", "should":
		default:
			return c.patient // "When I was nine" is not a question.
		}
	}
	if words[len(words)-2] == "and" || words[len(words)-2] == "or" {
		return c.patient
	}
	switch {
	case strings.HasSuffix(body, ".") || strings.HasSuffix(body, "?") || strings.HasSuffix(body, "!"):
	case strings.HasSuffix(body, ",") || strings.HasSuffix(body, ":") || strings.HasSuffix(body, ";") ||
		strings.HasSuffix(body, "-") || strings.HasSuffix(body, "—"):
		return c.patient
	default:
		// Parakeet sometimes omits punctuation even on short finished turns.
		// Wait longer than for a complete turn, without assuming a story.
		return c.uncertain
	}
	// A possible sentence ("Can you draw a dragon?") may still take a
	// modifier. Give requests and statements more room than explicit questions.
	if strings.HasSuffix(body, "?") {
		switch words[0] {
		case "what", "why", "how", "where", "who", "which", "when", "what's", "how's", "where's", "who's", "when's":
			return c.quick
		}
	}
	switch words[len(words)-1] {
	case "second", "seconds", "minute", "minutes", "hour", "hours":
		for _, prefix := range []string{"could you ", "can you ", "please "} {
			normalized = strings.TrimPrefix(normalized, prefix)
		}
		if len(words) >= 6 && (strings.HasPrefix(normalized, "start a timer for ") || strings.HasPrefix(normalized, "set a timer for ")) {
			return c.quick
		}
	}
	return c.complete
}

// One capture worker owns this accumulator. Keep the original clip for the
// same whole-clip STT path even if VAD is unavailable.
type utterance struct {
	pcm    []byte
	scan   int
	policy endpointPolicy
}

func (u *utterance) add(pcm []byte, vad endpointVAD) {
	const limit = sampleRate * 2 * int(endpointLimit/time.Second)
	u.pcm = append(u.pcm, pcm[:min(len(pcm), limit-len(u.pcm))]...)
	for vad != nil && u.scan+endpointBytes <= len(u.pcm) {
		frame := u.pcm[u.scan : u.scan+endpointBytes]
		u.scan += endpointBytes
		u.policy.observe(pcmDuration(u.scan), vad.detect(frame))
	}
}

const listenPreRollBytes = sampleRate * 2 * 400 / 1000

func (u *utterance) addFor(pcm []byte, vad endpointVAD, listen bool) {
	u.add(pcm, vad)
	if listen && !u.policy.voiced && len(u.pcm) > listenPreRollBytes {
		trim := len(u.pcm) - listenPreRollBytes
		trim &^= 1
		if trim > u.scan {
			trim = u.scan
		}
		if trim > 0 {
			copy(u.pcm, u.pcm[trim:])
			u.pcm = u.pcm[:len(u.pcm)-trim]
			u.scan -= trim
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
