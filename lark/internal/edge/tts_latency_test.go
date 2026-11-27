package edge

import (
	"os"
	"testing"
	"time"

	"lark/internal/audio"
)

// batchOnlyTTS hides the streaming API so the edge takes the pre-change
// full-synthesis path. Comparing both wrappers around the same loaded model
// isolates the first-audio latency win from model load time.
type batchOnlyTTS struct{ tts *audio.Kokoro }

func (b batchOnlyTTS) Generate(text string, voice audio.Voice, keepGoing func() bool) ([]float32, int) {
	return b.tts.Generate(text, voice, keepGoing)
}

func (b batchOnlyTTS) Close() {}

// TestKokoroFirstAudioLatency measures the time from a speak request to the
// first PCM frame queued for the wire, for the batch and streamed paths.
// Run with LARK_TTS_LATENCY=1 on a machine that has the Kokoro model.
func TestKokoroFirstAudioLatency(t *testing.T) {
	if os.Getenv("LARK_TTS_LATENCY") == "" {
		t.Skip("set LARK_TTS_LATENCY=1 to measure with the real Kokoro model")
	}
	kokoro, err := audio.OpenKokoro(audio.TTSModelDir(defaultDataDir()), audio.KokoroThreads)
	if err != nil {
		t.Skipf("Kokoro model unavailable: %v", err)
	}
	defer kokoro.Close()

	texts := []struct{ name, text string }{
		{"short", "Hello there."},
		{"long", "The quick brown fox jumps over the lazy dog. Pack my box with five dozen liquor jugs. How vexingly quick daft zebras jump!"},
	}
	const reps = 3

	// Load and warm both code paths once so the first measurement is not a
	// cold model page-in.
	for _, tc := range texts {
		_ = measureFirstAudio(t, kokoro, tc.text)
		_ = measureFirstAudio(t, batchOnlyTTS{kokoro}, tc.text)
	}

	for _, tc := range texts {
		var batch, stream time.Duration
		for range reps {
			batch += measureFirstAudio(t, batchOnlyTTS{kokoro}, tc.text)
			stream += measureFirstAudio(t, kokoro, tc.text)
		}
		t.Logf("%s (%d chars): batch=%s stream=%s", tc.name, len(tc.text), batch/reps, stream/reps)
	}
}

func measureFirstAudio(t *testing.T, synth Synthesizer, text string) time.Duration {
	t.Helper()
	st := newStreamTerminal(synth)
	started := time.Now()
	go st.s.run()
	st.s.push(text)
	select {
	case <-st.nc.pcmOut:
	case <-time.After(30 * time.Second):
		st.s.cancel()
		t.Fatal("timed out waiting for first PCM frame")
	}
	elapsed := time.Since(started)
	st.s.cancel()
	<-st.s.done
	return elapsed
}
