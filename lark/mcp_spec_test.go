package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A speculative turn may be aborted after the model has already decided to act.
// Mutating MCP calls wait for promotion; reads still answer immediately.
func TestSpeculationDefersMutatingTools(t *testing.T) {
	t.Parallel()
	b, _ := wsHarness(t, func(b *larkServer, _ string) { b.timers = newTimerManager(b) })
	if _, err := b.createCompanion(t.Context(), object{"id": "echo", "name": "Echo", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	spec := &specTurn{
		companionID: "echo",
		done:        make(chan specResult, 1),
		cancel:      func() {},
		cancelFill:  func() {},
		gate:        b.hub.gate,
	}
	spec.gate.add(spec)

	type result struct {
		out object
		err error
	}
	applied := make(chan result, 1)
	go func() {
		out, err := b.callMCP(t.Context(), "patch", object{
			"companion_id": "echo",
			"timer":        object{"op": "set", "in_seconds": json.Number("60"), "say": "tea"},
		})
		applied <- result{out, err}
	}()

	select {
	case r := <-applied:
		t.Fatalf("mutating call ran during speculation: %+v", r)
	case <-time.After(30 * time.Millisecond):
	}
	if timers, err := b.timers.list(t.Context(), "echo"); err != nil || len(timers) != 0 {
		t.Fatalf("timer created before promotion: %v %v", timers, err)
	}

	// Reads are not deferred.
	done := make(chan error, 1)
	go func() {
		_, err := b.callMCP(t.Context(), "state", object{"companion_id": "echo"})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read failed during speculation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("read was held behind a speculative turn")
	}

	spec.done <- specResult{}
	spec.promote(t.Context(), nil, nil, nil)
	select {
	case r := <-applied:
		if r.err != nil {
			t.Fatalf("deferred call failed on promotion: %v", r.err)
		}
	case <-time.After(time.Second):
		t.Fatal("deferred call was not released")
	}
	if timers, err := b.timers.list(t.Context(), "echo"); err != nil || len(timers) != 1 {
		t.Fatalf("timer after promotion = %v, %v; want 1", timers, err)
	}
}

func TestAbortedSpeculationExecutesNothing(t *testing.T) {
	t.Parallel()
	b, _ := wsHarness(t, nil)
	if _, err := b.createCompanion(t.Context(), object{"id": "echo", "name": "Echo", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	spec := &specTurn{
		companionID: "echo",
		done:        make(chan specResult, 1),
		cancel:      func() {},
		cancelFill:  func() {},
		gate:        b.hub.gate,
	}
	spec.gate.add(spec)
	before := b.emits.mark("echo")
	held := make(chan error, 1)
	go func() {
		_, err := b.callMCP(t.Context(), "emit", object{"to": "echo", "say": "hello"})
		held <- err
	}()
	select {
	case err := <-held:
		t.Fatalf("emit ran before the speculation settled: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	spec.abort()
	select {
	case err := <-held:
		if err == nil || !strings.Contains(err.Error(), "speculative") {
			t.Fatalf("aborted speculation returned %v, want a clear cancellation error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("aborted speculation left a tool call waiting")
	}
	if got := b.emits.mark("echo"); got != before {
		t.Fatalf("aborted speculation executed emit (mark %d -> %d)", before, got)
	}
}

// The gate attributes a call by its companion argument so one turn's writes
// are never held behind an unrelated speculation.
func TestSpeculationGateAttributesByCompanion(t *testing.T) {
	t.Parallel()
	g := &specGate{}
	a := &specTurn{companionID: "cake"}
	b := &specTurn{companionID: "mint"}
	g.add(a)
	g.add(b)
	if got := g.find(object{"companion_id": "mint"}); got != b {
		t.Fatalf("find(mint) = %p, want %p", got, b)
	}
	if got := g.find(object{"to": "cake"}); got != a {
		t.Fatalf("find(cake) = %p, want %p", got, a)
	}
	if got := g.find(object{}); got != nil {
		t.Fatalf("ambiguous call attributed to %p", got)
	}
	g.remove(a)
	g.remove(b)
	if got := g.find(object{}); got != nil {
		t.Fatalf("removed speculation still owns calls: %p", got)
	}
}

// Cost of deferring a mutating tool through a promoted speculation versus
// running it directly. Read tools never take this path.
func BenchmarkSpeculationToolHandoff(b *testing.B) {
	ctx := context.Background()
	g := &specGate{}
	run := func() (object, error) { return object{"ok": true}, nil }
	b.Run("direct", func(b *testing.B) {
		for b.Loop() {
			if _, err := run(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("deferred", func(b *testing.B) {
		for b.Loop() {
			s := &specTurn{gate: g, done: make(chan specResult, 1), cancel: func() {}, cancelFill: func() {}}
			g.add(s)
			done := make(chan error, 1)
			go func() {
				_, err := s.hold(ctx, run)
				done <- err
			}()
			s.done <- specResult{}
			s.promote(ctx, nil, nil, nil)
			if err := <-done; err != nil {
				b.Fatal(err)
			}
		}
	})
}
