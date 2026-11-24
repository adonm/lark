package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chatmail/rpc-client-go/v2/deltachat"
)

func fakeTunnelRunner(calls *[]string) tunnelRunner {
	return func(_ context.Context, args ...string) ([]byte, error) {
		*calls = append(*calls, strings.Join(args, " "))
		if len(args) >= 3 && args[0] == "route" && args[1] == "add" {
			return []byte("Route added: https://scoped.test.opentunnel.example → " + args[2]), nil
		}
		return nil, nil
	}
}

type remoteHarness struct {
	b     *larkServer
	base  string
	calls []string
	comp  companionState
	token string
}

func newRemoteHarness(t *testing.T) *remoteHarness {
	t.Helper()
	b, base := wsHarness(t, nil)
	b.voices = companionVoiceStore(b.db, heartVoice)
	b.remote = newRemoteManager(b, "fake-opentunnel")
	b.remote.runner = fakeTunnelRunner(&[]string{})
	c, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "an old chameleon"})
	if err != nil {
		t.Fatal(err)
	}
	h := &remoteHarness{b: b, base: base, comp: c}
	link, err := b.remote.issue(t.Context(), c.ID, "mum@example.org", "Mum")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	h.token = parsed.Query().Get("token")
	if h.token == "" {
		t.Fatalf("link has no token: %q", link)
	}
	return h
}

func (h *remoteHarness) scopedBase() string {
	h.b.remote.mu.Lock()
	defer h.b.remote.mu.Unlock()
	route := h.b.remote.routes[h.comp.ID]
	return "http://127.0.0.1:" + strconv.Itoa(route.LocalPort)
}

func remoteHTTP(t *testing.T, method, base, token, path string, body any, want int) []byte {
	t.Helper()
	var r *bytes.Reader
	if body == nil {
		r = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, base+path, r)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", jsonType)
	}
	if token != "" {
		req.Header.Set("X-Lark-Remote-Token", token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(res.Body)
	if res.StatusCode != want {
		t.Fatalf("%s %s status = %d, want %d (%s)", method, path, res.StatusCode, want, buf.String())
	}
	return buf.Bytes()
}

func remoteJSON(t *testing.T, method, base, token, path string, body any, want int) object {
	t.Helper()
	raw := remoteHTTP(t, method, base, token, path, body, want)
	var out object
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s %s: invalid JSON: %v", method, path, err)
		}
	}
	return out
}

