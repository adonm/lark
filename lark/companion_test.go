package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func companionObject(t *testing.T, b *bridge, id string) object {
	t.Helper()
	result, err := b.companionTool(t.Context(), object{"op": "get", "device_id": id})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(result["companion"])
	if err != nil {
		t.Fatal(err)
	}
	return testObject(t, raw)
}

func TestCompanionToolAndLiveFace(t *testing.T) {
	title := make(chan string, 4)
	b, base := wsHarness(t, func(b *bridge, dir string) {
		b.virtual = virtualStore(b.db)
		b.voices = deviceVoiceStore(b.db, heartVoice)
		b.oc = testOpenCode(t, b.root, dir, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPatch && r.URL.Path == "/api/session/ses_companion" {
				raw, _ := io.ReadAll(r.Body)
				title <- str(testObject(t, raw)["title"])
				fmt.Fprint(w, `{"data":{}}`)
				return
			}
			w.WriteHeader(404)
		})
	})
	post := func(path string, data object, status int) object {
		return testObject(t, testRequest(t, "POST", base+path, "unit", data, status))
	}
	created := post("/api/virtual-clients", object{}, 201)
	id := str(created["id"])
	if !virtualID.MatchString(id) || created["character"] != "New Cat" || created["companion"].(object)["configured"] != false {
		t.Fatal("virtual client did not start blank", created)
	}
	if err := b.oc.save(id, "ses_companion"); err != nil {
		t.Fatal(err)
	}
	call := func(fail bool, op string, args object) object {
		t.Helper()
		if op == "get" {
			return stateMe(t, base, id)
		}
		if op == "load_saved" {
			op = "load"
		}
		args["op"] = op
		return toolTest(t, base, "patch", object{"device_id": id, "companion": args}, fail)
	}
	blank := call(false, "get", object{})["companion"].(object)
	if blank["name"] != "New Cat" || blank["face_bg"] != "#FFF2E7" || blank["mark_color"] != "#CD806F" ||
		blank["accent_color"] != "#ED8F87" || blank["voice"] != heartVoice || blank["face_svg"] != blankCompanion().FaceSVG {
		t.Fatal("invalid blank starter", blank)
	}
	if got := turnPrompt(peer{DeviceID: id}, true, "voice", "Hi", blankCompanion()); got != "[New Cat ("+id+") · web · voice · setup]\nHi" {
		t.Fatal(got)
	}
	dev := testConnectWS(t, base, "unit", false)
	dev.json(t, object{"ev": "hello", "role": "web", "device_id": id})
	f := dev.until(t, hasWS("svg", "Say hi to meet me!"))
	if strings.Contains(string(f.data), "#F59E0B") {
		t.Fatal("starter face did not show its setup cue")
	}
	web := testConnectWS(t, base, "unit", true)
	web.json(t, object{"ev": "hello", "role": "web"})
	web.until(t, hasWS("peers", id))
	web.json(t, object{"ev": "watch", "device_id": id})
	web.until(t, hasWS("svg", "Say hi to meet me!"))
	for _, fields := range []object{
		{"face_bg": "navy"}, {"voice": "unknown"}, {"name": strings.Repeat("x", 41)},
		{"personality": strings.Repeat("x", 161)}, {"configured": "false"},
		{"face_svg": "<svg><g></svg>"},
		{"face_svg": `<svg viewBox="0 0 10 10"/>`},
	} {
		call(true, "update", fields)
	}
	toolTest(t, base, "patch", object{"device_id": id, "companion": object{"face_svg": strings.Repeat(" ", 16<<10+1)}}, true)
	if got := call(false, "get", object{})["companion"]; !reflect.DeepEqual(got, blank) {
		t.Fatal("invalid update changed the stored starter", got)
	}
	face := `<svg><rect width="360" height="360" fill="#172344"/><polygon points="140,70 220,70 180,42" fill="#C0FFEE" onclick="nope()"/>
		<script>nope()</script><foreignObject/><g data-pose="idle"><circle cx="112" cy="142" r="16" fill="#1A1926" data-id="text"/></g></svg>`
	cosmo := call(false, "update", object{"name": "Cosmo", "face_bg": "#172344", "mark_color": "#abcdef",
		"accent_color": "#aefc36", "personality": "curious space explorer", "voice": fableVoice, "face_svg": face})["companion"].(object)
	clean := str(cosmo["face_svg"])
	if cosmo["configured"] != true || cosmo["mark_color"] != "#ABCDEF" || cosmo["voice"] != fableVoice ||
		strings.Contains(clean, "nope") || strings.Contains(clean, "foreignObject") || strings.Contains(clean, "data-id") ||
		!strings.Contains(clean, `data-pose="idle"`) {
		t.Fatal("unexpected configured companion", cosmo)
	}
	expectValue(t, title, "Lark: Cosmo ("+id+")")
	frame := dev.until(t, hasWS("svg", "#172344"))
	if !hasWS("svg", "#C0FFEE")(frame) || !hasWS("svg", "#1A1926")(frame) {
		t.Fatal("live companion lost an extra or an animated eye")
	}
	faceSeen, companionSeen, savedSeen := false, false, false
	web.until(t, func(f wsFrame) bool {
		faceSeen = faceSeen || hasWS("svg", "#172344")(f)
		companionSeen = companionSeen || hasWS("companion", `"character":"Cosmo"`)(f)
		savedSeen = savedSeen || hasWS("saved_companions", "")(f)
		return faceSeen && companionSeen && savedSeen
	})
	list := apiObjects(t, base+"/api/devices", "devices")
	if len(list) != 1 || list[0].(object)["character"] != "Cosmo" || list[0].(object)["companion"].(object)["face_bg"] != "#172344" {
		t.Fatal("MCP devices did not reflect companion design", list)
	}
	profiles := apiObjects(t, base+"/api/companions", "companions")
	if len(profiles) != 1 || profiles[0].(object)["slug"] != "cosmo" || profiles[0].(object)["voice"] != fableVoice {
		t.Fatal("profile was not saved", profiles)
	}
	stored := b.companionFor(peer{DeviceID: id})
	if got := turnPrompt(peer{DeviceID: id}, true, "voice", "Hi", stored); got != "[Cosmo ("+id+") · web · voice · vibe: curious space explorer]\nHi" {
		t.Fatal(got)
	}
	if c := call(false, "reset_blank", object{})["companion"].(object); c["configured"] != false || c["personality"] != "" || c["face_svg"] != blankCompanion().FaceSVG {
		t.Fatal("reset_blank left configured state", c)
	}
	expectValue(t, title, "Lark: New Cat ("+id+")")
	dev.until(t, hasWS("svg", "Say hi to meet me!"))
	got := call(false, "load_saved", object{"slug": "cosmo"})["companion"]
	if !reflect.DeepEqual(cosmo, got) {
		t.Fatal("profile did not round-trip", got)
	}
	expectValue(t, title, "Lark: Cosmo ("+id+")")
	dev.until(t, hasWS("svg", "#172344"))
	companionPath := "/api/devices/" + id + "/companion"
	if out := post(companionPath, object{"personality": "stargazer"}, 200); out["companion"].(object)["personality"] != "stargazer" {
		t.Fatal(out)
	}
	testRequest(t, "DELETE", base+"/api/companions/cosmo", "unit", nil, 204)
	call(true, "load_saved", object{"slug": "cosmo"})
	post(companionPath, object{"save_profile": true}, 200)
	post(companionPath, object{"slug": "cosmo"}, 200)
	testRequest(t, "DELETE", base+"/api/companions/cosmo", "unit", nil, 204)
	if len(apiObjects(t, base+"/api/companions", "companions")) != 0 {
		t.Fatal("profile was not deleted")
	}
}

