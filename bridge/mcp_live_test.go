//go:build live

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"
)

func TestRealModelEchoEarCodeMode(t *testing.T) {
	if os.Getenv("ECHOEAR_REAL_MODEL") != "1" {
		t.Skip("opt in with scripts/check.sh --real-model")
	}
	smokeHome := os.Getenv("ECHOEAR_SMOKE_CONFIG_HOME")
	if smokeHome == "" {
		t.Fatal("set ECHOEAR_SMOKE_CONFIG_HOME to the separate config of a disposable bridge and OpenCode pair")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", smokeHome)
	config := liveConfig(t, home)
	port := config["ECHOEAR_BRIDGE_PORT"]
	ocURL, err := url.Parse(config["ECHOEAR_OPENCODE_URL"])
	portNum, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || portNum < 1 || portNum > 65535 || portNum == 8765 {
		t.Fatal("the smoke requires separate loopback ports; never point it at the installed bridge or OpenCode")
	}
	ocPort, err := strconv.Atoi(ocURL.Port())
	if err != nil || ocPort == 49375 || ocPort == 49376 {
		t.Fatal("the smoke requires separate loopback ports; never point it at the installed bridge or OpenCode")
	}
	client := pooledClient()
	client.Timeout = 105 * time.Second
	defer client.CloseIdleConnections()
	c := liveClient{client, "http://" + net.JoinHostPort("127.0.0.1", port), config["ECHOEAR_BRIDGE_TOKEN"]}
	catalog := liveJSON[struct {
		Result struct{ Tools []struct{ Name string } }
	}](t, c, "/mcp", object{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}, nil, "", nil)
	names := []string{}
	for _, tool := range catalog.Result.Tools {
		names = append(names, tool.Name)
	}
	if !slices.Equal(names, []string{"state", "patch", "app", "emit"}) {
		t.Fatal("unexpected live MCP catalog", names)
	}
	t.Logf("MCP catalog: %v", names)
	const character = "LiveMCP"
	const clients = "/api/virtual-clients"
	v := testObject(t, testRequest(t, http.MethodPost, c.base+clients, c.token,
		object{"character": character, "theme": "cake"}, http.StatusCreated))
	id := str(v["id"])
	if !virtualID.MatchString(id) {
		t.Fatal("unexpected virtual client ID")
	}
	defer testRequest(t, http.MethodDelete, c.base+clients+"/"+id, c.token, nil, http.StatusNoContent)
	sock := testConnectWS(t, c.base, c.token, true)
	sock.json(t, object{"ev": "hello", "role": "web", "device_id": id})
	sock.until(t, hasWS("svg", "cat-ring"))
	api := liveOpenCode(t, client, home, config)
	type serverInfo struct {
		Name   string
		Status struct{ Status string }
	}
	// A location's first API request can return before MCP autoconnect finishes.
	ready, cancelReady := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelReady()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	var mcp []serverInfo
	for {
		mcp, err = ocData[[]serverInfo](ready, api, http.MethodGet, "/api/mcp", nil)
		if err != nil {
			t.Fatal(err)
		}
		if slices.ContainsFunc(mcp, func(server serverInfo) bool {
			return server.Name == "echoear" && server.Status.Status == "connected"
		}) {
			break
		}
		select {
		case <-ready.Done():
			t.Fatalf("OpenCode echoear MCP server did not connect: %+v", mcp)
		case <-tick.C:
		}
	}
	if config["ECHOEAR_OPENCODE_URL"] != "" {
		type plugin struct {
			ID     string
			Source struct{ Type string }
		}
		plugins, err := ocData[[]plugin](t.Context(), api, http.MethodGet, "/api/plugin", nil)
		if err != nil || len(mcp) != 1 || slices.ContainsFunc(plugins, func(p plugin) bool { return p.Source.Type != "builtin" && p.ID != "echoear.model-fallback" }) {
			t.Fatalf("expected only EchoEar MCP and EchoEar or bundled plugins: %v", err)
		}
	}
	settings := openCodeSettings("echoear")
	root, cancel := context.WithCancel(t.Context())
	defer cancel()
	prefix := "echoear-live-mcp-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	liveSessions(t, api, prefix)
	o := newOpenCode(root, api, testDB(t, ":memory:"), liveOpenCodeDir(home, config), settings)
	settings, err = o.configuredSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	prompt := turnPrompt(peer{DeviceID: id, Character: character}, false, "typed",
		"Run a live screen and controls check. Use Code Mode execute to call tools.echoear.state({device_id:'"+id+
			"',include:['devices']}), tools.echoear.patch({device_id:'"+id+"',device:{sleep:false}}), tools.echoear.emit({to:'"+id+
			"',chime:'tap'}), and tools.echoear.app({devices:'"+id+
			"',exit:true}). Return all four tool results from execute, then say one short sentence. Do not call any other tool.",
		blankCat())
	turn, endTurn := context.WithTimeout(root, 60*time.Second)
	defer endTurn()
	_, sid, err := o.askPrepared(turn, prefix, func() (string, string) { return character, prompt }, turnStream{}, "typed", nil, nil)
	if err != nil {
		t.Fatalf("real-model Code Mode smoke failed (separate from release checks): %v", err)
	}
	assertLiveActor(t, api, sid, settings)
	type toolPart struct {
		Type, Name string
		State      struct {
			Status         string
			Input, Content json.RawMessage
		}
	}
	msgs, err := ocData[[]struct{ Content []toolPart }](t.Context(), api, http.MethodGet, "/api/session/"+sid+"/message?order=asc&limit=50", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"state": "active_app", "app": "hit_ids", "emit": "played", "patch": "sleeping"}
	attempted := map[string]bool{}
	var lastResult string
	for _, msg := range msgs {
		for _, part := range msg.Content {
			if part.Type != "tool" || part.Name != "execute" || part.State.Status != "completed" {
				continue
			}
			lastResult = truncate(string(part.State.Content), 512)
			for tool, field := range want {
				called := bytes.Contains(part.State.Input, []byte("tools.echoear."+tool)) || bytes.Contains(part.State.Input, []byte(`["`+tool+`"]`))
				attempted[tool] = attempted[tool] || called
				if called && bytes.Contains(part.State.Content, []byte(field)) {
					delete(want, tool)
				}
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("Code Mode results not seen: %v (tool references: %v; latest tool result: %s)", want, attempted, lastResult)
	}
	if messages := liveJSON[struct{ Messages []chatEntry }](t, c, "/api/devices/"+id+"/messages", nil, nil, "", nil).Messages; len(messages) != 0 {
		t.Fatal("Code Mode smoke test wrote to canonical chat history")
	}
	t.Log("echoear agent completed state, patch, app, and emit through execute")
}
