package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestCompanionLifecycleAndValidation(t *testing.T) {
	t.Parallel()
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.voices = companionVoiceStore(b.db, heartVoice)
	})
	var title chan string
	b.oc = testOpenCodeOnDB(t, b.root, t.TempDir(), b.db, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/session":
			fmt.Fprint(w, `{"id":"ses_companion"}`)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/instructions/entries/"):
			w.WriteHeader(204)
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
	if got := turnPrompt("voice", "Hi", blankCompanion()); !strings.HasPrefix(got, "[Speaker: someone in person at your device (name unknown) · voice · setup]\nHi\n[state: now: ") {
		t.Fatal(got)
	}
	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, "vocat-0001", id)
	waitSVG(t, b.hub, "vocat-0001", "home")

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
	if got := turnPrompt("voice", "Hi", stored); !strings.HasPrefix(got, "[Speaker: someone in person at your device (name unknown) · voice]\nHi\n[state: now: ") {
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

func TestCompanionIdentityEntryRefreshesOnEdit(t *testing.T) {
	t.Parallel()
	const id, sid = "lark-entry", "ses-entry"
	var mu sync.Mutex
	var entries []object
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.voices = companionVoiceStore(b.db, heartVoice)
		b.oc = testOpenCodeOnDB(t, b.root, dir, b.db, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/api/event":
				w.WriteHeader(404)
			case r.Method == http.MethodPut && r.URL.Path == "/api/experimental/session/"+sid+"/instructions/entries/lark.companion":
				var in struct{ Value object }
				if json.NewDecoder(r.Body).Decode(&in) != nil {
					t.Error("invalid instruction entry body")
				}
				mu.Lock()
				entries = append(entries, in.Value)
				mu.Unlock()
				w.WriteHeader(204)
			case r.Method == http.MethodGet && r.URL.Path == "/api/session/"+sid:
				sendJSON(w, 200, object{"data": object{"id": sid}})
			case r.Method == http.MethodPatch && r.URL.Path == "/api/session/"+sid:
				w.WriteHeader(200)
			default:
				t.Errorf("unexpected OpenCode work: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(404)
			}
		})
	})
	snapshot := func() []object {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(entries)
	}
	patch := func(args object) object {
		t.Helper()
		return toolTest(t, base, "patch", object{"companion_id": id, "companion": args}, false)["companion"].(object)
	}
	created := testObject(t, testRequest(t, http.MethodPost, base+"/api/companions", "unit", object{
		"id": id, "name": "Plum", "form": "a cat", "personality": "calm",
	}, 201))
	if str(created["id"]) != id {
		t.Fatalf("unexpected companion id %v", created["id"])
	}
	// No bound session: the next create writes the fresh identity, so no entry call.
	patch(object{"personality": "brave"})
	if got := snapshot(); len(got) != 0 {
		t.Fatalf("entry refreshed without a session: %+v", got)
	}
	if err := b.oc.save(id, sid); err != nil {
		t.Fatal(err)
	}
	// Same values, or fields outside the entry, leave it alone.
	patch(object{"personality": "brave", "form": "a bigger cat", "voice": fableVoice})
	if got := snapshot(); len(got) != 0 {
		t.Fatalf("unchanged identity refreshed the entry: %+v", got)
	}
	patch(object{"personality": "stargazer"})
	want := object{"id": id, "name": "Plum", "personality": "stargazer"}
	got := snapshot()
	if len(got) != 1 || !maps.Equal(got[0], want) {
		t.Fatalf("personality edit entry = %+v, want %+v", got, want)
	}
	if _, form := got[0]["form"]; form {
		t.Fatal("entry must not carry live form state")
	}
	// The next turn reuses the bound session, whose entry now has the new value.
	if next, err := b.oc.ensureSession(t.Context(), b.companionFor(id), false); err != nil || next != sid {
		t.Fatalf("next turn session = %q, %v", next, err)
	}
	if got = snapshot(); len(got) != 1 {
		t.Fatalf("next turn rewrote the identity entry: %+v", got)
	}
	patch(object{"name": "Cosmo"})
	got = snapshot()
	if len(got) != 2 || got[1]["name"] != "Cosmo" || got[1]["personality"] != "stargazer" {
		t.Fatalf("rename entry = %+v", got)
	}
}

func TestCompanionResetArchivesAndClearsScene(t *testing.T) {
	t.Parallel()
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
	waitSVG(t, b.hub, "vocat-0001", "home")
	if sid, err := b.oc.get(id); err != nil || sid != "" {
		t.Fatal("reset did not clear active session mapping", sid, err)
	}
}

func TestUpdateCompanionRejectsLegacyFields(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"svg_extras", "extras", "face_svg", "face_bg", "mark_color", "accent_color", "preset", "face_edits"} {
		_, err := updateCompanion(blankCompanion(), object{field: "x"}, companionVoices{})
		if e, ok := err.(*apiError); !ok || e.status != 400 || e.Error() != "Unknown companion field "+field {
			t.Errorf("updateCompanion accepted legacy field %s: %v", field, err)
		}
	}
}

func TestCompanionFormAndPortraitPrompt(t *testing.T) {
	t.Parallel()
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

func TestCompanionListeningToggle(t *testing.T) {
	t.Parallel()
	b, base := wsHarness(t, nil)
	created := testObject(t, testRequest(t, http.MethodPost, base+"/api/companions", "unit", object{
		"name": "Greg",
		"form": "an old chameleon",
	}, 201))
	id := str(created["id"])
	me := stateMe(t, base, id)
	if got := me["companion"].(object)["listening"]; got != false {
		t.Fatalf("default listening = %v, want false", got)
	}
	res := toolTest(t, base, "patch", object{
		"companion_id": id,
		"companion":    object{"listening": true},
	}, false)
	if got := res["companion"].(object)["listening"]; got != true {
		t.Fatalf("patched listening = %v, want true", got)
	}
	me = stateMe(t, base, id)
	if got := me["companion"].(object)["listening"]; got != true {
		t.Fatalf("state me.companion.listening = %v, want true", got)
	}
	for _, bad := range []any{1, "on", nil} {
		if _, err := updateCompanion(b.companionFor(id), object{"listening": bad}, b.voices); err == nil {
			t.Fatalf("expected error for listening=%v", bad)
		}
	}
	if _, err := updateCompanion(b.companionFor(id), object{"listen_seconds": 60}, b.voices); err == nil {
		t.Fatal("listen_seconds must be rejected after the boolean migration")
	}
}
