//go:build live && sqlite_fts5

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestQALocalLibraryBehaviour drives the installed service with a real model.
// It checks that the local library is preferred for reference and story
// turns, that it opens the reader silently, and that current questions still
// fall back to websearch. Temporary companions only; no contact is messaged.
func TestQALocalLibraryBehaviour(t *testing.T) {
	if os.Getenv("LARK_QA_LIBRARY") != "1" {
		t.Skip("set LARK_QA_LIBRARY=1 to run real-model library behaviour checks")
	}
	c := qaLiveClient(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	api := liveOpenCode(t, c.http, home, liveConfig(t, home))
	data := env("XDG_DATA_HOME", filepath.Join(home, ".local/share"))
	libDir := env("LARK_LIBRARY_DIR", filepath.Join(data, "lark", "library"))
	if _, err := os.Stat(filepath.Join(libDir, "simple.db")); err != nil {
		t.Skip("library is not installed")
	}

	type scenario struct {
		name, prompt       string
		wantLibrary        bool
		wantWeb, wantStory bool
	}
	scenarios := []scenario{
		{"baseline", "Say hello in a friendly voice.", false, false, false},
		{"reference", "Why is the sky blue?", true, false, false},
		{"story", "Tell me the story of the three little pigs.", true, false, true},
		{"current", "What is the current price of Bitcoin?", false, true, false},
	}
	suffix := fmt.Sprintf("%x", time.Now().UnixNano()&0xffff)
	var baselinePCM int64
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			beforeDiag := localLibraryDiagnostics(t, c)
			beforeOpens, _ := numInt(beforeDiag["opens"])
			const storyPattern = "%three little pigs%"
			var beforeStories map[string]bool
			if sc.wantStory {
				beforeStories = localLibraryStoryIDs(t, storyPattern)
				// Remove only rows this scenario creates; a story the user
				// already owned must survive the test.
				t.Cleanup(func() {
					localLibraryRemoveNewStories(t, storyPattern, beforeStories)
				})
			}
			compID := qaTempCompanion(t, c, "qa-lib-"+sc.name+"-"+suffix, "cat")
			script := fmt.Sprintf("attach %s\ntap 180 180\nsay %s\nwait thinking 30s\nwait idle 120s\ntimeline\n", compID, sc.prompt)
			out := qaSimRun(t, "vocat", "sim-lib-"+sc.name, script)
			firstPCM := int64(0)
			for _, row := range out {
				if events, ok := row["events"].([]any); ok {
					for _, event := range events {
						if m, _ := event.(object); m["event"] == "tts_first_pcm" {
							if ms, ok := numInt(m["t_ms"]); ok {
								firstPCM = int64(ms)
							}
						}
					}
				}
			}
			if sc.name == "baseline" {
				baselinePCM = firstPCM
			}
			sid := localLibrarySession(t, api, compID)
			msgs := localLibraryMessages(t, api, sid)
			sawLibrary := localLibraryTrace(msgs, "library")
			sawWeb := localLibraryTrace(msgs, "websearch")
			if sc.wantLibrary && !sawLibrary {
				t.Fatalf("expected a library tool call, trace: %s", localLibraryTraceText(msgs))
			}
			if !sc.wantLibrary && sawLibrary && sc.name != "current" {
				t.Fatalf("unexpected library call, trace: %s", localLibraryTraceText(msgs))
			}
			if sc.wantWeb && !sawWeb {
				t.Fatalf("expected websearch fallback, trace: %s", localLibraryTraceText(msgs))
			}
			if !sc.wantWeb && sawWeb {
				t.Fatalf("unexpected websearch, trace: %s", localLibraryTraceText(msgs))
			}
			if reply := localLibraryReply(msgs); reply == "" {
				t.Fatal("empty assistant reply")
			}
			if sc.wantStory {
				// Assert a fresh import rather than any row with this title:
				// a story already in the library would prove nothing.
				created := 0
				for id := range localLibraryStoryIDs(t, storyPattern) {
					if !beforeStories[id] {
						created++
					}
				}
				if created == 0 {
					t.Fatal("story was not imported")
				}
				afterOpens, _ := numInt(localLibraryDiagnostics(t, c)["opens"])
				if afterOpens <= beforeOpens {
					t.Fatal("reader did not open silently")
				}
			}
			t.Logf("%s: library=%v websearch=%v first_pcm=%dms added_vs_baseline=%dms reply=%q",
				sc.name, sawLibrary, sawWeb, firstPCM, firstPCM-baselinePCM, localLibraryReply(msgs))
		})
	}
}

