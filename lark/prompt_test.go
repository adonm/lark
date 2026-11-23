package main

import (
	"strings"
	"testing"
	"time"
)

// A companion must see its own live hardware, not the sim rows or another
// companion's device, and the state line must say what it is and how listening
// actually works.
func TestTurnPromptListsOwnOnlineHardwareTerminal(t *testing.T) {
	b, base := wsHarness(t, nil)
	own := testConnectWS(t, base, "unit", false)
	own.helloHardware(t, "vocat-33d8", "Cake", capBattery|capDrag)
	b.hub.mu.Lock()
	d := b.hub.terminals["vocat-33d8"]
	d.pct, d.vol, d.asleep = 87, 42, true
	b.hub.mu.Unlock()

	if err := b.terminals.save("sim-cleanup", terminalRecord{
		ID: "sim-cleanup", Kind: "hardware", CompanionID: "cake", IP: "127.0.0.1",
	}, false); err != nil {
		t.Fatal(err)
	}
	other := testConnectWS(t, base, "unit", false)
	other.helloHardware(t, "vocat-2cac", "Mint", capBattery|capDrag)

	companion := companionState{
		companionLook: companionLook{Name: "Cake", Configured: true, Form: "a playful young fox"},
		ListenSeconds: 300,
	}
	got := b.turnPromptWithState(t.Context(), "cake", "voice", "How much battery?", companion, turnChannel{}, time.Now())
	if !strings.Contains(got, "vocat-33d8(esp-vocat,online,battery:87,sleeping,vol:42)") {
		t.Fatalf("own terminal missing from prompt: %q", got)
	}
	if strings.Contains(got, "sim-cleanup") || strings.Contains(got, "vocat-2cac") {
		t.Fatalf("offline or foreign terminal leaked into prompt: %q", got)
	}
	if !strings.Contains(got, "listen: on (300s spans)") || !strings.Contains(got, "form: a playful young fox") {
		t.Fatalf("state line lost listening spans or form: %q", got)
	}

	entry, err := b.terminalList(false)
	if err != nil {
		t.Fatal(err)
	}
	for _, term := range entry {
		if str(term["id"]) == "vocat-33d8" && term["character"] != "Cake" {
			t.Fatalf("terminal reports character %v, want Cake", term["character"])
		}
	}
}
