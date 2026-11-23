package main

import (
	"bytes"
	"encoding/binary"
	"net/http"
	"testing"
)

func TestTerminalHitsRoute(t *testing.T) {
	t.Parallel()
	b, base := wsHarness(t, nil)
	testRequest(t, http.MethodPost, base+"/api/companions", "unit", object{"name": "Cake", "form": "a cat"}, 201)

	hw := testConnectWS(t, base, "unit", false)
	hw.helloV2(t, "vocat-33d8", "Cake")

	// Requires admin token.
	testRequest(t, http.MethodGet, base+"/api/terminals/vocat-33d8/hits", "", nil, 401)
	testRequest(t, http.MethodGet, base+"/api/terminals/unknown/hits", "unit", nil, 404)

	res := testObject(t, testRequest(t, http.MethodGet, base+"/api/terminals/vocat-33d8/hits", "unit", nil, 200))
	if res["terminal_id"] != "vocat-33d8" || res["screen"] != "home" || res["state"] != "idle" {
		t.Fatalf("unexpected hits header: %v", res)
	}
	if _, ok := numInt(res["rev"]); !ok {
		t.Fatalf("expected numeric rev in hits response, got %v", res["rev"])
	}
	hits, _ := res["hits"].([]any)
	var menuHit object
	for _, item := range hits {
		m, _ := item.(object)
		if m["id"] == "menu" {
			menuHit = m
			break
		}
	}
	if menuHit == nil || menuHit["label"] != "Menu" || menuHit["action"] != "menu" {
		t.Fatalf("expected labeled Menu hit, got %v", hits)
	}
	_ = b
}

func TestTTSRoute(t *testing.T) {
	t.Parallel()
	b, base := wsHarness(t, nil)
	b.worker = newSynthWorker(b.root, func() (speechEngine, error) {
		return fakeSpeechEngine(func(_ string, _ voiceChoice, _ func() bool) ([]float32, int) {
			return []float32{0.1, -0.1}, sampleRate
		}), nil
	})

	// Requires admin token.
	testRequest(t, http.MethodPost, base+"/api/tts", "", object{"text": "hi"}, 401)
	testRequest(t, http.MethodPost, base+"/api/tts", "unit", object{"text": ""}, 400)

	raw := testRequest(t, http.MethodPost, base+"/api/tts", "unit", object{"voice": heartVoice, "text": "hello"}, 200)
	if len(raw) != 48 || !bytes.Equal(raw[:4], []byte("RIFF")) || !bytes.Equal(raw[8:12], []byte("WAVE")) {
		t.Fatalf("expected 48-byte WAV, got %d bytes", len(raw))
	}
	if got := binary.LittleEndian.Uint32(raw[40:44]); got != 4 {
		t.Fatalf("expected data chunk size 4, got %d", got)
	}
}

func TestLeaveSceneResetsCompanionStatus(t *testing.T) {
	t.Parallel()
	b, base := wsHarness(t, nil)
	hw := testConnectWS(t, base, "unit", false)
	const id = "vocat-stop-status"
	hw.helloV2(t, id, "Cake")

	b.hub.mu.Lock()
	d := b.hub.terminals[id]
	b.hub.setCompanionStatus(d.companionID, statusSpeaking)
	d.ttsStarted = true
	b.hub.tapStop(d, d.last)
	st := b.hub.companionStatuses[d.companionID]
	b.hub.mu.Unlock()
	if st.state != statusIdle {
		t.Fatalf("tapStop left companion status = %d, want statusIdle (%d)", st.state, statusIdle)
	}
}
