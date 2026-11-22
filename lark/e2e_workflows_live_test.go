//go:build live

package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

type e2eTemplate struct {
	name, data, launch, after string
	x, y                      int
}

var e2eTemplates = []e2eTemplate{
	{"reader", `{"pages":["First page","Second page"]}`, "First page", "Second page", 236, 62},
	{"quiz", `{}`, "answer-1", "Correct!", 240, 200},
	{"spot-the-difference", `{}`, "pair-2", "Found them all!", 123, 181},
	{"drawing", `{}`, "ink-1", `stroke-width=\"6\"`, 180, 130},
	{"pet", `{"name":"Sparky"}`, "feed", "Yum!", 100, 252},
	{"timer", `{"seconds":2}`, "start", "Counting down", 180, 250},
	{"picture-math", `{"problems":[{"left":2,"right":3}]}`, "answer-0", "Correct!", 108, 203},
	{"memory-match", `{}`, "card-0", "", 100, 127},
	{"fireworks", `{}`, "sky", "", 180, 130},
}

func (s *e2eSuite) activeApp(t *testing.T, c *e2eCompanion) object {
	t.Helper()
	out := s.tool(t, "state", object{"device_id": c.id, "include": []string{"devices"}}, false)
	me, _ := out["me"].(object)
	active, _ := me["active_app"].(object)
	return active
}

func e2eAppResult(t *testing.T, out object) {
	t.Helper()
	if out["ok"] != true || out["layout_warnings"] != nil {
		t.Fatal("app launch not accepted with zero layout warnings")
	}
}

func (s *e2eSuite) apps(t *testing.T) {
	t.Run("SavedAppsAndNativeInput", func(t *testing.T) {
		for _, g := range []struct{ variant, label string }{{"esp-vocat", "AppsRound"}, {"esp-box-3", "AppsRect"}} {
			t.Run(g.label, func(t *testing.T) {
				c := s.board(t, g.variant, g.label)
				c.mirrorOn(t, s)
				for _, tc := range e2eTemplates {
					t.Run(tc.name, func(t *testing.T) {
						data, _ := decodeObject([]byte(tc.data))
						args := object{"devices": c.id, "template": tc.name, "data": data, "save_as": "e2e-" + tc.name}
						if g.variant == "esp-vocat" {
							e2eAppResult(t, s.tool(t, "app", args, false))
							c.scene(t, tc.launch)
							c.input(t, "tap", 180, 320, c.revision)
							c.scene(t, "companion-ring")
						}
						e2eAppResult(t, s.tool(t, "app", object{"devices": c.id, "load": "e2e-" + tc.name}, false))
						c.scene(t, tc.launch)
						base := slices.Clone(c.pixels)
						if tc.name == "drawing" {
							c.input(t, "tap", 124, 260, c.revision)
							c.op(t, "chime", "kind", `"tap"`)
							c.input(t, "drag", 150, 130, c.revision)
							c.scene(t, `r=\"3\"`)
							c.input(t, "drag", tc.x, tc.y, c.revision)
						} else {
							c.input(t, "tap", tc.x, tc.y, c.revision)
						}
						if tc.name == "fireworks" {
							// Its pop is an audio effect, not a firmware chime.
							c.ack(t, c.op(t, "tts", "state", `"end"`))
							bursts := strings.Count(c.svg, `data-sticker="star"`)
							c.sceneWhere(t, "new Storybook burst", func(f wsFrame) bool {
								return strings.Count(string(f.data), `data-sticker=\"star\"`) > bursts
							})
						} else if tc.after != "" {
							c.scene(t, tc.after)
						} else {
							c.expect(t, "native card flips", isDisplay)
						}
						if slices.Equal(c.pixels, base) {
							t.Fatal("native hit did not change decoded pixels")
						}
						if tc.name == "drawing" {
							c.input(t, "up", tc.x, tc.y, c.revision)
						}
						c.input(t, "tap", 180, 320, c.revision)
						c.scene(t, "companion-ring")
						if s.activeApp(t, c) != nil {
							t.Fatal("Home did not exit saved app")
						}
					})
				}
			})
		}
	})
	t.Run("SaveOnlyRenameDelete", func(t *testing.T) {
		c := s.browser(t, "EditApp")
		e2eAppResult(t, s.tool(t, "app", object{"devices": c.id, "load": "e2e-reader"}, false))
		c.scene(t, "First page")
		c.input(t, "tap", 236, 62, 0)
		c.scene(t, "Second page")
		saved, _ := s.tool(t, "app", object{"devices": c.id, "save_current": object{"name": "e2e-copy", "title": "Copy"}}, false)["saved"].(object)
		if saved["template"] != "reader" || saved["version"] == nil {
			t.Fatal("saving the active app did not preserve the template or expose a version")
		}
		patch := s.tool(t, "patch", object{"apps": object{"update": object{"name": "e2e-copy", "expected_version": saved["version"],
			"data": object{"pages": []string{"Updated page"}}}}}, false)
		if _, ok := patch["apps"].(object); !ok {
			t.Fatal("save-only update receipt missing")
		}
		active := s.activeApp(t, c)
		state, _ := active["state"].(object)
		if e2eNumber(state["page"]) != 1 {
			t.Fatal("saving or updating reset the active app")
		}
		s.tool(t, "patch", object{"apps": object{"rename": []object{{"from": "e2e-copy", "to": "e2e-renamed", "title": "Renamed copy"}}}}, false)
		s.tool(t, "app", object{"devices": c.id, "load": "e2e-copy"}, true)
		e2eAppResult(t, s.tool(t, "app", object{"devices": c.id, "load": "e2e-renamed"}, false))
		c.scene(t, "Updated page")
		c.tapScene(t, exitNode, "companion-ring")
		s.tool(t, "patch", object{"apps": object{"delete": []string{"e2e-renamed"}}}, false)
		s.tool(t, "app", object{"devices": c.id, "load": "e2e-renamed"}, true)
	})
	if s.real {
		t.Run("RealModelMCP", func(t *testing.T) {
			c := s.browser(t, "ModelApps")
			c.timeout = 70 * time.Second
			var calls []string
			for _, tc := range e2eTemplates {
				calls = append(calls, fmt.Sprintf("await tools.%s.app({devices:%q,template:%q,data:%s})", s.model.alias, c.id, tc.name, tc.data))
			}
			prompt := "Screen regression. In one Code Mode execute call, run the following calls sequentially. Return each result, then say one short sentence. Do not call any other tool:\nreturn [" +
				strings.Join(calls, ",\n") + "];"
			c.text(t, prompt)
			c.playback(t, time.Now())
			s.model.mu.Lock()
			launched := slices.Clone(s.model.templates)
			audit := slices.Clone(s.model.mcpAudit)
			s.model.mu.Unlock()
			if len(launched) < len(e2eTemplates) {
				t.Logf("isolated MCP results: %v", audit)
			}
			for _, tc := range e2eTemplates {
				if !slices.Contains(launched, tc.name) {
					t.Errorf("real model did not launch %s with zero layout warnings", tc.name)
				}
			}
		})
	}
}

