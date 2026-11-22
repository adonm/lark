package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestPairOpenCodeOnlyOnAuthenticatedLAN(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		user, pass, ok := r.BasicAuth()
		if r.Method != http.MethodPost || r.URL.Path != "/api/pair" || !ok || user != "opencode" || pass != "unit" {
			t.Error("unexpected OpenCode pairing request")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(object{"code": "test_code", "expires_in": 300})
	}))
	defer server.Close()
	api, err := openCodeUpstream(server.Client(), t.TempDir(), server.URL, "unit")
	if err != nil {
		t.Fatal(err)
	}
	b := &larkServer{token: "admin", oc: &openCode{upstream: api}, pairPort: 49376}
	lan := httptest.NewServer(b)
	defer lan.Close()
	scoped := httptest.NewServer(b.scoped(companionState{ID: "cake"}, "cat"))
	defer scoped.Close()
	const path = "/api/opencode/pair"
	for _, tc := range []struct {
		base, token string
		status      int
	}{
		{lan.URL, "", 401},
		{lan.URL, "cat", 401},
		{scoped.URL, "", 404},
		{scoped.URL, "cat", 404},
		{scoped.URL, "admin", 404},
	} {
		testRequest(t, http.MethodPost, tc.base+path, tc.token, nil, tc.status)
	}
	testRequest(t, http.MethodGet, lan.URL+path, "admin", nil, 404)
	if calls.Load() != 0 {
		t.Fatal("rejected requests minted pairing codes")
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, lan.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "not-the-lan.example"
	req.Header.Set("X-Lark-Token", "admin")
	res, err := lan.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var got pairingResponse
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || res.StatusCode != 200 || res.Header.Get("Cache-Control") != "no-store" ||
		!got.OK || len(got.Links) != 1 || got.Links[0].URL != "http://127.0.0.1:49376/auth/connect/test_code" ||
		time.Until(got.ExpiresAt) < 4*time.Minute || time.Until(got.ExpiresAt) > 5*time.Minute {
		t.Fatal("unexpected LAN pairing response")
	}
}

func TestOpenCodeExplicitEndpointAndDiscovery(t *testing.T) {
	state := t.TempDir()
	password := make(chan string, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if ok && user != "opencode" {
			t.Error("unexpected Basic auth user")
		}
		password <- pass
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()
	dir := filepath.Join(state, "opencode")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeMap(filepath.Join(dir, "service.json"), object{"url": server.URL, "password": "discovered"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ url, password, want string }{
		{"", "ignored", "discovered"},
		{server.URL, "dedicated", "dedicated"},
		{server.URL, "", ""},
	} {
		api, err := openCodeUpstream(server.Client(), state, tc.url, tc.password)
		if err != nil || !api.reachable(t.Context(), "/api/info") {
			t.Fatalf("OpenCode endpoint unavailable: %v", err)
		}
		if <-password != tc.want {
			t.Fatal("OpenCode sent the wrong Authorization header")
		}
	}
	if _, err := openCodeUpstream(server.Client(), state, "http://example.org:49375", "secret"); err == nil {
		t.Fatal("accepted a nonlocal OpenCode endpoint")
	}
}
