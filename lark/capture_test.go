package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCaptureAPIWritesLocalWAV(t *testing.T) {
	serial(t)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	b, base := wsHarness(t, func(b *larkServer, _ string) { b.endpoint = testEndpointSettings() })
	if _, err := b.createCompanion(t.Context(), object{"id": "cake", "name": "Cake", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	hw := testConnectWS(t, base, "unit", false)
	hw.helloHardware(t, "vocat-cap", "cake", capBridgeEndpoint|capDrag)

	type reply struct {
		body []byte
		err  error
	}
	done := make(chan reply, 1)
	go func() {
		body, _ := json.Marshal(object{"companion_id": "cake", "seconds": 1})
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/api/capture", bytes.NewReader(body))
		if err != nil {
			done <- reply{err: err}
			return
		}
		req.Header.Set("Content-Type", jsonType)
		req.Header.Set("X-Lark-Token", "unit")
		res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			done <- reply{err: err}
			return
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		done <- reply{body: raw}
	}()
	hw.until(t, isListenRecStart)
	hw.recStart(t, "")
	var seq uint16
	for range 50 { // one second of speech
		seq++
		hw.sendRTP(t, rtpPTRecPCM, seq, uint32(seq)*320, bytes.Repeat([]byte{0xd0, 0x07}, endpointBytes/2))
	}
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	var out object
	if err := json.Unmarshal(got.body, &out); err != nil {
		t.Fatalf("capture response %q: %v", got.body, err)
	}
	path := str(out["path"])
	if path == "" {
		t.Fatalf("capture failed: %s", got.body)
	}
	pcm, err := readWAVPCM16k(path)
	if err != nil || len(pcm) == 0 {
		t.Fatalf("capture WAV: %v (%d bytes)", err, len(pcm))
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("capture permissions: %v %v", info, err)
	}
}

func readCaptureEvent(t *testing.T, reader *bufio.Reader) object {
	t.Helper()
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading capture SSE: %v", err)
		}
		if !strings.HasPrefix(line, "event: capture") {
			continue
		}
		data, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading capture SSE data: %v", err)
		}
		var status object
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(data, "data: "))), &status); err != nil {
			t.Fatalf("capture SSE data %q: %v", data, err)
		}
		return status
	}
}

