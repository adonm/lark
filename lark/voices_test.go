package main

import (
	"context"
	"reflect"
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
		{heartVoice, "en-us", 3}, {lilyVoice, "en-gb", 23}, {fableVoice, "en-gb", 25},
		{"af_bella", "en-us", 2}, {"am_puck", "en-us", 18}, {"bm_george", "en-gb", 26},
		{"bf_alice", "en-gb", 20}, {"bf_isabella", "en-gb", 22},
		{"", "en-us", 3},
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
	for _, bad := range []string{"bf_emma", "bm_lewis", "unsupported"} {
		engine := fakeSpeechEngine(func(string, voiceChoice, func() bool) ([]float32, int) {
			t.Fatalf("unknown voice %q should not reach engine", bad)
			return nil, 0
		})
		if _, err := synthesizePCM(context.Background(), engine, bad, "Hello"); err == nil {
			t.Errorf("unknown voice %q succeeded; want error", bad)
		}
	}
}

func TestUnknownStoredVoiceErrors(t *testing.T) {
	b, _ := wsHarness(t, func(b *larkServer, dir string) {
		b.voices = companionVoiceStore(b.db, "")
	})
	const id = "legacy"
	if _, err := b.createCompanion(t.Context(), object{"id": id, "name": "Legacy", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"bf_emma", "bf_kelly", "bm_daniel", "bm_lewis", "retired"} {
		if _, err := b.db.Exec(`UPDATE companions SET voice=? WHERE id=?`, bad, id); err != nil {
			t.Fatal(err)
		}
		if b.voices.allowed(bad) {
			t.Errorf("%q unexpectedly allowed", bad)
		}
		if _, ok := b.companionByID(id); ok {
			t.Errorf("companionByID(%q) succeeded with unknown stored voice %q", id, bad)
		}
		if _, err := b.allCompanions(t.Context()); err == nil {
			t.Errorf("allCompanions succeeded with unknown stored voice %q", bad)
		}
	}
}

func TestMCPVoiceCatalog(t *testing.T) {
	want := []string{heartVoice, fableVoice, lilyVoice, "bm_george", "af_bella", "am_puck", "bf_alice", "bf_isabella"}
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
	for _, voice := range []string{lilyVoice, fableVoice, lilyVoice, fableVoice} {
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
	if _, err := b.createCompanion(t.Context(), object{"id": "green", "name": "Green Cat", "voice": fableVoice, "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.createCompanion(t.Context(), object{"id": "mint", "name": "Mint", "form": "a cat"}); err != nil {
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
	configured := companionVoiceStore(b.db, lilyVoice)
	if !configured.allowed(heartVoice) ||
		configured.get("cake", "cake") != lilyVoice ||
		configured.get("mint", "mint") != fableVoice {
		t.Fatal("explicit fallback voice or the Mint default was not respected")
	}
}

func TestCompanionVoiceAPI(t *testing.T) {
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.voices = companionVoiceStore(b.db, lilyVoice)
	})
	cake := testConnectWS(t, base, "unit", false)
	cake.hello(t, "vocat-33d8", "Cake")
	path := "/api/companions/CAKE/voice"
	testRequest(t, "POST", base+path, "", object{"voice": fableVoice}, 401)
	for _, voice := range []any{"bad", "bf_emma", 1, nil} {
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
	testRequest(t, "POST", base+"/api/companions", "unit", object{"name": "Green", "voice": "unknown", "form": "a cat"}, 400)
	created := testObject(t, testRequest(t, "POST", base+"/api/companions", "unit", object{
		"name": "Green", "form": "a green cat", "voice": "bm_george",
	}, 201))
	_ = str(created["id"])
	if listed := apiObjects(t, base+"/api/companions", "companions"); len(listed) != 2 ||
		created["voice"] != "bm_george" || listed[1].(object)["voice"] != "bm_george" {
		t.Fatal("companion did not persist its initial voice")
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
	c.tapMenu(t)
	waitSVG(t, b.hub, termID, "menu-next")
	c.tap(t, 248, 294, 0)
	p2svg := waitSVG(t, b.hub, termID, "menu-voice")
	vx, vy, ok := hitCenter(p2svg, "menu-voice")
	if !ok {
		t.Fatal("missing menu-voice hit")
	}
	c.tap(t, vx, vy, 0)
	svg := waitSVG(t, b.hub, termID, "voice-af_heart")
	for _, voice := range []int{1, 0} {
		choice := voiceCatalog[voice]
		b.hub.mu.Lock()
		hits := b.hub.terminals[termID].svg.hits
		b.hub.mu.Unlock()
		var tx, ty int
		found := false
		for _, h := range hits {
			if h.id == "voice-"+choice.ID {
				tx, ty, found = (h.x1+h.x2)/2, (h.y1+h.y2)/2, true
				break
			}
		}
		if !found {
			t.Fatalf("missing hit for %s in %v", choice.Name, hits)
		}
		c.tap(t, tx, ty, 0)
		c.barrier(t)
		waitSVG(t, b.hub, termID, "voice")
		if reloaded := companionVoiceStore(b.db, heartVoice); reloaded.get(id, "cake") != choice.ID {
			t.Errorf("voice picker did not persist %s", choice.ID)
		}
	}
	bx, by, ok := hitCenter(svg, "menu-back")
	if !ok {
		t.Fatal("missing menu-back hit")
	}
	c.tap(t, bx, by, 0)
	waitSVG(t, b.hub, termID, "menu-voice")
	testRequest(t, "POST", base+"/api/companions/"+id+"/voice", "unit", object{"voice": "af_bella"}, 200)
	if got := companionVoiceStore(b.db, heartVoice).get(id, "cake"); got != "af_bella" {
		t.Fatalf("voice API did not persist af_bella: %q", got)
	}
}
