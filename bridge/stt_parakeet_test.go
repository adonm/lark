package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestParakeetClipInput(t *testing.T) {
	voiced := []byte{0, 128, 0, 64}
	samples := parakeetSamples(voiced)
	pad := sampleRate * 150 / 1000
	if len(samples) != sampleRate*1900/1000 || samples[pad-1] != 0 ||
		samples[pad] != -1 || samples[pad+1] != .5 || samples[pad+2] != 0 {
		t.Fatal("short clip failed Hex padding/scaling")
	}
	long := make([]byte, 2*sampleRate*2)
	copy(long[len(long)-len(voiced):], voiced)
	samples = parakeetSamples(long)
	end := pad + len(long)/2
	if len(samples) != end+pad+sampleRate*400/1000 || samples[end-1] != .5 || samples[end] != 0 {
		t.Fatal("2 s clip failed Hex padding/scaling")
	}
	if n := len(parakeetSamples(make([]byte, 2*sampleRate*2))); n != sampleRate*2400/1000 {
		t.Fatalf("silent clip unexpectedly got EchoEar padding: %d", n)
	}
}

func TestParakeetText(t *testing.T) {
	for input, want := range map[string]string{
		"  Start a timer for 90 seconds.  ":                         "Start a timer for 90 seconds.",
		"See you soon, SUBTITLES BY THE AMARA.ORG COMMUNITY.":       "See you soon",
		"Subtitles by the Amara.org community were helpful.":        "Subtitles by the Amara.org community were helpful.",
		"Sous-titrage ST' 501; Untertitel der Amara.org-Community.": "",
	} {
		if got := cleanParakeetText(input); got != want {
			t.Errorf("clean(%q) = %q; want %q", input, got, want)
		}
	}
}

func TestParakeetRejectsWrongModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), parakeetModel)
	if err := checkParakeetModel(path); err == nil {
		t.Fatal("accepted missing model")
	}
	if err := os.WriteFile(path, []byte("not the pinned GGUF"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkParakeetModel(path); err == nil {
		t.Fatal("accepted unpinned model")
	}
}

func TestSTTProbeRequiresLoopbackAndBridgeToken(t *testing.T) {
	b := &bridge{token: "unit"}
	for _, tc := range []struct {
		addr, token string
		want        int
	}{
		{"127.0.0.1:1234", "", 401},
		{"10.0.0.1:1234", "unit", 404},
		{"[::1]:1234", "unit", 503}, // Authorized; no engines in this fixture.
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/diagnostics/stt", nil)
		r.RemoteAddr = tc.addr
		r.Header.Set("X-Lark-Token", tc.token)
		w := httptest.NewRecorder()
		b.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s: HTTP %d, want %d", tc.addr, w.Code, tc.want)
		}
	}
}
