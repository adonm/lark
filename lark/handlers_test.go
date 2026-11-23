package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

// A failing entropy source must never mint an all-zero token or ID.
func TestRandomHexFailsClosed(t *testing.T) {
	if id, err := randomHexFrom(failingReader{}, 8); err == nil || id != "" {
		t.Fatalf("randomHexFrom returned %q, %v; want an error and no id", id, err)
	}
	if id, err := randomHex(8); err != nil || len(id) != 16 {
		t.Fatalf("randomHex = %q, %v", id, err)
	}
}

// The HTTP listener is LAN-reachable for firmware OTA, so MCP must stay
// loopback-only even with a valid token.
func TestMCPRejectsNonLoopbackPeers(t *testing.T) {
	b, _ := wsHarness(t, nil)
	mcp := func(remoteAddr string) int {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
		req.RemoteAddr = remoteAddr
		req.Header.Set("X-Lark-Token", b.token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		b.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := mcp("192.168.1.20:5555"); code != http.StatusNotFound {
		t.Fatalf("LAN MCP request returned %d; want 404", code)
	}
	if code := mcp("[::1]:5555"); code == http.StatusNotFound {
		t.Fatalf("loopback MCP request was rejected as non-loopback")
	}
}
