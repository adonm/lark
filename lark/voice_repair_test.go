package main

import (
	"testing"
)

// A retired voice id must not hide the companion; it falls back to the theme
// default and stays visible for a repair.
func TestRetiredStoredVoiceFallsBack(t *testing.T) {
	t.Parallel()
	b, _ := wsHarness(t, nil)
	b.voices = companionVoiceStore(b.db, heartVoice)
	if _, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "an old chameleon"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.ExecContext(t.Context(), `UPDATE companions SET voice='retired_voice' WHERE id='greg'`); err != nil {
		t.Fatal(err)
	}
	c, ok := b.companionByID("greg")
	if !ok {
		t.Fatal("companion with a retired voice disappeared")
	}
	if c.Name != "Greg" || c.Voice != "retired_voice" {
		t.Fatalf("companion read = %+v", c)
	}
	if got := b.voiceFor("greg"); got != "af_heart" {
		t.Fatalf("voiceFor = %q; want the fallback", got)
	}
	entry := b.companionEntry(c, false)
	if entry["voice"] != "af_heart" || entry["voice_invalid"] != "retired_voice" {
		t.Fatalf("companion entry did not expose the repair hint: %+v", entry)
	}
}
