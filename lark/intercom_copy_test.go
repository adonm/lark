package main

import (
	"strings"
	"testing"
)

func TestWelcomeTextDocumentsRemoteOffAndStoryOpen(t *testing.T) {
	t.Parallel()
	text := welcomeText(companionState{companionLook: companionLook{Name: "Cake", Form: "a cat"}})
	if !strings.Contains(text, "/remote off") {
		t.Fatalf("welcome text omits /remote off: %q", text)
	}
	if !strings.Contains(text, "/story <name> — open a story on my device") {
		t.Fatalf("welcome text misdescribes /story: %q", text)
	}
	if strings.Contains(text, ".twee file\")") || strings.Contains(text, "download a story") {
		t.Fatalf("welcome text still calls /story a file download: %q", text)
	}
}

func TestIntercomConfirmationNamesTheReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		target     string
		recipients []companionTarget
		sent       bool
		partial    bool
		want       string
	}{
		{"Mum", nil, false, false, "Mum isn't paired yet."},
		{"everyone", nil, false, false, "No one is paired yet."},
		{"mum", []companionTarget{{ID: "mum", Name: "Mum"}}, false, false, "I couldn't reach Mum."},
		{"mum", []companionTarget{{ID: "mum", Name: "Mum"}}, true, true, "I sent your message, but couldn't reach everyone."},
		{"mum", []companionTarget{{ID: "mum", Name: "Mum"}}, true, false, "Sent to Mum!"},
	}
	for _, tc := range cases {
		_, reply := intercomConfirmation(tc.target, tc.recipients, tc.sent, tc.partial)
		if reply != tc.want {
			t.Errorf("intercomConfirmation(%q, %d, sent=%v, partial=%v) = %q; want %q",
				tc.target, len(tc.recipients), tc.sent, tc.partial, reply, tc.want)
		}
	}
}

// Both /api/companions and /api/terminals expose a single `online` boolean.
func TestTerminalPayloadUsesOneOnlineKey(t *testing.T) {
	t.Parallel()
	b, _ := wsHarness(t, nil)
	entry := b.terminalEntry(terminalRecord{ID: "box3-one", Kind: "hardware"}, true)
	if _, exists := entry["connected"]; exists {
		t.Fatalf("terminal payload still has a connected key: %+v", entry)
	}
	if entry["online"] != false {
		t.Fatalf("offline terminal online = %v", entry["online"])
	}
}
