package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"
)

const (
	sttEngine      = "parakeet"
	parakeetModel  = "parakeet-unified-en-0.6b-Q8_0.gguf"
	parakeetSHA256 = "4b50b6dd862bf6e346929aaf4f5eaacec003bfa3f56462d6c874b41ef2f38795"
)

func checkParakeetModel(path string) error {
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

type sttTiming struct {
	queue time.Duration
	run   time.Duration
}

type sttTimingKey struct{}

func withSTTTiming(ctx context.Context) (context.Context, *sttTiming) {
	t := &sttTiming{}
	return context.WithValue(ctx, sttTimingKey{}, t), t
}

func sttTimingFrom(ctx context.Context) *sttTiming {
	t, _ := ctx.Value(sttTimingKey{}).(*sttTiming)
	return t
}

func (b *larkServer) decodeClip(ctx context.Context, pcm []byte) (string, error) {
	if b.stt == nil {
		return "", errors.New("STT unavailable")
	}
	return b.stt(ctx, pcm)
}

// The host can check the installed TTS and STT together without creating a
// device, touching chat history, or routing audio to a board.
func (b *larkServer) probeSTT(r *http.Request) (object, error) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if !net.ParseIP(host).IsLoopback() {
		return nil, problem(404, "Not found")
	}
	if b.stt == nil || b.worker == nil || !b.worker.ready.Load() {
		return nil, problem(503, "Speech engines not ready")
	}
	ctx, cancel := context.WithTimeout(r.Context(), ttsWait)
	defer cancel()
	pcm, err := b.worker.synthesize(ctx, heartVoice, "The moon reflects sunlight.")
	if err != nil {
		return nil, problem(502, "STT probe synthesis failed")
	}
	start := time.Now()
	text, err := b.decodeClip(ctx, pcm)
	if err != nil {
		return nil, problem(502, "STT probe transcription failed")
	}
	return object{"transcript": text, "stt_ms": time.Since(start).Milliseconds(), "clip_ms": len(pcm) * 1000 / (2 * sampleRate)}, nil
}

// Match Lark's WAV padding, then Hex's 1.5 s minimum and 400 ms
// trailing silence for Unified EN. Already-16 kHz mono audio is not resampled.
func parakeetSamples(pcm []byte) []float32 {
	const pad = sampleRate * 150 / 1000
	start, tail := 0, 0
	for _, b := range pcm {
		if b != 0 {
			start, tail = pad, pad
			break
		}
	}
	samples := make([]float32, max(start+len(pcm)/2+tail, sampleRate*3/2)+sampleRate*400/1000)
	for i := range len(pcm) / 2 {
		samples[start+i] = float32(int16(le.Uint16(pcm[2*i:]))) / 32768
	}
	return samples
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
