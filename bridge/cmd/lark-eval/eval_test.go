package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const synthetic = "testdata/synthetic.json"

func TestScoreAndStubReplay(t *testing.T) {
	f, err := readFixture(synthetic)
	if err != nil {
		t.Fatal(err)
	}
	s := score(f.Sessions[0])
	if s.TurnCount != 2 || s.Succeeded != 1 || s.Interrupted != 1 || s.Aborted != 1 || s.ToolRoundsPer != .5 ||
		s.ToolErrors != 2 || s.LayoutWarnings != 1 || s.Tokens.Input != 14 || s.Tokens.Output != 20 ||
		s.Tokens.Reasoning != 2 || s.Tokens.Cache.Read != 42 || s.Tokens.Cache.Write != 7 || s.TokenSteps != 2 {
		t.Fatalf("unexpected fixture score: %+v", s)
	}
	first := s.Turns[0]
	if first.ToolCalls != 2 || first.ToolRounds != 1 || millis(first.FirstTokenMS) != "100ms" ||
		millis(first.CompletionMS) != "530ms" || millis(first.NextMessageMS) != "720ms" ||
		millis(s.CompletionMS.P95) != "530ms" || s.FirstTokenMS.N != 2 {
		t.Fatalf("unexpected timing and calls: %+v", first)
	}
	args := []string{"replay", "-fixture", synthetic, "-session", "Synthetic", "-turns", "2"}
	var stdout, stderr, second bytes.Buffer
	if err := run(t.Context(), args, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), args, &second, &stderr); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stdout.Bytes(), second.Bytes()) {
		t.Fatal("stub replay is not deterministic")
	}
	var report struct {
		Source   string
		Sessions []sessionScore
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || report.Source != "stub" || len(report.Sessions) != 1 {
		t.Fatal("stub did not emit a JSON report")
	}
	got := report.Sessions[0]
	if got.Session != "Synthetic#2" || got.Succeeded != 1 || got.LayoutWarnings != 1 || got.Turns[0].Turn != 2 ||
		millis(got.FirstTokenMS.P50) != "10ms" || millis(got.CompletionMS.P50) != "61ms" {
		t.Fatalf("unexpected stub projection: %+v", got)
	}
	for _, private := range []string{"Tell me", "synthetic-user", "Synthetic reply", "synthetic warning"} {
		if strings.Contains(stdout.String()+stderr.String(), private) {
			t.Fatal("replay output leaked a transcript field")
		}
	}
}

func TestExportIsReadOnlyAndPrivate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Error("export attempted a write")
			w.WriteHeader(405)
			return
		}
		if who, pass, _ := r.BasicAuth(); who != "opencode" || pass != "fake-password" {
			t.Error("expected Basic authentication")
			w.WriteHeader(401)
			return
		}
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/api/session" && q.Get("cursor") == "":
			_, _ = w.Write([]byte(`{"data":[{"id":"ses-cake","title":"Lark: CAKE (board)"},{"id":"ses-other","title":"Lark: Cakewalk (board)"}],"cursor":{"next":"test1"}}`))
		case r.URL.Path == "/api/session" && q.Get("cursor") == "test1":
			_, _ = w.Write([]byte(`{"data":[{"id":"ses-mint","title":"Lark: Mint"},{"id":"ses-greg","title":"Lark: greg"}]}`))
		case (r.URL.Path == "/api/session/ses-mint/message" || r.URL.Path == "/api/session/ses-cake/message" ||
			r.URL.Path == "/api/session/ses-greg/message") && q.Get("order") == "asc":
			_, _ = w.Write([]byte(`{"data":[{"id":"first","type":"user","text":"fictional transcript","time":{"created":1000}}],"cursor":{"next":"test2"}}`))
		case q.Get("cursor") == "test2" && !q.Has("order"):
			_, _ = w.Write([]byte(`{"data":[{"id":"second","type":"idle","outcome":"succeeded","time":{"created":1100}}]}`))
		default:
			t.Error("unexpected export endpoint")
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	api := newOpenCode(server.URL, "fake-password")
	defer api.client.CloseIdleConnections()
	f, err := collect(t.Context(), api)
	if err != nil || len(f.Sessions) != 3 || len(f.Sessions[0].Messages) != 2 || f.Sessions[0].Messages[0].Text != "fictional transcript" {
		t.Fatal("export omitted sessions or cursor pages")
	}
	dir := filepath.Join(t.TempDir(), "evals")
	if err := saveFixture(dir, f); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fixtureName)
	for name, want := range map[string]os.FileMode{dir: 0o700, path: 0o600} {
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm() != want {
			t.Fatal("export did not enforce private permissions")
		}
	}
	again, err := readFixture(path)
	if err != nil || !reflect.DeepEqual(f, again) {
		t.Fatal("fixture did not round-trip")
	}
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".git"), []byte("simulated worktree marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "worktree-link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	if err := saveFixture(filepath.Join(link, "would-leak"), f); err == nil {
		t.Fatal("export allowed a path into a Git worktree")
	}
}