func TestCompanionShiftsWithoutOverwritingSettledForm(t *testing.T) {
	b, base := wsHarness(t, func(b *bridge, _ string) { b.virtual = virtualStore(b.db) })
	v := testObject(t, testRequest(t, "POST", base+"/api/virtual-clients", "unit", object{}, 201))
	id := str(v["id"])
	entry := newChat("user", "You", "Let's try a new shape.", "typed")
	b.history.add(id, entry)
	patch := func(changes object) object {
		return toolTest(t, base, "patch", object{"device_id": id, "companion": changes}, false)["companion"].(object)
	}
	const owl = `<svg><circle cx="180" cy="150" r="70" fill="#AB6010"/></svg>`
	const robot = `<svg><rect x="110" y="80" width="140" height="140" fill="#492256"/></svg>`
	patch(object{"name": "Glimmer", "face_svg": owl, "voice": fableVoice, "save_profile": false})
	if len(apiObjects(t, base+"/api/companions", "companions")) != 0 {
		t.Fatal("temporary form was saved")
	}
	saved := patch(object{"save_profile": true})
	shifted := patch(object{"face_svg": robot, "voice": heartVoice, "save_profile": false})
	if shifted["name"] != saved["name"] || shifted["face_svg"] == saved["face_svg"] || shifted["voice"] != heartVoice {
		t.Fatal("shift did not apply to the active form")
	}
	patch(object{"op": "reset_blank"})
	restored := patch(object{"op": "load", "slug": "glimmer"})
	if !reflect.DeepEqual(restored, saved) {
		t.Fatal("settled form was overwritten by a shift")
	}
	got, err := b.history.list(t.Context(), id)
	if err != nil || len(got) != 1 || got[0].ID != entry.ID {
		t.Fatal("shift changed the conversation", err)
	}
}

