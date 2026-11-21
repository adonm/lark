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
		b.voices = deviceVoiceStore(b.db, "")
		b.virtual = virtualStore(b.db)
	})
	const id = "lark-legacy"
	for old, want := range map[string]string{
		"bf_alice": emmaVoice, "bf_isabella": emmaVoice, "bf_lily": emmaVoice,
		"bm_daniel": fableVoice, "bm_lewis": fableVoice, "retired": heartVoice,
	} {
		if err := voiceStore(b.db).save(id, old, false); err != nil {
			t.Fatal(err)
		}
		if b.voices.allowed(old) || b.voices.get(id, "mint") != want || b.companionFor(peer{DeviceID: id}).Voice != want {
			t.Errorf("%q did not resolve to %q", old, want)
		}
		if raw, _, err := voiceStore(b.db).get(id); err != nil || raw != old {
			t.Errorf("reading %q altered the stored voice to %q (%v)", old, raw, err)
		}
	}
	const virtual = "virt-01234567"
	if err := b.virtual.save(virtual, virtualClient{ID: virtual, Character: "Story Cat", Voice: "bf_lily"}, false); err != nil {
		t.Fatal(err)
	}
	if got := b.voices.get(virtual, "mint"); got != emmaVoice {
		t.Fatalf("legacy virtual voice resolved to %q", got)
	}
	if _, err := b.db.Exec("INSERT INTO saved_companions ("+savedCompanionColumns+") VALUES (?,?,?,?,?,?,?,?,?)",
		"story-cat", "Story Cat", "#FFFFFF", "#000000", "#000000", "", "bm_daniel", "", 0); err != nil {
		t.Fatal(err)
	}
	out, err := b.companionTool(t.Context(), object{"op": "list_saved"})
	if err != nil || len(out["companions"].([]savedCompanion)) != 1 || out["companions"].([]savedCompanion)[0].Voice != fableVoice {
		t.Fatalf("saved companion voice did not resolve to Fable: %v (%v)", out, err)
	}
}

