package main

import (
	"testing"
	"time"
)

func TestKokoroReplyBypassesQueuedWarmup(t *testing.T) {
	ctx := t.Context()
	w := &synthWorker{requests: make(chan ttsRequest, 1), warming: make(chan ttsRequest, 2), done: make(chan struct{})}
	started := make(chan string, 3)
	release := make(chan struct{})
	go w.run(ctx, func() (speechEngine, error) {
		return fakeSpeechEngine(func(text string, _ voiceChoice, _ func() bool) ([]float32, int) {
			started <- text
			if text == "first warm clip" {
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			return []float32{0, 0.2, 0}, kokoroRate
		}), nil
	})
	request := func(text string) ttsRequest {
		return ttsRequest{ctx: ctx, text: text, queuedAt: time.Now(), done: make(chan ttsResult, 1)}
	}
	w.warming <- request("first warm clip")
	expectValue(t, started, "first warm clip")
	w.warming <- request("second warm clip")
	w.requests <- request("reply")
	close(release)
	expectValue(t, started, "reply")
	expectValue(t, started, "second warm clip")
}