func TestVirtualCharacterFollowsCompanionName(t *testing.T) {
	b, base := wsHarness(t, func(b *bridge, _ string) {
		b.virtual = virtualStore(b.db)
	})
	v := testObject(t, testRequest(t, "POST", base+"/api/virtual-clients", "unit", object{"character": "Starter"}, 201))
	id := str(v["id"])
	path := base + "/api/devices/" + id + "/companion"
	assertNames := func(want string) {
		t.Helper()
		var character, name string
		if err := b.db.QueryRow(`SELECT v.character,c.name FROM virtual_clients v
			JOIN companions c ON c.device_id=v.id WHERE v.id=?`, id).Scan(&character, &name); err != nil {
			t.Fatal(err)
		}
		if character != want || name != want {
			t.Fatalf("virtual character = %q, companion name = %q; want both %q", character, name, want)
		}
	}
	assertNames("Starter")
	testRequest(t, "POST", path, "unit", object{"name": "Milo"}, 200)
	assertNames("Milo")
	testRequest(t, "POST", path, "unit", object{"name": "Cleo"}, 200)
	assertNames("Cleo")
	entry := newChat("user", "You", "Hi", "typed")
	b.history.add(id, entry)
	testRequest(t, "POST", path, "unit", object{"slug": "milo"}, 200)
	assertNames("Milo")
	if got, err := b.history.list(t.Context(), id); err != nil || len(got) != 1 || got[0].ID != entry.ID {
		t.Fatal("assignment changed the device's history", err)
	}
	toolTest(t, base, "patch", object{"device_id": id, "companion": object{"op": "reset_blank"}}, false)
	assertNames("New Cat")
}

