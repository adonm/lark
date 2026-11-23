package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Form edits must not spawn unbounded generations: one image is generated at a
// time per portrait, and a newer edit supersedes the queued one.
func TestPortraitBackgroundSingleFlight(t *testing.T) {
	t.Parallel()
	fixture := portraitFixture(t)
	var active, peak, calls atomic.Int32
	var lastPrompt atomic.Value
	release := make(chan struct{})
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "0123456789abcdef0123456789abcdef"), 0o700); err != nil {
		t.Fatal(err)
	}
	store := &portraitStore{dir: dir, generate: func(_ context.Context, prompt string, _ []byte) ([]byte, error) {
		calls.Add(1)
		lastPrompt.Store(prompt)
		current := active.Add(1)
		for {
			seen := peak.Load()
			if current <= seen || peak.CompareAndSwap(seen, current) {
				break
			}
		}
		<-release
		active.Add(-1)
		return fixture, nil
	}}
	b, _ := wsHarness(t, nil)
	b.portraits = store
	b.schedulePortraitBackground("0123456789abcdef0123456789abcdef", "a cat", "calm")
	b.schedulePortraitBackground("0123456789abcdef0123456789abcdef", "a dog", "bright")

	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	for time.Now().Before(deadline) {
		b.portraitBGMu.Lock()
		pending := len(b.portraitBG)
		b.portraitBGMu.Unlock()
		if pending == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if peak.Load() > 1 {
		t.Fatalf("%d generations ran concurrently; want one per portrait", peak.Load())
	}
	if calls.Load() != 1 {
		t.Fatalf("generated %d times; want one coalesced run", calls.Load())
	}
	if prompt, _ := lastPrompt.Load().(string); !strings.Contains(prompt, "a dog") {
		t.Fatalf("generation used a stale form: %q", prompt)
	}
	var mu sync.Mutex
	mu.Lock()
	b.portraitBGMu.Lock()
	left := len(b.portraitBG)
	b.portraitBGMu.Unlock()
	mu.Unlock()
	if left != 0 {
		t.Fatalf("queue entry survived completion: %d", left)
	}
}
