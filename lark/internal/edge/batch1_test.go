package edge

import (
	"slices"
	"sync"
	"testing"
)

func TestProbeScratchReusesTailAndResetsOnGeneration(t *testing.T) {
	var p probeScratch
	pcm := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	first := p.fill(pcm, 4, 1)
	if !slices.Equal(first, pcm[:4]) {
		t.Fatalf("first clip = %v", first)
	}
	base := &first[0]

	second := p.fill(pcm, 8, 1)
	if !slices.Equal(second, pcm) {
		t.Fatalf("grown clip = %v", second)
	}
	if &second[0] != base {
		t.Fatal("probe scratch reallocated while the utterance only grew")
	}

	// A listening pre-roll reset restarts u.pcm, so the cached prefix is stale.
	reset := p.fill(pcm[4:], 4, 2)
	if !slices.Equal(reset, pcm[4:]) {
		t.Fatalf("generation reset clip = %v", reset)
	}

	// Shrinking without a generation change still copies the new prefix.
	short := p.fill(pcm[:2], 2, 2)
	if !slices.Equal(short, pcm[:2]) {
		t.Fatalf("shrunk clip = %v", short)
	}
}

func TestFrameBuffersReuseTwoSlots(t *testing.T) {
	var b frameBuffers
	first := b.scratch(360, 360)
	if len(first) != 360*360 || b.valid {
		t.Fatalf("first scratch len=%d valid=%v", len(first), b.valid)
	}
	first[0] = 0xf800
	firstBase := &first[0]
	b.commit()
	if !b.valid || b.front[0] != 0xf800 {
		t.Fatal("commit did not promote the scratch frame")
	}

	second := b.scratch(360, 360)
	if &second[0] == firstBase {
		t.Fatal("scratch reused the visible front buffer")
	}
	second[0] = 0x07e0
	b.commit()
	third := b.scratch(360, 360)
	if &third[0] != firstBase {
		t.Fatal("third frame did not recycle the original front buffer")
	}

	// A resize invalidates the diff base and allocates a matching pair.
	resized := b.scratch(180, 180)
	if len(resized) != 180*180 || b.valid {
		t.Fatalf("resized scratch len=%d valid=%v", len(resized), b.valid)
	}
	if &resized[0] == firstBase {
		t.Fatal("resize kept the old framebuffer")
	}
}

func TestNativeConnFrameSlotReusesMsg(t *testing.T) {
	c := &nativeConn{frameNotify: make(chan struct{}, 1)}
	one := []byte{1, 2, 3}
	two := []byte{4, 5}
	c.enqueueFrame(one)
	if msg, ok := c.takeFrame(); !ok || !slices.Equal(msg.payload, one) {
		t.Fatalf("first frame = %v ok=%v", msg.payload, ok)
	}
	if _, ok := c.takeFrame(); ok {
		t.Fatal("frame slot was not cleared")
	}
	c.enqueueFrame(one)
	c.enqueueFrame(two)
	if msg, ok := c.takeFrame(); !ok || !slices.Equal(msg.payload, two) {
		t.Fatalf("latest frame = %v ok=%v", msg.payload, ok)
	}
	if c.framePending {
		t.Fatal("pending flag survived a take")
	}
}

// Concurrent enqueues/takes exercise the slot's mutex under -race and pin the
// reusable outMsg path at zero allocations.
func TestNativeConnFrameSlotAllocsAndRace(t *testing.T) {
	c := &nativeConn{frameNotify: make(chan struct{}, 1)}
	payload := make([]byte, 1024)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				c.enqueueFrame(payload)
				_, _ = c.takeFrame()
			}
		}()
	}
	wg.Wait()

	allocs := testing.AllocsPerRun(100, func() {
		c.enqueueFrame(payload)
		if _, ok := c.takeFrame(); !ok {
			t.Fatal("frame missing")
		}
	})
	if allocs != 0 {
		t.Fatalf("frame slot allocated %.0f times, want 0", allocs)
	}
}
