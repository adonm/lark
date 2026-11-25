package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func patchTimerTest(t *testing.T, base string, args object, failed bool) object {
	t.Helper()
	id := str(args["companion_id"])
	if id == "" {
		id = str(args["device_id"])
	}
	args = maps.Clone(args)
	delete(args, "device_id")
	delete(args, "companion_id")
	result := toolTest(t, base, "patch", object{"companion_id": id, "timer": args}, failed)
	if failed {
		return result
	}
	return result["timer"].(object)
}

func TestTimerLocalFireAndCancel(t *testing.T) {
	t.Parallel()
	tts := make(chan string, 8)
	b, base := wsHarness(t, func(b *larkServer, _ string) {
		b.voices = companionVoiceStore(b.db, fableVoice)
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
		b.history = &chatHistory{db: b.db}
		b.timers = newTimerManager(b)
		t.Cleanup(b.timers.close)
	})
	const id = "cake"
	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, "vocat-0001", "Cake")
	patchControl(t, base, id, object{"sleep": true})
	dev.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgPower, "")(f) && len(p) >= 1 && p[0] == 12
	})

	set := object{"companion_id": id, "in_seconds": 1, "label": "Cake break", "say": "Take a break."}
	timer := patchTimerTest(t, base, set, false)
	if got := toolTest(t, base, "state", object{"companion_id": id, "include": []string{"timers"}}, false)["timers"].([]any); len(got) != 1 || got[0].(object)["id"] != timer["id"] {
		t.Fatal(got)
	}
	dev.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgPower, "")(f) && len(p) >= 1 && p[0] == 85
	})
	dev.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgChime, "")(f) && len(p) >= 1 && p[0] == chimeIntercom
	})
	card := waitSVG(t, b.hub, "vocat-0001", "Cake break")
	if !strings.Contains(card, "Timer") || !strings.Contains(card, "Cake break") {
		t.Fatal(card)
	}
	dev.until(t, func(f protocolFrame) bool { return isPlayback(f) && bytes.Equal(framePayload(f), []byte{1, 2, 3, 4}) })
	expectValue(t, tts, fableVoice)
	if got := toolTest(t, base, "state", object{"companion_id": id, "include": []string{"timers"}}, false)["timers"].([]any); len(tts) != 0 || len(got) != 0 {
		t.Fatal("timer didn't synthesize and remove exactly once")
	}
	dev.tap(t, 180, 315, 0)
	waitSVG(t, b.hub, "vocat-0001", "home")
	entries, err := b.history.list(t.Context(), id)
	if err != nil || len(entries) != 2 || entries[0].Text != "Timer: Cake break" || entries[1].Source != "timer" {
		t.Fatal(entries, err)
	}
	set["in_seconds"] = 3600
	timer = toolTest(t, base, "patch", object{"timer": set}, false)["timer"].(object)
	s := toolTest(t, base, "state", object{"companion_id": id, "include": []string{"timers"}}, false)["timers"].([]any)
	if len(s) != 1 || s[0].(object)["id"] != timer["id"] || s[0].(object)["remaining_s"] != json.Number("3600") {
		t.Fatal(s)
	}
	if cancelled := patchTimerTest(t, base, object{"op": "cancel", "companion_id": id, "id": timer["id"]}, false); cancelled["cancelled"] != true {
		t.Fatal(cancelled)
	}
	if got := toolTest(t, base, "state", object{"companion_id": id, "include": []string{"timers"}}, false)["timers"].([]any); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestTimerRestartPrunesAndFiresOnce(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "lark.db")
	db := testDB(t, path)
	now := time.Now().UTC()
	const device = "greg"
	b := &larkServer{db: db, root: t.Context()}
	if _, err := b.createCompanion(t.Context(), object{"id": device, "name": "Greg", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	first := newTimerManager(b)
	ids := [3]string{testHex(t, 12), testHex(t, 12), testHex(t, 12)}
	for i, delay := range []time.Duration{time.Minute, time.Hour, 2 * time.Hour} {
		if _, err := first.set(t.Context(), savedTimer{ID: ids[i], CompanionID: device, Label: fmt.Sprint(i), Chime: "tap",
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
	b = &larkServer{db: db, root: root, terminals: terminalStore(db), history: &chatHistory{db: db}}
	b.hub = newTerminalHub(b, false)
	defer b.hub.shutdown()
	m := newTimerManager(b)
	defer m.close()
	fired := make(chan string, 4)
	m.onFired = func(id string) { fired <- id }
	if err := m.restore(time.Now()); err != nil {
		t.Fatal(err)
	}
	initial, err := m.list(t.Context(), device)
	if err != nil || len(initial) != 2 || initial[0].ID != ids[0] || initial[1].ID != ids[2] {
		t.Fatal("recovery did not prune only the stale timer", initial, err)
	}
	select {
	case gotID := <-fired:
		if gotID != ids[0] {
			t.Fatalf("unexpected timer fired: %q", gotID)
		}
	case <-time.After(timerGrace + 3*time.Second):
		t.Fatal("missed timer did not fire")
	}
	events, err := b.history.list(t.Context(), device)
	if err != nil || len(events) != 1 || events[0].Text != "Time's up! 0" {
		t.Fatal("missed timer did not fire once", events, err)
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
	t.Parallel()
	const id = "cake"
	b, base := wsHarness(t, func(b *larkServer, _ string) {
		b.timers = newTimerManager(b)
		t.Cleanup(b.timers.close)
	})
	board := testConnectWS(t, base, "unit", false)
	board.helloV2(t, "vocat-0003", "Cake")
	now := time.Now()
	timer := savedTimer{ID: testHex(t, 12), CompanionID: id, CreatedAt: now, FireAt: now.Add(time.Hour), Label: "Retry", Chime: "celebrate"}
	if _, err := b.timers.set(t.Context(), timer); err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.Exec("ALTER TABLE timers RENAME TO paused_timers"); err != nil {
		t.Fatal(err)
	}
	b.timers.fire(timer.ID, nil)
	if _, err := b.db.Exec("ALTER TABLE paused_timers RENAME TO timers"); err != nil {
		t.Fatal(err)
	}
	board.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgChime, "")(f) && len(p) >= 1 && p[0] == chimeIntercom
	})
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
	t.Parallel()
	const device, other, sid = "cake", "mint", "ses-reminder"
	prompt, release := make(chan string, 1), make(chan struct{})
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.timers = newTimerManager(b)
		t.Cleanup(b.timers.close)
		b.oc = testOpenCode(t, b.root, dir, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/session", "/api/session/" + sid:
				fmt.Fprintf(w, `{"data":{"id":%q}}`, sid)
			case "/api/experimental/session/" + sid + "/instructions/entries/lark.companion":
				w.WriteHeader(204)
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
	c.hello(t, "vocat-0004", "Cake")
	testConnectWS(t, base, "unit", false).hello(t, "vocat-0005", "Mint")
	args := object{"companion_id": device, "op": "set", "wake_agent": true, "in_seconds": 29}
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
		if !strings.HasPrefix(got, "[Event: timer]\nTimer fired: Timer\n[state: now: ") {
			t.Fatal(got)
		}
	case <-time.After(testWait(t)):
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
	t.Parallel()
	const owner, other = "owner", "other"
	b, base := wsHarness(t, func(b *larkServer, _ string) {
		b.timers = newTimerManager(b)
		t.Cleanup(b.timers.close)
		for _, id := range []string{owner, other} {
			if _, err := b.createCompanion(t.Context(), object{"id": id, "name": "Greg", "form": "a cat"}); err != nil {
				t.Fatal(err)
			}
		}
	})
	ownTimer := patchTimerTest(t, base, object{"companion_id": owner, "op": "set", "in_seconds": 3600}, false)
	_ = patchTimerTest(t, base, object{"companion_id": other, "op": "set", "in_seconds": 3600}, false)
	patchTimerTest(t, base, object{"companion_id": owner, "op": "cancel", "id": ownTimer["id"]}, false)
	ctx := context.Background()
	for i := range 31 {
		id := fmt.Sprintf("companion-%d", i/8)
		now := time.Now()
		if _, err := b.timers.set(ctx, savedTimer{ID: testHex(t, 12), CompanionID: id, CreatedAt: now, FireAt: now.Add(time.Hour)}); err != nil {
			t.Fatal(i, err)
		}
		if i == 7 {
			if _, err := b.timers.set(ctx, savedTimer{ID: testHex(t, 12), CompanionID: id, CreatedAt: now, FireAt: now.Add(time.Hour)}); err == nil {
				t.Fatal("allowed ninth timer")
			}
		}
	}
	now := time.Now()
	if _, err := b.timers.set(ctx, savedTimer{ID: testHex(t, 12), CompanionID: owner, CreatedAt: now, FireAt: now.Add(time.Hour)}); err == nil {
		t.Fatal("allowed 33rd timer")
	}
}

// Saying "tea" twice inside two minutes is one timer; distinct labels or a
// genuinely different time still create separate timers.
func TestTimerDuplicateSetUpdatesExisting(t *testing.T) {
	t.Parallel()
	b, base := wsHarness(t, func(b *larkServer, _ string) {
		b.timers = newTimerManager(b)
		t.Cleanup(b.timers.close)
	})
	const id = "cake"
	if _, err := b.createCompanion(t.Context(), object{"id": id, "name": "Cake", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	first := patchTimerTest(t, base, object{"companion_id": id, "label": "tea", "in_seconds": 60}, false)
	second := patchTimerTest(t, base, object{"companion_id": id, "label": " Tea ", "in_seconds": 90}, false)
	if first["id"] != second["id"] {
		t.Fatalf("near duplicate created a second timer: %v then %v", first, second)
	}
	state := toolTest(t, base, "state", object{"companion_id": id, "include": []string{"timers"}}, false)["timers"].([]any)
	if len(state) != 1 || state[0].(object)["id"] != first["id"] || state[0].(object)["remaining_s"] != json.Number("90") {
		t.Fatalf("duplicate timer state = %v", state)
	}
	patchTimerTest(t, base, object{"companion_id": id, "label": "coffee", "in_seconds": 90}, false)
	far := patchTimerTest(t, base, object{"companion_id": id, "label": "tea", "in_seconds": 3600}, false)
	if far["id"] == first["id"] {
		t.Fatal("far-apart tea timer was merged")
	}
	state = toolTest(t, base, "state", object{"companion_id": id, "include": []string{"timers"}}, false)["timers"].([]any)
	if len(state) != 3 {
		t.Fatalf("distinct timers = %v, want three", state)
	}
}