func TestReplayDisposesOnlyItsSession(t *testing.T) {
	for _, raw := range []string{productionOC, "http://127.0.0.1:049375", "http://127.0.0.1:49376", "https://example.com:9000"} {
		if _, err := isolatedReplayURL(raw); err == nil {
			t.Fatal("accepted non-disposable address")
		}
	}
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "provider error"}[fail], func(t *testing.T) {
			var calls []string
			const sid = "/api/session/ses-owned"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path := r.Method + " " + r.URL.Path
				calls = append(calls, path)
				switch path {
				case "GET /api/mcp":
					_, _ = w.Write([]byte(`{"data":[{"name":"lark","status":{"status":"connected"}}]}`))
				case "GET /api/agent/lark":
					_, _ = w.Write([]byte(`{"data":{"id":"lark","model":{"providerID":"user","id":"choice","variant":"low"}}}`))
				case "POST /api/session":
					var in struct {
						Model agentModel
						Agent string
					}
					if json.NewDecoder(r.Body).Decode(&in) != nil || in.Agent != "lark" ||
						in.Model != (agentModel{ProviderID: "user", ID: "choice", Variant: "low"}) {
						t.Error("replay lost the configured actor")
					}
					_, _ = w.Write([]byte(`{"data":{"id":"ses-owned"}}`))
				case "POST " + sid + "/prompt":
					var in struct{ Text string }
					if json.NewDecoder(r.Body).Decode(&in) != nil || in.Text != "Tell me a short story." {
						t.Error("replay sent the wrong turn")
					}
					if fail {
						w.WriteHeader(429)
						return
					}
					_, _ = w.Write([]byte(`{"data":{"id":"user"}}`))
				case "POST /api/experimental" + sid + "/wait", "POST " + sid + "/interrupt", "DELETE " + sid:
					w.WriteHeader(204)
				case "GET " + sid + "/message":
					_, _ = w.Write([]byte(`{"data":[{"id":"user","type":"user","time":{"created":1000}},{"id":"idle","type":"idle","outcome":"succeeded","time":{"created":1100}}]}`))
				default:
					t.Error("unexpected replay endpoint")
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			f, err := readFixture(synthetic)
			if err != nil {
				t.Fatal(err)
			}
			// Close the test transport before reading the request audit.
			scores, mode, err := replay(context.Background(), f, "Synthetic", "2", server.URL, t.TempDir(), 3*time.Second)
			server.Close()
			if fail {
				if err == nil || !strings.Contains(err.Error(), "HTTP 429") ||
					strings.Join(calls[len(calls)-2:], ",") != "POST "+sid+"/interrupt,DELETE "+sid {
					t.Fatal("failed replay did not interrupt and delete its session")
				}
			} else if err != nil || mode != "live" || len(scores) != 1 || scores[0].Succeeded != 1 ||
				calls[len(calls)-1] != "DELETE "+sid {
				t.Fatal("live replay did not score and delete its session")
			}
		})
	}
}
