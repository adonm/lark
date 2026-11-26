package audio

import (
	"slices"
	"sync"
	"testing"
)

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

// Concurrent extraction through the pooled scratch must not share FFT state:
// each goroutine's features have to match a single-threaded reference pass.
func TestMelScratchPoolConcurrent(t *testing.T) {
	pcm := make([]byte, SampleRate*2*3)
	for i := range pcm {
		pcm[i] = byte(i*31 + i/7)
	}
	want := make([]float32, smartTurnMels*smartTurnFrames)
	newWhisperMel().extractPCM16(pcm, want)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			scratch := melScratchPool.Get().(*melScratch)
			defer melScratchPool.Put(scratch)
			scratch.mel.extractPCM16(pcm, scratch.out)
			if !slices.Equal(scratch.out, want) {
				t.Error("pooled mel scratch differed from the reference pass")
			}
		}()
	}
	wg.Wait()
}