func localLibraryDB(t *testing.T) *sql.DB {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	data := env("XDG_DATA_HOME", filepath.Join(home, ".local/share"))
	path := env("LARK_DB_PATH", filepath.Join(data, "lark", "lark.db"))
	db, err := sql.Open("lark-sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// localLibraryStoryIDs returns the ids of stories whose title matches a SQL
// LIKE pattern.
func localLibraryStoryIDs(t *testing.T, pattern string) map[string]bool {
	t.Helper()
	db := localLibraryDB(t)
	defer db.Close()
	rows, err := db.Query(`SELECT id FROM stories WHERE lower(title) LIKE ?`, pattern)
	if err != nil {
		t.Fatalf("list stories: %v", err)
	}
	defer rows.Close()
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("list stories: %v", err)
		}
		ids[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list stories: %v", err)
	}
	return ids
}

// localLibraryRemoveNewStories deletes only stories that appeared since keep,
// plus their progress rows. Stories the test found beforehand are left alone.
func localLibraryRemoveNewStories(t *testing.T, pattern string, keep map[string]bool) {
	t.Helper()
	var created []string
	for id := range localLibraryStoryIDs(t, pattern) {
		if !keep[id] {
			created = append(created, id)
		}
	}
	if len(created) == 0 {
		return
	}
	db := localLibraryDB(t)
	defer db.Close()
	for _, id := range created {
		if _, err := db.Exec(`DELETE FROM story_progress WHERE story_id = ?`, id); err != nil {
			t.Fatalf("clear story progress %s: %v", id, err)
		}
		if _, err := db.Exec(`DELETE FROM stories WHERE id = ?`, id); err != nil {
			t.Fatalf("clear story %s: %v", id, err)
		}
	}
}

func localLibrarySession(t *testing.T, api upstream, companion string) string {
	t.Helper()
	// The Lark binding can be replaced while a turn is in flight. Resolve the
	// OpenCode session by its title so the trace check does not depend on the
	// sessions table.
	path := "/api/session?" + url.Values{"search": {companion}, "limit": {"10"}}.Encode()
	sessions, err := ocData[[]struct{ ID, Title string }](t.Context(), api, "GET", path, nil)
	if err != nil {
		t.Fatalf("list OpenCode sessions for %s: %v", companion, err)
	}
	want := "Lark: " + companion
	for _, session := range sessions {
		if session.Title == want || strings.HasPrefix(session.Title, want+" (") ||
			strings.HasSuffix(session.Title, "("+companion+")") {
			return session.ID
		}
	}
	t.Fatalf("no OpenCode session titled %q", want)
	return ""
}

type localLibraryPart struct {
	Type  string
	Name  string
	Text  string
	State struct {
		Status string
		Input  json.RawMessage
		Output json.RawMessage
	}
}

func localLibraryMessages(t *testing.T, api upstream, sid string) []struct{ Content []localLibraryPart } {
	t.Helper()
	msgs, err := ocData[[]struct{ Content []localLibraryPart }](t.Context(), api, "GET",
		"/api/session/"+sid+"/message?order=asc&limit=100", nil)
	if err != nil {
		t.Fatalf("read session %s: %v", sid, err)
	}
	return msgs
}

func localLibraryTrace(msgs []struct{ Content []localLibraryPart }, needle string) bool {
	needle = strings.ToLower(needle)
	for _, msg := range msgs {
		for _, part := range msg.Content {
			if part.Type != "tool" {
				continue
			}
			raw := strings.ToLower(part.Name + " " + string(part.State.Input) + " " + string(part.State.Output))
			if strings.Contains(raw, needle) {
				return true
			}
		}
	}
	return false
}

func localLibraryTraceText(msgs []struct{ Content []localLibraryPart }) string {
	var parts []string
	for _, msg := range msgs {
		for _, part := range msg.Content {
			if part.Type == "tool" && part.Name != "" {
				parts = append(parts, part.Name)
			}
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}

func localLibraryReply(msgs []struct{ Content []localLibraryPart }) string {
	var text string
	for _, msg := range msgs {
		for _, part := range msg.Content {
			if part.Type == "text" {
				text += part.Text
			}
		}
	}
	return strings.TrimSpace(text)
}

func localLibraryDiagnostics(t *testing.T, c *cliClient) object {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	diag, err := c.do(ctx, "GET", "/api/diagnostics", nil)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	lib, _ := diag["library"].(object)
	if lib == nil {
		return object{}
	}
	return lib
}