func TestMCPVoiceCatalog(t *testing.T) {
	want := []string{heartVoice, fableVoice, emmaVoice, "bm_george", "af_bella", "am_puck"}
	if len(voiceCatalog) != len(want) {
		t.Fatalf("got %d voice choices, want %d", len(voiceCatalog), len(want))
	}
	fields := mcpInputSchema("patch")["properties"].(object)
	for _, key := range []string{"device", "companion"} {
		schema := fields[key].(object)["properties"].(object)["voice"].(object)
		if got := schema["enum"]; !reflect.DeepEqual(got, want) {
			t.Errorf("MCP %s voice choices = %v, want %v", key, got, want)
		}
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

func TestDeviceVoiceDefaults(t *testing.T) {
	b, _ := wsHarness(t, func(b *larkServer, dir string) {
		b.voices = deviceVoiceStore(b.db, "")
		b.virtual = virtualStore(b.db)
		if err := b.virtual.save("virt-01234567", virtualClient{ID: "virt-01234567", Character: "Green Cat", Voice: fableVoice}, false); err != nil {
			t.Fatal(err)
		}
	})
	for _, tc := range []struct {
		device, character string
		want              string
	}{
		{"lark-33d8", "Cake", heartVoice},
		{"LARK-2CAC", "Other", heartVoice},
		{"lark-other", "Mint", fableVoice},
		{"virt-01234567", "Green Cat", fableVoice},
		{"virt-1234abcd", "Greg", heartVoice},
	} {
		if got := b.voiceFor(peer{DeviceID: tc.device, Character: tc.character}); got != tc.want {
			t.Errorf("%s: got voice %q, want %q", tc.device, got, tc.want)
		}
	}
	configured := deviceVoiceStore(b.db, emmaVoice)
	if !configured.allowed(heartVoice) ||
		configured.get("lark-33d8", "cake") != emmaVoice ||
		configured.get("lark-2cac", "mint") != fableVoice {
		t.Fatal("explicit fallback voice or the Mint default was not respected")
	}
}

func TestDeviceVoiceAPI(t *testing.T) {
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.voices = deviceVoiceStore(b.db, emmaVoice)
		b.virtual = virtualStore(b.db)
	})
	cake := testConnectWS(t, base, "unit", false)
	cake.hello(t, "lark-33d8", "Cake")
	profile := testObject(t, testRequest(t, "GET", base+"/api/scope", "unit", nil, 200))
	if profile["scoped"] != false {
		t.Fatal("LAN listener reported itself as scoped")
	}
	path := "/api/devices/LARK-33D8/voice"
	testRequest(t, "POST", base+path, "", object{"voice": fableVoice}, 401)
	for _, voice := range []any{"bad", "bf_alice", 1, nil} {
		testRequest(t, "POST", base+path, "unit", object{"voice": voice}, 400)
	}
	testRequest(t, "POST", base+path, "unit", object{"voice": heartVoice}, 200)
	got := testObject(t, testRequest(t, "POST", base+path, "unit", object{"voice": fableVoice}, 200))
	if got["device_id"] != "LARK-33D8" || got["voice"] != fableVoice {
		t.Fatalf("unexpected update response: %v", got)
	}
	if voice, _, err := voiceStore(b.db).get("lark-33d8"); err != nil || voice != fableVoice {
		t.Fatal("override not persisted under lowercase device ID")
	}
	if d := apiObjects(t, base+"/api/devices", "devices")[0].(object); d["voice"] != fableVoice {
		t.Fatal("LAN device list omitted the override")
	}
	testRequest(t, "POST", base+"/api/virtual-clients", "unit", object{"character": "Green", "theme": "mint", "voice": "unknown"}, 400)
	created := testObject(t, testRequest(t, "POST", base+"/api/virtual-clients", "unit", object{
		"character": "Green", "theme": "mint", "voice": "bm_george",
	}, 201))
	id := str(created["id"])
	if listed := apiObjects(t, base+"/api/virtual-clients", "virtual_clients"); len(listed) != 1 ||
		created["voice"] != "bm_george" || listed[0].(object)["voice"] != "bm_george" {
		t.Fatal("virtual client did not persist its initial voice")
	}
	v, ok := b.virtualByID(id)
	if !ok {
		t.Fatal("virtual client not found")
	}
	scoped := httptest.NewServer(b.scoped(v, "scoped-test"))
	defer scoped.Close()
	path = "/api/devices/" + id + "/voice"
	testRequest(t, "POST", scoped.URL+path, "unit", object{"voice": emmaVoice}, 401)
	testRequest(t, "POST", scoped.URL+"/api/devices/lark-33d8/voice", "scoped-test", object{"voice": emmaVoice}, 403)
	testRequest(t, "POST", scoped.URL+"/api/devices/virt-1234abcd/voice", "scoped-test", object{"voice": emmaVoice}, 403)
	testRequest(t, "GET", scoped.URL+path, "scoped-test", nil, 404)
	testRequest(t, "POST", scoped.URL+path, "scoped-test", object{"voice": heartVoice}, 200)
	scope := testObject(t, testRequest(t, "GET", scoped.URL+"/api/scope", "scoped-test", nil, 200))
	if scope["voice"] != heartVoice || !reflect.DeepEqual(profile["voices"], scope["voices"]) || len(scope["voices"].([]any)) != len(voiceCatalog) || b.voices.get("lark-33d8", "cake") != fableVoice {
		t.Fatal("scoped update did not stay on the scoped cat")
	}
}

func TestMenuVoicePicker(t *testing.T) {
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.voices = deviceVoiceStore(b.db, heartVoice)
	})
	const id = "lark-33d8"
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, id, "Cake")
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
		if reloaded := deviceVoiceStore(b.db, heartVoice); reloaded.get(id, "cake") != choice.ID {
			t.Errorf("voice picker did not persist %s", choice.ID)
		}
	}
	web.json(t, object{"ev": "tap", "id": "menu-back"})
	web.until(t, hasWS("svg", "menu-voice"))
	testRequest(t, "POST", base+"/api/devices/"+id+"/voice", "unit", object{"voice": "af_bella"}, 200)
	web.until(t, hasWS("svg", "Bella"))
}
