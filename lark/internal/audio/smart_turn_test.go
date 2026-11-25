package audio

import "testing"

func TestWhisperMelZeroAlloc(t *testing.T) {
	m := newWhisperMel()
	pcm := make([]byte, SampleRate*2*2) // 2s
	out := make([]float32, smartTurnMels*smartTurnFrames)
	allocs := testing.AllocsPerRun(5, func() {
		m.extractPCM16(pcm, out)
	})
	if allocs != 0 {
		t.Fatalf("extractPCM16 allocated %.0f times, want 0", allocs)
	}
}
