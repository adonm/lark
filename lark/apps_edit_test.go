package main

import (
	"strings"
	"testing"
)

func TestMCPSaveCurrentAndUpdateWithoutRelaunch(t *testing.T) {
	_, base := wsHarness(t, nil)
	const termID = "vocat-0001"
	const id = "cake"
	board := testConnectWS(t, base, "unit", false)
	board.helloV2(t, termID, "Cake")
	toolTest(t, base, "app", object{"companions": id, "template": "quiz",
		"data": object{"questions": []object{{"prompt": "Sky?", "choices": []string{"Blue", "Green"}, "answer": 0}}}}, false)
	board.until(t, isDisplay)
	board.tap(t, 180, 180, 0)
	board.until(t, isDisplay)
	save := toolTest(t, base, "app", object{"companions": id, "save_current": object{"name": "copy", "title": "Copy"}}, false)
	saved := save["saved"].(object)
	if saved["template"] != "quiz" || saved["title"] != "Copy" {
		t.Fatal(save)
	}
	args := object{"name": "copy", "expected_version": saved["version"], "data": object{"questions": []object{{"prompt": "Grass?", "choices": []string{"Green", "Red"}, "answer": 0}}}}
	patch := toolTest(t, base, "patch", object{"apps": object{"update": args}}, false)
	newVersion := patch["apps"].(object)["updated"].([]any)[0].(object)["version"]
	if newVersion == saved["version"] {
		t.Fatal(patch)
	}
	conflict := toolTest(t, base, "patch", object{"apps": object{"delete": []string{"copy"}, "update": []object{
		{"name": "copy", "title": "Wrong", "expected_version": args["expected_version"]},
	}}}, true)
	if !strings.Contains(str(conflict["error"]), "saved app not found") {
		t.Fatal(conflict)
	}
	conflict = toolTest(t, base, "patch", object{"apps": object{"update": args}}, true)
	if conflict["error"] != "version_conflict" || conflict["version"] != newVersion {
		t.Fatal(conflict)
	}
	if a := stateMe(t, base, id)["active_app"].(object); a["title"] != "Quiz" {
		t.Fatal("save or update relaunched the current app", a)
	}
	toolTest(t, base, "app", object{"companions": id, "load": "copy"}, false)
	board.until(t, isDisplay)
	board.tap(t, 75, 290, 0)
	board.until(t, isDisplay)
	toolTest(t, base, "app", object{"companions": id, "save_current": object{"name": "missing"}}, true)
}

func TestMCPDeviceLastAppError(t *testing.T) {
	_, base := wsHarness(t, nil)
	const termID = "vocat-0002"
	const id = "cake"
	board := testConnectWS(t, base, "unit", false)
	board.helloV2(t, termID, "Cake")
	const code = `function render(s,p,g){g.button("broken",40,40,40,40,"Tap")}
		function onTap(){throw new Error("oops")}`
	toolTest(t, base, "app", object{"companions": id, "title": "Broken", "code": code}, false)
	board.until(t, isDisplay)
	board.tap(t, 180, 160, 0)
	board.until(t, hasProto(msgStatus, "oops"))
	me := stateMe(t, base, id)
	got := me["companion"].(object)["last_app_error"].(object)
	if me["active_app"] != nil || got["action"] != "tap" || got["name"] != "broken" || !strings.Contains(str(got["error"]), "oops") {
		t.Fatal(me)
	}
	toolTest(t, base, "app", object{"companions": id, "template": "timer"}, false)
	if got := stateMe(t, base, id)["companion"].(object)["last_app_error"]; got != nil {
		t.Fatal(got)
	}
}
