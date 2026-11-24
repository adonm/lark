package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// qaFakeServer is the smallest slice of the installed server the QA run guard
// talks to: companion and terminal rows plus Delta Chat account ids. Companion
// deletes refuse while a terminal still links the row, like the real route.
type qaFakeServer struct {
	t            *testing.T
	mu           sync.Mutex
	companions   map[string]object
	terminals    map[string]string
	accounts     map[uint32]bool
	refuseComp   map[string]bool
	refuseAcc    map[uint32]bool
	refuseDetach map[string]bool
	nextAcc      uint32
	srv          *httptest.Server
}

func newQAFakeServer(t *testing.T) *qaFakeServer {
	t.Helper()
	f := &qaFakeServer{
		t:            t,
		companions:   map[string]object{},
		terminals:    map[string]string{},
		accounts:     map[uint32]bool{},
		refuseComp:   map[string]bool{},
		refuseAcc:    map[uint32]bool{},
		refuseDetach: map[string]bool{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *qaFakeServer) client() *cliClient {
	return &cliClient{base: f.srv.URL, token: "unit", http: f.srv.Client()}
}

func (f *qaFakeServer) addCompanion(id string, configured bool) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextAcc++
	acc := f.nextAcc
	f.companions[id] = object{"id": id, "configured": configured, "dc_account_id": int(acc)}
	f.accounts[acc] = true
	return acc
}

func (f *qaFakeServer) configure(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.companions[id]["configured"] = true
}

func (f *qaFakeServer) addTerminal(id, companionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminals[id] = companionID
}

func (f *qaFakeServer) attach(termID, companionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminals[termID] = companionID
}

func (f *qaFakeServer) hasCompanion(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.companions[id]
	return ok
}

func (f *qaFakeServer) hasTerminal(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.terminals[id]
	return ok
}

func (f *qaFakeServer) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	write := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.URL.Path == "/api/companions" && r.Method == http.MethodGet:
		ids := make([]string, 0, len(f.companions))
		for id := range f.companions {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		list := make([]any, 0, len(ids))
		for _, id := range ids {
			list = append(list, f.companions[id])
		}
		write(object{"companions": list})
	case r.URL.Path == "/api/terminals" && r.Method == http.MethodGet:
		ids := make([]string, 0, len(f.terminals))
		for id := range f.terminals {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		list := make([]any, 0, len(ids))
		for _, id := range ids {
			list = append(list, object{"id": id, "companion_id": f.terminals[id], "online": false})
		}
		write(object{"terminals": list})
	case r.URL.Path == "/api/diagnostics" && r.Method == http.MethodGet:
		ids := make([]uint32, 0, len(f.accounts))
		for id := range f.accounts {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		write(object{"deltachat_accounts": object{"ids": ids, "on_disk": len(ids)}})
	case strings.HasSuffix(r.URL.Path, "/detach") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/terminals/"), "/detach")
		if _, ok := f.terminals[id]; !ok {
			http.Error(w, `{"error":"Terminal not found"}`, http.StatusNotFound)
			return
		}
		if f.refuseDetach[id] {
			http.Error(w, `{"error":"cannot detach"}`, http.StatusInternalServerError)
			return
		}
		f.terminals[id] = ""
		write(object{"id": id})
	case strings.HasPrefix(r.URL.Path, "/api/terminals/") && r.Method == http.MethodDelete:
		id := strings.TrimPrefix(r.URL.Path, "/api/terminals/")
		if _, ok := f.terminals[id]; !ok {
			http.Error(w, `{"error":"Terminal not found"}`, http.StatusNotFound)
			return
		}
		delete(f.terminals, id)
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(r.URL.Path, "/api/companions/") && r.Method == http.MethodDelete:
		id := strings.TrimPrefix(r.URL.Path, "/api/companions/")
		comp, ok := f.companions[id]
		if !ok {
			http.Error(w, `{"error":"Companion not found"}`, http.StatusNotFound)
			return
		}
		if f.refuseComp[id] {
			http.Error(w, `{"error":"cannot delete"}`, http.StatusInternalServerError)
			return
		}
		for _, cid := range f.terminals {
			if cid == id {
				http.Error(w, `{"error":"Detach or remove terminals before deleting companion"}`, http.StatusConflict)
				return
			}
		}
		if acc := uint32(jsonInt(comp["dc_account_id"])); acc != 0 && !f.refuseAcc[acc] {
			delete(f.accounts, acc)
		}
		delete(f.companions, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func qaTestGuard(t *testing.T, f *qaFakeServer) *qaRunGuard {
	t.Helper()
	g, err := newQARunGuard(t.Context(), f.client())
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func qaTestCleanup(t *testing.T, g *qaRunGuard, timeout time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return g.cleanup(ctx)
}

// A companion that is configured and attached to a terminal after the test
// created it must still be removed. The link is detached first, and the
// sim-created blank companion and terminal disappear with it.
func TestQACleanupRemovesConfiguredAttachedCompanion(t *testing.T) {
	t.Parallel()
	f := newQAFakeServer(t)
	f.addCompanion("qa-existing", true)
	f.addTerminal("sim-keep", "qa-existing")
	g := qaTestGuard(t, f)

	f.addCompanion("new-companion", false)
	f.addTerminal("sim-qa", "new-companion")
	f.addCompanion("qa-temp", false)
	g.trackCompanion("qa-temp")
	f.configure("qa-temp")
	f.attach("sim-qa", "qa-temp")

	if err := qaTestCleanup(t, g, 2*time.Second); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	for _, id := range []string{"qa-temp", "new-companion", "sim-qa"} {
		if f.hasCompanion(id) || f.hasTerminal(id) {
			t.Errorf("%s survived cleanup", id)
		}
	}
	if !f.hasCompanion("qa-existing") || !f.hasTerminal("sim-keep") {
		t.Fatal("cleanup touched a pre-existing configured companion or its terminal")
	}
	f.mu.Lock()
	accounts := len(f.accounts)
	f.mu.Unlock()
	if accounts != 1 {
		t.Fatalf("account set not restored: %d accounts", accounts)
	}
}

// Protection comes from the id set, not from names: a pre-existing row whose
// name looks like QA debris is never removed.
func TestQACleanupLeavesPreexistingSimilarNames(t *testing.T) {
	t.Parallel()
	f := newQAFakeServer(t)
	f.addCompanion("qa-sim", true)
	f.addCompanion("new-companion", false)
	f.addTerminal("sim-manual", "qa-sim")
	g := qaTestGuard(t, f)

	f.addCompanion("qa-temp", false)
	g.trackCompanion("qa-temp")
	f.addTerminal("sim-qa", "qa-temp")

	if err := qaTestCleanup(t, g, 2*time.Second); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	for _, id := range []string{"qa-sim", "new-companion", "sim-manual"} {
		if !f.hasCompanion(id) && !f.hasTerminal(id) {
			t.Errorf("pre-existing %s was removed", id)
		}
	}
	if f.hasCompanion("qa-temp") || f.hasTerminal("sim-qa") {
		t.Fatal("created companion or terminal survived")
	}
}

// A remove call that reports an error for a row that disappeared anyway must
// not fail a run whose sets came back.
func TestQACleanupIgnoresTransientRemoveErrors(t *testing.T) {
	t.Parallel()
	f := newQAFakeServer(t)
	g := qaTestGuard(t, f)
	f.addCompanion("qa-race", false)
	g.trackCompanion("qa-race")
	f.addTerminal("sim-race", "qa-race")
	f.mu.Lock()
	f.refuseDetach["sim-race"] = true
	f.mu.Unlock()

	if err := qaTestCleanup(t, g, time.Second); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if f.hasCompanion("qa-race") || f.hasTerminal("sim-race") {
		t.Fatal("transient detach error left rows behind")
	}
}

// The post-run invariant must fail with the leftover id when a delete does not
// stick, instead of reporting a clean run.
func TestQACleanupFailsWithLeftoverCompanion(t *testing.T) {
	t.Parallel()
	f := newQAFakeServer(t)
	g := qaTestGuard(t, f)
	f.addCompanion("qa-stuck", false)
	g.trackCompanion("qa-stuck")
	f.mu.Lock()
	f.refuseComp["qa-stuck"] = true
	f.mu.Unlock()

	err := qaTestCleanup(t, g, 400*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "qa-stuck") {
		t.Fatalf("cleanup error = %v, want the leftover companion id", err)
	}
}

// An account whose removal lags or fails keeps the invariant red, because the
// account set is part of the before/after comparison.
func TestQACleanupFailsWithLeftoverAccount(t *testing.T) {
	t.Parallel()
	f := newQAFakeServer(t)
	g := qaTestGuard(t, f)
	acc := f.addCompanion("qa-account", false)
	g.trackCompanion("qa-account")
	f.mu.Lock()
	f.refuseAcc[acc] = true
	f.mu.Unlock()

	err := qaTestCleanup(t, g, 400*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "Delta Chat account") ||
		!strings.Contains(err.Error(), strconv.Itoa(int(acc))) {
		t.Fatalf("cleanup error = %v, want the leftover account id %d", err, acc)
	}
}