func waitCaptureListItem(t *testing.T, base, token, name string) object {
	t.Helper()
	deadline := time.Now().Add(testWait(t))
	for time.Now().Before(deadline) {
		raw := remoteJSON(t, http.MethodGet, base, token, "/remote/api/captures", nil, 200)
		items, _ := raw["captures"].([]any)
		for _, item := range items {
			m, _ := item.(object)
			if str(m["name"]) == name && jsonInt(m["size"]) > 44 {
				return m
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("capture %s never appeared in the list", name)
	return nil
}

func TestRemoteCaptureSSEAndRESTLifecycle(t *testing.T) {
	serial(t)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	h := newRemoteHarness(t)
	b := h.b
	b.endpoint = testEndpointSettings()
	b.setSTT(func(context.Context, []byte) (string, error) { return "hello from the capture", nil })
	dev := testConnectWS(t, h.base, "unit", false)
	dev.helloHardware(t, "vocat-cap", "greg", capBridgeEndpoint|capDrag)

	ctx, cancel := context.WithTimeout(t.Context(), testWait(t))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.base+"/remote/events?token="+url.QueryEscape(h.token), nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("events status %d", res.StatusCode)
	}
	reader := bufio.NewReader(res.Body)
	if initial := readCaptureEvent(t, reader); initial["state"] != "idle" {
		t.Fatalf("initial capture status = %+v", initial)
	}

	start := remoteJSON(t, http.MethodPost, h.base, h.token, "/remote/api/capture", object{"seconds": 2}, 200)
	name := str(start["name"])
	if strings.HasSuffix(name, ".wav") {
		t.Fatalf("capture name should be extension-free: %q", name)
	}
	if name == "" || str(start["id"]) != name || jsonInt(start["seconds"]) != 2 || str(start["started_at"]) == "" {
		t.Fatalf("unexpected capture start: %+v", start)
	}
	// Only one capture per companion at a time.
	remoteHTTP(t, http.MethodPost, h.base, h.token, "/remote/api/capture", object{"seconds": 2}, 409)

	dev.until(t, isListenRecStart)
	dev.recStart(t, "")
	var seq uint16
	for range 100 {
		seq++
		dev.sendRTP(t, rtpPTRecPCM, seq, uint32(seq)*320, bytes.Repeat([]byte{0xd0, 0x07}, endpointBytes/2))
	}

	sawRecording, sawProgress := false, false
	for {
		status := readCaptureEvent(t, reader)
		switch status["state"] {
		case "recording":
			sawRecording = true
			if str(status["name"]) != name {
				t.Fatalf("recording event named %q, want %q", str(status["name"]), name)
			}
			if jsonInt(status["remaining"]) < jsonInt(status["seconds"]) {
				sawProgress = true
			}
		case "done":
			if str(status["name"]) != name || status["ok"] != true {
				t.Fatalf("unexpected done event: %+v", status)
			}
			if !sawRecording {
				t.Fatal("capture finished without a recording event")
			}
			if !sawProgress {
				t.Fatal("capture progress was never throttled through a lower remaining count")
			}
			goto done
		}
	}
done:
	item := waitCaptureListItem(t, h.base, h.token, name)
	if jsonInt(item["seconds"]) != 2 {
		t.Fatalf("capture list duration = %+v", item)
	}

	// Range playback is token-scoped and serves the private WAV.
	wavReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		h.base+"/remote/api/captures/"+url.PathEscape(name)+".wav?token="+url.QueryEscape(h.token), nil)
	if err != nil {
		t.Fatal(err)
	}
	wavReq.Header.Set("Range", "bytes=0-99")
	wavRes, err := http.DefaultClient.Do(wavReq)
	if err != nil {
		t.Fatal(err)
	}
	partial, _ := io.ReadAll(wavRes.Body)
	wavRes.Body.Close()
	if wavRes.StatusCode != http.StatusPartialContent || len(partial) != 100 || wavRes.Header.Get("Content-Range") == "" {
		t.Fatalf("range response status=%d len=%d range=%q", wavRes.StatusCode, len(partial), wavRes.Header.Get("Content-Range"))
	}
	if kind := wavRes.Header.Get("Content-Type"); !strings.Contains(kind, "audio/wav") {
		t.Fatalf("WAV content type = %q", kind)
	}

	transcript := remoteJSON(t, http.MethodPost, h.base, h.token, "/remote/api/captures/"+url.PathEscape(name)+"/transcribe", object{}, 200)
	if str(transcript["transcript"]) != "hello from the capture" || str(transcript["engine"]) != sttEngine {
		t.Fatalf("unexpected transcript: %+v", transcript)
	}
	txtPath := filepath.Join(debugCaptureDir(), name+".txt")
	raw, err := os.ReadFile(txtPath)
	if err != nil || strings.TrimSpace(string(raw)) != "hello from the capture" {
		t.Fatalf("transcript sidecar: %q %v", raw, err)
	}
	if info, err := os.Stat(txtPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("transcript permissions: %v %v", info, err)
	}
	listed := waitCaptureListItem(t, h.base, h.token, name)
	if str(listed["transcript"]) != "hello from the capture" || listed["computing"] != false {
		t.Fatalf("transcript missing from list: %+v", listed)
	}

	remoteHTTP(t, http.MethodDelete, h.base, h.token, "/remote/api/captures/"+url.PathEscape(name), nil, 204)
	remoteHTTP(t, http.MethodGet, h.base, h.token, "/remote/api/captures", nil, 200)
	if _, err := os.Stat(wavPathForTest(name + ".wav")); !os.IsNotExist(err) {
		t.Fatalf("WAV survived delete: %v", err)
	}
	if _, err := os.Stat(txtPath); !os.IsNotExist(err) {
		t.Fatalf("transcript survived delete: %v", err)
	}
	remoteHTTP(t, http.MethodGet, h.base, h.token, "/remote/api/captures/"+url.PathEscape(name)+".wav", nil, 404)
}

func wavPathForTest(name string) string { return filepath.Join(debugCaptureDir(), name) }

func TestRemoteCaptureAuthValidationAndTraversal(t *testing.T) {
	serial(t)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	h := newRemoteHarness(t)
	remoteHTTP(t, http.MethodPost, h.base, "wrong", "/remote/api/capture", object{"seconds": 15}, 401)
	remoteHTTP(t, http.MethodGet, h.base, "", "/remote/api/captures", nil, 401)
	remoteHTTP(t, http.MethodDelete, h.base, "wrong", "/remote/api/captures/capture-greg-20260101-000000", nil, 401)

	remoteJSON(t, http.MethodPost, h.base, h.token, "/remote/api/capture", object{"seconds": 0}, 400)
	remoteJSON(t, http.MethodPost, h.base, h.token, "/remote/api/capture", object{"seconds": 61}, 400)
	// No terminal is attached to Greg in this harness.
	remoteJSON(t, http.MethodPost, h.base, h.token, "/remote/api/capture", object{"seconds": 15}, 409)

	if err := os.MkdirAll(debugCaptureDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	other := "capture-other-20260101-000000"
	if err := writeCaptureWAV(filepath.Join(debugCaptureDir(), other+".wav"), bytes.Repeat([]byte{1, 0}, 160)); err != nil {
		t.Fatal(err)
	}
	remoteHTTP(t, http.MethodGet, h.base, h.token, "/remote/api/captures/"+other+".wav", nil, 404)
	remoteHTTP(t, http.MethodDelete, h.base, h.token, "/remote/api/captures/"+other, nil, 404)
	remoteHTTP(t, http.MethodGet, h.base, h.token, "/remote/api/captures/..%2Fsecret.wav", nil, 404)
	if raw := remoteJSON(t, http.MethodGet, h.base, h.token, "/remote/api/captures", nil, 200); len(raw["captures"].([]any)) != 0 {
		t.Fatalf("another companion's capture leaked into the list: %+v", raw)
	}
}

func TestPruneCapturesRetentionCapAndCompanionDelete(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name string, age time.Duration, size int) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, bytes.Repeat([]byte{0}, size), 0o600); err != nil {
			t.Fatal(err)
		}
		mod := time.Now().Add(-age)
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	write("capture-greg-20260101-000000.wav", 20*24*time.Hour, 100)
	write("capture-greg-20260101-000000.txt", 20*24*time.Hour, 10)
	write("capture-greg-20260102-000000.wav", time.Hour, 100)
	write("capture-greg-20260102-000000.txt", time.Hour, 10)
	write("capture-milo-20260101-000000.wav", time.Hour, 100)

	removed, err := pruneCaptures(dir, 14*24*time.Hour, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 110 {
		t.Fatalf("pruned %d bytes; want both files of the expired capture", removed)
	}
	for _, name := range []string{"capture-greg-20260101-000000.wav", "capture-greg-20260101-000000.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived an expired prune", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "capture-greg-20260102-000000.wav")); err != nil {
		t.Fatalf("recent capture was pruned: %v", err)
	}

	// The byte cap prunes oldest-first even inside the age window.
	for i := range 3 {
		write(fmt.Sprintf("capture-greg-2026011%d-000000.wav", i), time.Duration(3-i)*time.Minute, 1000)
	}
	if _, err := pruneCaptures(dir, time.Hour, 1500); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "capture-greg-20260110-000000.wav")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("byte cap kept the oldest capture")
	}
	if _, err := os.Stat(filepath.Join(dir, "capture-greg-20260112-000000.wav")); err != nil {
		t.Fatalf("byte cap pruned the newest capture: %v", err)
	}

	if err := deleteCompanionCaptures(dir, "greg"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if owner, ok := captureCompanion(entry.Name()); ok && owner == "greg" {
			t.Fatalf("companion capture survived delete: %s", entry.Name())
		}
	}
}