func TestCompanionResetArchivesAndClearsScene(t *testing.T) {
	b, base := wsHarness(t, func(b *bridge, dir string) {
		b.virtual = virtualStore(b.db)
		b.voices = deviceVoiceStore(b.db, heartVoice)
		b.oc = testOpenCodeOnDB(t, b.root, dir, b.db, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/api/session/ses_reset/interrupt" {
				fmt.Fprint(w, `{"data":{"interrupted":false}}`)
				return
			}
			w.WriteHeader(404)
		})
	})
	v := testObject(t, testRequest(t, "POST", base+"/api/virtual-clients", "unit", object{"character": "Keepsake", "voice": fableVoice}, 201))
	id := str(v["id"])
	if err := b.oc.save(id, "ses_reset"); err != nil {
		t.Fatal(err)
	}
	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, id, "Ignored")
	toolTest(t, base, "app", object{"devices": id, "svg": `<svg><text x="180" y="180">Party</text></svg>`}, false)
	testRequest(t, "POST", base+"/api/devices/"+id+"/reset", "unit", object{}, 200)
	dev.until(t, hasWS("svg", "Say hi to meet me!"))
	s := stateMe(t, base, id)
	if s["active_app"] != nil || s["device"].(object)["screen"].(object)["title"] != "Companion" {
		t.Fatal("reset did not close app", s)
	}
	if sid, err := b.oc.get(id); err != nil || sid != "" {
		t.Fatal("reset did not clear active session mapping", sid, err)
	}
	if v, ok := b.virtualByID(id); !ok || v.Character != "New Cat" {
		t.Fatal("reset did not update virtual client character", v.Character)
	}
	profiles := apiObjects(t, base+"/api/companions", "companions")
	if len(profiles) != 1 || profiles[0].(object)["name"] != "Keepsake" || profiles[0].(object)["face_bg"] != storybookPalettes["peach"].bg || profiles[0].(object)["voice"] != fableVoice {
		t.Fatal("reset did not archive companion", profiles)
	}
}

func TestFaceSVGPoseFilterAndChrome(t *testing.T) {
	companion := blankCompanion()
	companion.Name, companion.Configured = "Cake", true
	companion.FaceSVG = `<svg>
		<rect width="360" height="360" fill="#AAAAAA" data-id="fake" data-hit="0,0,360,360"/>
		<g><circle data-pose="idle" r="15" fill="#111111"/><circle data-pose="blink" r="15" fill="#222222"/></g>
		<g data-pose="talk"><circle r="15" fill="#333333"/></g>
		<g data-pose="sleep"><circle r="15" fill="#444444"/></g>
	</svg>`
	marks := []string{"#111111", "#222222", "#333333", "#444444"}
	for _, tc := range []struct {
		pose companionPose
		mark int
	}{
		{companionPose{mood: "idle"}, 0},
		{companionPose{mood: "idle", blink: true}, 1},
		{companionPose{mood: "speaking"}, 2},
		{companionPose{mood: "sleepy"}, 3},
		{companionPose{mood: "thinking"}, 0},
		{companionPose{mood: "listening"}, 0},
	} {
		svg := companionSVG(companion, tc.pose)
		for i, mark := range marks {
			if strings.Contains(svg, mark) != (i == tc.mark) {
				t.Fatalf("%+v selected the wrong pose: %s", tc.pose, svg)
			}
		}
		if !strings.Contains(svg, "#AAAAAA") || strings.Contains(svg, `data-id="fake"`) {
			t.Fatal("shared art lost or gained a hit target", svg)
		}
		if tc.pose.mood == "listening" && !strings.Contains(svg, menuDanger) {
			t.Fatal("listening halo missing")
		}
	}
	screen, err := buildScreen(companionSVG(companion, companionPose{mood: "idle"}), false, roundScreen)
	if err != nil || hitID(screen.hits, 180, 300) != "menu" {
		t.Fatalf("art blocked Menu: %v, %v", screen.hits, err)
	}
}

func TestUpdateCompanionLegacyFaceFields(t *testing.T) {
	for _, field := range []string{"svg_extras", "extras"} {
		_, _, err := updateCompanion(blankCompanion(), object{field: "<svg/>"}, deviceVoices{})
		if e, ok := err.(*apiError); !ok || e.status != 400 || e.Error() != "Unknown companion field "+field {
			t.Errorf("updateCompanion accepted legacy field %s: %v", field, err)
		}
	}
}

