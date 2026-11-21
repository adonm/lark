package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type mockTunnel struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *mockTunnel) run(_ context.Context, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, slices.Clone(args))
	if slices.Equal(args, []string{"status"}) {
		return []byte("Hostname: home.opentunnel.example\nunrelated.opentunnel.example → 127.0.0.1:1234\nService: running\n"), nil
	}
	if len(args) == 5 && args[0] == "route" && args[3] == "--profile" && profileKey.MatchString(args[4]) {
		switch args[1] {
		case "add":
			if strings.HasPrefix(args[2], "127.0.0.1:") {
				return []byte("Route added: https://scoped.home.opentunnel.example → " + args[2]), nil
			}
		case "remove":
			if args[2] == "scoped" {
				return nil, nil
			}
		}
	}
	return nil, fmt.Errorf("unexpected mock CLI arguments")
}

func apiObjects(t *testing.T, url, key string) []any {
	t.Helper()
	return testObject(t, testRequest(t, "GET", url, "unit", nil, 200))[key].([]any)
}

func TestVirtualClientsPersistenceAndDeviceMerge(t *testing.T) {
	b, base := wsHarness(t, func(b *bridge, dir string) {
		b.virtual = virtualStore(b.db)
	})
	if got := apiObjects(t, base+"/api/virtual-clients", "virtual_clients"); len(got) != 0 {
		t.Fatal(got)
	}
	for _, input := range []object{
		{"character": "##"},
		{"character": "Plum", "voice": "unknown"},
	} {
		testRequest(t, "POST", base+"/api/virtual-clients", "unit", input, 400)
	}
	v := testObject(t, testRequest(t, "POST", base+"/api/virtual-clients", "unit", object{"character": "  Violet  "}, 201))
	id := str(v["id"])
	if v["character"] != "Violet" || v["online"] != false || !virtualID.MatchString(id) || v["created_at"] == "" {
		t.Fatal(v)
	}
	if v, _, err := virtualStore(b.db).get(id); err != nil || v.Character != "Violet" {
		t.Fatal("virtual client not persisted")
	}
	physical := testConnectWS(t, base, "unit", false)
	physical.hello(t, "lark-1234", "Cake")
	devices := apiObjects(t, base+"/api/devices", "devices")
	if len(devices) != 2 || devices[0].(object)["kind"] != "physical" || devices[0].(object)["ws_connected"] != true ||
		devices[1].(object)["kind"] != "virtual" || devices[1].(object)["online"] != false || devices[1].(object)["device_id"] != id {
		t.Fatal(devices)
	}
	virtual := testConnectWS(t, base, "unit", false)
	scene := virtual.hello(t, id, "Ignored")
	if !bytes.Contains(scene.data, []byte(storybookPalettes["peach"].bg)) {
		t.Fatal("configured cat palette ignored")
	}
	if v := apiObjects(t, base+"/api/virtual-clients", "virtual_clients")[0].(object); v["ws_connected"] != true {
		t.Fatal(v)
	}
	testRequest(t, "DELETE", base+"/api/virtual-clients/"+id, "unit", nil, 204)
	virtual.until(t, func(f wsFrame) bool { return f.op == wsClose })
	testRequest(t, "DELETE", base+"/api/virtual-clients/"+id, "unit", nil, 404)
	if clients, err := virtualStore(b.db).all(); err != nil || len(clients) != 0 || len(apiObjects(t, base+"/api/devices", "devices")) != 1 {
		t.Fatal("deleted virtual client still visible")
	}
}

