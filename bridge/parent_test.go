package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testParentToken = "test-parent-secret-not-for-deploy"

func parentRequest(t *testing.T, base, method, path string, args any, want int) object {
	t.Helper()
	var body io.Reader
	if args != nil {
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(raw)
	}
	r, err := http.NewRequestWithContext(t.Context(), method, base+path, body)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("X-Lark-Token", "unit")
	r.Header.Set("X-Lark-Parent-Token", testParentToken)
	r.Header.Set("Content-Type", jsonType)
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != want {
		t.Fatalf("%s HTTP %d, want %d: %s (%v)", path, res.StatusCode, want, raw, err)
	}
	return testObject(t, raw)
}

func TestParentEnforcesQuietLimitAndPause(t *testing.T) {
	const id, peerID = "lark-managed", "lark-friend"
	const path = "/api/devices/" + id + "/parent"
	for _, reason := range []parentReason{parentQuiet, parentLimit, parentPause} {
		t.Run(string(reason), func(t *testing.T) {
			b, base := wsHarness(t, func(b *bridge, _ string) {
				b.parentToken = testParentToken
				b.worker = fakeTTS(b.root, nil)
				b.timers = newTimerManager(b)
				t.Cleanup(b.timers.close)
			})
			dev := testConnectWS(t, base, "unit", false)
			friend := testConnectWS(t, base, "unit", false)
			for _, pr := range []struct {
				ws       *testWS
				id, name string
			}{{dev, id, "Mint"}, {friend, peerID, "Cake"}} {
				pr.ws.json(t, object{"ev": "hello", "role": "device", "proto": 2, "device_id": pr.id, "character": pr.name,
					"caps": object{"wake_word": true, "call": true}})
				pr.ws.until(t, isDisplay)
			}
			viewer := testConnectWS(t, base, "unit", false)
			viewer.json(t, object{"ev": "hello", "role": "web"})
			viewer.json(t, object{"ev": "watch", "device_id": id})
			viewer.until(t, hasWS("svg", "companion-ring"))
			viewer.json(t, object{"ev": "wake_word", "enabled": true, "device_id": id})
			dev.until(t, hasWS("wake_word", `"enabled":true`))
			dev.json(t, object{"ev": "wake_word"})
			dev.until(t, hasWS("rec", `"action":"start"`))
			dev.json(t, object{"ev": "rec_start"})
			dev.barrier(t)

			now := time.Now().UTC()
			switch reason {
			case parentQuiet:
				parentRequest(t, base, "POST", path, object{"action": "settings", "timezone": "UTC",
					"quiet_start": now.Add(-time.Hour).Format("15:04"), "quiet_end": now.Add(time.Hour).Format("15:04"), "daily_minutes": 0}, 200)
			case parentLimit:
				parentRequest(t, base, "POST", path, object{"action": "settings", "timezone": "UTC",
					"quiet_start": "", "quiet_end": "", "daily_minutes": 1}, 200)
				b.ws.mu.Lock()
				b.ws.parents[id].used = time.Minute
				b.ws.parentRefresh(id, time.Now())
				b.ws.mu.Unlock()
			case parentPause:
				parentRequest(t, base, "POST", path, object{"action": "pause", "minutes": 5}, 200)
			}
			dev.until(t, hasWS("rec", `"cancelled":true`))
			dev.until(t, hasWS("power", `"brightness":20`))
			dev.until(t, hasWS("wake_word", `"enabled":false`))
			title, _ := reason.notice()
			viewer.until(t, hasWS("svg", title))
			if reason == parentLimit {
				dev.until(t, isPlayback)
			}

			for _, event := range []object{{"ev": "tap", "x": 180, "y": 215}, {"ev": "head", "action": "pet"},
				{"ev": "wake_word"}, {"ev": "rec_start"}, {"ev": "rec_request"}, {"ev": "text", "text": "unlock the device"},
				{"ev": "call", "target": peerID}, {"ev": "intercom", "target": peerID, "text": "hello"}} {
				dev.json(t, event)
			}
			dev.write(t, maskedFrame(wsBinary, true, []byte{micPrefix, 0, 0}))
			dev.json(t, object{"ev": "rec_end"})
			dev.barrier(t)
			friend.json(t, object{"ev": "call", "target": id})
			friend.barrier(t)
			testRequest(t, "POST", base+"/api/chat", "unit", object{"device_id": id, "text": "hello"}, 423)
			toolTest(t, base, "patch", object{"device_id": id, "device": object{"sleep": false}}, true)
			toolTest(t, base, "patch", object{"device_id": id, "parent_policy": object{"restricted": false}}, true)
			toolTest(t, base, "app", object{"devices": id, "svg": `<svg viewBox="0 0 360 360"/>`}, true)
			patchTimerTest(t, base, object{"device_id": id, "in_seconds": 20, "label": "Rest"}, true)
			for _, wake := range []bool{false, true} {
				b.deliverTimer(t.Context(), savedTimer{DeviceID: id, Label: "Should not arrive", WakeAgent: wake})
			}
			for _, pair := range [][2]string{{id, peerID}, {peerID, id}} {
				out := toolTest(t, base, "emit", object{"to": pair[0], "intercom": object{"from": pair[1], "text": "hello"}}, pair[1] == id)
				if pair[0] == id {
					r := out["receipts"].([]any)[0].(object)
					if r["status"] != "skipped" || r["reason"] != string(reason) {
						t.Fatal("incoming note wasn't declined by policy", r)
					}
				}
			}
			got := toolTest(t, base, "state", object{"device_id": id, "include": []string{"devices"}}, false)
			policy := got["me"].(object)["device"].(object)["parent_policy"].(object)
			if policy["restricted"] != true || policy["reason"] != string(reason) {
				t.Fatal("wrong policy on MCP state", policy)
			}
			b.ws.mu.Lock()
			d, f := b.ws.devices[id], b.ws.devices[peerID]
			blocked := d.asleep && d.parentReason == reason && !d.recording.active() && len(d.recording.pcm) == 0 &&
				d.callState == callNone && f.callState == callNone && d.app == nil && !d.busy && d.card == nil && d.view == nil
			b.ws.mu.Unlock()
			if !blocked {
				t.Fatal("blocked device accepted input or a delivery")
			}

			parentRequest(t, base, "POST", path, object{"action": "unlock", "minutes": 5}, 200)
			dev.until(t, hasWS("wake_word", `"enabled":true`))
			dev.json(t, object{"ev": "wake_word"})
			dev.until(t, hasWS("rec", `"action":"start"`))
		})
	}
}

