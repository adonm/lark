package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testRequest(t *testing.T, method, url, token string, data any, want int) []byte {
	t.Helper()
	var in io.Reader
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		in = bytes.NewReader(raw)
	}
	r, err := http.NewRequestWithContext(t.Context(), method, url, in)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", jsonType)
	r.Header.Set("X-Lark-Token", token)
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != want {
		t.Fatalf("%s: HTTP %d, want %d (%s; %v)", r.URL.Path, res.StatusCode, want, raw, err)
	}
	return raw
}

func testObject(t *testing.T, raw []byte) object {
	t.Helper()
	value, err := decodeObject(raw)
	if err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return value
}

func rpcTest(t *testing.T, base, method string, params object) object {
	t.Helper()
	raw := testRequest(t, "POST", base+"/mcp", "unit", object{"jsonrpc": "2.0", "id": "test", "method": method, "params": params}, 200)
	value := testObject(t, raw)
	if value["id"] != "test" || value["jsonrpc"] != "2.0" {
		t.Fatalf("invalid JSON-RPC: %s", raw)
	}
	result, _ := value["result"].(object)
	if result == nil {
		t.Fatalf("RPC error: %s", raw)
	}
	return result
}

func toolTest(t *testing.T, base, tool string, args object, failed bool) object {
	t.Helper()
	result := rpcTest(t, base, "tools/call", object{"name": tool, "arguments": args})
	value, _ := result["structuredContent"].(object)
	if value == nil || result["isError"] != failed {
		t.Fatalf("%s: %+v (expected error: %v)", tool, result, failed)
	}
	return value
}

func fakeTTS(ctx context.Context, requests *atomic.Int32) *synthWorker {
	w := &synthWorker{requests: make(chan ttsRequest), warming: make(chan ttsRequest), done: make(chan struct{})}
	w.ready.Store(true)
	go func() {
		defer close(w.done)
		for {
			req, ok := w.next(ctx)
			if !ok {
				return
			}
			if requests != nil {
				requests.Add(1)
			}
			req.done <- ttsResult{pcm: []byte{1, 2, 3, 4}}
		}
	}()
	return w
}

