package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestAppTemplatesHitCoordinates(t *testing.T) {
	for _, geometry := range []screenGeometry{roundScreen, {320, 240, "rect"}} {
		for _, tc := range []struct {
			template, data, action, id, expected string
			x, y                                 int
		}{
			{"reader", `{"pages":["First","Second"]}`, "tap", "next", "Second", 236, 62},
			{"quiz", `{}`, "tap", "answer-1", "Correct!", 240, 200},
			{"spot-the-difference", `{}`, "tap", "pair-2", "Found them all!", 123, 181},
			{"drawing", `{}`, "drag", "canvas", `stroke-width="6"`, 180, 130},
			{"pet", `{"name":"Sparky"}`, "tap", "feed", "Yum!", 100, 252},
			{"timer", `{"seconds":2}`, "tap", "start", "Counting down", 180, 250},
			{"picture-math", `{"problems":[{"left":2,"right":3}]}`, "tap", "answer-0", "Correct!", 108, 203},
			{"memory-match", `{}`, "tap", "card-0", "", 100, 127},
			{"fireworks", `{}`, "tap", "sky", "", 180, 130},
		} {
			t.Run(fmt.Sprintf("%dx%d/%s", geometry.width, geometry.height, tc.template), func(t *testing.T) {
				data, err := decodeObject([]byte(tc.data))
				if err != nil {
					t.Fatal(err)
				}
				var spec savedApp
				if err := spec.setTemplate(tc.template, data); err != nil {
					t.Fatal(err)
				}
				d := &wsTerminal{screen: geometry, client: &wsClient{role: "device"}}
				a, views, err := initApp(spec, []appPlayer{{id: "board"}}, nil, []*wsTerminal{d}, nil, buildScreen)
				if err != nil {
					t.Fatal(err)
				}
				inverse := func(x, y int) (int, int) {
					side := min(geometry.width, geometry.height)
					return geometry.inverseTap((geometry.width-side)/2+x*side/360, (geometry.height-side)/2+y*side/360)
				}
				check := func(view svgScreen) {
					t.Helper()
					if !strings.Contains(view.source, `data-sticker="`) {
						t.Fatal("missing Storybook art")
					}
					if warnings := appLayoutWarnings([]svgScreen{view}, geometry.shape == "round"); len(warnings) != 0 {
						t.Fatal(warnings)
					}
					x, y := inverse(180, 320)
					if got := hitID(view.hits, x, y); got != exitNode {
						t.Fatalf("Home hit %q", got)
					}
					if !strings.Contains(view.source, `aria-label="Home"`) {
						t.Fatal("injected exit is not labeled Home")
					}
					for i, h := range view.hits {
						if !inScreenCircle(h.x1, h.y1) || !inScreenCircle(h.x2, h.y1) ||
							!inScreenCircle(h.x1, h.y2) || !inScreenCircle(h.x2, h.y2) {
							t.Errorf("padded target clips: %v", h)
						}
						for _, other := range view.hits[:i] {
							if h.x1 <= other.x2 && other.x1 <= h.x2 && h.y1 <= other.y2 && other.y1 <= h.y2 {
								t.Errorf("padded targets overlap: %q and %q", h.id, other.id)
							}
						}
					}
				}
				view := views[0]
				check(view)
				click := func(kind, expected string, x, y int) {
					t.Helper()
					x, y = inverse(x, y)
					id := hitID(view.hits, x, y)
					if id != expected {
						t.Fatalf("%s at %d,%d: want %q, got %q", kind, x, y, expected, id)
					}
					views, err := a.update(appEvent{kind: kind, i: 0, id: id, x: x, y: y}, nil)
					if err != nil {
						t.Fatal(err)
					}
					view = views[0]
					check(view)
				}
				if tc.template == "drawing" {
					click("tap", "ink-1", 124, 260)
					click("drag", "canvas", 150, 130)
				}
				bursts := strings.Count(view.raw, `data-sticker="star"`)
				click(tc.action, tc.id, tc.x, tc.y)
				if tc.template == "fireworks" && strings.Count(view.raw, `data-sticker="star"`) != bursts+1 {
					t.Fatal("tap did not add a Storybook burst")
				}
				if tc.expected != "" && !strings.Contains(view.raw, tc.expected) {
					t.Fatalf("after input expected %q in %s", tc.expected, view.raw)
				}
				if tc.template == "memory-match" {
					var s struct{ Deck []string }
					raw, err := a.stateJSON()
					if err != nil || json.Unmarshal(raw, &s) != nil || !strings.Contains(view.raw, ">"+s.Deck[0]+"<") {
						t.Fatalf("did not reveal tapped card: %s, %s", raw, view.raw)
					}
					for i, card := range s.Deck[1:] {
						if card == s.Deck[0] {
							j := i + 1
							click("tap", fmt.Sprintf("card-%d", j), 100+j%3*77, 127+j/3*50)
							break
						}
					}
				}
			})
		}
	}
}

