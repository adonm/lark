package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func bareActor(t *testing.T) (*terminalHub, *terminalSession) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := &terminalHub{b: &larkServer{root: ctx}, terminals: map[string]*terminalSession{}}
	d := &terminalSession{
		client: &terminalClient{ctx: ctx, cancel: cancel},
		id:     "t", key: "t", screen: roundScreen, companion: blankCompanion(),
	}
	h.terminals[d.key] = d
	d.startActor(h)
	return h, d
}

func TestActorSerializesConcurrentUpdates(t *testing.T) {
	t.Parallel()
	_, d := bareActor(t)
	defer d.close()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				d.update(func(d *terminalSession) { d.finished++ })
			}
		}()
	}
	wg.Wait()
	if got := d.state().finished; got != 800 {
		t.Fatalf("finished = %d, want 800 serialized updates", got)
	}
}

func TestActorCloseDrainsQueuedWork(t *testing.T) {
	t.Parallel()
	_, d := bareActor(t)

	var ran atomic.Int32
	for range 32 {
		if !d.post(terminalEvent{kind: evUpdate, fn: func(*terminalSession) { ran.Add(1) }}) {
			t.Fatal("post rejected before close")
		}
	}
	d.close()
	if got := ran.Load(); got != 32 {
		t.Fatalf("ran %d queued events, want 32", got)
	}
}

func TestActorDropsWorkAfterClose(t *testing.T) {
	t.Parallel()
	_, d := bareActor(t)
	d.close()
	if d.post(terminalEvent{kind: evUpdate, fn: func(*terminalSession) {}}) {
		t.Fatal("post accepted after close")
	}
	if d.update(func(*terminalSession) {}) {
		t.Fatal("update accepted after close")
	}
	if got := d.state().finished; got != 0 {
		t.Fatalf("state after close = %+v, want zero", got)
	}
}

func TestActorShutdownCancelsCaptureAndPlayback(t *testing.T) {
	t.Parallel()
	_, d := bareActor(t)
	ctx, cancel := context.WithCancel(context.Background())
	d.update(func(d *terminalSession) {
		d.recording.start(time.Now(), "", false)
		d.stopAudio = cancel
		d.ttsStarted = true
	})
	d.close()
	if ctx.Err() == nil {
		t.Fatal("shutdown did not cancel playback")
	}
	if st := d.state(); st.recording.active() || st.ttsStarted || st.stopAudio {
		t.Fatalf("shutdown left live state: %+v", st)
	}
}