func TestMCPProtocol(t *testing.T) {
	_, base := wsHarness(t, nil)
	const id = "lark-test"
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, id, "Cake")
	testRequest(t, "GET", base+"/mcp", "unit", nil, 405)
	testRequest(t, "DELETE", base+"/mcp", "unit", nil, 204)
	testRequest(t, "POST", base+"/mcp", "", object{}, 401)
	testRequest(t, "POST", base+"/mcp", "unit", []string{"bad"}, 400)
	init := rpcTest(t, base, "initialize", object{"protocolVersion": "2025-06-18"})
	if init["protocolVersion"] != "2025-06-18" || init["serverInfo"].(object)["name"] != "lark" {
		t.Fatal(init)
	}
	testRequest(t, "POST", base+"/mcp", "unit", object{"jsonrpc": "2.0", "method": "notifications/initialized"}, 202)
	if len(rpcTest(t, base, "ping", object{})) != 0 {
		t.Fatal("ping should return an empty result")
	}
	names := []string{}
	for _, raw := range rpcTest(t, base, "tools/list", object{})["tools"].([]any) {
		tool := raw.(object)
		names = append(names, str(tool["name"]))
		if tool["inputSchema"].(object)["type"] != "object" {
			t.Fatal(tool)
		}
	}
	if !slices.Equal(names, []string{"state", "patch", "story", "emit"}) {
		t.Fatal(names)
	}
	for _, name := range []string{"speak", "inspect", "act", "run_app", "store", "app", "build"} {
		old := testObject(t, testRequest(t, "POST", base+"/mcp", "unit", object{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": object{"name": name}}, 200))
		if old["error"].(object)["code"] != json.Number("-32602") {
			t.Fatal(old)
		}
	}
	snap := toolTest(t, base, "state", object{"companion_id": "cake"}, false)
	me := snap["me"].(object)
	if len(snap) != 5 || me["companion"].(object)["name"] != "Cake" || len(me["terminals"].([]any)) != 1 {
		t.Fatal(snap)
	}
	patchControl(t, base, "cake", object{"volume": 40})
	toolTest(t, base, "patch", object{"memory": object{"scope": "companion:cake", "key": "best", "value": 7, "expected_version": 0}}, false)
	if v := toolTest(t, base, "patch", object{"memory": object{"op": "delete", "scope": "companion:cake", "key": "best", "expected_version": 0}}, true); v["version"] != json.Number("1") {
		t.Fatal(v)
	}
}

func TestMCPClosedArguments(t *testing.T) {
	_, base := wsHarness(t, nil)
	for _, tc := range []struct {
		tool string
		args object
		path string
	}{
		{"state", object{"include": []string{}, "device": "typo"}, "device"},
		{"patch", object{"companion_id": "test", "companion": object{"save_profle": true}}, "companion.save_profle"},
		{"patch", object{"timer": object{"seconds": 5}}, "timer.seconds"},
		{"patch", object{"memory": []object{{"scope": "shared", "key": "k", "value": 1, "typo": 2}}}, "memory.typo"},
		{"emit", object{"to": "all", "intercom": object{"from": "test", "text": "Hello", "typo": 1}}, "intercom.typo"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			out := toolTest(t, base, tc.tool, tc.args, true)
			if out["error"] != tc.path+": unknown argument" {
				t.Fatal(out)
			}
		})
	}
	mem := object{"device_id": []any{1, nil, object{"anything": true}}}
	toolTest(t, base, "patch", object{"memory": object{"scope": "shared", "key": "flex", "value": mem}}, false)
	out := toolTest(t, base, "state", object{"include": []string{"memory"}}, false)
	if v := out["memory"].(object)["shared"].(object)["flex"].(object)["value"].(object); len(v["device_id"].([]any)) != 3 {
		t.Fatal(v)
	}
}

func stateMe(t *testing.T, base, id string) object {
	t.Helper()
	return toolTest(t, base, "state", object{"companion_id": id, "include": []string{}}, false)["me"].(object)
}

func patchControl(t *testing.T, base, id string, terminal object) object {
	t.Helper()
	return toolTest(t, base, "patch", object{"companion_id": id, "terminal": terminal}, false)["terminal"].(object)
}

func TestMCPStateSlices(t *testing.T) {
	b, base := wsHarness(t, func(b *larkServer, _ string) {
		b.timers = newTimerManager(b)
		t.Cleanup(b.timers.close)
	})
	const termID, id = "vocat-0001", "cake"
	board := testConnectWS(t, base, "unit", false)
	board.helloHardware(t, termID, "Cake", capBattery|capDrag)
	var st [16]byte
	rssi, batt := int16(-62), int16(73)
	binary.LittleEndian.PutUint16(st[0:2], uint16(rssi))
	binary.LittleEndian.PutUint16(st[2:4], uint16(batt))
	st[5] = 24
	board.sendProto(t, msgTelemetry, st[:])
	board.barrier(t)
	b.ws.mu.Lock()
	wifiOK := b.ws.terminals[termID].telemetry.WiFi != nil
	b.ws.mu.Unlock()
	if !wifiOK {
		t.Fatal("missing telemetry wifi")
	}
	toolTest(t, base, "patch", object{"companion_id": id, "companion": object{"name": "Slices"}, "timer": object{"in_seconds": 300, "chime": "tap"},
		"memory": []object{{"scope": "shared", "key": "score", "value": object{"points": 7}}, {"scope": "companion:" + id, "key": "seen", "value": []any{true, nil}}}}, false)
	message := newChat("user", "You", "latest canonical message", "typed")
	message.Timestamp = time.Now().UTC().Add(time.Second)
	b.history.add(id, message)
	b.diag.mu.Lock()
	b.diag.metrics[diagE2E].add(12, 32)
	b.diag.mu.Unlock()

	args := object{"companion_id": id, "include": []string{"companions", "terminals", "timers", "memory", "messages", "diagnostics"},
		"messages_limit": 1}
	s := toolTest(t, base, "state", args, false)
	term, companion := s["terminals"].(object)[termID].(object), s["companions"].(object)[id].(object)
	if term["wifi_rssi"] != json.Number("-62") || term["cpu_pct"] != json.Number("24") || term["battery_pct"] != json.Number("73") ||
		companion["name"] != "Slices" {
		t.Fatal("missing live terminal or companion state", term)
	}
	if len(s["timers"].([]any)) != 1 {
		t.Fatal("missing timer")
	}
	mem, msgs := s["memory"].(object), s["messages"].([]any)
	if mem["shared"].(object)["score"].(object)["value"].(object)["points"] != json.Number("7") ||
		!slices.Equal(mem["companion:"+id].(object)["seen"].(object)["value"].([]any), []any{true, nil}) ||
		len(msgs) != 1 || msgs[0].(object)["text"] != "latest canonical message" {
		t.Fatal(mem, msgs)
	}
	diag, now := s["diagnostics"].(object), s["now"].(object)
	latency := diag["latency"].(object)[diagMetricNames[diagE2E]].(object)
	if latency["p50"] != json.Number("12") || latency["p95"] != json.Number("12") ||
		diag["bridge"] == nil || diag["tts"] == nil || diag["host"] == nil || len(diag["terminals"].([]any)) != 1 ||
		now["unix_ms"] == nil || now["local"] == "" {
		t.Fatal(diag, now)
	}
	only := toolTest(t, base, "state", object{"include": []string{"timers"}}, false)
	if len(only) != 2 || len(only["timers"].([]any)) != 1 {
		t.Fatal("unrequested slice leaked", only)
	}
}

func TestEmitIntercom(t *testing.T) {
	b, base := wsHarness(t, func(b *larkServer, _ string) {
		b.worker = fakeTTS(b.root, nil)
	})
	const from, to, text = "cake", "mint", "Dinner is ready."
	if _, err := b.createCompanion(t.Context(), object{"id": to, "name": "Mint", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	sender, recipient := testConnectWS(t, base, "unit", false), testConnectWS(t, base, "unit", false)
	sender.hello(t, "vocat-0001", "Cake")
	recipient.hello(t, "vocat-0002", to)
	out := toolTest(t, base, "emit", object{"to": "all", "intercom": object{"from": from, "text": text}}, false)
	if !slices.Equal(out["intercom"].(object)["sent_to"].([]any), []any{to}) {
		t.Fatal(out)
	}
	recipient.until(t, isPlayback)
	waitSVG(t, b.ws, "vocat-0002", text)
	for _, id := range []string{from, to} {
		s := toolTest(t, base, "state", object{"companion_id": id, "include": []string{"messages"}}, false)
		msgs := s["messages"].([]any)
		if len(msgs) != 1 || msgs[0].(object)["source"] != "intercom" || msgs[0].(object)["text"] != text {
			t.Fatal(msgs)
		}
	}
}

func TestMCPControl(t *testing.T) {
	b, base := wsHarness(t, nil)
	const termID, id = "vocat-control", "cake"
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, termID, "Cake")

	for _, invalid := range []object{
		{}, {"volume": -1}, {"volume": 101}, {"volume": 1.5}, {"sleep": "true"},
	} {
		toolTest(t, base, "patch", object{"companion_id": id, "terminal": invalid}, true)
	}
	b.ws.mu.Lock()
	d := b.ws.terminals[termID]
	b.ws.startTurn(d, time.Now(), false, "typed", func(context.Context, uint64) {})
	turn := d.turn
	b.ws.mu.Unlock()
	out := patchControl(t, base, id, object{"volume": 0, "sleep": true})
	if out["terminal_id"] != termID || out["volume"] != json.Number("0") || out["sleeping"] != true {
		t.Fatal(out)
	}
	for _, client := range []*testWS{c} {
		client.until(t, func(f wsFrame) bool {
			p := framePayload(f)
			return hasProto(msgVolume, "")(f) && len(p) >= 1 && p[0] == 0
		})
		client.until(t, func(f wsFrame) bool {
			p := framePayload(f)
			return hasProto(msgPower, "")(f) && len(p) >= 1 && p[0] == 12
		})
	}
	b.ws.finishTurn(d, turn, nil, nil)
	checkStatus := func(mode, volume string) {
		t.Helper()
		me := stateMe(t, base, id)
		comp := me["companion"].(object)
		vol := me["terminals"].([]any)[0].(object)["volume"].(object)
		if comp["mode"] != mode || vol["volume"] != json.Number(volume) {
			t.Fatal(me)
		}
	}
	checkStatus("sleepy", "0")
	if wake := patchControl(t, base, id, object{"sleep": false}); wake["sleeping"] != false || wake["volume"] != json.Number("0") {
		t.Fatal(wake)
	}
	c.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgPower, "")(f) && len(p) >= 1 && p[0] == 85
	})
	if vol := patchControl(t, base, id, object{"volume": 42}); vol["sleeping"] != false || vol["volume"] != json.Number("42") {
		t.Fatal(vol)
	}
	c.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgVolume, "")(f) && len(p) >= 1 && p[0] == 42
	})
	checkStatus("idle", "42")
}