func TestAppTemplateSaveAndMemoryRefresh(t *testing.T) {
	_, base := wsHarness(t, nil)
	const id = "lark-template"
	board := testConnectWS(t, base, "unit", false)
	board.helloV2(t, id, "Reader")
	out := toolTest(t, base, "app", object{"companions": "reader", "template": "reader", "save_as": "story",
		"data": object{"title": "Story", "pages": []string{"A frog", "The end"}}}, false)
	saved := out["saved"].(object)
	if saved["name"] != "story" || saved["title"] != "Story" || saved["template"] != "reader" || saved["version"] == nil || out["layout_warnings"] != nil {
		t.Fatal(out)
	}
	board.until(t, isDisplay)
	board.json(t, object{"ev": "tap", "x": 236, "y": 62})
	board.until(t, isDisplay)
	if a := stateMe(t, base, "reader")["active_app"].(object); a["state"].(object)["page"] != json.Number("1") {
		t.Fatal(a)
	}
	board.json(t, object{"ev": "tap", "x": 180, "y": 320})
	board.until(t, isDisplay)
	if a := stateMe(t, base, "reader")["active_app"]; a != nil {
		t.Fatal(a)
	}
	patch := toolTest(t, base, "patch", object{"memory": object{"scope": "app:story", "key": "content", "expected_version": 0,
		"value": object{"pages": []string{"A fox"}}}}, false)
	if patch["memory"].(object)["app:story"].(object)["content"].(object)["version"] != json.Number("1") {
		t.Fatal(patch)
	}
	reloaded := toolTest(t, base, "app", object{"companions": "reader", "load": "story"}, false)
	if hits := reloaded["hit_ids"].([]any); len(hits) != 1 || hits[0] != exitNode {
		t.Fatal(reloaded)
	}
	toolTest(t, base, "app", object{"companions": "reader", "template": "quiz",
		"data": object{"questions": []object{{"prompt": "Q", "choices": []string{"A", "B"}, "answer": 0, "typo": true}}}}, true)
}

func TestAppTemplateCountdownRearmsAfterFinish(t *testing.T) {
	_, base := wsHarness(t, nil)
	const id = "lark-countdown"
	board := testConnectWS(t, base, "unit", false)
	board.helloV2(t, id, "Cake")
	toolTest(t, base, "app", object{"companions": "cake", "template": "timer", "data": object{"seconds": 1}}, false)
	board.until(t, isDisplay)
	for range 2 {
		a := stateMe(t, base, "cake")["active_app"].(object)
		if a["ticks_left"] != json.Number("0") {
			t.Fatal("idle timer scheduled ticks", a)
		}
		board.json(t, object{"ev": "tap", "x": 180, "y": 250})
		board.until(t, hasWS("chime", `"kind":"intercom"`)) // Celebrate at zero.
		board.until(t, isDisplay)
		a = stateMe(t, base, "cake")["active_app"].(object)
		s := a["state"].(object)
		if s["left"] != json.Number("0") || s["end"] != json.Number("0") || a["ticks_left"] != json.Number("0") {
			t.Fatal(a)
		}
	}
}
