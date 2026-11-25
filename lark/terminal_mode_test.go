package main

import (
	"testing"
	"time"

	"lark/internal/pixel"
)

func TestModeTransitionsAcceptTheLifecycle(t *testing.T) {
	t.Parallel()
	legal := [][2]terminalMode{
		{modeIdle, modeIdle},
		{modeIdle, modeRecording},
		{modeIdle, modeListening},
		{modeIdle, modeThinking},
		{modeIdle, modeSpeaking},
		{modeIdle, modeMenu},
		{modeIdle, modeStory},
		{modeRecording, modeThinking},
		{modeRecording, modeIdle},
		{modeListening, modeThinking},
		{modeThinking, modeSpeaking},
		{modeSpeaking, modeThinking},
		{modeSpeaking, modeListening},
		{modeMenu, modeStory},
		{modeStory, modeMenu},
	}
	for _, tr := range legal {
		if !modeTransitionAllowed(tr[0], tr[1]) {
			t.Errorf("%v -> %v rejected", tr[0], tr[1])
		}
	}
}

func TestModeTransitionRejectsRecordingToSpeaking(t *testing.T) {
	t.Parallel()
	d := &terminalSession{id: "t", mode: modeRecording}
	if err := d.transitionMode(modeSpeaking); err == nil {
		t.Fatal("recording -> speaking was accepted")
	}
	if d.mode != modeRecording {
		t.Fatalf("mode after rejected transition = %v, want recording", d.mode)
	}
}

func TestModeTransitionRejectsUnknownModes(t *testing.T) {
	t.Parallel()
	d := &terminalSession{id: "t"}
	if err := d.transitionMode(terminalMode(99)); err == nil {
		t.Fatal("unknown destination mode was accepted")
	}
	if d.mode != modeIdle {
		t.Fatalf("mode after rejected transition = %v, want idle", d.mode)
	}
}

func TestDeriveModeFollowsTheFlags(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		d    terminalSession
		want terminalMode
	}{
		{"idle", terminalSession{}, modeIdle},
		{"busy", terminalSession{busy: true}, modeThinking},
		{"tts", terminalSession{ttsStarted: true}, modeSpeaking},
		{"ack", terminalSession{awaitAck: true}, modeSpeaking},
		{"busy then speaking", terminalSession{busy: true, ttsStarted: true}, modeSpeaking},
	} {
		if got := tc.d.deriveMode(); got != tc.want {
			t.Fatalf("%s: mode = %v, want %v", tc.name, got, tc.want)
		}
	}
	d := terminalSession{}
	d.recording.start(testNow(), "", true)
	d.recording.listen = true
	if got := d.deriveMode(); got != modeListening {
		t.Fatalf("listening window mode = %v, want listening", got)
	}
	d.drawer = pixel.DrawerStoryReader
	d.recording.clear()
	if got := d.deriveMode(); got != modeStory {
		t.Fatalf("story mode = %v, want story", got)
	}
	d.drawer = pixel.DrawerMenu
	if got := d.deriveMode(); got != modeMenu {
		t.Fatalf("menu mode = %v, want menu", got)
	}
}

func testNow() time.Time { return time.Now() }

// waitTerm polls the session actor until cond holds.
func waitTerm(t *testing.T, d *terminalSession, cond func(terminalState) bool) terminalState {
	t.Helper()
	deadline := time.Now().Add(testWait(t))
	for {
		st := d.state()
		if cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for terminal %s state", d.id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// updateTerm runs fn in the session actor.
func updateTerm(t *testing.T, d *terminalSession, fn func(*terminalSession)) {
	t.Helper()
	if !d.update(fn) {
		t.Fatalf("terminal %s actor stopped", d.id)
	}
}

func recordingActive(d *terminalSession) bool {
	st := d.state()
	return st.recording.active()
}
