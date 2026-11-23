package main

import (
	"testing"
)

// A sleeping panel must not have its radio woken every 12 s; waking returns to
// the full liveness rate.
func TestPingIntervalFollowsSleep(t *testing.T) {
	b, _ := wsHarness(t, nil)
	srv, err := newTerminalServer(b.ws, "127.0.0.1", "0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.shutdown()

	const id = "lark-ping1"
	dev, reject := dialNativeDevice(t, srv, id, "unit", 0)
	if dev == nil {
		t.Fatalf("terminal rejected: %q", reject)
	}

	b.ws.mu.Lock()
	d := b.ws.terminals[id]
	b.ws.mu.Unlock()
	if d == nil || d.client.native == nil {
		t.Fatal("terminal did not register a native client")
	}
	nc := d.client.native

	if got := nc.pingInterval(); got != pingAwake {
		t.Fatalf("awake ping interval = %v, want %v", got, pingAwake)
	}
	b.ws.mu.Lock()
	b.ws.setSleeping(d, true)
	b.ws.mu.Unlock()
	if got := nc.pingInterval(); got != pingAsleep {
		t.Fatalf("sleeping ping interval = %v, want %v", got, pingAsleep)
	}
	b.ws.mu.Lock()
	b.ws.setSleeping(d, false)
	b.ws.mu.Unlock()
	if got := nc.pingInterval(); got != pingAwake {
		t.Fatalf("woken ping interval = %v, want %v", got, pingAwake)
	}
}