func TestParentAdminAndChildPause(t *testing.T) {
	b, base := wsHarness(t, func(b *bridge, _ string) { b.parentToken = testParentToken })
	d := testConnectWS(t, base, "unit", false)
	const id, text = "lark-mint", "Stop working for five minutes so I can’t use you."
	const path = "/api/devices/" + id + "/parent"
	d.hello(t, id, "Mint")
	d.json(t, object{"ev": "text", "text": text})
	d.until(t, hasWS("parent_policy", `"reason":"paused"`))
	testRequest(t, "POST", base+"/api/chat", "unit", object{"device_id": id, "text": "pause for one minute"}, 423)
	testRequest(t, "GET", base+"/api/parent", "", nil, 401)
	testRequest(t, "GET", base+"/api/parent", "unit", nil, 403)
	testRequest(t, "POST", base+path, "unit", object{"action": "unlock", "minutes": 5}, 403)
	scoped := httptest.NewServer(b.scoped(virtualClient{ID: "virt-0123abcd"}, "scoped"))
	defer scoped.Close()
	testRequest(t, "GET", scoped.URL+"/app/parent", "scoped", nil, 404)
	testRequest(t, "GET", scoped.URL+"/app/parent.js", "scoped", nil, 404)
	testRequest(t, "GET", scoped.URL+"/api/parent", "scoped", nil, 404)
	parentRequest(t, scoped.URL, "POST", path, object{"action": "unlock", "minutes": 5}, 404)
	public := httptest.NewRequest("GET", "/api/parent", nil)
	public.RemoteAddr = "203.0.113.1:42"
	public.Header.Set("X-Lark-Token", "unit")
	public.Header.Set("X-Lark-Parent-Token", testParentToken)
	public.Header.Set("X-Forwarded-For", "127.0.0.1")
	res := httptest.NewRecorder()
	b.ServeHTTP(res, public)
	if res.Code != 403 {
		t.Fatal("parent controls accepted a non-LAN request")
	}
	if list := rpcTest(t, base, "tools/list", object{})["tools"].([]any); len(list) != 4 {
		t.Fatal("MCP no longer has four tools")
	}
	b.chat(id, newChat("user", "You", "private transcript text", "typed"), true)
	view := parentRequest(t, base, "GET", "/api/parent", nil, 200)
	row := view["devices"].([]any)[0].(object)
	raw, _ := json.Marshal(view)
	if row["reason"] != "paused" || row["recent"].(object)["talk_turns"] != json.Number("1") || bytes.Contains(raw, []byte("private transcript text")) {
		t.Fatal("parent activity didn't show counts only")
	}
	parentRequest(t, base, "POST", path, object{"action": "settings", "quiet_start": "22:00", "quiet_end": "", "timezone": "UTC", "daily_minutes": 0}, 400)
	// Reconnecting without wake-word capability must not drop a persisted pause.
	d = testConnectWS(t, base, "unit", false)
	d.hello(t, id, "Mint")
	d.json(t, object{"ev": "rec_start"})
	d.until(t, hasWS("rec", `"cancelled":true`))
	parentRequest(t, base, "POST", path, object{"action": "unlock", "minutes": 5}, 200)
	d.until(t, hasWS("parent_policy", `"reason":""`))
	for _, text := range []string{"pause for 99 minutes", "don't pause for five minutes", "tell me to pause for five minutes"} {
		if childPauseMinutes(text) != 0 {
			t.Fatal("treated discussion or an excessive duration as a command")
		}
	}
}

func TestParentTokenSeparateAndPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "parent.token")
	token, err := loadParentToken(path, "unit")
	if token != "" || err != nil {
		t.Fatal("absent credential should leave parent administration disabled")
	}
	if err := os.WriteFile(path, []byte(testParentToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err = loadParentToken(path, "unit")
	if token != testParentToken || err != nil {
		t.Fatal("private, distinct credential wasn't loaded")
	}
	if _, err := loadParentToken(path, testParentToken); err == nil {
		t.Fatal("shared bridge credential was accepted")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadParentToken(path, "unit"); err == nil {
		t.Fatal("world-readable credential was accepted")
	}
}

func TestParentTearsDownActiveAppCallAndOfflineChat(t *testing.T) {
	b, base := wsHarness(t, func(b *bridge, _ string) { b.parentToken = testParentToken })
	const id, peerID = "lark-active", "lark-partner"
	const path = "/api/devices/" + id + "/parent"
	d := testConnectWS(t, base, "unit", false)
	peer := testConnectWS(t, base, "unit", false)
	for _, p := range []struct {
		ws *testWS
		id string
	}{{d, id}, {peer, peerID}} {
		p.ws.json(t, object{"ev": "hello", "role": "device", "proto": 2, "device_id": p.id, "character": "Cake", "caps": object{"call": true}})
		p.ws.until(t, isDisplay)
	}
	toolTest(t, base, "app", object{"devices": id, "svg": `<svg viewBox="0 0 360 360"><text x="150" y="160">Game</text></svg>`}, false)
	parentRequest(t, base, "POST", path, object{"action": "pause", "minutes": 5}, 200)
	b.ws.mu.Lock()
	running := b.ws.devices[id].app != nil
	b.ws.mu.Unlock()
	if running {
		t.Fatal("app wasn't removed on pause")
	}
	parentRequest(t, base, "POST", path, object{"action": "unlock", "minutes": 15}, 200)
	d.json(t, object{"ev": "call", "target": peerID})
	peer.until(t, hasWS("chime", `"kind":"ring"`))
	peer.json(t, object{"ev": "call", "target": id})
	d.until(t, hasWS("call", `"state":"start"`))
	parentRequest(t, base, "POST", path, object{"action": "pause", "minutes": 5}, 200)
	d.until(t, hasWS("call", `"state":"end"`))
	peer.until(t, hasWS("call", `"state":"end"`))
	d.write(t, maskedFrame(wsClose, true, wsCloseFrame(1000).data))
	d.until(t, func(f wsFrame) bool { return f.op == wsClose })
	parentRequest(t, base, "POST", path, object{"action": "unlock", "minutes": 15}, 200)
	h := b.ws
	h.mu.Lock()
	child, done, err := h.parentOffline(t.Context(), id, time.Now())
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	parentRequest(t, base, "POST", path, object{"action": "pause", "minutes": 5}, 200)
	if child.Err() != context.Canceled {
		t.Fatal("offline turn wasn't canceled on pause")
	}
}

func TestParentCivilTimeAndPersistence(t *testing.T) {
	ctx := t.Context()
	db := testDB(t, ":memory:")
	h := &wsHub{b: &bridge{db: db}}
	key := "lark-clock"
	for _, tc := range []struct {
		zone   string
		before string
		after  string
	}{
		{"America/New_York", "2026-11-02T04:59:50Z", "2026-11-02T05:00:10Z"},
		// Chile springs forward at midnight; 00:00 never occurs.
		{"America/Santiago", "2026-09-06T03:59:50Z", "2026-09-06T04:00:10Z"},
	} {
		t.Run(tc.zone, func(t *testing.T) {
			loc, err := time.LoadLocation(tc.zone)
			if err != nil {
				t.Fatal(err)
			}
			parse := func(s string) time.Time {
				stamp, err := time.Parse(time.RFC3339, s)
				if err != nil {
					t.Fatal(err)
				}
				return stamp
			}
			before, after := parse(tc.before), parse(tc.after)
			settings := parentSettings{Timezone: tc.zone, QuietStart: "22:00", QuietEnd: "07:00", DailyMinutes: 1}
			if settings.reason(before, loc, time.Minute) != parentQuiet || settings.reason(after, loc, 0) != parentQuiet ||
				settings.reason(time.Date(2026, 11, 2, 7, 0, 0, 0, loc), loc, time.Minute) != parentLimit {
				t.Fatal("overnight quiet hours or the exact end boundary is wrong")
			}
			id := key + "-" + strings.ToLower(strings.ReplaceAll(tc.zone, "/", "-"))
			if err := h.parentSet(id, settings, loc, before); err != nil {
				t.Fatal(err)
			}
			p := h.parents[id]
			p.using, p.used = true, 59*time.Second
			if err := h.parentAccount(id, p, after, true); err != nil {
				t.Fatal(err)
			}
			if p.day != after.In(loc).Format(time.DateOnly) || p.used != 10*time.Second {
				t.Fatal("usage didn't split and reset at local midnight", p.day, p.used)
			}
			var old int64
			if err := db.QueryRowContext(ctx, `SELECT used_ms FROM parent_usage WHERE device_id=? AND day=?`, id, before.In(loc).Format(time.DateOnly)).Scan(&old); err != nil || old != 69000 {
				t.Fatal("previous day was not retained", old, err)
			}
			restarted := &wsHub{b: &bridge{db: db}}
			if r := restarted.parentRefresh(id, after); r != parentQuiet || restarted.parents[id].used != 10*time.Second {
				t.Fatal("settings or usage did not persist across a hub restart")
			}
		})
	}
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)
	second := first.Add(time.Hour)
	s := parentSettings{Timezone: loc.String(), QuietStart: "01:00", QuietEnd: "02:00"}
	if s.reason(first, loc, 0) != parentQuiet || s.reason(second, loc, 0) != parentQuiet {
		t.Fatal("repeated local hour did not honor quiet hours in both offsets")
	}
	s.DailyMinutes = 1
	s.PauseUntil, s.OverrideUntil = first.UnixMilli(), second.UnixMilli()
	s.QuietStart, s.QuietEnd = "", ""
	if s.reason(first, loc, time.Minute) != "" || s.reason(second, loc, time.Minute) != parentLimit {
		t.Fatal("override did not expire at the exact instant or reset the usage")
	}
}

func TestParentScreensNoLayoutWarnings(t *testing.T) {
	for _, geometry := range []screenGeometry{roundScreen, {320, 240, "rect"}} {
		for _, reason := range []parentReason{parentQuiet, parentPause, parentLimit} {
			view := parentScene(reason)
			s, err := buildScreen(view.svg(""), false, geometry)
			if err != nil || len(s.fb) != geometry.width*geometry.height || len(s.hits) != 0 ||
				len(appLayoutWarnings([]svgScreen{s}, geometry.shape == "round")) != 0 || !strings.Contains(s.source, view.title) {
				t.Fatalf("%s %s notice did not render cleanly without wake controls: %v", geometry.shape, reason, err)
			}
		}
	}
}
