package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestCompanionLifecycleAndValidation(t *testing.T) {
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.voices = companionVoiceStore(b.db, heartVoice)
	})
	var title chan string
	b.oc = testOpenCodeOnDB(t, b.root, t.TempDir(), b.db, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/session":
			fmt.Fprint(w, `{"id":"ses_companion"}`)
		case r.Method == http.MethodPatch && r.URL.Path == "/api/session/ses_companion":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			title <- str(body["title"])
			w.WriteHeader(200)
		default:
			w.WriteHeader(404)
		}
	})
	title = make(chan string, 4)

	created := testObject(t, testRequest(t, http.MethodPost, base+"/api/companions", "unit", object{}, 201))
	id := str(created["id"])
	if !validCompanionID(id) || created["name"] != "New Companion" || created["configured"] != false {
		t.Fatal("unexpected blank companion", created)
	}
	if err := b.oc.save(id, "ses_companion"); err != nil {
		t.Fatal(err)
	}
	call := func(fail bool, op string, args object) object {
		t.Helper()
		if op == "get" {
			return stateMe(t, base, id)
		}
		args["op"] = op
		return toolTest(t, base, "patch", object{"companion_id": id, "companion": args}, fail)
	}
	blank := call(false, "get", object{})["companion"].(object)
	if blank["name"] != "New Companion" || blank["voice"] != heartVoice {
		t.Fatal("invalid blank starter", blank)
	}
	if got := turnPrompt(id, "voice", "Hi", blankCompanion()); !strings.HasPrefix(got, "[In person at New Companion's device · New Companion ("+id+") · voice · setup]\nHi\n[state: now: ") {
		t.Fatal(got)
	}
	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, "vocat-0001", id)
	waitSVG(t, b.ws, "vocat-0001", "home")

	for _, fields := range []object{
		{"face_bg": "#112233"}, {"voice": "unknown"}, {"name": strings.Repeat("x", 41)},
		{"personality": strings.Repeat("x", 161)}, {"configured": "false"},
		{"face_svg": "<svg/>"}, {"preset": object{}},
	} {
		call(true, "update", fields)
	}
	cosmo := call(false, "update", object{
		"name":        "Cosmo",
		"form":        "a space otter",
		"personality": "curious space explorer",
		"voice":       fableVoice,
	})["companion"].(object)
	if cosmo["configured"] != true || cosmo["voice"] != fableVoice || cosmo["form"] != "a space otter" {
		t.Fatal("unexpected configured companion", cosmo)
	}
	expectValue(t, title, "Lark: Cosmo ("+id+")")

	list := apiObjects(t, base+"/api/companions", "companions")
	if len(list) != 1 || list[0].(object)["name"] != "Cosmo" || list[0].(object)["form"] != "a space otter" {
		t.Fatal("companions API did not reflect companion", list)
	}
	stored := b.companionFor(id)
	if got := turnPrompt(id, "voice", "Hi", stored); !strings.HasPrefix(got, "[In person at Cosmo's device · Cosmo ("+id+") · voice · vibe: curious space explorer]\nHi\n[state: now: ") {
		t.Fatal(got)
	}
	if c := call(false, "reset_blank", object{})["companion"].(object); c["configured"] != false || c["personality"] != "" {
		t.Fatal("reset_blank left configured state", c)
	}
	expectValue(t, title, "Lark: New Companion ("+id+")")
	testRequest(t, "DELETE", base+"/api/companions/"+id, "unit", nil, 409)
	testRequest(t, "POST", base+"/api/terminals/vocat-0001/detach", "unit", object{}, 200)
	testRequest(t, "DELETE", base+"/api/companions/"+id, "unit", nil, 204)
	if len(apiObjects(t, base+"/api/companions", "companions")) != 0 {
		t.Fatal("companion was not deleted")
	}
}