func TestFaceSVGPaletteChanges(t *testing.T) {
	blank := blankCompanion()
	designed, _, err := updateCompanion(blank, object{"face_bg": "#123456"}, deviceVoices{})
	if err != nil || designed.FaceSVG != designed.defaultFace() || !designed.Configured ||
		!strings.Contains(designed.FaceSVG, "#123456") || strings.Contains(designed.FaceSVG, blank.FaceBG) {
		t.Fatal("palette did not regenerate the blank/default face", err)
	}
	face := `<svg><rect width="360" height="360" fill="#123456"/><circle r="25" fill="` + blank.MarkColor + `"/>
		<g data-pose="idle"><circle cx="95" cy="110" r="15" fill="#AA00FF"/></g></svg>`
	custom, _, err := updateCompanion(designed, object{"face_svg": face}, deviceVoices{})
	if err != nil {
		t.Fatal(err)
	}
	recolored, _, err := updateCompanion(custom, object{"face_bg": "#223344", "mark_color": "#ABCDEF"}, deviceVoices{})
	if err != nil || !strings.Contains(recolored.FaceSVG, "#223344") || !strings.Contains(recolored.FaceSVG, "#ABCDEF") ||
		!strings.Contains(recolored.FaceSVG, "#AA00FF") || strings.Contains(recolored.FaceSVG, "#123456") {
		t.Fatal("palette edit lost its custom eye or failed to recolor", err)
	}
}

func TestCakeFaceSVGEditAndSleep(t *testing.T) {
	_, base := wsHarness(t, nil)
	const id = "lark-33d8"
	dev := testConnectWS(t, base, "unit", false)
	dev.json(t, object{"ev": "hello", "role": "device", "device_id": id, "character": "Cake", "proto": 2})
	dev.until(t, hasWS("volume", `"level":55`))
	before := stateMe(t, base, id)
	device, look := before["device"].(object), before["companion"].(object)
	face := str(look["face_svg"])
	edit := strings.Replace(face, "</svg>", `<circle cx="112" cy="142" r="17" fill="#123ABC"/><circle cx="248" cy="142" r="17" fill="#123ABC"/></svg>`, 1)
	if face == edit || device["auto_volume"] != true || device["sleeping"] != false || look["configured"] != true {
		t.Fatal("state didn't return the editable starter face and device state")
	}
	update := object{"face_svg": edit}
	result := toolTest(t, base, "patch", object{"device_id": id, "device": object{"sleep": true}, "companion": update}, false)
	if result["device"].(object)["sleeping"] != true {
		t.Fatal("combined patch did not set sleep", result)
	}
	dev.until(t, hasWS("power", `"brightness":12`))
	toolTest(t, base, "patch", object{"device_id": id, "companion": update}, false)
	after := stateMe(t, base, id)
	device, look = after["device"].(object), after["companion"].(object)
	if device["sleeping"] != true || device["mode"] != "sleepy" || look["face_svg"] == face {
		t.Fatal("appearance edit woke the companion or failed to save")
	}
	companion := blankCompanion()
	companion.Name, companion.Configured = "Cake", true
	companion.FaceSVG = str(look["face_svg"])
	awake := companionSVG(companion, companionPose{mood: "idle"})
	if strings.Count(awake, "#123ABC") != 2 || strings.Contains(awake, `x="95" y="123"`) ||
		strings.Contains(awake, "M95 142H129") || strings.Contains(awake, `<circle cx="107"`) {
		t.Fatal("original awake eyes still render under the edit", awake)
	}
}

