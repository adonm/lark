package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

type rolloverHarness struct {
	db       *larkDB
	oc       *openCode
	prompts  int
	deleted  []string
	mu       sync.Mutex
	api      *httptest.Server
	sessions map[string]bool
}

func newRolloverHarness(t *testing.T, staleContract bool) *rolloverHarness {
	t.Helper()
	h := &rolloverHarness{db: testDB(t, t.TempDir()+"/lark.db"), sessions: map[string]bool{"ses-old": true}}
	h.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/session":
			h.prompts++
			h.sessions["ses-new"] = true
			sendJSON(w, 200, object{"data": object{"id": "ses-new", "agent": "lark",
				"model": object{"provider": "test", "id": "model"}, "permissions": []any{}}})
		case r.Method == http.MethodPut && r.URL.Path == "/api/experimental/session/ses-new/instructions/entries/lark.companion":
			var in object
			_ = json.NewDecoder(r.Body).Decode(&in)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/session/ses-old":
			delete(h.sessions, "ses-old")
			h.deleted = append(h.deleted, "ses-old")
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(h.api.Close)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	h.oc = newOpenCode(ctx, upstream{"OpenCode", h.api.Client(), fixedEndpoint{base: h.api.URL}}, h.db, "/workspace",
		ocSettings{Agent: "lark", Model: ocModel{Provider: "test", ID: "model"}})
	h.oc.contract = "current"
	if err := h.db.setMeta(t.Context(), mcpSchemaMeta, "current"); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *rolloverHarness) seed(t *testing.T, contract string, after int64) companionState {
	t.Helper()
	tx, err := h.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	c := blankCompanion()
	c.ID, c.Name, c.Configured = "greg", "Greg", true
	if err := writeCompanion(t.Context(), tx, c.ID, c); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(t.Context(),
		`INSERT INTO sessions (companion_id,session_id,updated_at,contract_hash,context_after_ms) VALUES (?,?,?,?,?)`,
		"greg", "ses-old", 1, contract, after); err != nil {
		t.Fatal(err)
	}
	c, _ = (&larkServer{db: h.db}).companionByID("greg")
	return c
}

// A contract rollover must advance the dialogue watermark, replace the cached
// model entry, and retire the old session instead of leaving it behind.
func TestSessionRolloverAdvancesWatermarkAndRetiresOld(t *testing.T) {
	t.Parallel()
	h := newRolloverHarness(t, false)
	const oldAfter = int64(1000)
	c := h.seed(t, "old-contract", oldAfter)
	h.oc.mu.Lock()
	h.oc.sessionModel["ses-old"] = ocModel{Provider: "test", ID: "model"}
	h.oc.mu.Unlock()

	sid, err := h.oc.ensureSession(t.Context(), c, true)
	if err != nil {
		t.Fatal(err)
	}
	if sid != "ses-new" {
		t.Fatalf("rollover produced %q", sid)
	}
	var after int64
	if err := h.db.QueryRowContext(t.Context(), `SELECT context_after_ms FROM sessions WHERE companion_id='greg'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after <= oldAfter {
		t.Fatalf("watermark stayed at %d; want an advanced value", after)
	}
	h.oc.mu.Lock()
	_, oldCached := h.oc.sessionModel["ses-old"]
	newCached := h.oc.sessionModel["ses-new"]
	h.oc.mu.Unlock()
	if oldCached || newCached == (ocModel{}) {
		t.Fatalf("sessionModel after rollover: %+v", h.oc.sessionModel)
	}
	h.mu.Lock()
	deleted := append([]string(nil), h.deleted...)
	h.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != "ses-old" {
		t.Fatalf("old session was not retired: %v", deleted)
	}
}
