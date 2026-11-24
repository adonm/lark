package main

import "testing"

func TestTurnTransitionsAcceptTheLifecycle(t *testing.T) {
	t.Parallel()
	legal := [][2]turnPhase{
		{turnIdle, turnIdle},
		{turnIdle, turnRunning},
		{turnIdle, turnSpeaking},
		{turnRunning, turnIdle},
		{turnRunning, turnSpeaking},
		{turnSpeaking, turnIdle},
		{turnSpeaking, turnRunning},
		{turnSpeaking, turnSpeaking},
	}
	for _, tr := range legal {
		if !turnTransitionAllowed(tr[0], tr[1]) {
			t.Errorf("%v -> %v rejected", tr[0], tr[1])
		}
	}
}

func TestTurnTransitionRejectsASecondStart(t *testing.T) {
	t.Parallel()
	d := &terminalSession{id: "t"}
	if err := d.transitionTurn(turnRunning); err != nil {
		t.Fatalf("first start: %v", err)
	}
	if err := d.transitionTurn(turnRunning); err == nil {
		t.Fatal("a second running phase was accepted without cancelling the first turn")
	}
	if d.turnPhase != turnRunning {
		t.Fatalf("phase after rejected transition = %v, want running", d.turnPhase)
	}
	if err := d.transitionTurn(turnIdle); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

func TestTurnTransitionRejectsUnknownPhases(t *testing.T) {
	t.Parallel()
	d := &terminalSession{id: "t"}
	if err := d.transitionTurn(turnPhase(99)); err == nil {
		t.Fatal("unknown destination phase was accepted")
	}
	if d.turnPhase != turnIdle {
		t.Fatalf("phase after rejected transition = %v, want idle", d.turnPhase)
	}
}

func TestTurnPhaseSettlesFromTheFlags(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		d    terminalSession
		want turnPhase
	}{
		{"idle", terminalSession{}, turnIdle},
		{"busy", terminalSession{busy: true}, turnRunning},
		{"tts", terminalSession{ttsStarted: true}, turnSpeaking},
		{"ack", terminalSession{awaitAck: true}, turnSpeaking},
		{"busy then speaking", terminalSession{busy: true, ttsStarted: true}, turnSpeaking},
	} {
		d := tc.d
		d.settleTurnPhase()
		if d.turnPhase != tc.want {
			t.Fatalf("%s: phase = %v, want %v", tc.name, d.turnPhase, tc.want)
		}
	}
}
