package main

import (
	"encoding/json"
	"strings"
	"testing"

	"lark/internal/stories"
)

const sampleBranchingTwee = `:: StoryTitle
The Whispering Woods

:: StoryData
{"ifid":"12345678-1234-4123-8123-1234567890ab","start":"Woods"}

:: Woods {"bg":"misty pine forest"}
You stand at a fork in the trail.
<<set $visited to true>><<set $coins to 2>>
[[Take the mossy path->Moss]]
[[Follow the lantern|Lantern]]
[[Wait]]

:: Moss
A tiny frog offers you a key.
<<set $has_key to true>>
[[Go to the gate->Gate]]

:: Lantern
The lantern warms your hands.
[[Go to the gate->Gate]]

:: Wait
You hear an owl hoot.
[[Follow the lantern|Lantern]]

:: Gate {"end":true}
<<if $has_key>>The gate clicks open to a sunlit garden!<<else>>The gate is locked, so you rest under the stars.<<endif>>
`

func TestStoryMCPAndProgressRoundTrip(t *testing.T) {
	t.Parallel()
	b, base := wsHarness(t, nil)
	out := toolTest(t, base, "story", object{
		"op":     "create",
		"id":     "whispering-woods",
		"twee":   sampleBranchingTwee,
		"author": "Greg",
	}, false)
	if out["id"] != "whispering-woods" || out["passages"] != json.Number("5") {
		t.Fatalf("unexpected create output: %v", out)
	}

	got := toolTest(t, base, "story", object{"op": "get", "id": "whispering-woods"}, false)
	if got["title"] != "The Whispering Woods" {
		t.Fatalf("unexpected get output: %v", got)
	}

	snap := toolTest(t, base, "state", object{"include": []string{"stories"}}, false)
	if snap["stories"].(object)["whispering-woods"].(object)["title"] != "The Whispering Woods" {
		t.Fatalf("unexpected state stories: %v", snap)
	}

	// Progress round-trip.
	wantProg := stories.Progress{
		CompanionID: "cake",
		StoryID:     "whispering-woods",
		Passage:     "Moss",
		Page:        1,
		Flags:       map[string]stories.Value{"visited": {Bool: true}, "coins": {IsNum: true, Num: 2}},
	}
	if err := b.setStoryProgress(t.Context(), wantProg); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := b.getStoryProgress(t.Context(), "cake", "whispering-woods")
	if err != nil || !ok || loaded.Passage != "Moss" || loaded.Page != 1 || !loaded.Flags["visited"].Truthy() || loaded.Flags["coins"].Num != 2 {
		t.Fatalf("progress mismatch: ok=%v err=%v got=%+v", ok, err, loaded)
	}

	// Update and delete.
	updatedTwee := strings.Replace(sampleBranchingTwee, "The Whispering Woods", "Whispering Woods II", 1)
	upd := toolTest(t, base, "story", object{"op": "update", "id": "whispering-woods", "twee": updatedTwee}, false)
	if upd["title"] != "Whispering Woods II" {
		t.Fatalf("unexpected update: %v", upd)
	}
	del := toolTest(t, base, "story", object{"op": "delete", "id": "whispering-woods"}, false)
	if del["deleted"] != true {
		t.Fatalf("unexpected delete: %v", del)
	}
	if _, ok, _ := b.getStoryProgress(t.Context(), "cake", "whispering-woods"); ok {
		t.Fatal("story delete did not clean up story_progress")
	}
}
