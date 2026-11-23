package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Models load in the background so the HTTP listener, terminals and admin
// pages are usable while Parakeet, Kokoro and Smart Turn initialize.
const (
	modelLoading = "loading"
	modelReady   = "ready"
	modelError   = "error"

	// modelWait bounds how long a turn waits for a model that is still
	// loading. A load error returns immediately with a named error.
	modelWait = 60 * time.Second
)

// modelState is one background-loaded model: its state, the load error, and a
// completion signal that turns wait on instead of racing the load.
type modelState struct {
	name    string
	started time.Time

	mu      sync.Mutex
	sig     condSignal
	state   string
	err     error
	readyAt time.Time
}

func newModelState(name string, started time.Time) *modelState {
	return &modelState{name: name, started: started, state: modelLoading}
}

func (m *modelState) setReady() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.state, m.err, m.readyAt = modelReady, nil, time.Now()
	m.mu.Unlock()
	m.sig.notify()
}

func (m *modelState) setError(err error) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.state, m.err = modelError, err
	m.mu.Unlock()
	m.sig.notify()
}

func (m *modelState) get() (string, error) {
	if m == nil {
		return modelError, fmt.Errorf("model is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state, m.err
}

func (m *modelState) isReady() bool {
	state, _ := m.get()
	return state == modelReady
}

func (m *modelState) failed() bool {
	state, _ := m.get()
	return state == modelError
}

// await blocks while the model loads. A load error is returned immediately;
// ctx bounds the wait for a slow load.
func (m *modelState) await(ctx context.Context) error {
	if m == nil {
		return fmt.Errorf("model is unavailable")
	}
	for {
		ch := m.sig.pipe()
		state, err := m.get()
		switch state {
		case modelReady:
			return nil
		case modelError:
			return fmt.Errorf("%s unavailable: %w", m.name, err)
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return fmt.Errorf("%s still loading: %w", m.name, ctx.Err())
		}
	}
}

func (m *modelState) report() object {
	state, err := m.get()
	out := object{"state": state}
	if err != nil {
		out["error"] = err.Error()
	}
	if state == modelReady {
		m.mu.Lock()
		started := m.started
		readyAt := m.readyAt
		m.mu.Unlock()
		if !started.IsZero() && !readyAt.IsZero() {
			out["ready_ms"] = readyAt.Sub(started).Milliseconds()
		}
	}
	return out
}

type modelSet struct {
	stt       *modelState
	tts       *modelState
	smartTurn *modelState
	vad       *modelState
}

func newModelSet(started time.Time) modelSet {
	return modelSet{
		stt:       newModelState("Parakeet", started),
		tts:       newModelState("Kokoro", started),
		smartTurn: newModelState("Smart Turn", started),
		vad:       newModelState("Silero", started),
	}
}

func (s modelSet) report() object {
	out := object{}
	for key, model := range map[string]*modelState{
		"parakeet": s.stt, "kokoro": s.tts, "smart_turn": s.smartTurn, "silero": s.vad,
	} {
		if model != nil {
			out[key] = model.report()
		}
	}
	return out
}

func (s modelSet) allReady() bool {
	for _, model := range []*modelState{s.stt, s.tts, s.smartTurn, s.vad} {
		if model != nil && !model.isReady() {
			return false
		}
	}
	return true
}
