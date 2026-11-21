package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestMCPTimerUpdatePreservesIdentity(t *testing.T) {
	b, base := wsHarness(t, func(b *bridge, _ string) {
		b.timers = newTimerManager(b)
		t.Cleanup(b.timers.close)
	})
	const id = "lark-timer-edit"
	board := testConnectWS(t, base, "unit", false)
	board.helloV2(t, id, "Cake")
	timer := patchTimerTest(t, base, object{"device_id": id, "in_seconds": 60, "label": "First", "chime": "tap", "say": "Ready", "notes": "C5:150"}, false)
	timerID := str(timer["id"])
	b.timers.mu.Lock()
	old := b.timers.handles[timerID]
	b.timers.mu.Unlock()
	got := patchTimerTest(t, base, object{"device_id": id, "id": timerID, "op": "update", "in_seconds": 120, "label": "Later", "say": "", "notes": []string{}}, false)
	if got["fire_at"] == timer["fire_at"] || got["created_at"] != timer["created_at"] || got["id"] != timerID ||
		got["label"] != "Later" || got["chime"] != "tap" || got["say"] != "" || len(got["notes"].([]any)) != 0 {
		t.Fatal(got)
	}
	if _, claimed := b.timers.claim(timerID, old); claimed {
		t.Fatal("a stale callback claimed the rescheduled timer")
	}
	patchTimerTest(t, base, object{"device_id": "another-cat", "id": timerID, "op": "update", "label": "Wrong owner"}, true)
	patchTimerTest(t, base, object{"device_id": id, "id": timerID, "op": "update", "app": "missing"}, true)
	read := toolTest(t, base, "state", object{"device_id": id, "include": []string{"timers"}}, false)["timers"].([]any)
	if len(read) != 1 || read[0].(object)["label"] != "Later" || read[0].(object)["created_at"] != timer["created_at"] {
		t.Fatal(read)
	}
}

func TestMCPHistoryCursor(t *testing.T) {
	b, base := wsHarness(t, nil)
	const id = "lark-history-page"
	board := testConnectWS(t, base, "unit", false)
	board.helloV2(t, id, "Cake")
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, msg := range []string{"same-a", "same-b", "same-c"} {
		b.history.add(id, chatEntry{ID: msg, Sender: "User", Role: "user", Text: msg, Source: "typed", Timestamp: now})
	}
	first := toolTest(t, base, "state", object{"device_id": id, "include": []string{"messages"}, "messages_limit": 2}, false)
	list := first["messages"].([]any)
	if len(list) != 2 || list[0].(object)["id"] != "same-b" || list[1].(object)["id"] != "same-c" || first["messages_has_more"] != true {
		t.Fatal(first)
	}
	next := toolTest(t, base, "state", object{"device_id": id, "include": []string{"messages"},
		"messages_limit": 2, "messages_cursor": first["messages_next_cursor"]}, false)
	list = next["messages"].([]any)
	if len(list) != 1 || list[0].(object)["id"] != "same-a" || next["messages_has_more"] != false || next["messages_next_cursor"] != nil {
		t.Fatal(next)
	}
	toolTest(t, base, "state", object{"device_id": id, "include": []string{"messages"},
		"messages_cursor": object{"before_ts": now.UnixMilli()}}, true)
}