func TestCompanionReassign(t *testing.T) {
	for _, mode := range []string{"swap", "move"} {
		t.Run(mode, func(t *testing.T) {
			const fromID, fromSID, toSID = "lark-from", "ses_from", "ses_to"
			interrupts := make(chan string, 2)
			titles := make(chan string, 2)
			b, base := wsHarness(t, func(b *bridge, dir string) {
				b.virtual = virtualStore(b.db)
				b.voices = deviceVoiceStore(b.db, heartVoice)
				b.oc = testOpenCode(t, b.root, dir, func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/interrupt") {
						interrupts <- strings.TrimPrefix(r.URL.Path, "/api/session/")
						fmt.Fprint(w, `{"data":{"interrupted":true}}`)
					} else if r.Method == http.MethodPatch {
						raw, _ := io.ReadAll(r.Body)
						titles <- strings.TrimPrefix(r.URL.Path, "/api/session/") + ":" + str(testObject(t, raw)["title"])
						fmt.Fprint(w, `{"data":{}}`)
					} else {
						w.WriteHeader(404)
					}
				})
				b.oc.store = sessionStore(b.db)
			})
			post := func(path string, data object, status int) []byte {
				return testRequest(t, "POST", base+path, "unit", data, status)
			}
			target := testObject(t, post("/api/virtual-clients", object{"character": "Target", "voice": "bm_george"}, 201))
			toID := str(target["id"])
			from := testConnectWS(t, base, "unit", false)
			to := testConnectWS(t, base, "unit", false)
			from.hello(t, fromID, "Cake")
			mirror := testConnectWS(t, base, "unit", true)
			mirror.watch(t, fromID)
			to.hello(t, toID, "Ignored")
			source := post("/api/devices/"+fromID+"/companion", object{"name": "Source", "voice": "bf_emma", "face_bg": "#654321", "personality": "happy"}, 200)
			mirror.until(t, hasWS("svg", "#654321"))
			for id, sid := range map[string]string{fromID: fromSID, toID: toSID} {
				if err := b.oc.save(id, sid); err != nil {
					t.Fatal(err)
				}
			}
			canceled := make(chan string, 2)
			b.ws.mu.Lock()
			for _, id := range []string{fromID, toID} {
				d := b.ws.devices[id]
				d.busy = true
				d.cancel = func() { canceled <- id }
			}
			b.ws.mu.Unlock()

			const endpoint = "/api/companions/reassign"
			args := object{"from_device_id": fromID, "to_device_id": toID}
			testRequest(t, "POST", base+endpoint, "", args, 401)
			post(endpoint, object{"from_device_id": fromID, "to_device_id": fromID}, 400)
			post(endpoint, object{"from_device_id": fromID, "to_device_id": toID, "mode": "copy"}, 400)
			if mode == "move" {
				args["mode"] = mode
			} // Exercise the default swap.
			if got := testObject(t, post(endpoint, args, 200))["mode"]; got != mode {
				t.Fatal(got)
			}
			expectValue(t, interrupts, fromSID+"/interrupt")
			expectValue(t, interrupts, toSID+"/interrupt")
			expectValue(t, canceled, fromID)
			expectValue(t, canceled, toID)
			wantName, wantBG, wantVoice, wantSID := "Target", storybookPalettes["peach"].bg, "bm_george", toSID
			if mode == "move" {
				wantName, wantBG, wantVoice, wantSID = "New Cat", "#FFF2E7", heartVoice, ""
			} else {
				expectValue(t, titles, toSID+":Lark: Target ("+fromID+")")
			}
			expectValue(t, titles, fromSID+":Lark: Source ("+toID+")")
			mirror.until(t, hasWS("svg", wantBG))
			to.until(t, hasWS("svg", "#654321"))
			if gets := companionObject(t, b, toID); !reflect.DeepEqual(gets, testObject(t, source)["companion"]) {
				t.Fatal("target did not receive source design and voice", gets)
			}
			companion := companionObject(t, b, fromID)
			if companion["name"] != wantName || companion["face_bg"] != wantBG || companion["voice"] != wantVoice || companion["configured"] != (mode == "swap") {
				t.Fatal("incorrect source state after reassignment", companion)
			}
			for id, expected := range map[string]string{fromID: wantSID, toID: fromSID} {
				if sid, err := b.oc.get(id); err != nil || sid != expected {
					t.Fatalf("session mapping for %s = %s, want %s: %v", id, sid, expected, err)
				}
			}
			profiles := apiObjects(t, base+"/api/companions", "companions")
			i := slices.IndexFunc(profiles, func(v any) bool { return v.(object)["slug"] == "target" })
			if mode == "move" && (i < 0 || profiles[i].(object)["voice"] != "bm_george") || mode == "swap" && i >= 0 {
				t.Fatal("target archival is incorrect", profiles)
			}
		})
	}
}
