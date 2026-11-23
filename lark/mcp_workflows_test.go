package main

import (
	"testing"
	"time"
)

func TestMCPTimerUpdatePreservesIdentity(t *testing.T) {
	t.Parallel()
	b, base := wsHarness(t, func(b *larkServer, _ string) {
		b.timers = newTimerManager(b)
		t.Cleanup(b.timers.close)
	})
	const id = "cake"
	board := testConnectWS(t, base, "unit", false)
	board.helloV2(t, "vocat-0001", "Cake")
	timer := patchTimerTest(t, base, object{"companion_id": id, "in_seconds": 60, "label": "First", "chime": "tap", "say": "Ready", "notes": "C5:150"}, false)
	timerID := str(timer["id"])
	b.timers.mu.Lock()
	old := b.timers.handles[timerID]
	b.timers.mu.Unlock()
	got := patchTimerTest(t, base, object{"companion_id": id, "id": timerID, "op": "update", "in_seconds": 120, "label": "Later", "say": "", "notes": []string{}}, false)
	if got["fire_at"] == timer["fire_at"] || got["created_at"] != timer["created_at"] || got["id"] != timerID ||
		got["label"] != "Later" || got["chime"] != "tap" || got["say"] != "" || len(got["notes"].([]any)) != 0 {
		t.Fatal(got)
	}
	if _, claimed := b.timers.claim(timerID, old); claimed {
		t.Fatal("a stale callback claimed the rescheduled timer")
	}
	patchTimerTest(t, base, object{"companion_id": "another-cat", "id": timerID, "op": "update", "label": "Wrong owner"}, true)
	read := toolTest(t, base, "state", object{"companion_id": id, "include": []string{"timers"}}, false)["timers"].([]any)
	if len(read) != 1 || read[0].(object)["label"] != "Later" || read[0].(object)["created_at"] != timer["created_at"] {
		t.Fatal(read)
	}
}

func TestMCPHistoryCursor(t *testing.T) {
	t.Parallel()
	b, base := wsHarness(t, nil)
	const id = "cake"
	board := testConnectWS(t, base, "unit", false)
	board.helloV2(t, "vocat-0002", "Cake")
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, msg := range []string{"same-a", "same-b", "same-c"} {
		b.history.add(id, chatEntry{ID: msg, Sender: "User", Role: "user", Text: msg, Source: "typed", Timestamp: now})
	}
	first := toolTest(t, base, "state", object{"companion_id": id, "include": []string{"messages"}, "messages_limit": 2}, false)
	list := first["messages"].([]any)
	if len(list) != 2 || list[0].(object)["id"] != "same-b" || list[1].(object)["id"] != "same-c" || first["messages_has_more"] != true {
		t.Fatal(first)
	}
	next := toolTest(t, base, "state", object{"companion_id": id, "include": []string{"messages"},
		"messages_limit": 2, "messages_cursor": first["messages_next_cursor"]}, false)
	list = next["messages"].([]any)
	if len(list) != 1 || list[0].(object)["id"] != "same-a" || next["messages_has_more"] != false || next["messages_next_cursor"] != nil {
		t.Fatal(next)
	}
	toolTest(t, base, "state", object{"companion_id": id, "include": []string{"messages"},
		"messages_cursor": object{"before_ts": now.UnixMilli()}}, true)
}

func TestMCPEmitReceipts(t *testing.T) {
	t.Parallel()
	b, base := wsHarness(t, nil)
	cake := testConnectWS(t, base, "unit", false)
	cake.hello(t, "vocat-0001", "Cake")
	mint := testConnectWS(t, base, "unit", false)
	mint.hello(t, "vocat-0002", "Mint")
	const speaker = "speaker"
	board := testConnectWS(t, base, "unit", false)
	board.helloV2(t, "vocat-0003", "Speaker")
	offline := toolTest(t, base, "emit", object{"to": "everyone", "sfx": "pop"}, false)
	if rs := offline["receipts"].([]any); offline["played"] != false || len(rs) != 1 || rs[0].(object)["reason"] != "not_connected" {
		t.Fatal(offline)
	}
	result := toolTest(t, base, "emit", object{"to": "all", "chime": "tap", "intercom": object{"from": "cake", "text": "Hello"}}, false)
	check := map[string]string{}
	for _, item := range result["receipts"].([]any) {
		r := item.(object)
		check[str(r["action"])+"/"+str(r["companion_id"])] = str(r["status"])
	}
	if result["played"] != true || check["chime/"+speaker] != "queued" || check["chime/mint"] != "queued" ||
		check["intercom/"+speaker] != "queued" || check["intercom/mint"] != "queued" {
		t.Fatal(result)
	}
	_ = b
}
