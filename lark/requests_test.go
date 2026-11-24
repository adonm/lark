package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func decodeTestBody(t *testing.T, dst any, body string) error {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", jsonType)
	return decodeRequest(req, dst)
}

// A typed endpoint must accept its documented fields and reject anything
// else loudly; silent ignores are what let `companion` pass for
// `companion_id`.
func TestDecodeRequestRejectsUnknownAndMistypedFields(t *testing.T) {
	t.Parallel()
	var volume volumeRequest
	if err := decodeTestBody(t, &volume, `{"volume":40}`); err != nil || volume.Volume == nil || *volume.Volume != 40 {
		t.Fatalf("valid volume body = %+v, %v", volume, err)
	}
	var patch companionPatch
	if err := decodeTestBody(t, &patch, `{"name":"Greg","listening":true}`); err != nil ||
		patch.Name == nil || *patch.Name != "Greg" || patch.Listening == nil || !*patch.Listening {
		t.Fatalf("valid companion patch = %+v, %v", patch, err)
	}
	for _, tc := range []struct {
		dst        any
		body, want string
	}{
		{&volumeRequest{}, `{"volume":40,"companion":1}`, `companion: unknown field`},
		{&companionPatch{}, `{"name":"Greg","companion_id":"greg"}`, `companion_id: unknown field`},
		{&volumeRequest{}, `{"volume":"loud"}`, `volume: expected a number`},
		{&companionPatch{}, `{"listening":"yes"}`, `listening: expected a boolean`},
		{&captureRequest{}, `{"companion_id":"greg","secs":5}`, `secs: unknown field`},
	} {
		err := decodeTestBody(t, tc.dst, tc.body)
		var api *apiError
		if err == nil || !errors.As(err, &api) || api.status != 400 || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v; want 400 containing %q", tc.body, err, tc.want)
		}
	}
}

func TestDecodeInputTimerEdit(t *testing.T) {
	t.Parallel()
	var edit timerEdit
	if err := decodeInput(object{"op": "set", "in_seconds": json.Number("90"), "say": "tea"}, &edit); err != nil {
		t.Fatal(err)
	}
	if edit.Op == nil || *edit.Op != "set" || edit.InSeconds == nil || *edit.InSeconds != 90 || edit.Say == nil {
		t.Fatalf("timer edit = %+v", edit)
	}
	if err := decodeInput(object{"op": "set", "in_second": 5}, &edit); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown timer field = %v", err)
	}
}

func TestCaptureSecondsBoundsAndDefaults(t *testing.T) {
	t.Parallel()
	if got, err := captureSeconds(nil, captureDefaultSeconds); err != nil || got != captureDefaultSeconds {
		t.Fatalf("default = %d, %v", got, err)
	}
	for _, want := range []int{captureMinSeconds, 30, captureMaxSeconds} {
		if got, err := captureSeconds(&want, captureDefaultSeconds); err != nil || got != want {
			t.Fatalf("seconds %d = %d, %v", want, got, err)
		}
	}
	for _, bad := range []int{0, captureMaxSeconds + 1} {
		if _, err := captureSeconds(&bad, captureDefaultSeconds); err == nil || !strings.Contains(err.Error(), "seconds must be between") {
			t.Fatalf("seconds %d = %v", bad, err)
		}
	}
}

// The REST boundary rejects an unknown key with a field error instead of
// ignoring it and returning a companion that never changed.
func TestCompanionPatchRejectsUnknownField(t *testing.T) {
	t.Parallel()
	b, _ := wsHarness(t, nil)
	if _, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "an old chameleon"}); err != nil {
		t.Fatal(err)
	}
	body := `{"companion":"greg","listening":true}`
	req := httptest.NewRequest(http.MethodPatch, "/api/companions/greg", strings.NewReader(body))
	req.Header.Set("Content-Type", jsonType)
	req.Header.Set("X-Lark-Token", "unit")
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, req)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "companion: unknown field") {
		t.Fatalf("PATCH unknown field = %d %s", rec.Code, rec.Body.String())
	}
	if b.companionFor("greg").Listening {
		t.Fatal("unknown field was applied")
	}
}
