package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDeployModels(t *testing.T) {
	t.Parallel()
	ready := object{"state": modelReady}
	cases := []struct {
		name        string
		health      object
		wantStates  string
		wantReady   bool
		wantFailure string
	}{
		{
			name: "all ready",
			health: object{"models": object{
				"parakeet": ready, "kokoro": ready, "smart_turn": ready, "silero": ready,
			}},
			wantStates: "parakeet=ready kokoro=ready smart_turn=ready silero=ready",
			wantReady:  true,
		},
		{
			name: "still loading",
			health: object{"models": object{
				"parakeet": ready, "kokoro": object{"state": "loading"},
			}},
			wantStates: "parakeet=ready kokoro=loading",
		},
		{
			name: "model error",
			health: object{"models": object{
				"parakeet": object{"state": modelError, "error": "weights missing"},
				"kokoro":   ready,
			}},
			wantStates:  "parakeet=error kokoro=ready",
			wantFailure: "parakeet: weights missing",
		},
		{
			// No aggregate stt/tts fallback: a server without the models map
			// is not ready, and the deploy must say so.
			name:       "missing model map",
			health:     object{"stt": true, "tts": true},
			wantStates: "models=missing",
		},
		{
			name:       "no models at all",
			health:     object{"models": object{}},
			wantStates: "",
			wantReady:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			states, isReady, failure := deployModels(tc.health)
			if states != tc.wantStates || isReady != tc.wantReady || failure != tc.wantFailure {
				t.Fatalf("deployModels = (%q, %v, %q), want (%q, %v, %q)",
					states, isReady, failure, tc.wantStates, tc.wantReady, tc.wantFailure)
			}
		})
	}
}

func TestWaitDeployHealth(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		n := calls.Add(1)
		switch {
		case n == 1:
			// Not listening yet.
			http.Error(w, "down", http.StatusBadGateway)
		case n < 4:
			w.Write([]byte(`{"models":{"parakeet":{"state":"loading"}}}`))
		default:
			w.Write([]byte(`{"models":{"parakeet":{"state":"ready"}}}`))
		}
	}))
	defer srv.Close()
	c := &cliClient{base: srv.URL, token: "unit", http: srv.Client()}
	var out, errOut bytes.Buffer
	serving, models, err := waitDeployHealth(context.Background(), c, &out, &errOut)
	if err != nil {
		t.Fatalf("waitDeployHealth: %v (%s)", err, errOut.String())
	}
	if serving <= 0 || models < serving {
		t.Fatalf("serving=%v models=%v; want models >= serving > 0", serving, models)
	}
	if !strings.Contains(out.String(), "models parakeet=loading") || !strings.Contains(out.String(), "serving in") {
		t.Fatalf("health progress not reported:\n%s", out.String())
	}
}

func TestWaitDeployHealthFailsOnModelError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":{"kokoro":{"state":"error","error":"bad weights"}}}`))
	}))
	defer srv.Close()
	c := &cliClient{base: srv.URL, token: "unit", http: srv.Client()}
	_, _, err := waitDeployHealth(context.Background(), c, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "kokoro: bad weights") {
		t.Fatalf("waitDeployHealth error = %v; want the model failure", err)
	}
}

func TestInstallBinaryKeepsPrevious(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	binary := filepath.Join(dir, "lark")
	build := filepath.Join(dir, ".deploy")
	if err := os.WriteFile(binary, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(build, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	previous, err := installBinary(build, binary)
	if err != nil {
		t.Fatal(err)
	}
	if previous == "" {
		t.Fatal("installBinary kept no previous binary")
	}
	if got, _ := os.ReadFile(binary); string(got) != "new" {
		t.Fatalf("installed binary = %q, want new", got)
	}
	if err := restoreBinary(previous, binary); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(binary); string(got) != "old" {
		t.Fatalf("restored binary = %q, want old", got)
	}
}

func TestInstallBinaryFirstInstallHasNothingToRestore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	binary := filepath.Join(dir, "lark")
	build := filepath.Join(dir, ".deploy")
	if err := os.WriteFile(build, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	previous, err := installBinary(build, binary)
	if err != nil {
		t.Fatal(err)
	}
	if previous != "" {
		t.Fatalf("first install reported previous %q", previous)
	}
}

func TestDirDigestTracksContent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first, err := dirDigest(filepath.Join(dir, "missing"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode.jsonc"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := dirDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "opencode.jsonc"), []byte(`{"a":1}`), 0o600)
	third, err := dirDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || second == third {
		t.Fatalf("dirDigest did not change: %q %q %q", first, second, third)
	}
}
