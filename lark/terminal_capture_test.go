package main

import (
	"testing"
	"time"
)

func TestCaptureTransitions(t *testing.T) {
	t.Parallel()
	type step struct {
		event string
		phase capturePhase
		late  bool
	}
	for _, tc := range []struct {
		name  string
		steps []step
	}{
		{"client starts and drains", []step{{"start", captureOpen, false}, {"stop", captureStoppingOpen, false},
			{"stop", captureStoppingOpen, false}, {"end", captureIdle, false}}},
		{"bridge starts and drains", []step{{"request", captureRequested, false}, {"ack", captureOpen, false},
			{"stop", captureStoppingOpen, false}, {"end", captureIdle, false}}},
		{"ack arrives after stop", []step{{"request", captureRequested, false}, {"stop", captureStoppingRequested, false},
			{"ack", captureStoppingOpen, false}, {"end", captureIdle, false}}},
		{"cancel suppresses late ack until end", []step{{"request", captureRequested, false},
			{"cancel", captureIdle, true}, {"ack", captureIdle, true}, {"end", captureIdle, false},
			{"start", captureOpen, false}}},
		{"cancel after ack", []step{{"request", captureRequested, false}, {"ack", captureOpen, false},
			{"cancel", captureIdle, false}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c deviceCapture
			now := time.Now()
			for _, s := range tc.steps {
				switch s.event {
				case "start", "request":
					c.start(now, "Mint", s.event == "request")
				case "ack":
					if c.acceptsStart() {
						c.acknowledge()
					}
				case "stop":
					want := c.phase == captureOpen || c.phase == captureRequested
					if c.requestStop() != want {
						t.Fatal("wrong stop admission")
					}
				case "cancel":
					c.suppressLateStart()
					c.clear()
				case "end":
					c.receivedEnd()
					c.clear()
				}
				if c.phase != s.phase || c.ignoreStart != s.late || c.open() != (s.phase == captureOpen || s.phase == captureStoppingOpen) {
					t.Fatalf("after %s: phase=%d ignores start=%t", s.event, c.phase, c.ignoreStart)
				}
				if !c.active() && (!c.started.IsZero() || c.target != "" || len(c.pcm) != 0) {
					t.Fatal("idle capture retained its data")
				}
				if c.active() && (!c.started.Equal(now) || c.target != "Mint") {
					t.Fatal("transition lost capture identity")
				}
			}
		})
	}
}
