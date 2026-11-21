package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func patchTimerTest(t *testing.T, base string, args object, failed bool) object {
	t.Helper()
	id := str(args["device_id"])
	args = maps.Clone(args)
	delete(args, "device_id")
	result := toolTest(t, base, "patch", object{"device_id": id, "timer": args}, failed)
	if failed {
		return result
	}
	return result["timer"].(object)
}

func TestTimerLocalFireAndCancel(t *testing.T) {
	tts := make(chan string, 8)
	_, base := wsHarness(t, func(b *bridge, _ string) {
		b.voices = deviceVoiceStore(b.db, fableVoice)
		b.worker = &synthWorker{requests: make(chan ttsRequest)}
		b.worker.ready.Store(true)
		go func() {
			for {
				select {
				case <-b.root.Done():
					return
				case req := <-b.worker.requests:
					if req.text == "Take a break." {
						tts <- req.voice
					}
					req.done <- ttsResult{pcm: []byte{1, 2, 3, 4}}
				}
			}
		}()
		b.history = &chatHistory{b.db}
		b.timers = newTimerManager(b)
		t.Cleanup(b.timers.close)
	})
	const id = "lark-timer"
	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, id, "Cake")
	web := testConnectWS(t, base, "unit", false)
	web.json(t, object{"ev": "hello", "role": "web"})
	web.json(t, object{"ev": "watch", "device_id": id})
	web.until(t, hasWS("svg", ""))
	patchControl(t, base, id, object{"sleep": true})
	dev.until(t, hasWS("power", `"brightness":12`))

	set := object{"device_id": id, "in_seconds": 1, "label": "Cake break", "say": "Take a break."}
	timer := patchTimerTest(t, base, set, false)
	hasUpdate := func(f wsFrame) bool { return bytes.Contains(f.data, []byte(`"type":"timers"`)) }
	web.until(t, hasUpdate)
	if got := apiObjects(t, base+"/api/timers?device_id="+id, "timers"); len(got) != 1 || got[0].(object)["id"] != timer["id"] {
		t.Fatal(got)
	}
	dev.until(t, hasWS("power", `"brightness":85`))
	dev.until(t, hasWS("chime", `"kind":"intercom"`))
	var card wsFrame
	updated := false
	web.until(t, func(f wsFrame) bool {
		if hasWS("svg", "up!")(f) {
			card = f
		}
		updated = updated || hasUpdate(f)
		return updated && card.data != nil
	})
	if !bytes.Contains(card.data, []byte("OK")) || !bytes.Contains(card.data, []byte("Cake break")) {
		t.Fatal(string(card.data))
	}
	dev.until(t, func(f wsFrame) bool { return f.op == wsBinary && bytes.Equal(f.data, []byte{pcmPrefix, 1, 2, 3, 4}) })
	expectValue(t, tts, fableVoice)
	if len(tts) != 0 || len(apiObjects(t, base+"/api/timers", "timers")) != 0 {
		t.Fatal("timer didn't synthesize and remove exactly once")
	}
	dev.json(t, object{"ev": "tap", "x": 180, "y": 315})
	web.until(t, hasWS("svg", "companion-ring"))
	entries := apiObjects(t, base+"/api/devices/"+id+"/messages", "messages")
	if len(entries) != 2 || entries[0].(object)["text"] != "Timer: Cake break" || entries[1].(object)["source"] != "timer" {
		t.Fatal(entries)
	}
	set["in_seconds"] = 3600
	timer = toolTest(t, base, "patch", object{"timer": set}, false)["timer"].(object)
	s := toolTest(t, base, "state", object{"device_id": id, "include": []string{"timers"}}, false)["timers"].([]any)
	if len(s) != 1 || s[0].(object)["id"] != timer["id"] || s[0].(object)["remaining_s"] != json.Number("3600") {
		t.Fatal(s)
	}
	if cancelled := patchTimerTest(t, base, object{"op": "cancel", "device_id": id, "id": timer["id"]}, false); cancelled["cancelled"] != true {
		t.Fatal(cancelled)
	}
	if got := apiObjects(t, base+"/api/timers?device_id="+id, "timers"); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestTimerLaunchesSavedApp(t *testing.T) {
	b, base := wsHarness(t, func(b *bridge, _ string) {
		b.timers = newTimerManager(b)
		t.Cleanup(b.timers.close)
	})
	const id, name = "lark-scheduled", "stretch"
	dev, web := testConnectWS(t, base, "unit", false), testConnectWS(t, base, "unit", true)
	dev.hello(t, id, "Cake")
	web.json(t, object{"ev": "hello", "role": "web"})
	web.json(t, object{"ev": "watch", "device_id": id})
	web.until(t, hasWS("svg", "companion-ring"))
	set := object{"op": "set", "device_id": id, "in_seconds": 1, "app": name}
	if out := patchTimerTest(t, base, set, true); !strings.Contains(str(out["error"]), "App not found") {
		t.Fatal(out)
	}
	if err := b.saveApp(t.Context(), savedApp{Name: name, Title: "Stretch", Kind: "app",
		Code: `function render(){return '<svg><text x="80" y="180">Stretch now</text></svg>';}`}); err != nil {
		t.Fatal(err)
	}
	if out := patchTimerTest(t, base, set, false); out["app"] != name {
		t.Fatal(out)
	}
	dev.until(t, hasWS("chime", `"kind":"intercom"`))
	web.until(t, hasWS("svg", "Stretch now"))
	if a := stateMe(t, base, id)["active_app"].(object); a["name"] != name {
		t.Fatal(a)
	}
}

func TestTimerRestartPrunesAndFiresOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lark.db")
	db := testDB(t, path)
	now := time.Now().UTC()
	const device = "virt-01234567"
	if err := virtualStore(db).save(device, virtualClient{ID: device, Character: "Greg", CreatedAt: now}, false); err != nil {
		t.Fatal(err)
	}
	b := &bridge{db: db, root: t.Context()}
	first := newTimerManager(b)
	ids := [3]string{randomHex(12), randomHex(12), randomHex(12)}
	for i, delay := range []time.Duration{time.Minute, time.Hour, 2 * time.Hour} {
		if err := first.set(t.Context(), savedTimer{ID: ids[i], DeviceID: device, Label: fmt.Sprint(i), Chime: "tap",
			CreatedAt: now.Add(-time.Hour), FireAt: now.Add(delay)}); err != nil {
			t.Fatal(err)
		}
	}
	first.close()
	for i, delay := range []time.Duration{-time.Minute, -3 * time.Minute} {
		if _, err := db.Exec("UPDATE timers SET fire_at=? WHERE id=?", now.Add(delay).Format(time.RFC3339Nano), ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = testDB(t, path)
	root, cancel := context.WithCancel(t.Context())
	defer cancel()
	b = &bridge{db: db, root: root, virtual: virtualStore(db), history: &chatHistory{db}, peers: peers{deviceStore(db)}}
	b.ws = newWSHub(b, false)
	defer b.ws.shutdown()
	m := newTimerManager(b)
	defer m.close()
	if err := m.restore(time.Now()); err != nil {
		t.Fatal(err)
	}
	initial, err := m.list(t.Context(), device)
	if err != nil || len(initial) != 2 || initial[0].ID != ids[0] || initial[1].ID != ids[2] {
		t.Fatal("recovery did not prune only the stale timer", initial, err)
	}
	deadline := time.Now().Add(timerGrace + 3*time.Second)
	for {
		events, err := b.history.list(t.Context(), device)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) == 1 {
			if events[0].Text != "Time's up! 0" {
				t.Fatal(events)
			}
			break
		}
		if len(events) > 1 || time.Now().After(deadline) {
			t.Fatal("missed timer did not fire once", events)
		}
		time.Sleep(20 * time.Millisecond)
	}
	m.fire(ids[0], nil)
	if events, err := b.history.list(t.Context(), device); err != nil || len(events) != 1 {
		t.Fatal("fired again", events, err)
	}
	if left, err := m.list(t.Context(), device); err != nil || len(left) != 1 || left[0].ID != ids[2] {
		t.Fatal("future timer didn't survive", left, err)
	}
}

func TestTimerClaimRetriesAfterDatabaseError(t *testing.T) {
	const id = "lark-timer-retry"
	b, base := wsHarness(t, func(b *bridge, _ string) {
		b.timers = newTimerManager(b)
		t.Cleanup(b.timers.close)
	})
	board := testConnectWS(t, base, "unit", false)
	board.helloV2(t, id, "Cake")
	now := time.Now()
	timer := savedTimer{ID: randomHex(12), DeviceID: id, CreatedAt: now, FireAt: now.Add(time.Hour), Label: "Retry", Chime: "celebrate"}
	if err := b.timers.set(t.Context(), timer); err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.Exec("ALTER TABLE timers RENAME TO paused_timers"); err != nil {
		t.Fatal(err)
	}
	b.timers.fire(timer.ID, nil)
	if _, err := b.db.Exec("ALTER TABLE paused_timers RENAME TO timers"); err != nil {
		t.Fatal(err)
	}
	board.until(t, hasWS("chime", `"kind":"intercom"`))
	b.timers.fire(timer.ID, nil)
	left, err := b.timers.list(t.Context(), id)
	if err != nil || len(left) != 0 {
		t.Fatalf("retry did not consume timer: %v (%v)", left, err)
	}
	events, err := b.history.list(t.Context(), id)
	if err != nil || len(events) != 1 || events[0].Text != "Time's up! Retry" {
		t.Fatalf("retry did not deliver exactly once: %v (%v)", events, err)
	}
}

