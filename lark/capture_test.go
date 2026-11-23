package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestCaptureAPIWritesLocalWAV(t *testing.T) {
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