func TestScopedTunnelIsolationAndOwnedLifecycle(t *testing.T) {
	r := &mockTunnel{}
	settings := openCodeSettings("lark")
	settings.Model = ocModel{"unit", "test", ""}
	var creates atomic.Int32
	prompts := make(chan string, 3)
	oc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session":
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, `{"data":[]}`)
				return
			}
			creates.Add(1)
			var input ocSettings
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || !slices.Equal(input.Permissions, settings.Permissions) {
				t.Error("virtual cat did not get standard tool permissions")
			}
			_, _ = io.WriteString(w, `{"data":{"id":"ses_shared"}}`)
		case "/api/session/ses_shared":
			_ = json.NewEncoder(w).Encode(object{"data": struct {
				ID string `json:"id"`
				ocSettings
			}{"ses_shared", settings}})
		case "/api/session/ses_shared/prompt":
			var input struct{ Text string }
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			select {
			case prompts <- input.Text:
			default:
				t.Error("unexpected OpenCode turn")
			}
			_, _ = io.WriteString(w, `{"data":{"id":"msg-user"}}`)
		case "/api/experimental/session/ses_shared/wait":
			w.WriteHeader(204)
		case "/api/session/ses_shared/message":
			_, _ = io.WriteString(w, `{"data":[{"id":"msg-answer","type":"assistant","finish":"stop","content":[{"type":"text","text":"Hello!"}]},{"id":"msg-user","type":"user"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer oc.Close()
	b, base := wsHarness(t, func(b *bridge, dir string) {
		b.virtual = virtualStore(b.db)
		b.tunnels = newTunnelManager(b, "mock-opentunnel", r.run)
		b.history = &chatHistory{b.db}
		b.stt = func(context.Context, []byte) (string, error) { return "Voice to Cake.", nil }
		path := filepath.Join(dir, "service.json")
		if err := writeMap(path, object{"url": oc.URL, "password": "unit"}); err != nil {
			t.Fatal(err)
		}
		b.oc = newOpenCode(b.root, upstream{"OpenCode", pooledClient(), &fileCache[endpoint]{path: path, read: openCodeEndpoint}},
			b.db, dir, settings)
	})
	defer b.tunnels.shutdown()
	id := str(testObject(t, testRequest(t, "POST", base+"/api/virtual-clients", "unit", object{"character": "Plum"}, 201))["id"])
	status := testObject(t, testRequest(t, "GET", base+"/api/tunnel/status", "unit", nil, 200))
	if len(status) != 4 || status["installed"] != true || status["service_running"] != true || status["hostname"] != "home.opentunnel.example" {
		t.Fatal(status)
	}
	testRequest(t, "POST", base+"/api/tunnels", "unit", object{"device_id": "admin"}, 404)
	testRequest(t, "DELETE", base+"/api/tunnels/virt-00000000", "unit", nil, 404)
	result := testObject(t, testRequest(t, "POST", base+"/api/tunnels", "unit", object{"device_id": id}, 201))
	if result["active"] != true || result["route_name"] != "scoped" || result["device_id"] != id {
		t.Fatal("bad tunnel response")
	}
	again := testObject(t, testRequest(t, "POST", base+"/api/tunnels", "unit", object{"device_id": id}, 201))
	if again["share_url"] != result["share_url"] {
		t.Fatal("tunnel add not idempotent")
	}
	if details := apiObjects(t, base+"/api/devices", "devices")[0].(object)["tunnel"].(object); details["active"] != true {
		t.Fatal("unified list missing tunnel")
	}
	restored := newTunnelManager(b, "mock-opentunnel", r.run)
	defer restored.shutdown()
	b.tunnels.mu.Lock()
	before, _, err := b.tunnels.store.get(id)
	if err != nil {
		t.Fatal(err)
	}
	live := b.tunnels.live[id]
	delete(b.tunnels.live, id)
	b.tunnels.mu.Unlock()
	live.close()
	restored.restore()
	if info := restored.info(id); info == nil || info["active"] != true || info["share_url"] != result["share_url"] {
		t.Fatal("scoped listener did not restore without CLI")
	}
	port := strconv.Itoa(before.LocalPort)
	scoped := "http://" + net.JoinHostPort("127.0.0.1", port)
	token := before.Token
	if !scopeKey.MatchString(token) || !strings.HasSuffix(str(result["share_url"]), "/app?token="+token) {
		t.Fatal("bad scoped share URL")
	}
	physical := testConnectWS(t, base, "unit", false)
	physical.hello(t, "lark-private", "Cake")
	mirror := testConnectWS(t, base, "unit", true)
	mirror.watch(t, "lark-private")
	testRequest(t, "GET", scoped+"/app", "", nil, 200)
	testRequest(t, "GET", scoped+"/app/client.js", "", nil, 200)
	testRequest(t, "GET", scoped+"/api/scope", "unit", nil, 401)
	scope := testObject(t, testRequest(t, "GET", scoped+"/api/scope?token="+token, "", nil, 200))
	if scope["scoped"] != true || scope["device_id"] != id || scope["character"] != "Plum" ||
		scope["companion"].(object)["face_bg"] != storybookPalettes["peach"].bg {
		t.Fatal(scope)
	}
	for _, path := range []string{"/api/devices", "/api/tunnels", "/api/virtual-clients", "/api/tunnel/status", "/ota/version.json", "/mcp", "/health", "/api/audio/abcd", "/api/companions"} {
		testRequest(t, "GET", scoped+path, token, nil, 404)
	}
	testRequest(t, "POST", scoped+"/mcp", token, object{}, 404)
	testRequest(t, "POST", scoped+"/api/devices/"+id+"/companion", token, object{"name": "No"}, 404)
	testRequest(t, "DELETE", scoped+"/api/companions/plum", token, nil, 404)
	testRequest(t, "GET", scoped+"/api/devices/"+id+"/messages", token, nil, 200)
	testRequest(t, "GET", scoped+"/api/devices/lark-private/messages", token, nil, 403)
	testRequest(t, "POST", scoped+"/api/chat", token, object{"device_id": "lark-private", "text": "Question?"}, 403)
	testRequest(t, "POST", scoped+"/api/chat", token, object{"device_id": id, "text": ""}, 400)
	chat := testObject(t, testRequest(t, "POST", scoped+"/api/chat", token, object{"device_id": id, "text": "Question?"}, 200))
	if chat["reply"] != "Hello!" || chat["device_id"] != id || chat["session_id"] != "ses_shared" || chat["audio_url"] != nil {
		t.Fatal("tunneled chat did not return a scoped reply")
	}
	header := "[Plum (" + id + ") · web · typed]"
	assertPrompt := func(text string) {
		t.Helper()
		select {
		case got := <-prompts:
			if got != header+"\n"+text || stripLarkHeader(got) != text {
				t.Fatal("virtual cat prompt identity or provenance is incorrect")
			}
		default:
			t.Fatal("did not receive an OpenCode prompt")
		}
	}
	assertPrompt("Question?")
	lan := testObject(t, testRequest(t, "POST", base+"/api/chat", "unit", object{"device_id": id, "text": "Another question?"}, 200))
	assertPrompt("Another question?")
	if lan["session_id"] != chat["session_id"] || creates.Load() != 1 {
		t.Fatal("LAN and tunnel did not reuse one OpenCode session")
	}
	history := "/api/devices/" + id + "/messages"
	tunneled := testRequest(t, "GET", scoped+history, token, nil, 200)
	if !bytes.Contains(tunneled, []byte("Hello!")) || !bytes.Equal(tunneled, testRequest(t, "GET", base+history, "unit", nil, 200)) {
		t.Fatal("LAN and tunnel histories differ")
	}
	testRequest(t, "POST", scoped+"/api/reset", token, object{"device_id": "lark-private"}, 404)
	testRequest(t, "POST", scoped+"/api/reset", token, object{"device_id": id}, 404)
	testRequest(t, "POST", scoped+"/api/companions/reassign", token, object{"from_device_id": id, "to_device_id": "lark-private"}, 404)
	testRequest(t, "GET", scoped+"/api/diagnostics", token, nil, 404)
	testRequest(t, "GET", scoped+"/api/diagnostics", "unit", nil, 404)
	testRequest(t, "POST", scoped+"/api/chat", token, object{"device_id": id, "text": "Tell Cake Hi from HTTP."}, 200)
	mirror.until(t, hasWS("svg", "Hi from HTTP."))
	_, rejected := dialTestWS(t, scoped, "unit", false, nil)
	if rejected.StatusCode != 401 {
		t.Fatal(rejected.StatusCode)
	}
	rejected.Body.Close()
	_, rejected = dialTestWS(t, scoped, token, false, object{"Origin": "https://other.example"})
	if rejected.StatusCode != 403 {
		t.Fatal(rejected.StatusCode)
	}
	rejected.Body.Close()
	conn, upgrade := dialTestWS(t, scoped, token, true, object{"Origin": "https://127.0.0.1:" + port})
	if upgrade.StatusCode != 101 {
		t.Fatal(upgrade.StatusCode)
	}
	conn.json(t, object{"ev": "hello", "role": "device", "device_id": "lark-private", "character": "Not allowed"})
	conn.until(t, hasWS("error", `"status":403`))
	conn.hello(t, id, "Ignored")
	peers := conn.until(t, hasWS("peers", id))
	if !bytes.Contains(peers.data, []byte(`"character":"Cake","device_id":"lark-private","online":true`)) ||
		bytes.Contains(peers.data, []byte(`"ip"`)) {
		t.Fatal("scoped WebSocket did not expose the minimal online peer list")
	}
	conn.json(t, object{"ev": "watch", "device_id": "lark-private"})
	conn.until(t, hasWS("error", `"status":403`))
	conn.json(t, object{"ev": "watch", "device_id": id})
	conn.until(t, hasWS("svg", ""))
	conn.json(t, object{"ev": "text", "text": "WebSocket question?"})
	conn.until(t, chatFrame("Hello!"))
	assertPrompt("WebSocket question?")
	conn.until(t, hasWS("mode", `"mode":"idle"`))
	conn.json(t, object{"ev": "text", "text": "Tell Cake Hi from text."})
	mirror.until(t, hasWS("svg", "Hi from text."))
	conn.until(t, hasWS("mode", `"mode":"idle"`))
	conn.json(t, object{"ev": "intercom", "target": "lark-private", "text": "Hi from WS intercom."})
	mirror.until(t, hasWS("svg", "Hi from WS intercom."))
	conn.until(t, hasWS("mode", `"mode":"idle"`))
	conn.json(t, object{"ev": "tap", "id": "companion", "x": 180, "y": 316})
	conn.until(t, hasWS("svg", "companion-ring"))
	conn.json(t, object{"ev": "tap", "x": 140, "y": 313})
	menu := conn.until(t, hasWS("svg", "menu-intercom"))
	for _, id := range []string{"menu-stats", "menu-games", "menu-memories", "menu-next"} {
		if bytes.Contains(menu.data, []byte(id)) {
			t.Fatalf("scoped SVG offers unexpected %s", id)
		}
	}
	conn.json(t, object{"ev": "tap", "x": 115, "y": 130})
	conn.until(t, hasWS("svg", "intercom-lark-private"))
	conn.json(t, object{"ev": "tap", "x": 245, "y": 116})
	conn.until(t, hasWS("rec", `"target":"lark-private"`))
	conn.json(t, object{"ev": "rec_start", "target": "lark-private"})
	pcm := bytes.Repeat([]byte{12, 0}, 120)
	conn.write(t, maskedFrame(wsBinary, true, append([]byte{micPrefix}, pcm...)))
	conn.json(t, object{"ev": "rec_end", "target": "lark-private"})
	got := physical.until(t, func(f wsFrame) bool { return f.op == wsBinary && f.data[0] == pcmPrefix })
	if !bytes.Equal(got.data[1:], pcm) {
		t.Fatal("tunneled menu did not deliver recorded PCM")
	}
	mirror.until(t, hasWS("svg", "Voice to Cake."))
	conn.until(t, hasWS("mode", `"mode":"idle"`))
	conn.json(t, object{"ev": "tap", "id": "companion", "x": 180, "y": 316})
	conn.until(t, hasWS("svg", "companion-ring"))
	conn.json(t, object{"ev": "reset", "device_id": id})
	conn.json(t, object{"ev": "tap", "id": "new", "x": 230, "y": 104})
	conn.json(t, object{"ev": "watch", "device_id": id})
	conn.until(t, hasWS("svg", storybookPalettes["peach"].bg))
	testRequest(t, "POST", base+"/api/devices/"+id+"/reset", "unit", object{}, 200)
	faceSeen, companionSeen := false, false
	conn.until(t, func(f wsFrame) bool {
		faceSeen = faceSeen || hasWS("svg", "#FFF2E7")(f)
		companionSeen = companionSeen || hasWS("companion", `"configured":false`)(f)
		return faceSeen && companionSeen
	})
	scope = testObject(t, testRequest(t, "GET", scoped+"/api/scope", token, nil, 200))
	if scope["character"] != "New Cat" || scope["companion"].(object)["configured"] != false {
		t.Fatal("scope retained the old companion after reset", scope)
	}
	if !bytes.Contains(testRequest(t, "GET", base+history, "unit", nil, 200), []byte("Hello!")) {
		t.Fatal("LAN reset discarded canonical conversation history")
	}
	restored.shutdown()
	conn.until(t, func(f wsFrame) bool { return f.op == wsClose })
	testRequest(t, "DELETE", base+"/api/virtual-clients/"+id, "unit", nil, 204)
	if len(apiObjects(t, base+"/api/tunnels", "tunnels")) != 0 {
		t.Fatal("owned route not removed")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) != 3 || !slices.Equal(r.calls[0], []string{"status"}) ||
		!slices.Equal(r.calls[1], []string{"route", "add", "127.0.0.1:" + port, "--profile", before.Profile}) ||
		!slices.Equal(r.calls[2], []string{"route", "remove", "scoped", "--profile", before.Profile}) {
		t.Fatal("unexpected OpenTunnel CLI calls")
	}
}

func TestNoSpeakToolAndFinalVoiceIsPersisted(t *testing.T) {
	var synthesis atomic.Int32
	var b *bridge
	var base string
	const id = "lark-spoken"
	oc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session":
			_, _ = io.WriteString(w, `{"data":{"id":"ses_speech"}}`)
		case "/api/session/ses_speech/prompt":
			_, err := b.callMCP(r.Context(), "speak", object{"device_id": id, "text": "Already spoken."})
			if err == nil {
				t.Error("obsolete speak tool remains callable")
			}
			_, _ = io.WriteString(w, `{"data":{"id":"msg-user"}}`)
		case "/api/experimental/session/ses_speech/wait":
			w.WriteHeader(204)
		case "/api/session/ses_speech/message":
			_, _ = io.WriteString(w, `{"data":[{"id":"msg-answer","type":"assistant","finish":"stop","content":[{"type":"text","text":"Done."}]},{"id":"msg-user","type":"user"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer oc.Close()
	b, base = wsHarness(t, func(b *bridge, dir string) {
		b.worker = fakeTTS(b.root, &synthesis)
		path := filepath.Join(dir, "service.json")
		if err := writeMap(path, object{"url": oc.URL, "password": "unit"}); err != nil {
			t.Fatal(err)
		}
		b.oc = newOpenCode(b.root, upstream{"OpenCode", pooledClient(), &fileCache[endpoint]{path: path, read: openCodeEndpoint}},
			b.db, dir, ocSettings{})
	})
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, id, "Cake")
	w := testConnectWS(t, base, "unit", false)
	w.json(t, object{"ev": "hello", "role": "web"})
	w.until(t, hasWS("peers", id))
	w.json(t, object{"ev": "watch", "device_id": id})
	w.until(t, hasWS("svg", ""))
	c.json(t, object{"ev": "text", "text": "Say something."})
	w.until(t, chatFrame("Say something."))
	w.until(t, chatFrame("Done."))
	c.until(t, hasWS("tts", `"state":"end"`))
	if synthesis.Load() != 1 {
		t.Fatal("the final prose was not synthesized exactly once")
	}
}
