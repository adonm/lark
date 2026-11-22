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

func TestCompanionsAndTerminalsPersistence(t *testing.T) {
	b, base := wsHarness(t, nil)
	if got := apiObjects(t, base+"/api/companions", "companions"); len(got) != 0 {
		t.Fatal(got)
	}
	for _, input := range []object{
		{"name": "##"},
		{"name": "Plum", "voice": "unknown"},
	} {
		testRequest(t, "POST", base+"/api/companions", "unit", input, 400)
	}
	c := testObject(t, testRequest(t, "POST", base+"/api/companions", "unit", object{"name": "  Violet  "}, 201))
	id := str(c["id"])
	if id != "violet" || c["name"] != "Violet" || c["online"] != false || c["dormant"] != true {
		t.Fatal(c)
	}
	term := testObject(t, testRequest(t, "POST", base+"/api/terminals", "unit", object{"kind": "web", "companion_id": id}, 201))
	termID := str(term["id"])
	if !webTerminalID.MatchString(termID) || term["companion_id"] != id {
		t.Fatal(term)
	}
	physical := testConnectWS(t, base, "unit", false)
	physical.hello(t, "vocat-1234", "Cake")
	terminals := apiObjects(t, base+"/api/terminals", "terminals")
	if len(terminals) != 2 {
		t.Fatal(terminals)
	}
	web := testConnectWS(t, base, "unit", false)
	scene := web.hello(t, termID, "")
	if !bytes.Contains(scene.data, []byte(storybookPalettes["peach"].bg)) {
		t.Fatal("configured companion palette ignored")
	}
	testRequest(t, "DELETE", base+"/api/terminals/"+termID, "unit", nil, 200+4)
	web.until(t, func(f wsFrame) bool { return f.op == wsClose })
	testRequest(t, "DELETE", base+"/api/terminals/"+termID, "unit", nil, 404)
	if terms, err := b.terminals.all(); err != nil || len(terms) != 1 {
		t.Fatal("deleted web terminal still visible", terms, err)
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
	b, base := wsHarness(t, func(b *larkServer, dir string) {
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
	id := str(testObject(t, testRequest(t, "POST", base+"/api/companions", "unit", object{"name": "Plum"}, 201))["id"])
	status := testObject(t, testRequest(t, "GET", base+"/api/tunnel/status", "unit", nil, 200))
	if len(status) != 4 || status["installed"] != true || status["service_running"] != true || status["hostname"] != "home.opentunnel.example" {
		t.Fatal(status)
	}
	testRequest(t, "POST", base+"/api/tunnels", "unit", object{"companion_id": "admin"}, 404)
	testRequest(t, "DELETE", base+"/api/tunnels/missing", "unit", nil, 404)
	result := testObject(t, testRequest(t, "POST", base+"/api/tunnels", "unit", object{"companion_id": id}, 201))
	if result["active"] != true || result["route_name"] != "scoped" || result["companion_id"] != id {
		t.Fatal("bad tunnel response")
	}
	again := testObject(t, testRequest(t, "POST", base+"/api/tunnels", "unit", object{"companion_id": id}, 201))
	if again["share_url"] != result["share_url"] {
		t.Fatal("tunnel add not idempotent")
	}
	if details := apiObjects(t, base+"/api/companions", "companions")[0].(object)["tunnel"].(object); details["active"] != true {
		t.Fatal("companions list missing tunnel")
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
	physical.hello(t, "vocat-9999", "Cake")
	mirror := testConnectWS(t, base, "unit", true)
	mirror.watch(t, "cake")
	testRequest(t, "GET", scoped+"/app", "", nil, 200)
	testRequest(t, "GET", scoped+"/app/client.js", "", nil, 200)
	testRequest(t, "GET", scoped+"/api/scope", "unit", nil, 401)
	scope := testObject(t, testRequest(t, "GET", scoped+"/api/scope?token="+token, "", nil, 200))
	if scope["scoped"] != true || scope["companion_id"] != id || scope["name"] != "Plum" ||
		scope["companion"].(object)["face_bg"] != storybookPalettes["peach"].bg {
		t.Fatal(scope)
	}
	for _, path := range []string{"/api/terminals", "/api/tunnels", "/api/tunnel/status", "/ota/version.json", "/mcp", "/health", "/api/audio/abcd", "/api/companions"} {
		testRequest(t, "GET", scoped+path, token, nil, 404)
	}
	testRequest(t, "POST", scoped+"/mcp", token, object{}, 404)
	testRequest(t, "PATCH", scoped+"/api/companions/"+id, token, object{"name": "No"}, 404)
	testRequest(t, "DELETE", scoped+"/api/companions/"+id, token, nil, 404)
	testRequest(t, "GET", scoped+"/api/companions/"+id+"/messages", token, nil, 200)
	testRequest(t, "GET", scoped+"/api/companions/cake/messages", token, nil, 403)
	testRequest(t, "POST", scoped+"/api/chat", token, object{"companion_id": "cake", "text": "Question?"}, 403)
	testRequest(t, "POST", scoped+"/api/chat", token, object{"companion_id": id, "text": ""}, 400)
	chat := testObject(t, testRequest(t, "POST", scoped+"/api/chat", token, object{"companion_id": id, "text": "Question?"}, 200))
	if chat["reply"] != "Hello!" || chat["companion_id"] != id || chat["session_id"] != "ses_shared" || chat["audio_url"] != nil {
		t.Fatal("tunneled chat did not return a scoped reply")
	}
	assertPrompt := func(kind, text string) {
		t.Helper()
		wantHeader := "[Plum (" + id + ") · " + kind + " · typed]"
		select {
		case got := <-prompts:
			if got != wantHeader+"\n"+text || stripLarkHeader(got) != text {
				t.Fatal("companion prompt identity or provenance is incorrect", got)
			}
		default:
			t.Fatal("did not receive an OpenCode prompt")
		}
	}
	assertPrompt("hardware", "Question?")
	lan := testObject(t, testRequest(t, "POST", base+"/api/chat", "unit", object{"companion_id": id, "text": "Another question?"}, 200))
	assertPrompt("hardware", "Another question?")
	if lan["session_id"] != chat["session_id"] || creates.Load() != 1 {
		t.Fatal("LAN and tunnel did not reuse one OpenCode session")
	}
	history := "/api/companions/" + id + "/messages"
	tunneled := testRequest(t, "GET", scoped+history, token, nil, 200)
	if !bytes.Contains(tunneled, []byte("Hello!")) || !bytes.Equal(tunneled, testRequest(t, "GET", base+history, "unit", nil, 200)) {
		t.Fatal("LAN and tunnel histories differ")
	}
	testRequest(t, "POST", scoped+"/api/companions/cake/reset", token, object{}, 404)
	testRequest(t, "POST", scoped+"/api/companions/"+id+"/reset", token, object{}, 404)
	testRequest(t, "GET", scoped+"/api/diagnostics", token, nil, 404)
	testRequest(t, "GET", scoped+"/api/diagnostics", "unit", nil, 404)
	testRequest(t, "POST", scoped+"/api/chat", token, object{"companion_id": id, "text": "Tell Cake Hi from HTTP."}, 200)
	mirror.until(t, hasSVG("Hi from HTTP."))
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
	conn.sendProto(t, msgHello, encodeTerminalHello(terminalHello{
		protoVersion: protoVersion1, kind: termKindHardware, displayFormats: dispRGB565,
		terminalID: "vocat-9999", companionID: "cake",
	}))
	conn.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgReject, "")(f) && len(p) >= 1 && p[0] == rejectBadToken
	})
	conn.hello(t, "web-00001111", "")
	peers := conn.until(t, hasProto(msgPeers, id))
	if !bytes.Contains(framePayload(peers), []byte("Cake")) ||
		bytes.Contains(framePayload(peers), []byte("127.0.0.1")) {
		t.Fatal("scoped WebSocket did not expose the minimal online companion list")
	}
	conn.sendProto(t, msgWatch, appendStr8(nil, id))
	conn.until(t, hasSVG(""))
	conn.text(t, "WebSocket question?")
	conn.until(t, hasProto(msgEvent, "Hello!"))
	assertPrompt("web", "WebSocket question?")
	doneOrIdle := func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgStatus, "")(f) && len(p) >= 5 && (p[4] == statusDone || p[4] == statusIdle)
	}
	conn.until(t, doneOrIdle)
	conn.text(t, "Tell Cake Hi from text.")
	mirror.until(t, hasSVG("Hi from text."))
	conn.until(t, doneOrIdle)
	conn.tap(t, 180, 316, 0)
	conn.until(t, hasSVG("companion-ring"))
	conn.tap(t, 140, 313, 0)
	menu := conn.until(t, hasSVG("menu-intercom"))
	for _, id := range []string{"menu-stats", "menu-games", "menu-memories", "menu-next"} {
		if bytes.Contains(menu.data, []byte(id)) {
			t.Fatalf("scoped SVG offers unexpected %s", id)
		}
	}
	conn.tap(t, 115, 130, 0)
	conn.until(t, hasSVG("intercom-cake"))
	conn.tap(t, 245, 116, 0)
	conn.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "cake")(f) && len(p) >= 1 && p[0] == recOpStart
	})
	conn.recStart(t, "cake")
	pcm := bytes.Repeat([]byte{12, 0}, 120)
	conn.sendRTP(t, rtpPTRecPCM, 1, 320, 1, pcm)
	conn.recEnd(t, "cake", false)
	got := physical.until(t, isPlayback)
	if !bytes.Equal(framePayload(got), pcm) {
		t.Fatal("tunneled menu did not deliver recorded PCM")
	}
	mirror.until(t, hasSVG("Voice to Cake."))
	conn.until(t, doneOrIdle)
	conn.tap(t, 180, 316, 0)
	conn.until(t, hasSVG("companion-ring"))
	conn.sendProto(t, msgWatch, appendStr8(nil, id))
	conn.until(t, hasSVG(storybookPalettes["peach"].bg))
	testRequest(t, "POST", base+"/api/companions/"+id+"/reset", "unit", object{}, 200)
	faceSeen, companionSeen := false, false
	conn.until(t, func(f wsFrame) bool {
		faceSeen = faceSeen || hasSVG("#FFF2E7")(f)
		companionSeen = companionSeen || hasProto(msgCompanion, "New Companion")(f)
		return faceSeen && companionSeen
	})
	scope = testObject(t, testRequest(t, "GET", scoped+"/api/scope", token, nil, 200))
	if scope["name"] != "New Companion" || scope["companion"].(object)["configured"] != false {
		t.Fatal("scope retained the old companion after reset", scope)
	}
	if !bytes.Contains(testRequest(t, "GET", base+history, "unit", nil, 200), []byte("Hello!")) {
		t.Fatal("LAN reset discarded canonical conversation history")
	}
	restored.shutdown()
	conn.until(t, func(f wsFrame) bool { return f.op == wsClose })
	testRequest(t, "DELETE", base+"/api/terminals/web-00001111", "unit", nil, 204)
	testRequest(t, "DELETE", base+"/api/companions/"+id, "unit", nil, 204)
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
	var b *larkServer
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
	b, base = wsHarness(t, func(b *larkServer, dir string) {
		b.worker = fakeTTS(b.root, &synthesis)
		path := filepath.Join(dir, "service.json")
		if err := writeMap(path, object{"url": oc.URL, "password": "unit"}); err != nil {
			t.Fatal(err)
		}
		b.oc = newOpenCode(b.root, upstream{"OpenCode", pooledClient(), &fileCache[endpoint]{path: path, read: openCodeEndpoint}},
			b.db, dir, ocSettings{})
	})
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, "vocat-0001", "Cake")
	w := testConnectWS(t, base, "unit", false)
	w.watch(t, "cake")
	c.text(t, "Say something.")
	w.until(t, hasProto(msgEvent, "Say something."))
	w.until(t, hasProto(msgEvent, "Done."))
	c.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgTTSCtrl, "")(f) && len(p) >= 1 && p[0] == ttsOpEnd
	})
	if synthesis.Load() != 1 {
		t.Fatal("the final prose was not synthesized exactly once")
	}
}