func (s *e2eSuite) policy(t *testing.T, cat *e2eCompanion, args object, reason string) {
	t.Helper()
	row := testObject(t, e2eRequest(t, s.liveClient, "POST", "/api/devices/"+cat.id+"/parent", s.parent, args, 200))
	if row["reason"] != reason {
		t.Fatalf("parent API reason %q, want %q", row["reason"], reason)
	}
	cat.op(t, "parent_policy", "reason", fmt.Sprintf("%q", reason))
}

func (s *e2eSuite) restricted(t *testing.T, c, peer *e2eCompanion, isolatedPrompts bool) {
	t.Helper()
	prompts := s.model.prompts.Load()
	c.recStart(t, "")
	c.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpCancel
	})
	c.text(t, "Make me a dinosaur quiz.")
	s.request(t, "POST", "/api/chat", object{"device_id": c.id, "text": "Make me a dinosaur quiz."}, 423)
	call := s.tool(t, "emit", object{"to": peer.id, "call": object{"from": c.id}}, false)
	receipts, _ := call["receipts"].([]any)
	if len(receipts) != 1 || receipts[0].(object)["status"] != "skipped" {
		t.Fatal("parent policy did not block the call")
	}
	s.tool(t, "emit", object{"to": peer.id, "intercom": object{"from": c.id, "text": "Locked intercom"}}, true)
	c.syncFrames(t)
	if isolatedPrompts && s.model.prompts.Load() != prompts {
		t.Fatal("parent controls let a blocked Talk or build reach the model")
	}
}

