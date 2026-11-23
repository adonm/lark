package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// After a restart the MCP catalog refresh runs in the background. The first
// turn used to fail with "OpenCode MCP catalog refresh pending"; it must wait
// for the refresh and then create its session.
func TestFirstTurnWaitsForMCPCatalogRefresh(t *testing.T) {
	serial(t)
	const id, sid = "lark-contract", "ses-contract"
	o := testOpenCode(t, t.Context(), t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session":
			sendJSON(w, 200, object{"data": object{"id": sid}})
		case "/api/experimental/session/" + sid + "/instructions/entries/lark.companion":
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	})
	o.settings = ocSettings{Agent: "lark", Model: ocModel{"provider", "model", ""}}
	o.contract = "catalog-after-restart"
	o.contractWait = 5 * time.Second
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = o.db.setMeta(context.Background(), mcpSchemaMeta, o.contract)
	}()
	started := time.Now()
	got, err := o.ensureSession(t.Context(), companionState{ID: id, companionLook: companionLook{Name: "Cat"}}, true)
	if err != nil || got != sid {
		t.Fatalf("first turn after restart: session %q, %v", got, err)
	}
	if waited := time.Since(started); waited < 100*time.Millisecond {
		t.Fatalf("session was created before the catalog refresh landed (%s)", waited)
	}
}

func TestTurnFailsClearlyWhenCatalogRefreshNeverLands(t *testing.T) {
	t.Parallel()
	o := testOpenCode(t, t.Context(), t.TempDir(), func(http.ResponseWriter, *http.Request) {})
	o.settings = ocSettings{Agent: "lark", Model: ocModel{"provider", "model", ""}}
	o.contract = "catalog-never"
	o.contractWait = 150 * time.Millisecond
	if _, err := o.ensureSession(t.Context(), companionState{ID: "lark-contract", companionLook: companionLook{Name: "Cat"}}, true); err == nil ||
		!strings.Contains(err.Error(), "catalog refresh pending") {
		t.Fatalf("want a bounded refresh error, got %v", err)
	}
}

func TestHealthReportsPerModelState(t *testing.T) {
	t.Parallel()
	b := &larkServer{models: newModelSet(time.Now()), endpoint: &endpointSettings{}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	body, err := b.route(rec, req, "/health")
	if err != nil {
		t.Fatal(err)
	}
	health, _ := body.(object)
	models, _ := health["models"].(object)
	for _, name := range []string{"parakeet", "kokoro", "smart_turn", "silero"} {
		model, _ := models[name].(object)
		if str(model["state"]) != modelLoading {
			t.Fatalf("%s state = %q, want loading", name, str(model["state"]))
		}
	}
	if health["models_ready"] != false {
		t.Fatalf("models_ready = %v while loading", health["models_ready"])
	}
	b.models.stt.setReady()
	b.models.tts.setReady()
	b.models.smartTurn.setReady()
	b.models.vad.setReady()
	body, err = b.route(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil), "/health")
	if err != nil {
		t.Fatal(err)
	}
	models, _ = body.(object)["models"].(object)
	for name, raw := range models {
		if state := str(raw.(object)["state"]); state != modelReady {
			t.Fatalf("%s state = %q, want ready", name, state)
		}
	}
}

func TestDecodeClipWaitsForParakeet(t *testing.T) {
	serial(t)
	b := &larkServer{models: newModelSet(time.Now())}
	want := func(context.Context, []byte) (string, error) { return "hello", nil }
	go func() {
		time.Sleep(150 * time.Millisecond)
		b.setSTT(want)
		b.models.stt.setReady()
	}()
	text, err := b.decodeClip(t.Context(), []byte{0, 0})
	if err != nil || text != "hello" {
		t.Fatalf("decode while Parakeet loads: %q, %v", text, err)
	}
}

func TestDecodeClipReportsFailedModel(t *testing.T) {
	t.Parallel()
	b := &larkServer{models: newModelSet(time.Now())}
	b.models.stt.setError(errVADLoad)
	if _, err := b.decodeClip(t.Context(), []byte{0, 0}); err == nil ||
		!strings.Contains(err.Error(), "Parakeet unavailable") {
		t.Fatalf("want a named load error, got %v", err)
	}
}

func TestTTSWorkerIsAvailableWhileLoading(t *testing.T) {
	t.Parallel()
	root, stop := context.WithCancel(t.Context())
	defer stop()
	loaded := make(chan struct{})
	state := newModelState("Kokoro", time.Now())
	worker := newSynthWorkerWithState(root, state, func() (speechEngine, error) {
		<-loaded
		return fakeSpeechEngine(func(string, voiceChoice, func() bool) ([]float32, int) {
			return []float32{0.25}, sampleRate
		}), nil
	})
	defer func() { stop(); <-worker.done }()
	if !worker.available() {
		t.Fatal("a loading TTS model must still accept a turn's speech")
	}
	close(loaded)
	if _, err := worker.synthesize(t.Context(), heartVoice, "Hi"); err != nil {
		t.Fatal(err)
	}
	if !worker.available() || !worker.ready.Load() || !state.isReady() {
		t.Fatal("worker did not become available")
	}
}
