package main

import (
	"testing"
)

// A sleeping panel must not have its radio woken every 12 s; waking returns to
// the full liveness rate.
func TestPingIntervalFollowsSleep(t *testing.T) {
	b, _ := wsHarness(t, nil)
	srv, err := newTerminalServer(b.hub, "127.0.0.1", "0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.shutdown()

	const id = "lark-ping1"
	dev, reject := dialNativeDevice(t, srv, id, "unit", 0)
	if dev == nil {
		t.Fatalf("terminal rejected: %q", reject)
	}

	b.hub.mu.Lock()
	d := b.hub.terminals[id]
	b.hub.mu.Unlock()
	if d == nil || d.client.native == nil {
		t.Fatal("terminal did not register a native client")
	}
	nc := d.client.native

	if got := nc.pingInterval(); got != pingAwake {
		t.Fatalf("awake ping interval = %v, want %v", got, pingAwake)
	}
	b.hub.mu.Lock()
	b.hub.setSleeping(d, true)
	b.hub.mu.Unlock()
	if got := nc.pingInterval(); got != pingAsleep {
		t.Fatalf("sleeping ping interval = %v, want %v", got, pingAsleep)
	}
	b.hub.mu.Lock()
	b.hub.setSleeping(d, false)
	b.hub.mu.Unlock()
	if got := nc.pingInterval(); got != pingAwake {
		t.Fatalf("woken ping interval = %v, want %v", got, pingAwake)
	}
}