func TestRemotePagePWAAndScopedAuth(t *testing.T) {
	t.Parallel()
	h := newRemoteHarness(t)
	page := string(remoteHTTP(t, http.MethodGet, h.base, "", "/remote?token="+url.QueryEscape(h.token), nil, 200))
	for _, want := range []string{"data-token=", "Greg", "/remote/pwa/manifest.webmanifest\"",
		"/remote/app.js", "cdn.jsdelivr.net/npm/@knadh/oat@1.1.0/oat.min.css", "/remote/page.css",
		`aria-controls="panel-status"`, `aria-live="polite"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("remote page missing %q: %s", want, page)
		}
	}
	if strings.Contains(page, "manifest.webmanifest?token=") || strings.Contains(page, "style.css") {
		t.Fatal("page still puts the token in asset URLs or uses the deleted stylesheet")
	}
	if got := string(remoteHTTP(t, http.MethodGet, h.base, "", "/remote/page.css", nil, 200)); !strings.Contains(got, "[hidden=\"until-found\"]") {
		t.Fatal("page stylesheet not served")
	}
	remoteHTTP(t, http.MethodGet, h.base, "", "/remote/style.css", nil, 401)
	manifestRaw := remoteHTTP(t, http.MethodGet, h.base, "", "/remote/pwa/manifest.webmanifest?token="+url.QueryEscape(h.token), nil, 200)
	var manifest map[string]any
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest["name"] != "Greg" || manifest["start_url"] == "" {
		t.Fatalf("unexpected manifest: %s", manifestRaw)
	}
	icon := remoteHTTP(t, http.MethodGet, h.base, "", "/remote/pwa/icon-192.png?token="+url.QueryEscape(h.token), nil, 200)
	img, err := png.Decode(bytes.NewReader(icon))
	if err != nil || img.Bounds().Dx() != 192 {
		t.Fatalf("invalid PWA icon: %v %v", err, img)
	}
	remoteHTTP(t, http.MethodGet, h.base, "", "/remote", nil, 401)
	remoteHTTP(t, http.MethodGet, h.base, "", "/remote?token=wrong", nil, 401)
	remoteHTTP(t, http.MethodPost, h.base, "wrong", "/remote/api/listen", object{"listening": true}, 401)

	scoped := h.scopedBase()
	remoteHTTP(t, http.MethodGet, scoped, "", "/remote?token=wrong", nil, 401)
	scopedPage := string(remoteHTTP(t, http.MethodGet, scoped, "", "/remote?token="+url.QueryEscape(h.token), nil, 200))
	if !strings.Contains(scopedPage, "data-token=") {
		t.Fatal("scoped listener did not serve the remote page")
	}
	res, err := http.Get(h.base + "/remote?token=" + url.QueryEscape(h.token))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	cookies := res.Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly {
		t.Fatalf("remote cookie is not Secure+HttpOnly: %+v", cookies)
	}
}

func TestRemoteRESTCommandsAndHistoryCursor(t *testing.T) {
	t.Parallel()
	h := newRemoteHarness(t)
	b := h.b
	h.b.voices = companionVoiceStore(b.db, heartVoice)
	if _, _, err := b.saveStory(t.Context(), "lost_island", ":: StoryTitle\nLost Island\n\n:: Start\nShore.\n\n[[Cave->Cave]]\n\n:: Cave {\"end\":true}\nFound treasure.\n", "", true); err != nil {
		t.Fatal(err)
	}
	if err := b.setStoryProgress(t.Context(), storyProgress{CompanionID: "greg", StoryID: "lost_island", Passage: "Cave"}); err != nil {
		t.Fatal(err)
	}

	baseTS := time.Now().Add(-10 * time.Minute).UnixMilli()
	for i, msg := range []struct {
		id, role, sender, text, source string
	}{
		{"m1", "user", "You", "[You are Greg (greg). Speaker: in person]\nHow is the weather?", "voice"},
		{"m2", "companion", "Greg", "Clear skies today.", "voice"},
		{"m3", "user", "Mum", "Dinner at seven.", "deltachat"},
		{"m4", "intercom", "Milo", "Tea is ready.", "intercom"},
		{"m5", "user", "Dad", "Borrow the car?", "deltachat"},
	} {
		if _, err := b.db.ExecContext(t.Context(),
			`INSERT INTO messages (id,companion_id,role,sender,text,source,channel,card_title,ts_ms) VALUES (?,?,?,?,?,?,?,?,?)`,
			msg.id, "greg", msg.role, msg.sender, msg.text, msg.source, channelForSource(msg.source), "", baseTS+int64(i)*1000); err != nil {
			t.Fatal(err)
		}
	}

	// Listen.
	res := remoteJSON(t, http.MethodPost, h.base, h.token, "/remote/api/listen", object{"listening": true}, 200)
	if res["ok"] != true || !b.companionFor("greg").Listening {
		t.Fatalf("listen failed: %+v", res)
	}
	// Say records the spoken turn.
	res = remoteJSON(t, http.MethodPost, h.base, h.token, "/remote/api/say", object{"text": "Good morning!"}, 200)
	if res["ok"] != true {
		t.Fatalf("say failed: %+v", res)
	}
	b.history.flush("greg")
	entries, err := b.history.list(t.Context(), "greg")
	if err != nil {
		t.Fatal(err)
	}
	foundSay := false
	for _, e := range entries {
		if e.Text == "/say Good morning!" && e.Sender == "Remote" {
			foundSay = true
		}
	}
	if !foundSay {
		t.Fatalf("say not recorded: %+v", entries)
	}

	// Voice response carries the new state immediately.
	res = remoteJSON(t, http.MethodPost, h.base, h.token, "/remote/api/voice", object{"voice": "bm_fable"}, 200)
	stateObj, _ := res["state"].(object)
	comp, _ := stateObj["companion"].(object)
	if b.voiceFor("greg") != "bm_fable" || comp["voice"] != "bm_fable" {
		t.Fatalf("voice not applied: %+v", res)
	}

	// Story restart clears progress and opens the start passage.
	res = remoteJSON(t, http.MethodPost, h.base, h.token, "/remote/api/story/restart", object{"story": "lost_island"}, 200)
	if res["ok"] != true {
		t.Fatalf("story restart failed: %+v", res)
	}
	if _, ok, err := b.getStoryProgress(t.Context(), "greg", "lost_island"); err != nil || ok {
		t.Fatalf("story progress not cleared: ok=%v err=%v", ok, err)
	}

	// History pages with an opaque cursor and applies filters/search.
	page1 := remoteJSON(t, http.MethodGet, h.base, h.token, "/remote/api/history?limit=2&channel=all", nil, 200)
	items, _ := page1["items"].([]any)
	if len(items) != 2 || page1["has_more"] != true || str(page1["next_cursor"]) == "" {
		t.Fatalf("unexpected history page 1: %+v", page1)
	}
	page2 := remoteJSON(t, http.MethodGet, h.base, h.token, "/remote/api/history?limit=2&channel=all&cursor="+url.QueryEscape(str(page1["next_cursor"])), nil, 200)
	if items2, _ := page2["items"].([]any); len(items2) == 0 {
		t.Fatalf("cursor returned no second page: %+v", page2)
	}
	// The scoped link shows only its own contact's Delta Chat traffic and
	// never intercom or another contact's messages.
	own := remoteJSON(t, http.MethodGet, h.base, h.token, "/remote/api/history?channel=deltachat&limit=20", nil, 200)
	ownItems, _ := own["items"].([]any)
	if len(ownItems) != 1 {
		t.Fatalf("own-contact Delta Chat history = %+v; want only Mum", own)
	}
	if row, _ := ownItems[0].(object); row["text"] != "Dinner at seven." {
		t.Fatalf("wrong Delta Chat row exposed: %+v", row)
	}
	remoteJSON(t, http.MethodGet, h.base, h.token, "/remote/api/history?channel=intercom", nil, 400)
	search := remoteJSON(t, http.MethodGet, h.base, h.token, "/remote/api/history?channel=in_person&q=weather", nil, 200)
	searchItems, _ := search["items"].([]any)
	if len(searchItems) != 1 {
		t.Fatalf("search returned %d rows: %+v", len(searchItems), search)
	}
	row, _ := searchItems[0].(object)
	if row["speaker"] != "Someone in person" || row["text"] != "How is the weather?" {
		t.Fatalf("unexpected search row: %+v", row)
	}
	remoteJSON(t, http.MethodGet, h.base, h.token, "/remote/api/history?channel=bogus", nil, 400)
}

func TestRemoteEventsStreamsInitialStateAndChanges(t *testing.T) {
	t.Parallel()
	h := newRemoteHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.base+"/remote/events?token="+url.QueryEscape(h.token), nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("events status=%d type=%q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	reader := bufio.NewReader(res.Body)
	readState := func() object {
		t.Helper()
		deadline := time.Now().Add(testWait(t))
		event := ""
		for time.Now().Before(deadline) {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(line, "event: ") {
				event = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
				continue
			}
			if event == "state" && strings.HasPrefix(line, "data: ") {
				var snap object
				if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data: "))), &snap); err != nil {
					t.Fatal(err)
				}
				return snap
			}
		}
		t.Fatal("timed out reading SSE state")
		return nil
	}
	initial := readState()
	comp, _ := initial["companion"].(object)
	if comp["name"] != "Greg" || comp["portrait"] == nil {
		t.Fatalf("unexpected initial SSE state: %+v", initial)
	}
	if _, err := h.b.patchCompanion(t.Context(), "greg", object{"form": "a very old chameleon"}); err != nil {
		t.Fatal(err)
	}
	changed := readState()
	comp, _ = changed["companion"].(object)
	if comp["form"] != "a very old chameleon" {
		t.Fatalf("SSE did not stream the change: %+v", changed)
	}
}

func TestRemoteTunnelCommandIssueRotateAndRevoke(t *testing.T) {
	t.Parallel()
	b, _, stub := dcTestHarness(t)
	b.remote = newRemoteManager(b, "fake-opentunnel")
	var calls []string
	b.remote.runner = fakeTunnelRunner(&calls)
	c, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "an old chameleon"})
	if err != nil {
		t.Fatal(err)
	}
	mum := deltachat.Contact{Id: 10, DisplayName: "Mum", Address: "mum@example.org", IsVerified: true}
	stub.mu.Lock()
	stub.contacts[c.DCAccountID][10] = mum
	stub.chats[c.DCAccountID][200] = deltachat.BasicChat{Id: 200, ChatType: deltachat.ChatTypeSingle}
	stub.chatContacts[c.DCAccountID][200] = []uint32{deltachat.ContactSelf, 10}
	stub.msgs[c.DCAccountID][1] = deltachat.Message{Id: 1, ChatId: 200, FromId: 10, Text: "/remote", ViewType: deltachat.ViewtypeText}
	stub.mu.Unlock()

	b.dc.handleMessage(t.Context(), c.DCAccountID, 1)
	stub.mu.Lock()
	if len(stub.sent) != 1 || stub.sent[0].data.Text == nil {
		stub.mu.Unlock()
		t.Fatalf("expected one link message, got %+v", stub.sent)
	}
	link := *stub.sent[0].data.Text
	stub.mu.Unlock()
	if !strings.HasPrefix(link, "https://scoped.test.opentunnel.example/remote?token=") || strings.ContainsAny(link, " \n") {
		t.Fatalf("unexpected /remote link: %q", link)
	}
	first, _ := url.Parse(link)
	oldToken := first.Query().Get("token")

	stub.mu.Lock()
	stub.msgs[c.DCAccountID][2] = deltachat.Message{Id: 2, ChatId: 200, FromId: 10, Text: "/remote", ViewType: deltachat.ViewtypeText}
	stub.mu.Unlock()
	b.dc.handleMessage(t.Context(), c.DCAccountID, 2)
	stub.mu.Lock()
	secondLink := *stub.sent[1].data.Text
	stub.mu.Unlock()
	if secondLink == link {
		t.Fatal("/remote did not mint a fresh link")
	}
	remoteHTTP(t, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(b.remote.routes["greg"].LocalPort), "", "/remote?token="+url.QueryEscape(oldToken), nil, 401)
	remoteJSON(t, http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(b.remote.routes["greg"].LocalPort), "", "/remote/api/listen", object{"listening": true}, 401)

	// /remote off revokes the current token and removes the tunnel.
	stub.mu.Lock()
	stub.msgs[c.DCAccountID][3] = deltachat.Message{Id: 3, ChatId: 200, FromId: 10, Text: "/remote off", ViewType: deltachat.ViewtypeText}
	stub.mu.Unlock()
	b.dc.handleMessage(t.Context(), c.DCAccountID, 3)
	if len(b.remote.routes) != 0 || len(b.remote.tokens) != 0 {
		t.Fatalf("remote state survived revoke: routes=%v tokens=%v", b.remote.routes, b.remote.tokens)
	}
	var removed bool
	for _, call := range calls {
		if strings.Contains(call, "route remove") {
			removed = true
		}
	}
	if !removed {
		t.Fatalf("opentunnel route was not removed: %v", calls)
	}
}

// A failed route add must leave the previously issued link working and must
// not leave a half-installed route behind.
func TestRemoteFailedRouteAddKeepsOldToken(t *testing.T) {
	t.Parallel()
	h := newRemoteHarness(t)
	b := h.b
	m := b.remote
	m.mu.Lock()
	route := m.routes[h.comp.ID]
	scope := m.live[h.comp.ID]
	delete(m.live, h.comp.ID)
	m.mu.Unlock()
	scope.close()

	// Occupy the saved port so restoring the listener fails.
	block, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(route.LocalPort))
	if err != nil {
		t.Skipf("cannot occupy saved port: %v", err)
	}
	defer block.Close()

	oldMeta, err := b.db.getMeta(t.Context(), remoteTokenKey(h.comp.ID, "mum@example.org"))
	if err != nil || oldMeta == "" {
		t.Fatalf("token meta missing: %q %v", oldMeta, err)
	}
	if _, err := m.issue(t.Context(), h.comp.ID, "mum@example.org", "Mum"); err == nil {
		t.Fatal("issue succeeded with the saved port occupied")
	}
	remoteJSON(t, http.MethodGet, h.base, h.token, "/remote/api/history", nil, 200)
	after, err := b.db.getMeta(t.Context(), remoteTokenKey(h.comp.ID, "mum@example.org"))
	if err != nil || after != oldMeta {
		t.Fatalf("failed issue rewrote token meta: %q -> %q (%v)", oldMeta, after, err)
	}
}

func TestRemoteTunnelRestoresSavedRouteAndToken(t *testing.T) {
	serial(t)
	b, _ := wsHarness(t, nil)
	b.voices = companionVoiceStore(b.db, heartVoice)
	if _, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "an old chameleon"}); err != nil {
		t.Fatal(err)
	}
	b.remote = newRemoteManager(b, "fake-opentunnel")
	var calls []string
	b.remote.runner = fakeTunnelRunner(&calls)
	link, err := b.remote.issue(t.Context(), "greg", "mum@example.org", "Mum")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(link)
	token := parsed.Query().Get("token")
	b.remote.close()

	b.remote = newRemoteManager(b, "fake-opentunnel")
	b.remote.runner = fakeTunnelRunner(&calls)
	t.Cleanup(b.remote.close)
	b.remote.restore(t.Context())
	if len(b.remote.routes) != 1 || len(b.remote.tokens) != 1 {
		t.Fatalf("restore lost state: routes=%d tokens=%d", len(b.remote.routes), len(b.remote.tokens))
	}
	base := "http://127.0.0.1:" + strconv.Itoa(b.remote.routes["greg"].LocalPort)
	remoteJSON(t, http.MethodGet, base, token, "/remote/api/history", nil, 200)
	remoteJSON(t, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(b.remote.routes["greg"].LocalPort), "wrong", "/remote/api/history", nil, 401)
}

func TestRemoteMissingOpentunnelAndCompanionDelete(t *testing.T) {
	t.Parallel()
	b, _, stub := dcTestHarness(t)
	b.remote = newRemoteManager(b, "")
	c, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "an old chameleon"})
	if err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	stub.contacts[c.DCAccountID][10] = deltachat.Contact{Id: 10, DisplayName: "Mum", Address: "mum@example.org", IsVerified: true}
	stub.chats[c.DCAccountID][200] = deltachat.BasicChat{Id: 200, ChatType: deltachat.ChatTypeSingle}
	stub.chatContacts[c.DCAccountID][200] = []uint32{deltachat.ContactSelf, 10}
	stub.msgs[c.DCAccountID][1] = deltachat.Message{Id: 1, ChatId: 200, FromId: 10, Text: "/remote", ViewType: deltachat.ViewtypeText}
	stub.mu.Unlock()
	b.dc.handleMessage(t.Context(), c.DCAccountID, 1)
	stub.mu.Lock()
	if len(stub.sent) != 1 || !strings.Contains(*stub.sent[0].data.Text, "opentunnel") {
		stub.mu.Unlock()
		t.Fatalf("missing opentunnel reply = %+v", stub.sent)
	}
	stub.mu.Unlock()

	// A saved route is torn down with its companion.
	b.remote = newRemoteManager(b, "fake-opentunnel")
	var calls []string
	b.remote.runner = fakeTunnelRunner(&calls)
	if _, err := b.remote.issue(t.Context(), "greg", "mum@example.org", "Mum"); err != nil {
		t.Fatal(err)
	}
	if len(b.remote.routes) != 1 {
		t.Fatal("route was not created")
	}
	if err := b.deleteCompanion(t.Context(), "greg"); err != nil {
		t.Fatal(err)
	}
	if len(b.remote.routes) != 0 || len(b.remote.tokens) != 0 {
		t.Fatalf("companion delete left remote state: routes=%v tokens=%v", b.remote.routes, b.remote.tokens)
	}
}

func TestRemoteManifestIsTokenFree(t *testing.T) {
	t.Parallel()
	b, _ := wsHarness(t, nil)
	if _, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "an old chameleon"}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	b.serveRemotePWA(rec, httptest.NewRequest(http.MethodGet, "/remote/pwa/manifest.webmanifest", nil),
		"/remote/pwa/manifest.webmanifest", remoteToken{CompanionID: "greg"}, "secret-token-value")
	if rec.Code != 200 {
		t.Fatalf("manifest status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "token") || strings.Contains(body, "secret-token-value") {
		t.Fatalf("manifest leaks the link token: %s", body)
	}
	var manifest struct {
		StartURL string `json:"start_url"`
		Icons    []struct {
			Src string `json:"src"`
		} `json:"icons"`
	}
	if err := json.Unmarshal([]byte(body), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.StartURL != "/remote" {
		t.Fatalf("start_url = %q", manifest.StartURL)
	}
	if len(manifest.Icons) == 0 {
		t.Fatal("manifest has no icons")
	}
	for _, icon := range manifest.Icons {
		if !strings.HasPrefix(icon.Src, "/remote/pwa/") {
			t.Fatalf("icon src = %q", icon.Src)
		}
	}
}

func TestRemoteVolumeStoryOpenAndRevoke(t *testing.T) {
	t.Parallel()
	h := newRemoteHarness(t)
	b := h.b
	if _, _, err := b.saveStory(t.Context(), "lost_island", ":: StoryTitle\nLost Island\n\n:: Start {\"end\": true}\nShore.\n", "", true); err != nil {
		t.Fatal(err)
	}
	dev := testConnectWS(t, h.base, "unit", false)
	dev.hello(t, "vocat-r001", "greg")

	res := remoteJSON(t, http.MethodPost, h.base, h.token, "/remote/api/volume", object{"terminal": "vocat-r001", "volume": 73}, 200)
	if res["ok"] != true {
		t.Fatalf("volume failed: %+v", res)
	}
	if level := b.volumePref("vocat-r001"); level != 73 {
		t.Fatalf("volume not persisted: %d", level)
	}

	res = remoteJSON(t, http.MethodPost, h.base, h.token, "/remote/api/story/open", object{"story": "lost_island"}, 200)
	if res["ok"] != true {
		t.Fatalf("story open failed: %+v", res)
	}
	b.hub.mu.Lock()
	d := b.hub.terminals["vocat-r001"]
	opened := d != nil && d.drawer == drawerStoryReader
	b.hub.mu.Unlock()
	if !opened {
		t.Fatal("story did not open on the attached terminal")
	}

	remoteJSON(t, http.MethodPost, h.base, h.token, "/remote/api/revoke", object{}, 200)
	if len(b.remote.tokens) != 0 {
		t.Fatalf("revoke left tokens: %v", b.remote.tokens)
	}
	remoteHTTP(t, http.MethodGet, h.base, "", "/remote?token="+url.QueryEscape(h.token), nil, 401)
}

// A missing terminal is a not-found; a terminal owned by another companion is
// out of scope. Both used to answer 403.
func TestRemoteVolumeScopeErrors(t *testing.T) {
	t.Parallel()
	h := newRemoteHarness(t)
	remoteJSON(t, http.MethodPost, h.base, h.token, "/remote/api/volume", object{"terminal": "nosuch", "volume": 50}, 404)
	if _, err := h.b.createCompanion(t.Context(), object{"id": "milo", "name": "Milo", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	if err := h.b.terminals.save("vocat-r002", terminalRecord{ID: "vocat-r002", Kind: "hardware", CompanionID: "milo"}, false); err != nil {
		t.Fatal(err)
	}
	remoteJSON(t, http.MethodPost, h.base, h.token, "/remote/api/volume", object{"terminal": "vocat-r002", "volume": 50}, 403)
}
