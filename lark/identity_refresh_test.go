package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// identityHarness wires a real database to a fake OpenCode that records the
// identity PUTs and can fail them on demand.
type identityHarness struct {
	db      *larkDB
	oc      *openCode
	api     *httptest.Server
	fail    atomic.Bool
	puts    atomic.Int32
	last    atomic.Value // string: name written
	putGate chan struct{}
	gated   chan struct{}
	gateOne sync.Once
}

func newIdentityHarness(t *testing.T) *identityHarness {
	t.Helper()
	h := &identityHarness{db: testDB(t, t.TempDir()+"/lark.db")}
	h.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/experimental/session/ses-one/instructions/entries/lark.companion" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if h.putGate != nil {
			h.gateOne.Do(func() { close(h.gated) })
			<-h.putGate
		}
		var in struct {
			Value struct{ Name string }
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if h.fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		h.puts.Add(1)
		h.last.Store(in.Value.Name)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(h.api.Close)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	h.oc = newOpenCode(ctx, upstream{"OpenCode", h.api.Client(), fixedEndpoint{base: h.api.URL}}, h.db, "/workspace", ocSettings{})
	return h
}

func (h *identityHarness) create(t *testing.T, name string) companionState {
	t.Helper()
	tx, err := h.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	c := blankCompanion()
	c.ID, c.Name, c.Configured = "greg", name, true
	if err := writeCompanion(t.Context(), tx, c.ID, c); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(t.Context(), `INSERT INTO sessions (companion_id,session_id,updated_at,contract_hash,context_after_ms) VALUES (?,?,?,?,?)`,
		c.ID, "ses-one", 1, "", 0); err != nil {
		t.Fatal(err)
	}
	c, _ = (&larkServer{db: h.db}).companionByID(c.ID)
	return c
}

func (h *identityHarness) synced(t *testing.T) (int64, int64) {
	t.Helper()
	var version, synced int64
	if err := h.db.QueryRowContext(t.Context(), `SELECT identity_version,identity_synced FROM companions WHERE id='greg'`).Scan(&version, &synced); err != nil {
		t.Fatal(err)
	}
	return version, synced
}

func TestIdentityRefreshReconcilesAfterFailure(t *testing.T) {
	t.Parallel()
	h := newIdentityHarness(t)
	h.create(t, "Greg")
	if _, err := h.db.ExecContext(t.Context(), `UPDATE companions SET name='Gregory', identity_version=identity_version+1, identity_synced=0 WHERE id='greg'`); err != nil {
		t.Fatal(err)
	}
	h.fail.Store(true)
	h.oc.reconcileCompanionIdentity(t.Context(), "greg")
	if version, synced := h.synced(t); version == 0 || synced != 0 {
		t.Fatalf("failed PUT advanced the marker: version=%d synced=%d", version, synced)
	}
	h.fail.Store(false)
	h.oc.reconcileCompanionIdentity(t.Context(), "greg")
	version, synced := h.synced(t)
	if version == 0 || synced != version {
		t.Fatalf("reconcile did not acknowledge the edit: version=%d synced=%d", version, synced)
	}
	if h.last.Load() != "Gregory" {
		t.Fatalf("identity PUT carried %v", h.last.Load())
	}
}

// An edit that lands while an older PUT is in flight must stay dirty and the
// next reconcile must write the newest values.
func TestIdentityRefreshNewestWins(t *testing.T) {
	t.Parallel()
	h := newIdentityHarness(t)
	h.create(t, "Greg")
	if _, err := h.db.ExecContext(t.Context(), `UPDATE companions SET name='First', identity_version=identity_version+1, identity_synced=0 WHERE id='greg'`); err != nil {
		t.Fatal(err)
	}
	h.putGate, h.gated = make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.oc.reconcileCompanionIdentity(context.Background(), "greg")
	}()
	// Let the PUT start, then land a newer edit before it is acknowledged.
	<-h.gated
	if _, err := h.db.ExecContext(t.Context(), `UPDATE companions SET name='Second', identity_version=identity_version+1, identity_synced=0 WHERE id='greg'`); err != nil {
		t.Fatal(err)
	}
	close(h.putGate)
	<-done
	version, synced := h.synced(t)
	if synced >= version {
		t.Fatalf("stale PUT was acknowledged: version=%d synced=%d", version, synced)
	}
	h.oc.reconcileCompanionIdentity(t.Context(), "greg")
	if _, synced := h.synced(t); synced != version {
		t.Fatalf("second reconcile did not acknowledge: version=%d synced=%d", version, synced)
	}
	if h.last.Load() != "Second" {
		t.Fatalf("newest identity did not win: %v", h.last.Load())
	}
}