func TestCaptureListIsCappedAndTranscriptReadIsBounded(t *testing.T) {
	serial(t)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	b, _ := wsHarness(t, nil)
	if err := os.MkdirAll(debugCaptureDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range captureListMax + 3 {
		name := fmt.Sprintf("capture-greg-20260101-%06d", i)
		if err := writeCaptureWAV(filepath.Join(debugCaptureDir(), name+".wav"), []byte{1, 0, 2, 0}); err != nil {
			t.Fatal(err)
		}
	}
	big := bytes.Repeat([]byte("x"), captureReadMax+1024)
	if err := os.WriteFile(filepath.Join(debugCaptureDir(), "capture-greg-20260101-000001.txt"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := b.captureFiles("greg")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != captureListMax {
		t.Fatalf("capture list length = %d; want %d", len(files), captureListMax)
	}
	for _, file := range files {
		if len(str(file["transcript"])) > captureReadMax {
			t.Fatalf("transcript was not bounded: %d bytes", len(str(file["transcript"])))
		}
	}
}

func TestWriteCaptureWAVCleansUpOnError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "already-a-directory")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeCaptureWAV(target, []byte{1, 0, 2, 0}); err == nil {
		t.Fatal("writing over a directory succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".capture-") {
			t.Fatalf("partial temp file survived: %s", entry.Name())
		}
	}
	ok := filepath.Join(dir, "capture-cake-20260101-000000.wav")
	if err := writeCaptureWAV(ok, []byte{1, 0, 2, 0}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(ok); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("capture permissions: %v %v", info, err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("capture directory permissions: %v %v", info, err)
	}
}