func (c *condSignal) wait(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		c.mu.Lock()
		ch := c.ch
		if ch == nil {
			ch = make(chan struct{})
			c.ch = ch
		}
		c.mu.Unlock()
		if ready() {
			return
		}
		select {
		case <-ch:
		case <-deadline.C:
			t.Fatal("condition did not become true")
		}
	}
}

func waitPool(t *testing.T, ready func() bool) {
	t.Helper()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for !ready() {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("condition did not become true")
		}
	}
}

func TestMCPStdioForwards(t *testing.T) {
	var posts atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer unit" {
			t.Error("incorrect proxy request")
		}
		posts.Add(1)
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if len(req.ID) == 0 {
			w.WriteHeader(202)
			return
		}
		_, _ = fmt.Fprintf(w, "{\n\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"method\":%q}\n}", req.ID, req.Method)
	}))
	defer s.Close()
	t.Setenv("LARK_TOKEN", "")
	t.Setenv("LARK_PORT", "")
	file := filepath.Join(t.TempDir(), "lark.env")
	port := s.Listener.Addr().(*net.TCPAddr).Port
	if err := os.WriteFile(file, []byte("LARK_TOKEN='unit'\nLARK_PORT="+strconv.Itoa(port)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	url, token, err := mcpProxyConfig(file)
	if err != nil || url != s.URL+"/mcp" || token != "unit" {
		t.Fatal("proxy could not load its env file")
	}
	var out bytes.Buffer
	in := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n" +
		"{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n{\"jsonrpc\":\"2.0\",\"id\":\"p\",\"method\":\"ping\"}\n")
	if err := proxyMCP(t.Context(), s.Client(), url, token, in, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if posts.Load() != 3 || len(lines) != 2 {
		t.Fatal("proxy did not forward all messages / suppressed notification response")
	}
	got := map[string]bool{}
	for _, line := range lines {
		value := testObject(t, []byte(line))
		got[str(value["result"].(object)["method"])] = true
	}
	if !got["initialize"] || !got["ping"] {
		t.Fatal(out.String())
	}
}
