package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
)

func TestMCPSchemaHash(t *testing.T) {
	original := hashMCPSchema(mcpTools, mcpInstructions, mcpRuntimeContract, "")
	if raw, err := hex.DecodeString(original); err != nil || len(raw) != 32 || original != hashMCPSchema(mcpTools, mcpInstructions, mcpRuntimeContract, "") {
		t.Fatal("schema hash is not a deterministic SHA-256 hex digest")
	}
	for _, key := range []string{"name", "description", "inputSchema"} {
		tools := slices.Clone(mcpTools)
		tools[0] = maps.Clone(tools[0])
		tools[0][key] = "changed"
		if hashMCPSchema(tools, mcpInstructions, mcpRuntimeContract, "") == original {
			t.Errorf("changing %s did not change the schema hash", key)
		}
	}
	for _, tc := range []struct{ name, instructions, runtime, prompt string }{
		{"instructions", "New instructions.", mcpRuntimeContract, ""},
		{"runtime", mcpInstructions, "new runtime", ""},
		{"agent prompt", mcpInstructions, mcpRuntimeContract, "New agent prompt."},
	} {
		if hashMCPSchema(mcpTools, tc.instructions, tc.runtime, tc.prompt) == original {
			t.Errorf("changing %s did not change the schema hash", tc.name)
		}
	}
}

func mcpServerStatus(name, status string) []object {
	return []object{{"name": name, "status": object{"status": status}}}
}

func TestCompactAPIOnlyOnAuthenticatedLAN(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b := &bridge{token: "unit"}
	connects := new(atomic.Int32)
	b.oc = testOpenCode(t, ctx, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session/ses-cake":
			sendJSON(w, 200, object{"data": object{"id": "ses-cake"}})
		case "/api/session/ses-cake/compact":
			if raw, _ := io.ReadAll(r.Body); string(raw) != `{"delivery":"queue"}` {
				t.Error("compaction did not queue")
			}
			sendJSON(w, 200, object{"data": object{"id": "msg-test", "sessionID": "ses-cake", "type": "compaction"}})
		case "/api/session/ses-cake/message/msg-test":
			sendJSON(w, 200, object{"data": object{"id": "msg-test", "type": "compaction", "status": "completed"}})
		case "/api/experimental/mcp/lark/connect":
			connects.Add(1)
			w.WriteHeader(204)
		case "/api/mcp":
			sendJSON(w, 200, object{"data": mcpServerStatus("lark", "connected")})
		case "/api/experimental/session/ses-cake/wait":
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	})
	if err := b.oc.save("Cake", "ses-cake"); err != nil {
		t.Fatal(err)
	}
	lan := httptest.NewServer(b)
	defer lan.Close()
	scoped := httptest.NewServer(b.scoped(virtualClient{ID: "virt-12345678"}, "scoped"))
	defer scoped.Close()
	const path = "/api/opencode/compact"
	for _, token := range []string{"", "scoped", "unit"} {
		testRequest(t, http.MethodPost, scoped.URL+path, token, nil, 404)
	}
	testRequest(t, http.MethodPost, lan.URL+path+"?token=unit", "", nil, 401)
	testRequest(t, http.MethodPost, lan.URL+path, "scoped", nil, 401)
	testRequest(t, http.MethodGet, lan.URL+path, "unit", nil, 404)
	testRequest(t, http.MethodPost, lan.URL+path, "unit", object{"reconnect_mcp": "yes"}, 400)
	check := func(input any, compacted, skipped []string) {
		t.Helper()
		out := testRequest(t, http.MethodPost, lan.URL+path, "unit", input, 200)
		var got compactResult
		if err := json.Unmarshal(out, &got); err != nil || !got.OK ||
			got.Compacted == nil || !slices.Equal(got.Compacted, compacted) ||
			got.Skipped == nil || !slices.Equal(got.Skipped, skipped) {
			t.Fatalf("unexpected compaction result: %s", out)
		}
	}
	check(nil, []string{"Cake"}, nil)
	check(object{"device_id": "unknown"}, nil, []string{"unknown"})
	check(object{"device_id": "Cake", "reconnect_mcp": true}, []string{"Cake"}, nil)
	if connects.Load() != 1 {
		t.Fatal("manual MCP reconnect should only run when requested")
	}
}
