package audio

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

const (
	KokoroPack    = "kokoro-multi-lang-v1_0"
	KokoroThreads = 8
	KokoroRate    = 24000
)

func TTSModelDir(local string) string {
	return env("TTS_MODEL_DIR", filepath.Join(local, "tts-models", KokoroPack))
}

type Kokoro struct{ tts *sherpa_onnx.OfflineTts }

func OpenKokoro(dir string, threads int) (*Kokoro, error) {
	model := sherpa_onnx.OfflineTtsKokoroModelConfig{
		Model: filepath.Join(dir, "model.onnx"), Voices: filepath.Join(dir, "voices.bin"),
		Tokens: filepath.Join(dir, "tokens.txt"), DataDir: filepath.Join(dir, "espeak-ng-data"),
		Lang: "en-us", LengthScale: 1,
	}
	for _, path := range []string{model.Model, model.Voices, model.Tokens, filepath.Join(model.DataDir, "phondata")} {
		if info, err := os.Stat(path); err != nil {
			return nil, err
		} else if !info.Mode().IsRegular() || info.Size() == 0 {
			return nil, fmt.Errorf("invalid Kokoro model asset: %s", path)
		}
	}
	tts := sherpa_onnx.NewOfflineTts(&sherpa_onnx.OfflineTtsConfig{
		Model:           sherpa_onnx.OfflineTtsModelConfig{Kokoro: model, Provider: "cpu", NumThreads: threads},
		MaxNumSentences: 1, SilenceScale: 0.2,
	})
	if tts == nil {
		return nil, fmt.Errorf("could not load Kokoro model from %s", dir)
	}
	if rate := tts.SampleRate(); rate != KokoroRate {
		sherpa_onnx.DeleteOfflineTts(tts)
		return nil, fmt.Errorf("Kokoro model sample rate is %d Hz, want %d Hz", rate, KokoroRate)
	}
	return &Kokoro{tts}, nil
}

func (k *Kokoro) Generate(text string, voice Voice, keepGoing func() bool) ([]float32, int) {
	lang := voice.Lang
	if lang == "en-gb" {
		// The model pack's British eSpeak voice is the file lang/gmw/en.
		lang = "en"
	}
	config := sherpa_onnx.GenerationConfig{
		Sid: voice.SID, Speed: 1, SilenceScale: 0.2, Extra: []byte(`{"lang":"` + lang + `"}`),
	}
	more := true
	callback := func(_ []float32, _ float32) bool {
		more = keepGoing()
		return more
	}
	var samples []float32
	for text != "" && more {
		var chunk string
		chunk, text = splitKokoroText(text)
		audio := k.tts.GenerateWithConfig(chunk, &config, callback)
		if audio == nil || len(audio.Samples) == 0 || audio.SampleRate != KokoroRate {
			return nil, 0
		}
		if samples == nil {
			samples = audio.Samples
		} else {
			samples = append(samples, audio.Samples...)
		}
	}
	return samples, KokoroRate
}

// Native callbacks cannot preempt a single ONNX inference. Break long sentences
// at a clause boundary if possible, or between words, to bound that work.
func splitKokoroText(text string) (chunk, rest string) {
	const limit = 160
	if len(text) <= limit {
		return text, ""
	}
	prefix := text[:limit+1]
	split := strings.LastIndexByte(prefix, ' ')
	if split <= 0 {
		return text, ""
	}
	for i := split; i > limit/2; i-- {
		if prefix[i] == ' ' && strings.ContainsRune(",;:.?!", rune(prefix[i-1])) {
			split = i
			break
		}
	}
	return text[:split], text[split+1:]
}

func (k *Kokoro) Close() { sherpa_onnx.DeleteOfflineTts(k.tts) }
