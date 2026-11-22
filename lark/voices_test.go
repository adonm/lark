package main

import (
	"context"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

type fakeSpeechEngine func(string, voiceChoice, func() bool) ([]float32, int)

func (f fakeSpeechEngine) generate(text string, voice voiceChoice, keepGoing func() bool) ([]float32, int) {
	return f(text, voice, keepGoing)
}

func (fakeSpeechEngine) close() {}

func TestTTSVoiceSelection(t *testing.T) {
	for _, tt := range []struct {
		id, lang string
		sid      int
	}{
		{heartVoice, "en-us", 3}, {emmaVoice, "en-gb", 21}, {fableVoice, "en-gb", 25},
		{"af_bella", "en-us", 2}, {"am_puck", "en-us", 18}, {"bm_george", "en-gb", 26},
		{"bf_alice", "en-gb", 21}, {"bm_lewis", "en-gb", 25},
		{"", "en-us", 3}, {"unsupported", "en-us", 3},
	} {
		const text = "Café"
		engine := fakeSpeechEngine(func(got string, voice voiceChoice, keepGoing func() bool) ([]float32, int) {
			if got != text || voice.SID != tt.sid || voice.Lang != tt.lang || !keepGoing() {
				t.Errorf("voice %q reached engine as %q, %+v", tt.id, got, voice)
			}
			return []float32{0.25}, sampleRate
		})
		if _, err := synthesizePCM(context.Background(), engine, tt.id, text); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRetiredVoicesRemainReadable(t *testing.T) {
	b, _ := wsHarness(t, func(b *larkServer, dir string) {
		b.voices = companionVoiceStore(b.db, "")
	})
	const id = "legacy"
	if _, err := b.createCompanion(t.Context(), object{"id": id, "name": "Legacy"}); err != nil {
		t.Fatal(err)
	}
	for old, want := range map[string]string{
		"bf_alice": emmaVoice, "bf_isabella": emmaVoice, "bf_lily": emmaVoice,
		"bm_daniel": fableVoice, "bm_lewis": fableVoice, "retired": heartVoice,
	} {
		if _, err := b.db.Exec(`UPDATE companions SET voice=? WHERE id=?`, old, id); err != nil {
			t.Fatal(err)
		}
		if b.voices.allowed(old) || b.voices.get(id, "mint") != want || b.companionFor(id).Voice != want {
			t.Errorf("%q did not resolve to %q", old, want)
		}
		var raw string
		if err := b.db.QueryRow(`SELECT voice FROM companions WHERE id=?`, id).Scan(&raw); err != nil || raw != old {
			t.Errorf("reading %q altered the stored voice to %q (%v)", old, raw, err)
		}
	}
}

func TestMCPVoiceCatalog(t *testing.T) {
	want := []string{heartVoice, fableVoice, emmaVoice, "bm_george", "af_bella", "am_puck"}
	if len(voiceCatalog) != len(want) {
		t.Fatalf("got %d voice choices, want %d", len(voiceCatalog), len(want))
	}
	fields := mcpInputSchema("patch")["properties"].(object)
	schema := fields["companion"].(object)["properties"].(object)["voice"].(object)
	if got := schema["enum"]; !reflect.DeepEqual(got, want) {
		t.Errorf("MCP companion voice choices = %v, want %v", got, want)
	}
}

func TestSpeechClipsCacheByVoice(t *testing.T) {
	ctx := t.Context()
	var calls atomic.Int32
	w := fakeTTS(ctx, &calls)
	var clips speechClips
	for _, voice := range []string{emmaVoice, fableVoice, emmaVoice, fableVoice} {
		if pcm, err := clips.get(ctx, w, voice, "Hello!"); err != nil || len(pcm) == 0 {
			t.Fatalf("voice %s: no PCM (%v)", voice, err)
		}
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("synthesized %d clips, want one per voice", n)
	}
}

func TestCompanionVoiceDefaults(t *testing.T) {
	b, _ := wsHarness(t, func(b *larkServer, dir string) {
		b.voices = companionVoiceStore(b.db, "")
	})
	if _, err := b.createCompanion(t.Context(), object{"id": "green", "name": "Green Cat", "voice": fableVoice}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.createCompanion(t.Context(), object{"id": "mint", "name": "Mint"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id   string
		want string
	}{
		{"cake", heartVoice},
		{"mint", fableVoice},
		{"green", fableVoice},
	} {
		if got := b.voiceFor(tc.id); got != tc.want {
			t.Errorf("%s: got voice %q, want %q", tc.id, got, tc.want)
		}
	}
	configured := companionVoiceStore(b.db, emmaVoice)
	if !configured.allowed(heartVoice) ||
		configured.get("cake", "cake") != emmaVoice ||
		configured.get("mint", "mint") != fableVoice {
		t.Fatal("explicit fallback voice or the Mint default was not respected")
	}
}

func TestCompanionVoiceAPI(t *testing.T) {
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.voices = companionVoiceStore(b.db, emmaVoice)
	})
	cake := testConnectWS(t, base, "unit", false)
	cake.hello(t, "vocat-33d8", "Cake")
	profile := testObject(t, testRequest(t, "GET", base+"/api/scope", "unit", nil, 200))
	if profile["scoped"] != false {
		t.Fatal("LAN listener reported itself as scoped")
	}
	path := "/api/companions/CAKE/voice"
	testRequest(t, "POST", base+path, "", object{"voice": fableVoice}, 401)
	for _, voice := range []any{"bad", "bf_alice", 1, nil} {
		testRequest(t, "POST", base+path, "unit", object{"voice": voice}, 400)
	}
	testRequest(t, "POST", base+path, "unit", object{"voice": heartVoice}, 200)
	got := testObject(t, testRequest(t, "POST", base+path, "unit", object{"voice": fableVoice}, 200))
	if got["companion_id"] != "cake" || got["voice"] != fableVoice {
		t.Fatalf("unexpected update response: %v", got)
	}
	if b.companionFor("cake").Voice != fableVoice {
		t.Fatal("override not persisted under lowercase companion ID")
	}
	if d := apiObjects(t, base+"/api/companions", "companions")[0].(object); d["voice"] != fableVoice {
		t.Fatal("LAN companion list omitted the override")
	}
	testRequest(t, "POST", base+"/api/companions", "unit", object{"name": "Green", "voice": "unknown"}, 400)
	created := testObject(t, testRequest(t, "POST", base+"/api/companions", "unit", object{
		"name": "Green", "voice": "bm_george",
	}, 201))
	id := str(created["id"])
	if listed := apiObjects(t, base+"/api/companions", "companions"); len(listed) != 2 ||
		created["voice"] != "bm_george" || listed[1].(object)["voice"] != "bm_george" {
		t.Fatal("companion did not persist its initial voice")
	}
	c, ok := b.companionByID(id)
	if !ok {
		t.Fatal("companion not found")
	}
	scoped := httptest.NewServer(b.scoped(c, "scoped-test"))
	defer scoped.Close()
	path = "/api/companions/" + id + "/voice"
	testRequest(t, "POST", scoped.URL+path, "unit", object{"voice": emmaVoice}, 401)
	testRequest(t, "POST", scoped.URL+"/api/companions/cake/voice", "scoped-test", object{"voice": emmaVoice}, 403)
	testRequest(t, "POST", scoped.URL+"/api/companions/other/voice", "scoped-test", object{"voice": emmaVoice}, 403)
	testRequest(t, "GET", scoped.URL+path, "scoped-test", nil, 404)
	testRequest(t, "POST", scoped.URL+path, "scoped-test", object{"voice": heartVoice}, 200)
	scope := testObject(t, testRequest(t, "GET", scoped.URL+"/api/scope", "scoped-test", nil, 200))
	if scope["voice"] != heartVoice || !reflect.DeepEqual(profile["voices"], scope["voices"]) || len(scope["voices"].([]any)) != len(voiceCatalog) || b.voices.get("cake", "cake") != fableVoice {
		t.Fatal("scoped update did not stay on the scoped companion")
	}
}

func TestMenuVoicePicker(t *testing.T) {
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.voices = companionVoiceStore(b.db, heartVoice)
	})
	const termID = "vocat-33d8"
	const id = "cake"
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, termID, "Cake")
	web := testConnectWS(t, base, "unit", true)
	web.watch(t, id)
	web.json(t, object{"ev": "tap", "id": "menu"})
	web.until(t, hasWS("svg", "menu-next"))
	web.json(t, object{"ev": "tap", "id": "menu-next"})
	web.until(t, hasWS("svg", "Heart"))
	web.json(t, object{"ev": "tap", "id": "menu-voice"})
	web.until(t, hasWS("svg", "voice-george"))
	for _, voice := range []int{1, 2, 3, 0} {
		choice := voiceCatalog[voice]
		web.json(t, object{"ev": "tap", "id": "voice-" + strings.ToLower(choice.Name)})
		web.until(t, hasWS("svg", menuWarning))
		if reloaded := companionVoiceStore(b.db, heartVoice); reloaded.get(id, "cake") != choice.ID {
			t.Errorf("voice picker did not persist %s", choice.ID)
		}
	}
	web.json(t, object{"ev": "tap", "id": "menu-back"})
	web.until(t, hasWS("svg", "menu-voice"))
	testRequest(t, "POST", base+"/api/companions/"+id+"/voice", "unit", object{"voice": "af_bella"}, 200)
	web.until(t, hasWS("svg", "Bella"))
}