func TestCompanionResetArchivesAndClearsScene(t *testing.T) {
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.voices = companionVoiceStore(b.db, heartVoice)
		b.oc = testOpenCodeOnDB(t, b.root, dir, b.db, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/api/session/ses_reset/interrupt" {
				fmt.Fprint(w, `{"data":{"interrupted":false}}`)
				return
			}
			w.WriteHeader(404)
		})
	})
	c := testObject(t, testRequest(t, "POST", base+"/api/companions", "unit", object{"name": "Keepsake", "voice": fableVoice, "form": "a cat"}, 201))
	id := str(c["id"])
	if err := b.oc.save(id, "ses_reset"); err != nil {
		t.Fatal(err)
	}
	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, "vocat-0001", id)
	testRequest(t, "POST", base+"/api/companions/"+id+"/reset", "unit", object{}, 200)
	waitSVG(t, b.ws, "vocat-0001", "home")
	if sid, err := b.oc.get(id); err != nil || sid != "" {
		t.Fatal("reset did not clear active session mapping", sid, err)
	}
}

func TestUpdateCompanionRejectsLegacyFields(t *testing.T) {
	for _, field := range []string{"svg_extras", "extras", "face_svg", "face_bg", "mark_color", "accent_color", "preset", "face_edits"} {
		_, err := updateCompanion(blankCompanion(), object{field: "x"}, companionVoices{})
		if e, ok := err.(*apiError); !ok || e.status != 400 || e.Error() != "Unknown companion field "+field {
			t.Errorf("updateCompanion accepted legacy field %s: %v", field, err)
		}
	}
}

func TestCompanionFormAndPortraitPrompt(t *testing.T) {
	b, base := wsHarness(t, nil)
	testRequest(t, http.MethodPost, base+"/api/companions", "unit", object{"name": "Greg"}, 400)
	created := testObject(t, testRequest(t, http.MethodPost, base+"/api/companions", "unit", object{
		"name": "Greg",
		"form": "an old chameleon",
	}, 201))
	if created["form"] != "an old chameleon" {
		t.Fatalf("created form = %v", created["form"])
	}
	id := str(created["id"])
	me := stateMe(t, base, id)
	if me["companion"].(object)["form"] != "an old chameleon" {
		t.Fatalf("state me.companion.form = %v", me["companion"].(object)["form"])
	}
	// Update without form preserves existing form.
	updated, err := updateCompanion(b.companionFor(id), object{"personality": "gentle"}, b.voices)
	if err != nil || updated.Form != "an old chameleon" {
		t.Fatalf("updateCompanion lost form: %+v err=%v", updated, err)
	}
	// Explicit form update changes it.
	updated, err = updateCompanion(updated, object{"form": "a mossy gecko"}, b.voices)
	if err != nil || updated.Form != "a mossy gecko" {
		t.Fatalf("updateCompanion did not update form: %+v err=%v", updated, err)
	}
	prompt := portraitPrompt(portraitSpec{Prompt: "wearing spectacles", Form: "an old chameleon", BG: "#F3ECFA", Style: portraitPixelStyle})
	if !strings.Contains(prompt, "an old chameleon") {
		t.Fatalf("portraitPrompt missing form: %s", prompt)
	}
}

func TestCompanionListenSeconds(t *testing.T) {
	b, base := wsHarness(t, nil)
	created := testObject(t, testRequest(t, http.MethodPost, base+"/api/companions", "unit", object{
		"name": "Greg",
		"form": "an old chameleon",
	}, 201))
	id := str(created["id"])
	me := stateMe(t, base, id)
	if got := me["companion"].(object)["listen_seconds"]; got != json.Number("0") {
		t.Fatalf("default listen_seconds = %v, want 0", got)
	}
	res := toolTest(t, base, "patch", object{
		"companion_id": id,
		"companion":    object{"listen_seconds": 60},
	}, false)
	if got := res["companion"].(object)["listen_seconds"]; got != json.Number("60") {
		t.Fatalf("patched listen_seconds = %v, want 60", got)
	}
	me = stateMe(t, base, id)
	if got := me["companion"].(object)["listen_seconds"]; got != json.Number("60") {
		t.Fatalf("state me.companion.listen_seconds = %v, want 60", got)
	}
	for _, bad := range []any{-1, 301, 12.5, "60"} {
		if _, err := updateCompanion(b.companionFor(id), object{"listen_seconds": bad}, b.voices); err == nil {
			t.Fatalf("expected error for listen_seconds=%v", bad)
		}
	}
}
