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
	"reflect"
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
	if !slices.Equal(names, []string{"state", "patch", "app", "emit"}) {
		t.Fatal(names)
	}
	for _, name := range []string{"speak", "inspect", "act", "run_app", "store"} {
		old := testObject(t, testRequest(t, "POST", base+"/mcp", "unit", object{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": object{"name": name}}, 200))
		if old["error"].(object)["code"] != json.Number("-32602") {
			t.Fatal(old)
		}
	}
	snap := toolTest(t, base, "state", object{"companion_id": "cake"}, false)
	me := snap["me"].(object)
	if len(snap) != 6 || me["companion"].(object)["name"] != "Cake" || len(me["terminals"].([]any)) != 1 {
		t.Fatal(snap)
	}
	patchControl(t, base, "cake", object{"volume": 40})
	toolTest(t, base, "patch", object{"memory": object{"scope": "app:scores", "key": "best", "value": 7, "expected_version": 0}}, false)
	if v := toolTest(t, base, "patch", object{"memory": object{"op": "delete", "scope": "app:scores", "key": "best", "expected_version": 0}}, true); v["version"] != json.Number("1") {
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
		{"patch", object{"apps": object{"delete": []string{"book"}, "typo": true}}, "apps.typo"},
		{"patch", object{"apps": object{"rename": []object{{"from": "book", "to": "novel", "typo": true}}}}, "apps.rename.typo"},
		{"app", object{"delete_saved": "sample", "typo": true}, "typo"},
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
	const code = `function render(s){return '<svg><text x="80" y="180">'+s.n+'</text></svg>';}`
	toolTest(t, base, "app", object{"companions": id, "save_as": "slices", "code": code, "initial_state": object{"n": 8}}, false)
	history, err := b.history.read(t.Context(), id, 1)
	if err != nil || len(history) != 1 || history[0].Source != "app" {
		t.Fatalf("missing app history: %v (%v)", history, err)
	}
	message := newChat("user", "You", "latest canonical message", "typed")
	message.Timestamp = history[0].Timestamp.Add(time.Millisecond)
	b.history.add(id, message)
	b.diag.mu.Lock()
	b.diag.metrics[diagE2E].add(12, 32)
	b.diag.mu.Unlock()

	args := object{"companion_id": id, "include": []string{"companions", "terminals", "apps", "timers", "memory", "messages", "diagnostics"},
		"include_code": true, "messages_limit": 1}
	s := toolTest(t, base, "state", args, false)
	term, me := s["terminals"].(object)[termID].(object), s["me"].(object)
	active, companion := me["active_app"].(object), s["companions"].(object)[id].(object)
	if term["wifi_rssi"] != json.Number("-62") || term["cpu_pct"] != json.Number("24") || term["battery_pct"] != json.Number("73") ||
		me["companion"].(object)["active_app_state"].(object)["n"] != json.Number("8") || active["state"].(object)["n"] != json.Number("8") ||
		companion["name"] != "Slices" || !strings.Contains(str(me["companion"].(object)["face_svg"]), "data-pose") {
		t.Fatal("missing live terminal, companion or app state", term)
	}
	if s["apps"].(object)["slices"].(object)["code"] != code || len(s["timers"].([]any)) != 1 {
		t.Fatal("missing saved sources or timer")
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
	only := toolTest(t, base, "state", object{"include": []string{"apps"}}, false)
	if len(only) != 2 || only["apps"].(object)["slices"].(object)["code"] != nil {
		t.Fatal("unrequested slice or source leaked", only)
	}
}

func TestMCPSavedAppReadback(t *testing.T) {
	b, base := wsHarness(t, nil)
	const code = "function render(s) {\n  return '<svg><text>'+s.page+'</text></svg>';\n}"
	const svg = `<svg viewBox="0 0 360 360"><text>My story</text></svg>`
	if err := b.saveApp(t.Context(), savedApp{Name: "my-story", Title: "My Story", Kind: "app", Code: code, SVG: svg,
		TickMS: 500, MaxTicks: 20}); err != nil {
		t.Fatal(err)
	}
	snapshot := toolTest(t, base, "state", object{"app_name": "my-story"}, false)
	if empty := snapshot["app"].(object)["state"].(object); len(empty) != 0 {
		t.Fatal(empty)
	}
	progress := object{"page": json.Number("3"), "marks": []any{"foreword", nil}}
	toolTest(t, base, "patch", object{"memory": []object{
		{"scope": "app:my-story", "key": "progress", "value": progress},
		{"scope": "app:my-story", "key": "bookmark", "value": nil},
		{"scope": "game:my-story", "key": "private", "value": true},
	}}, false)
	snapshot = toolTest(t, base, "state", object{"app_name": " MY-STORY ", "include_code": false}, false)
	app := snapshot["app"].(object)
	wantState := object{"progress": progress, "bookmark": nil}
	updatedAt, err := app["updated_at"].(json.Number).Int64()
	if len(snapshot) != 2 || snapshot["now"] == nil || app["name"] != "my-story" ||
		app["title"] != "My Story" || app["kind"] != "app" || app["code"] != code || app["svg"] != svg ||
		app["tick_ms"] != json.Number("500") || app["max_ticks"] != json.Number("20") ||
		!reflect.DeepEqual(app["state"], wantState) || err != nil || updatedAt <= 0 {
		t.Fatal(snapshot)
	}
	if snap := toolTest(t, base, "state", object{"app_name": "my-story", "include": []string{"apps"}}, false); snap["app"] == nil || snap["apps"] == nil {
		t.Fatal(snap)
	}
	for _, tc := range []struct {
		name any
		err  string
	}{
		{"missing", "saved app not found"},
		{"bad/slug", "app_name: invalid saved app slug"},
		{true, "app_name: expected a string"},
	} {
		if got := toolTest(t, base, "state", object{"app_name": tc.name}, true); got["error"] != tc.err {
			t.Fatal(got)
		}
	}
}

func TestMCPSavedAppEdits(t *testing.T) {
	b, base := wsHarness(t, func(b *larkServer, _ string) {
		b.timers = newTimerManager(b)
		t.Cleanup(b.timers.close)
	})
	const id = "cake"
	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, "vocat-book", "Cake")
	watcher := testConnectWS(t, base, "unit", false)
	watcher.sendProto(t, msgHello, encodeTerminalHello(terminalHello{protoVersion: protoVersion1, kind: termKindWeb, displayFormats: dispSVG}))
	watcher.until(t, hasProto(msgPeers, ""))
	savedAppsFrame := func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgEvent, "")(f) && len(p) >= 5 && p[4] == eventSavedApps
	}
	noSavedAppsFrame := func() {
		t.Helper()
		watcher.write(t, maskedFrame(wsPing, true, nil))
		watcher.until(t, func(f wsFrame) bool {
			if savedAppsFrame(f) {
				t.Fatal("unexpected saved_apps notification")
			}
			return f.op == wsPong
		})
	}
	patch := func(args object, failed bool) object {
		t.Helper()
		return toolTest(t, base, "patch", object{"apps": args}, failed)
	}
	const svg = `<svg><text>Book</text></svg>`
	for _, name := range []string{"book", "taken", "discard"} {
		if err := b.saveApp(t.Context(), savedApp{Name: name, Title: "Original", Kind: "drawing", SVG: svg}); err != nil {
			t.Fatal(err)
		}
	}
	timer := patchTimerTest(t, base, object{"companion_id": id, "in_seconds": 600, "app": "book"}, false)
	assertTimerApp := func(name string) {
		t.Helper()
		timers := toolTest(t, base, "state", object{"include": []string{"timers"}}, false)["timers"].([]any)
		if len(timers) != 1 || timers[0].(object)["id"] != timer["id"] || timers[0].(object)["app"] != name {
			t.Fatalf("expected armed timer for %s, got %v", name, timers)
		}
	}
	if _, err := b.db.ExecContext(t.Context(), "UPDATE apps SET updated_at=1"); err != nil {
		t.Fatal(err)
	}
	written := toolTest(t, base, "patch", object{"memory": []object{
		{"scope": "app:book", "key": "page", "value": 2},
		{"scope": "app:book", "key": "page", "value": 3},
		{"scope": "app:book", "key": "mark", "value": true},
		{"scope": "app:discard", "key": "mark", "value": "keep on rollback"},
		{"scope": "app:reserved", "key": "page", "value": 9},
	}}, false)["memory"].(object)
	scopes := []string{"app:book", "app:novel", "app:discard", "app:reserved", "app:taken"}
	stored := func() object {
		t.Helper()
		return toolTest(t, base, "state", object{"include": []string{"memory"}, "memory_scopes": scopes}, false)["memory"].(object)
	}
	before := stored()
	apps := patch(object{"delete": []string{"discard"}, "rename": []object{{"from": "book", "to": "novel"}, {"from": "novel", "to": "taken"}}}, true)
	if apps["error"] != "saved app already exists: taken" || !reflect.DeepEqual(stored(), before) {
		t.Fatal("collision did not roll back app state", apps)
	}
	assertTimerApp("book")
	list := toolTest(t, base, "state", object{"include": []string{"apps"}}, false)["apps"].(object)
	if len(list) != 3 || list["book"] == nil || list["taken"] == nil || list["discard"] == nil || list["novel"] != nil {
		t.Fatal("collision did not roll back saved apps", list)
	}
	for _, tc := range []struct {
		args object
		err  string
	}{
		{object{"delete": []string{"book", "../bad"}}, "apps.delete: invalid saved app slug"},
		{object{"rename": []object{{"from": "book"}}}, "apps.rename: expected to or title"},
		{object{"rename": []object{{"title": "Missing source"}}}, "apps.rename.from: invalid saved app slug"},
		{object{"rename": []object{{"from": "book", "title": " "}}}, "apps.rename.title: expected a non-empty title"},
		{object{"rename": []object{{"from": "missing", "title": "No source"}}}, "saved app not found: missing"},
		{object{"rename": []object{{"from": "book", "to": "reserved"}}}, "saved app state already exists: reserved"},
	} {
		if got := patch(tc.args, true); got["error"] != tc.err {
			t.Fatal(got)
		}
	}
	noSavedAppsFrame()

	got := patch(object{"rename": []object{{"from": " Book ", "to": "NOVEL", "title": "First Edition"}}}, false)["apps"].(object)
	renamed := []any{object{"from": "book", "to": "novel", "title": "First Edition"}}
	if !reflect.DeepEqual(got["renamed"], renamed) || len(got["deleted"].([]any)) != 0 {
		t.Fatal(got)
	}
	watcher.until(t, savedAppsFrame)
	toolTest(t, base, "state", object{"app_name": "book"}, true)
	app := toolTest(t, base, "state", object{"app_name": "novel"}, false)["app"].(object)
	if app["name"] != "novel" || app["title"] != "First Edition" || app["kind"] != "drawing" ||
		app["svg"] != svg || app["updated_at"] == json.Number("1") ||
		app["tick_ms"] != json.Number("0") || app["max_ticks"] != json.Number("0") {
		t.Fatal(app)
	}
	assertTimerApp("novel")
	if mem := stored(); len(mem["app:book"].(object)) != 0 || !reflect.DeepEqual(mem["app:novel"], written["app:book"]) {
		t.Fatal("state or metadata did not move", mem)
	}

	got = patch(object{"rename": []object{{"from": "novel", "title": "Second Edition"}}}, false)["apps"].(object)
	renamed = []any{object{"from": "novel", "to": "novel", "title": "Second Edition"}}
	if !reflect.DeepEqual(got["renamed"], renamed) {
		t.Fatal(got)
	}
	watcher.until(t, savedAppsFrame)
	app = toolTest(t, base, "state", object{"app_name": "novel"}, false)["app"].(object)
	if app["title"] != "Second Edition" || !reflect.DeepEqual(stored()["app:novel"], written["app:book"]) {
		t.Fatal(app)
	}

	blocked := patch(object{"delete": []string{"discard", "novel"}}, true)
	if blocked["error"] != "saved app is used by an armed timer: novel" {
		t.Fatal(blocked)
	}
	assertTimerApp("novel")
	toolTest(t, base, "state", object{"app_name": "discard"}, false)
	noSavedAppsFrame()
	patchTimerTest(t, base, object{"op": "cancel", "device_id": id, "id": timer["id"]}, false)

	got = patch(object{"delete": []string{"novel", "missing", "discard"}}, false)["apps"].(object)
	if !slices.Equal(got["deleted"].([]any), []any{"novel", "discard"}) || len(got["renamed"].([]any)) != 0 {
		t.Fatal(got)
	}
	watcher.until(t, savedAppsFrame)
	toolTest(t, base, "state", object{"app_name": "novel"}, true)
	toolTest(t, base, "state", object{"app_name": "discard"}, true)
	if mem := stored(); len(mem["app:novel"].(object)) != 0 || len(mem["app:discard"].(object)) != 0 || !reflect.DeepEqual(mem["app:reserved"], written["app:reserved"]) {
		t.Fatal("delete did not remove only its own state", mem)
	}
	got = patch(object{"delete": []string{"novel"}}, false)["apps"].(object)
	if len(got["deleted"].([]any)) != 0 {
		t.Fatal(got)
	}
	noSavedAppsFrame()
}

func TestEmitIntercom(t *testing.T) {
	b, base := wsHarness(t, func(b *larkServer, _ string) {
		b.worker = fakeTTS(b.root, nil)
	})
	const from, to, text = "cake", "mint", "Dinner is ready."
	if _, err := b.createCompanion(t.Context(), object{"id": to, "name": "Mint"}); err != nil {
		t.Fatal(err)
	}
	sender, recipient := testConnectWS(t, base, "unit", false), testConnectWS(t, base, "unit", true)
	sender.hello(t, "vocat-0001", "Cake")
	recipient.helloWeb(t, "web-bee10000", to, capDrag, false)
	out := toolTest(t, base, "emit", object{"to": "all", "intercom": object{"from": from, "text": text}}, false)
	if !slices.Equal(out["intercom"].(object)["sent_to"].([]any), []any{to}) {
		t.Fatal(out)
	}
	var card, audio bool
	recipient.until(t, func(f wsFrame) bool {
		card, audio = card || hasSVG(text)(f), audio || isPlayback(f)
		return card && audio
	})
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
	watcher := testConnectWS(t, base, "unit", false)
	watcher.watch(t, id)
	watcher.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgPower, "")(f) && len(p) >= 1 && p[0] == 85
	})

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
	for _, client := range []*testWS{c, watcher} {
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

func chatFrame(text string) func(wsFrame) bool {
	return func(f wsFrame) bool {
		return f.op == wsText && bytes.Contains(f.data, []byte(`"type":"chat"`)) && bytes.Contains(f.data, []byte(text))
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
