package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func companionObject(t *testing.T, b *larkServer, id string) object {
	t.Helper()
	result, err := b.companionTool(t.Context(), object{"op": "get", "companion_id": id})
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
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.voices = companionVoiceStore(b.db, heartVoice)
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
	created := post("/api/companions", object{}, 201)
	id := str(created["id"])
	if id != "new-companion" || created["name"] != "New Companion" || created["configured"] != false || created["dormant"] != true {
		t.Fatal("companion did not start blank", created)
	}
	const termID = "vocat-0001"
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
	if blank["name"] != "New Companion" || blank["face_bg"] != "#FFF2E7" || blank["mark_color"] != "#CD806F" ||
		blank["accent_color"] != "#ED8F87" || blank["voice"] != heartVoice || blank["face_svg"] != blankCompanion().FaceSVG {
		t.Fatal("invalid blank starter", blank)
	}
	if got := turnPrompt(id, "voice", "Hi", blankCompanion()); !strings.HasPrefix(got, "[New Companion ("+id+") · hardware · voice · now: ") ||
		!strings.HasSuffix(got, " · setup]\nHi") {
		t.Fatal(got)
	}
	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, "vocat-0001", id)
	svg := waitSVG(t, b.ws, "vocat-0001", "Say hi to meet me!")
	if strings.Contains(svg, "#F59E0B") {
		t.Fatal("starter face did not show its setup cue")
	}
	for _, fields := range []object{
		{"face_bg": "navy"}, {"voice": "unknown"}, {"name": strings.Repeat("x", 41)},
		{"personality": strings.Repeat("x", 161)}, {"configured": "false"},
		{"face_svg": "<svg><g></svg>"},
		{"face_svg": `<svg viewBox="0 0 10 10"/>`},
	} {
		call(true, "update", fields)
	}
	toolTest(t, base, "patch", object{"companion_id": id, "companion": object{"face_svg": strings.Repeat(" ", 16<<10+1)}}, true)
	if got := call(false, "get", object{})["companion"].(object); !reflect.DeepEqual(got["face_svg"], blank["face_svg"]) {
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
	svg = waitSVG(t, b.ws, "vocat-0001", "#172344")
	if !strings.Contains(svg, "#C0FFEE") || !strings.Contains(svg, "#1A1926") {
		t.Fatal("live companion lost an extra or an animated eye")
	}
	list := apiObjects(t, base+"/api/companions", "companions")
	if len(list) != 1 || list[0].(object)["name"] != "Cosmo" || list[0].(object)["face_bg"] != "#172344" {
		t.Fatal("companions API did not reflect companion design", list)
	}
	stored := b.companionFor(id)
	if got := turnPrompt(id, "voice", "Hi", stored); !strings.HasPrefix(got, "[Cosmo ("+id+") · hardware · voice · now: ") ||
		!strings.HasSuffix(got, " · vibe: curious space explorer]\nHi") {
		t.Fatal(got)
	}
	if c := call(false, "reset_blank", object{})["companion"].(object); c["configured"] != false || c["personality"] != "" || c["face_svg"] != blankCompanion().FaceSVG {
		t.Fatal("reset_blank left configured state", c)
	}
	expectValue(t, title, "Lark: New Companion ("+id+")")
	waitSVG(t, b.ws, "vocat-0001", "Say hi to meet me!")
	testRequest(t, "DELETE", base+"/api/companions/"+id, "unit", nil, 409)
	post("/api/terminals/"+termID+"/detach", object{}, 200)
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
	c := testObject(t, testRequest(t, "POST", base+"/api/companions", "unit", object{"name": "Keepsake", "voice": fableVoice}, 201))
	id := str(c["id"])
	if err := b.oc.save(id, "ses_reset"); err != nil {
		t.Fatal(err)
	}
	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, "vocat-0001", id)
	toolTest(t, base, "app", object{"companions": id, "svg": `<svg><text x="180" y="180">Party</text></svg>`}, false)
	testRequest(t, "POST", base+"/api/companions/"+id+"/reset", "unit", object{}, 200)
	waitSVG(t, b.ws, "vocat-0001", "Say hi to meet me!")
	s := stateMe(t, base, id)
	if s["active_app"] != nil {
		t.Fatal("reset did not close app", s)
	}
	if sid, err := b.oc.get(id); err != nil || sid != "" {
		t.Fatal("reset did not clear active session mapping", sid, err)
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
		_, err := updateCompanion(blankCompanion(), object{field: "<svg/>"}, companionVoices{})
		if e, ok := err.(*apiError); !ok || e.status != 400 || e.Error() != "Unknown companion field "+field {
			t.Errorf("updateCompanion accepted legacy field %s: %v", field, err)
		}
	}
}

