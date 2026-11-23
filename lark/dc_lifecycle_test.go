package main

import (
	"testing"
)

// Accounts left behind by a partial provision are visible to doctor, removed
// while the RPC is ready, and queued durably when it is not.
func TestDeltaChatAccountReconciliation(t *testing.T) {
	t.Parallel()
	b, _, stub := dcTestHarness(t)
	s := b.dc
	orphan, err := stub.AddAccount()
	if err != nil {
		t.Fatal(err)
	}
	rec := s.reconcileAccounts(t.Context())
	if ids, _ := rec["orphans"].([]uint32); len(ids) != 1 || ids[0] != orphan {
		t.Fatalf("orphan account not reported: %+v", rec)
	}
	s.dropAccount(t.Context(), orphan)
	stub.mu.Lock()
	removed := len(stub.removed)
	stub.mu.Unlock()
	if removed != 1 {
		t.Fatalf("dropAccount did not remove the orphan: %v", stub.removed)
	}

	// With the RPC unavailable the removal is recorded, then applied on the
	// next readiness pass.
	s.mu.Lock()
	s.rpc = nil
	s.mu.Unlock()
	s.dropAccount(t.Context(), 42)
	if value, err := b.db.getMeta(t.Context(), dcRemovePending+"42"); err != nil || value == "" {
		t.Fatalf("pending removal not recorded: %q %v", value, err)
	}
	s.mu.Lock()
	s.rpc = stub
	s.mu.Unlock()
	s.retryPendingRemovals(t.Context())
	if value, _ := b.db.getMeta(t.Context(), dcRemovePending+"42"); value != "" {
		t.Fatalf("pending removal survived retry: %q", value)
	}
}