func TestTimerWakeAgentGuard(t *testing.T) {
	const device, other, sid = "lark-timer", "lark-other", "ses-reminder"
	prompt, release := make(chan string, 1), make(chan struct{})
	b, base := wsHarness(t, func(b *bridge, dir string) {
		b.timers = newTimerManager(b)
		t.Cleanup(b.timers.close)
		b.oc = testOpenCode(t, b.root, dir, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/session", "/api/session/" + sid:
				fmt.Fprintf(w, `{"data":{"id":%q}}`, sid)
			case "/api/session/" + sid + "/prompt":
				raw, _ := io.ReadAll(r.Body)
				prompt <- str(testObject(t, raw)["text"])
				select {
				case <-release:
					_, _ = io.WriteString(w, `{"data":{"id":"msg1"}}`)
				case <-r.Context().Done():
				}
			case "/api/experimental/session/" + sid + "/wait":
				w.WriteHeader(204)
			case "/api/session/" + sid + "/message":
				_, _ = io.WriteString(w, `{"data":[{"id":"msg2","type":"assistant","finish":"stop","content":[{"type":"text","text":"Done"}]},{"id":"msg1","type":"user"}]}`)
			default:
				w.WriteHeader(404)
			}
		})
	})
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, device, "Cake")
	testConnectWS(t, base, "unit", false).hello(t, other, "Mint")
	args := object{"device_id": device, "op": "set", "wake_agent": true, "in_seconds": 29}
	patchTimerTest(t, base, args, true)
	args["in_seconds"] = 30
	args["at"] = time.Now().Add(time.Hour).Format(time.RFC3339)
	patchTimerTest(t, base, args, true)
	delete(args, "at")
	timer := patchTimerTest(t, base, args, false)
	// Invoke the real callback now rather than waiting 30 seconds.
	finished := make(chan struct{})
	go func() { defer close(finished); b.timers.fire(str(timer["id"]), nil) }()
	select {
	case got := <-prompt:
		if got != "[Cake (lark-timer) · hardware · timer]\nTimer fired: Timer" {
			t.Fatal(got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("missing timer prompt")
	}
	conflict := patchTimerTest(t, base, args, true)
	if !strings.Contains(str(conflict["error"]), "timer-triggered") {
		t.Fatal(conflict)
	}
	patchTimerTest(t, base, object{"device_id": other, "op": "set", "wake_agent": true, "in_seconds": 3600}, false)
	args["wake_agent"] = false
	patchTimerTest(t, base, args, false)
	close(release)
	expectValue(t, finished, struct{}{})
	args["wake_agent"] = true
	patchTimerTest(t, base, args, false)
	patchTimerTest(t, base, object{"device_id": device, "op": "cancel", "all": true}, false)
}

func TestScopedTimerIsolationAndCaps(t *testing.T) {
	const owner, other = "virt-01234567", "virt-abcdef12"
	b, base := wsHarness(t, func(b *bridge, _ string) {
		b.virtual = virtualStore(b.db)
		b.timers = newTimerManager(b)
		t.Cleanup(b.timers.close)
		for _, id := range []string{owner, other} {
			if err := b.virtual.save(id, virtualClient{ID: id, Character: "Greg", CreatedAt: time.Now()}, false); err != nil {
				t.Fatal(err)
			}
		}
	})
	ownTimer := patchTimerTest(t, base, object{"device_id": owner, "op": "set", "in_seconds": 3600}, false)
	otherTimer := patchTimerTest(t, base, object{"device_id": other, "op": "set", "in_seconds": 3600}, false)
	v, _ := b.virtualByID(owner)
	scope := httptest.NewServer(b.scoped(v, "scoped"))
	defer scope.Close()
	get := testObject(t, testRequest(t, "GET", scope.URL+"/api/timers", "scoped", nil, 200))["timers"].([]any)
	if len(get) != 1 || get[0].(object)["id"] != ownTimer["id"] {
		t.Fatal(get)
	}
	testRequest(t, "GET", scope.URL+"/api/timers?device_id="+other, "scoped", nil, 403)
	testRequest(t, "DELETE", scope.URL+"/api/timers/"+str(otherTimer["id"]), "scoped", nil, 403)
	testRequest(t, "GET", scope.URL+"/api/memory/scopes", "scoped", nil, 404)
	testRequest(t, "GET", scope.URL+"/api/assets", "scoped", nil, 404)
	testRequest(t, "DELETE", scope.URL+"/api/timers/"+str(ownTimer["id"]), "scoped", nil, 204)
	ctx := context.Background()
	for i := range 31 {
		id := fmt.Sprintf("device-%d", i/8)
		now := time.Now()
		if err := b.timers.set(ctx, savedTimer{ID: randomHex(12), DeviceID: id, CreatedAt: now, FireAt: now.Add(time.Hour)}); err != nil {
			t.Fatal(i, err)
		}
		if i == 7 {
			if err := b.timers.set(ctx, savedTimer{ID: randomHex(12), DeviceID: id, CreatedAt: now, FireAt: now.Add(time.Hour)}); err == nil {
				t.Fatal("allowed ninth timer")
			}
		}
	}
	now := time.Now()
	if err := b.timers.set(ctx, savedTimer{ID: randomHex(12), DeviceID: owner, CreatedAt: now, FireAt: now.Add(time.Hour)}); err == nil {
		t.Fatal("allowed 33rd timer")
	}
}
