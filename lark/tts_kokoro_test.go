package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const kokoroSample = "Come along, little fox, and let's look for the stars."

func kokoroTestDir(t testing.TB) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := ttsModelDir(filepath.Join(env("XDG_DATA_HOME", filepath.Join(home, ".local/share")), "lark"))
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		t.Skip("Kokoro model pack is not installed")
	} else if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestKokoroReal(t *testing.T) {
	t.Parallel()
	k, err := openKokoro(kokoroTestDir(t), kokoroThreads)
	if err != nil {
		t.Fatal(err)
	}
	defer k.close()

	var heard [][]byte
	for _, voice := range voiceCatalog {
		t.Run(voice.Name, func(t *testing.T) {
			start := time.Now()
			pcm, err := synthesizePCM(t.Context(), k, voice.ID, kokoroSample)
			elapsed := time.Since(start).Seconds()
			seconds := float64(len(pcm)) / (2 * sampleRate)
			if err != nil || seconds < 1 || seconds > 8 {
				t.Fatalf("invalid clip: duration %.2fs, error %v", seconds, err)
			}
			var sum, peak float64
			for i := 0; i < len(pcm); i += 2 {
				v := float64(int16(le.Uint16(pcm[i:]))) / 32768
				sum += v * v
				peak = max(peak, math.Abs(v))
			}
			rms := math.Sqrt(sum / float64(len(pcm)/2))
			if rms < 0.01 || peak >= 0.90 {
				t.Fatalf("silent or clipped audio: RMS %.3f, peak %.3f", rms, peak)
			}
			for _, previous := range heard {
				if bytes.Equal(pcm, previous) {
					t.Fatal("voice rendered the same audio as another speaker")
				}
			}
			heard = append(heard, pcm)
			t.Logf("%.1fms, %.2fs audio, RTF %.3f, RMS %.3f, peak %.3f", elapsed*1000, seconds, elapsed/seconds, rms, peak)
		})
	}

	t.Run("EnglishAccents", func(t *testing.T) {
		const text = "The little otter took a bath after dancing in the park."
		voice := voiceByID(heartVoice)
		us, rate := k.generate(text, voice, func() bool { return true })
		voice.Lang = "en-gb"
		gb, gbRate := k.generate(text, voice, func() bool { return true })
		if rate != kokoroRate || gbRate != kokoroRate || len(us) == 0 || len(gb) == 0 {
			t.Fatal("missing English audio")
		}
		if len(us) == len(gb) {
			t.Fatal("the accent-sensitive fixture had the same duration in both accents")
		}
		t.Logf("same speaker: US %.2fs, UK %.2fs", float64(len(us))/kokoroRate, float64(len(gb))/kokoroRate)
	})

	for _, fixture := range []struct{ name, text string }{
		{"CancelNativeBatch", "The little fox found a star. It gave a big cheer. Then it curled up and went to sleep."},
		{"CancelLongSentence", strings.Repeat("The little fox went looking for the stars, ", 7) + "and then it curled up and went to sleep."},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			voice := voiceByID(heartVoice)
			chunks := 0
			fullStart := time.Now()
			full, _ := k.generate(fixture.text, voice, func() bool { chunks++; return true })
			fullTime := time.Since(fullStart)
			if chunks < 2 {
				t.Fatalf("cancellation fixture must span native batches, got %d", chunks)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			stoppedAt, canceledChunks := 0, 0
			engine := fakeSpeechEngine(func(text string, voice voiceChoice, keepGoing func() bool) ([]float32, int) {
				samples, rate := k.generate(text, voice, func() bool {
					canceledChunks++
					cancel()
					return keepGoing()
				})
				stoppedAt = len(samples)
				return samples, rate
			})
			start := time.Now()
			pcm, err := synthesizePCM(ctx, engine, voice.ID, fixture.text)
			if !errors.Is(err, context.Canceled) || pcm != nil || canceledChunks != 1 || stoppedAt == 0 || stoppedAt >= len(full) {
				t.Fatalf("did not stop after first native batch: %d/%d samples, %d callbacks, %v", stoppedAt, len(full), canceledChunks, err)
			}
			t.Logf("callback stopped after %d/%d samples (%v vs %v)", stoppedAt, len(full), time.Since(start), fullTime)
		})
	}
}

func BenchmarkKokoroThreads(b *testing.B) {
	dir := kokoroTestDir(b)
	for _, threads := range []int{4, 6, 8, 12} {
		b.Run(fmt.Sprint(threads), func(b *testing.B) {
			k, err := openKokoro(dir, threads)
			if err != nil {
				b.Fatal(err)
			}
			defer k.close()
			for _, voice := range voiceCatalog {
				b.Run(voice.Name, func(b *testing.B) {
					ctx := b.Context()
					if _, err := synthesizePCM(ctx, k, voice.ID, kokoroSample); err != nil {
						b.Fatal(err)
					}
					total := 0
					for b.Loop() {
						pcm, err := synthesizePCM(ctx, k, voice.ID, kokoroSample)
						if err != nil {
							b.Fatal(err)
						}
						total += len(pcm)
					}
					seconds := float64(total) / (2 * sampleRate)
					b.ReportMetric(seconds/float64(b.N), "audio-s/op")
					b.ReportMetric(b.Elapsed().Seconds()/seconds, "RTF")
				})
			}
		})
	}
}