func TestMCPCallStatusHangupAndEmitReceipts(t *testing.T) {
	b, base := wsHarness(t, nil)
	cake := callClient(t, b, base, callCake, "Cake")
	mint := callClient(t, b, base, callMint, "Mint")
	const speaker = "lark-speaker"
	board := testConnectWS(t, base, "unit", false)
	board.helloV2(t, speaker, "Speaker")
	device := func(id string) object { return stateMe(t, base, id)["device"].(object) }
	start := toolTest(t, base, "emit", object{"to": callMint, "call": object{"from": callCake}}, false)
	call := start["call"].(object)
	callID := call["id"].(json.Number)
	if d := device(callMint); d["can_call"] != true || d["call"].(object)["peer_device_id"] != callCake ||
		d["call"].(object)["direction"] != "incoming" || d["call"].(object)["state"] != "ringing" {
		t.Fatal(d)
	}
	mint.until(t, hasWS("svg", "call-answer"))
	tapCallControl(t, mint, "call-answer")
	mint.until(t, hasWS("call", `"state":"start"`))
	if c := device(callCake)["call"].(object); c["id"] != callID || c["state"] != "connected" || c["started_at"] == nil {
		t.Fatal(c)
	}
	suppressed := toolTest(t, base, "emit", object{"to": callMint, "notes": "C5:150"}, false)
	if r := suppressed["receipts"].([]any)[0].(object); suppressed["played"] != false || r["status"] != "skipped" || r["reason"] != "on_call" {
		t.Fatal(suppressed)
	}
	offline := toolTest(t, base, "emit", object{"to": "everyone", "sfx": "pop"}, false)
	if rs := offline["receipts"].([]any); offline["played"] != false || len(rs) != 1 || rs[0].(object)["reason"] != "not_connected" {
		t.Fatal(offline)
	}
	result := toolTest(t, base, "emit", object{"to": "all", "chime": "tap", "intercom": object{"from": callCake, "text": "Hello"}}, false)
	check := map[string]string{}
	for _, item := range result["receipts"].([]any) {
		r := item.(object)
		check[str(r["action"])+"/"+str(r["device_id"])] = str(r["status"])
	}
	if result["played"] != true || check["chime/"+speaker] != "queued" || check["chime/"+callMint] != "skipped" ||
		check["intercom/"+speaker] != "queued" || check["intercom/"+callMint] != "skipped" {
		t.Fatal(result)
	}
	if result := toolTest(t, base, "emit", object{"to": callMint, "call": object{"op": "hangup", "id": 42}}, false); result["call"] != nil ||
		result["receipts"].([]any)[0].(object)["reason"] != "call_changed" {
		t.Fatal(result)
	}
	hangup := toolTest(t, base, "emit", object{"to": callMint, "call": object{"op": "hangup", "id": callID}}, false)
	if c := hangup["call"].(object); c["state"] != "ended" || c["id"] != callID || len(hangup["receipts"].([]any)) != 2 {
		t.Fatal(hangup)
	}
	cake.until(t, hasWS("call", `"state":"end"`))
	mint.until(t, hasWS("call", `"state":"end"`))
	if device(callCake)["call"] != nil || device(callMint)["call"] != nil {
		t.Fatal("hangup left one participant in the call")
	}
}

func TestMCPFacePartEditsAndFullSVGFallback(t *testing.T) {
	b, base := wsHarness(t, nil)
	const id = "lark-face-parts"
	c := blankCompanion()
	c.Name, c.Configured = "Cake", true
	c.FaceSVG = defaultFaceArtwork(c.FaceBG, c.MarkColor, c.AccentColor, c.Configured, true)
	tx, err := b.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = writeCompanion(t.Context(), tx, id, c); err == nil {
		err = tx.Commit()
	}
	if err != nil {
		t.Fatal(err)
	}
	board := testConnectWS(t, base, "unit", false)
	board.helloV2(t, id, "Cake")
	patch := func(edit object, fail bool) object {
		return toolTest(t, base, "patch", object{"device_id": id, "companion": edit}, fail)
	}
	replaced := []object{{"part": "eyes", "pose": "talk", "svg": `<circle cx="112" cy="141" r="11" fill="#ABCDEF"/>`}}
	out := patch(object{"face_edits": replaced}, false)
	raw := str(out["companion"].(object)["face_svg"])
	layer := func(pose string) string {
		t.Helper()
		root, err := parseFaceSVG(raw)
		if err != nil {
			t.Fatal(err)
		}
		return svgString(faceLayer(root, pose))
	}
	talk, idle := layer("talk"), layer("idle")
	if !strings.Contains(talk, "#ABCDEF") || strings.Contains(talk, "M93 149") || !strings.Contains(talk, "F47E92") ||
		strings.Contains(idle, "#ABCDEF") || !strings.Contains(idle, `x="95"`) || !strings.Contains(raw, `data-part="forehead"`) {
		t.Fatalf("face edit was not confined to talking eyes: idle=%s talk=%s", idle, talk)
	}
	patch(object{"face_edits": []object{{"part": "nose", "svg": "", "typo": true}}}, true)
	patch(object{"face_svg": `<svg><rect width="360" height="360" fill="#224466"/></svg>`}, false)
	errOut := patch(object{"face_edits": replaced}, true)
	if !strings.Contains(str(errOut["error"]), "complete face_svg") {
		t.Fatal(errOut)
	}
}