func TestFaceSVGPaletteChanges(t *testing.T) {
	blank := blankCompanion()
	designed, err := updateCompanion(blank, object{"face_bg": "#123456"}, companionVoices{})
	if err != nil || designed.FaceSVG != designed.defaultFace() || !designed.Configured ||
		!strings.Contains(designed.FaceSVG, "#123456") || strings.Contains(designed.FaceSVG, blank.FaceBG) {
		t.Fatal("palette did not regenerate the blank/default face", err)
	}
	face := `<svg><rect width="360" height="360" fill="#123456"/><circle r="25" fill="` + blank.MarkColor + `"/>
		<g data-pose="idle"><circle cx="95" cy="110" r="15" fill="#AA00FF"/></g></svg>`
	custom, err := updateCompanion(designed, object{"face_svg": face}, companionVoices{})
	if err != nil {
		t.Fatal(err)
	}
	recolored, err := updateCompanion(custom, object{"face_bg": "#223344", "mark_color": "#ABCDEF"}, companionVoices{})
	if err != nil || !strings.Contains(recolored.FaceSVG, "#223344") || !strings.Contains(recolored.FaceSVG, "#ABCDEF") ||
		!strings.Contains(recolored.FaceSVG, "#AA00FF") || strings.Contains(recolored.FaceSVG, "#123456") {
		t.Fatal("palette edit lost its custom eye or failed to recolor", err)
	}
}

func TestCakeFaceSVGEditAndSleep(t *testing.T) {
	_, base := wsHarness(t, nil)
	const termID = "vocat-33d8"
	const id = "cake"
	dev := testConnectWS(t, base, "unit", false)
	dev.helloHardware(t, termID, "Cake", capDrag)
	dev.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgVolume, "")(f) && len(p) >= 1 && p[0] == 55
	})
	before := stateMe(t, base, id)
	look := before["companion"].(object)
	term := before["terminals"].([]any)[0].(object)
	vol := term["volume"].(object)
	face := str(look["face_svg"])
	edit := strings.Replace(face, "</svg>", `<circle cx="112" cy="142" r="17" fill="#123ABC"/><circle cx="248" cy="142" r="17" fill="#123ABC"/></svg>`, 1)
	if face == edit || vol["auto_volume"] != true || look["sleeping"] != false || look["configured"] != true {
		t.Fatal("state didn't return the editable starter face and terminal state")
	}
	update := object{"face_svg": edit}
	result := toolTest(t, base, "patch", object{"companion_id": id, "terminal": object{"sleep": true}, "companion": update}, false)
	if result["terminal"].(object)["sleeping"] != true {
		t.Fatal("combined patch did not set sleep", result)
	}
	dev.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgPower, "")(f) && len(p) >= 1 && p[0] == 12
	})
	toolTest(t, base, "patch", object{"companion_id": id, "companion": update}, false)
	after := stateMe(t, base, id)
	look = after["companion"].(object)
	if look["sleeping"] != true || look["mode"] != "sleepy" || look["face_svg"] == face {
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
