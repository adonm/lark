package audio

import (
	"bytes"
	"math"
	"testing"
)

func testSamples(n int) []float32 {
	raw := make([]float32, n)
	for i := range raw {
		// A mix of tones and an envelope so every filter phase sees signal.
		raw[i] = float32(0.55*math.Sin(float64(i)*0.037) + 0.2*math.Sin(float64(i)*0.31))
	}
	return raw
}

func streamResample(raw []float32, chunks []int) []byte {
	r := NewResampler(KokoroRate)
	var out []byte
	rest := raw
	for _, n := range chunks {
		if n > len(rest) {
			n = len(rest)
		}
		out = r.Push(out, rest[:n])
		rest = rest[n:]
	}
	if len(rest) > 0 {
		out = r.Push(out, rest)
	}
	return r.Flush(out)
}

func TestResamplerMatchesFullSynthesis(t *testing.T) {
	raw := testSamples(5000)
	want, err := SynthPCM(raw, KokoroRate)
	if err != nil {
		t.Fatal(err)
	}
	splits := map[string][]int{
		"whole":   {len(raw)},
		"single":  repeat(1, len(raw)),
		"uneven":  {7, 1, 160, 31, 500, 2048},
		"clauses": {160, 160, 160, 160, 160},
		"large":   {4096, 904},
	}
	for name, chunks := range splits {
		got := streamResample(raw, chunks)
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: streamed PCM differs from full synthesis (got %d bytes, want %d)", name, len(got), len(want))
		}
	}
}

func TestResamplerPhaseCarriesAcrossChunks(t *testing.T) {
	raw := testSamples(1000)
	want, err := SynthPCM(raw, KokoroRate)
	if err != nil {
		t.Fatal(err)
	}
	r := NewResampler(KokoroRate)
	var out []byte
	for i := 0; i < len(raw); i += 3 {
		end := min(i+3, len(raw))
		before := len(out)
		out = r.Push(out, raw[i:end])
		// Some chunks are too short to complete a filter window; the phase and
		// history must carry into the next call instead of resetting.
		if i == 0 && len(out) != before {
			t.Fatalf("first 3-sample chunk emitted %d bytes, want 0", len(out)-before)
		}
	}
	out = r.Flush(out)
	if !bytes.Equal(out, want) {
		t.Fatal("phase-carry output differs from full synthesis")
	}
}

func TestResamplerFlushIsIdempotent(t *testing.T) {
	raw := testSamples(200)
	r := NewResampler(KokoroRate)
	out := r.Flush(r.Push(nil, raw))
	if again := r.Flush(out); len(again) != len(out) {
		t.Fatalf("second flush appended %d bytes", len(again)-len(out))
	}
}

func TestResamplerPassthroughAt16k(t *testing.T) {
	raw := testSamples(500)
	want, err := SynthPCM(raw, SampleRate)
	if err != nil {
		t.Fatal(err)
	}
	r := NewResampler(SampleRate)
	var out []byte
	out = r.Push(out, raw[:123])
	out = r.Push(out, raw[123:])
	out = r.Flush(out)
	if !bytes.Equal(out, want) {
		t.Fatal("16 kHz passthrough differs from batch conversion")
	}
}

func repeat(n, count int) []int {
	out := make([]int, count/n)
	for i := range out {
		out[i] = n
	}
	return out
}
