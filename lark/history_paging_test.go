package main

import (
	"testing"
)

func TestReadHistoryPageRejectsNonPositiveLimit(t *testing.T) {
	t.Parallel()
	b, _ := wsHarness(t, nil)
	for _, limit := range []int{0, -1} {
		if _, err := b.readHistoryPage(t.Context(), "greg", limit, nil); err == nil {
			t.Fatalf("limit %d was accepted", limit)
		}
	}
}

func TestStoredSessionIDIsValidatedBeforeUse(t *testing.T) {
	t.Parallel()
	b, _ := wsHarness(t, nil)
	o := &openCode{db: b.db, store: &sessionStore{db: b.db.DB, readDB: b.db.readDB()}}
	if _, err := b.db.ExecContext(t.Context(),
		`INSERT INTO sessions (companion_id,session_id,updated_at,contract_hash,context_after_ms) VALUES ('greg','../admin',1,'',0)`); err != nil {
		t.Fatal(err)
	}
	if sid, err := o.get("greg"); err == nil || sid != "" {
		t.Fatalf("invalid stored session id accepted: %q %v", sid, err)
	}
	if _, err := o.readBinding(t.Context(), "greg"); err == nil {
		t.Fatal("readBinding accepted an invalid session id")
	}
}
