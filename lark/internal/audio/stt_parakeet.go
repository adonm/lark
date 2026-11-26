package audio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode"
)

const (
	sttEngine      = "parakeet"
	ParakeetModel  = "parakeet-unified-en-0.6b-Q8_0.gguf"
	parakeetSHA256 = "4b50b6dd862bf6e346929aaf4f5eaacec003bfa3f56462d6c874b41ef2f38795"
)

func CheckParakeetModel(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return errors.New("Parakeet model missing; run scripts/setup-host.sh")
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil || hex.EncodeToString(sum.Sum(nil)) != parakeetSHA256 {
		return errors.New("Parakeet model does not match the pinned GGUF")
	}
	return nil
}

type sttProbeKey struct{}

func WithProbe(ctx context.Context) context.Context {
	return context.WithValue(ctx, sttProbeKey{}, true)
}

func isSTTProbe(ctx context.Context) bool {
	v, _ := ctx.Value(sttProbeKey{}).(bool)
	return v
}

// setSTT swaps the transcription backend. Tests change it mid-flight while
// capture probes read it.
// The host can check the installed TTS and STT together without creating a
// device, touching chat history, or routing audio to a board.
// Match Lark's WAV padding, then Hex's 1.5 s minimum and 400 ms
// trailing silence for Unified EN. Already-16 kHz mono audio is not resampled.
func parakeetSamples(pcm []byte) []float32 {
	return parakeetSamplesInto(nil, pcm)
}

func parakeetSamplesInto(dst []float32, pcm []byte) []float32 {
	const pad = SampleRate * 150 / 1000
	start, tail := 0, 0
	for _, b := range pcm {
		if b != 0 {
			start, tail = pad, pad
			break
		}
	}
	total := max(start+len(pcm)/2+tail, SampleRate*3/2) + SampleRate*400/1000
	if cap(dst) < total {
		dst = make([]float32, total)
	} else {
		dst = dst[:total]
		clear(dst)
	}
	for i := range len(pcm) / 2 {
		dst[start+i] = float32(int16(le.Uint16(pcm[2*i:]))) / 32768
	}
	return dst
}

// Hex strips these known subtitle hallucinations only at the very end.
var parakeetCredit = func() *regexp.Regexp {
	const sep = `[^\pL\pN]`
	credits := []string{
		"sous titrage société radio canada", "sous titrage st 501",
		"sous titres réalisés par la communauté d amara org",
		"sous titres par la communauté d amara org",
		"subtitles by the amara org community", "untertitel der amara org community",
	}
	for i, text := range credits {
		credits[i] = strings.ReplaceAll(text, " ", sep+"+")
	}
	return regexp.MustCompile(`(?i)(?:^|` + sep + `)(` + strings.Join(credits, "|") + `)` + sep + `*$`)
}()

func cleanParakeetText(text string) string {
	for {
		match := parakeetCredit.FindStringSubmatchIndex(text)
		if match == nil {
			return strings.TrimSpace(text)
		}
		text = strings.TrimRightFunc(text[:match[2]], func(r rune) bool {
			return unicode.IsSpace(r) || strings.ContainsRune(",;:-–—", r)
		})
	}
}