func (s *e2eSuite) builderParents(t *testing.T) {
	c, peer := s.browser(t, "Builder"), s.browser(t, "BuilderPeer")
	now := time.Now().UTC()
	settings := object{"action": "settings", "timezone": "UTC", "quiet_start": now.Add(-time.Hour).Format("15:04"),
		"quiet_end": now.Add(time.Hour).Format("15:04"), "daily_minutes": 0}
	s.policy(t, c, settings, string(parentQuiet))
	s.restricted(t, c, peer, true)
	s.policy(t, c, object{"action": "unlock", "minutes": 2}, "")
	c.scene(t, "companion-ring")

	start := time.Now()
	c.text(t, "Make me a dinosaur quiz. Use the quiz template with one simple question.")
	c.playback(t, start)
	if !strings.Contains(c.svg, "Cancel build") {
		c.scene(t, "Cancel build")
	}
	s.policy(t, c, object{"action": "pause", "minutes": 2}, string(parentPause))
	s.restricted(t, c, peer, true)
	if !s.real {
		close(s.model.build)
	}
	// A built artifact is available in the library, but announcement is deferred.
	deadline := time.Now().Add(50 * time.Second)
	for {
		v := testObject(t, s.request(t, "GET", "/api/apps", nil, 200))
		apps, _ := v["apps"].([]any)
		if slices.ContainsFunc(apps, func(raw any) bool {
			app, _ := raw.(object)
			return app["template"] == "quiz" && strings.Contains(strings.ToLower(str(app["name"])), "dino")
		}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background job did not save the requested quiz")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if slices.ContainsFunc(s.history(t, c.id, ""), func(row chatEntry) bool { return row.SceneTitle == "Ready" }) {
		t.Fatal("Ready was announced during parent pause")
	}
	s.policy(t, c, object{"action": "unlock", "minutes": 2}, "")
	c.playback(t, start)
	if !strings.Contains(c.svg, "build-done") {
		c.scene(t, "build-done")
	}
	if !slices.ContainsFunc(s.history(t, c.id, ""), func(row chatEntry) bool {
		return row.SceneTitle == "Ready" && strings.Contains(strings.ToLower(row.Text), "ready")
	}) {
		t.Fatal("idle Ready card missing from canonical history")
	}
	c.tap(t, "build-done")
	t.Logf("build request → unlocked Ready %s", time.Since(start).Round(time.Millisecond))
	s.policy(t, c, object{"action": "end_override"}, string(parentQuiet))
}

func (s *e2eSuite) parentBudget(t *testing.T) {
	a, b := s.browser(t, "Budget"), s.browser(t, "BudgetPeer")
	a.timeout, b.timeout = 67*time.Second, 67*time.Second
	args := object{"action": "settings", "timezone": "UTC", "quiet_start": "", "quiet_end": "", "daily_minutes": 1}
	row := testObject(t, e2eRequest(t, s.liveClient, "POST", "/api/devices/"+a.id+"/parent", s.parent, args, 200))
	if row["reason"] != "" {
		t.Fatal("fresh daily limit should allow a call")
	}
	e2eRing(t, a, b)
	b.tap(t, "call-answer")
	start := time.Now()
	e2eStartCall(t, a, b)
	a.op(t, "parent_policy", "reason", `"daily_limit"`)
	b.op(t, "call", "state", `"end"`, "reason", `"Companion is resting"`)
	if elapsed := time.Since(start); elapsed < 58*time.Second || elapsed > 65*time.Second {
		t.Fatalf("daily minute limit fired at %s", elapsed)
	} else {
		t.Logf("connected call → daily limit %s", elapsed.Round(time.Millisecond))
	}
	s.restricted(t, a, b, false)
}

func (s *e2eSuite) data(t *testing.T) {
	c := s.browser(t, "Data")
	timerList := "/api/timers?device_id=" + c.id
	timer := func(in object) object {
		t.Helper()
		out := s.tool(t, "patch", object{"device_id": c.id, "timer": in}, false)
		value, _ := out["timer"].(object)
		return value
	}
	start := time.Now()
	firing := timer(object{"in_seconds": 1, "label": "Blue kite", "say": "Time to fly your kite."})
	if firing["id"] == nil {
		t.Fatal("timer create receipt is missing ID")
	}
	c.op(t, "chime", "kind", `"intercom"`)
	c.playback(t, start)
	if !strings.Contains(c.svg, "Blue kite") {
		c.scene(t, "Blue kite")
	}
	timers := testObject(t, s.request(t, "GET", timerList, nil, 200))
	if len(timers["timers"].([]any)) != 0 {
		t.Fatal("fired timer remained scheduled")
	}
	c.tapScene(t, "companion", "companion-ring")

	patch := s.tool(t, "patch", object{"memory": object{"scope": c.id, "key": "kite", "value": "My kite is bright blue."}}, false)
	if patch["memory"] == nil {
		t.Fatal("memory write receipt is missing")
	}
	c.tapScene(t, "menu", "menu-next")
	c.tapScene(t, "menu-next", "menu-memories")
	c.tapScene(t, "menu-memories", "menu-row-0")
	c.tapScene(t, "menu-row-0", "My kite is bright blue.")
	c.tapScene(t, "companion", "companion-ring")

	// A fired timer stores a card and spoken line. Page by the public compound
	// cursor rather than mocking older messages into the database.
	p1 := s.historyPage(t, c.id, nil)
	if !p1.HasMore || p1.Next == nil || len(p1.Messages) != 1 || p1.Messages[0].Source != "timer" {
		t.Fatal("expected a timer event and non-empty continuation cursor")
	}
	p2 := s.historyPage(t, c.id, p1.Next)
	if p2.HasMore || len(p2.Messages) != 1 || p2.Messages[0].ID == p1.Messages[0].ID ||
		p2.Messages[0].Text != "Timer: Blue kite" {
		t.Fatal("history cursor repeated or skipped the earlier timer card")
	}
	later := timer(object{"in_seconds": 3600, "label": "Cancel me"})
	if timer(object{"op": "cancel", "id": later["id"]})["cancelled"] != true {
		t.Fatal("future timer did not cancel")
	}
}
