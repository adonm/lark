//go:build live

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const liveVoicePrompt = "What is a right triangle? Please explain in four complete sentences."

type liveClient struct {
	client      *http.Client
	base, token string
}

func (c liveClient) open(t *testing.T, path string, body []byte, media string, headers map[string]string) *http.Response {
	t.Helper()
	method := http.MethodGet
	if body != nil {
		method = http.MethodPost
	}
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("construct request for %s: %v", path, err)
	}
	if c.token != "" {
		req.Header.Set("X-Lark-Token", c.token)
	}
	if media != "" {
		req.Header.Set("Content-Type", media)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := c.client.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return res
}

func (c liveClient) read(t *testing.T, path string, body []byte, media string, headers map[string]string, status int) ([]byte, *http.Response) {
	t.Helper()
	res := c.open(t, path, body, media, headers)
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != status {
		t.Fatalf("%s: HTTP %d (want %d), read error: %v", path, res.StatusCode, status, err)
	}
	return raw, res
}

func liveJSON[T any](t *testing.T, c liveClient, path string, input any, raw []byte, media string, headers map[string]string) T {
	t.Helper()
	if input != nil {
		raw, _ = json.Marshal(input)
		media = jsonType
	}
	got, _ := c.read(t, path, raw, media, headers, 200)
	var v T
	if err := json.Unmarshal(got, &v); err != nil {
		t.Fatalf("%s returned invalid JSON: %v", path, err)
	}
	return v
}

func liveConfig(t *testing.T, home string) map[string]string {
	t.Helper()
	path := filepath.Join(env("XDG_CONFIG_HOME", filepath.Join(home, ".config")), "lark/lark.env")
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal("cannot read deployed bridge configuration")
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || (!strings.HasPrefix(key, "LARK_") && key != "TTS_MODEL_DIR" && key != "GGML_VK_VISIBLE_DEVICES") {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, `"`) {
			if unquoted, err := strconv.Unquote(value); err == nil {
				value = unquoted
			}
		} else if strings.HasPrefix(value, `'`) {
			value = strings.Trim(value, `'`)
		}
		out[key] = env(key, value)
	}
	for _, key := range []string{"LARK_TOKEN", "LARK_PORT", "LARK_OPENCODE_AGENT",
		"LARK_OPENCODE_URL", "LARK_OPENCODE_PASSWORD", "LARK_OPENCODE_DIR",
		"TTS_MODEL_DIR", "GGML_VK_VISIBLE_DEVICES"} {
		out[key] = env(key, out[key])
	}
	if !tokenRE.MatchString(out["LARK_TOKEN"]) {
		t.Fatal("missing live bridge token")
	}
	return out
}

func liveOpenCode(t *testing.T, client *http.Client, home string, conf map[string]string) upstream {
	t.Helper()
	state := env("XDG_STATE_HOME", filepath.Join(home, ".local/state"))
	api, err := openCodeUpstream(client, state, conf["LARK_OPENCODE_URL"], conf["LARK_OPENCODE_PASSWORD"])
	if err != nil {
		t.Fatal(err)
	}
	return api
}

func liveOpenCodeDir(home string, conf map[string]string) string {
	if dir := conf["LARK_OPENCODE_DIR"]; dir != "" {
		return dir
	}
	return filepath.Join(env("XDG_DATA_HOME", filepath.Join(home, ".local/share")), "lark/opencode/workspace")
}

func liveHealthy(t *testing.T, c liveClient) {
	t.Helper()
	health := liveJSON[object](t, c, "/health", nil, nil, "", nil)
	for _, k := range []string{"ok", "stt", "opencode", "tts"} {
		if health[k] != true {
			t.Fatalf("unhealthy live backend %s", k)
		}
	}
	if health["tts_engine"] != kokoroEngine {
		t.Fatal("TTS engine metadata mismatch")
	}
	if health["stt_engine"] != sttEngine {
		t.Fatal("STT engine metadata mismatch")
	}
}

func livePCMOK(pcm []byte) bool {
	if len(pcm) <= 8000 || len(pcm)%2 != 0 {
		return false
	}
	var power int64
	for i := 0; i < len(pcm); i += 2 {
		v := int64(int16(le.Uint16(pcm[i:])))
		power += v * v
	}
	return power > 100*100*int64(len(pcm)/2)
}

func liveSessions(t *testing.T, oc upstream, prefix string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		type session struct{ ID, Title string }
		path := "/api/session?" + url.Values{"search": {prefix}, "limit": {"50"}}.Encode()
		owned := func() ([]string, error) {
			found, err := ocData[[]session](ctx, oc, http.MethodGet, path, nil)
			var ids []string
			for _, s := range found {
				if strings.HasPrefix(s.Title, "Lark: ") && strings.Contains(s.Title, prefix) && sessionIDRE.MatchString(s.ID) {
					ids = append(ids, s.ID)
				}
			}
			return ids, err
		}
		ids, err := owned()
		if err != nil {
			t.Errorf("cannot locate sessions for cleanup: %v", err)
			return
		}
		for _, id := range ids {
			if err := oc.jsonCall(ctx, http.MethodDelete, "/api/session/"+id, nil, nil); err != nil {
				t.Errorf("cannot delete test session: %v", err)
			}
		}
		left, err := owned()
		if len(left) != 0 || err != nil {
			t.Errorf("session cleanup: %d left (%v)", len(left), err)
		} else {
			t.Logf("deleted %d OpenCode test session(s); 0 left", len(ids))
		}
	})
}

func assertLiveActor(t *testing.T, oc upstream, sid string, settings ocSettings) {
	t.Helper()
	if !sessionIDRE.MatchString(sid) {
		t.Fatal("missing OpenCode session ID")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	got, err := ocData[[]struct {
		Agent string
		Model ocModel
	}](ctx, oc, http.MethodGet, "/api/session/"+sid+"/message?order=desc&limit=1&type=assistant", nil)
	if err != nil || len(got) != 1 || got[0].Agent != settings.Agent || got[0].Model != settings.Model {
		t.Fatalf("wrong agent or model on final message: %v", err)
	}
}

func liveIsolated(t *testing.T, data string, conf map[string]string, token string, client *http.Client) liveClient {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "lark")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"lark/stt-models", "lark/tts-models"} {
		if err := os.Symlink(filepath.Join(data, rel), filepath.Join(root, rel)); err != nil {
			t.Fatal(err)
		}
	}
	binary := os.Getenv("LARK_TEST_BIN")
	if binary == "" || !filepath.IsAbs(binary) {
		t.Fatal("run scripts/check.sh --live to build an isolated bridge from this checkout")
	}
	cmd := exec.Command(binary, "serve")
	cmd.Env = append(os.Environ(), "XDG_DATA_HOME="+root, "LARK_TOKEN="+token, "LARK_HOST=127.0.0.1",
		"LARK_PORT=0", "LARK_DB_PATH="+filepath.Join(state, "lark.db"),
		"LARK_OTA_DIR="+root, "LARK_REPO_DIR=..",
		"LARK_OPENCODE_AGENT="+conf["LARK_OPENCODE_AGENT"],
		"LARK_OPENCODE_URL="+conf["LARK_OPENCODE_URL"], "LARK_OPENCODE_PASSWORD="+conf["LARK_OPENCODE_PASSWORD"],
		"LARK_OPENCODE_DIR="+filepath.Join(root, "workspace"), "LARK_OPENCODE_PAIR_PORT=0",
		"TTS_MODEL_DIR="+conf["TTS_MODEL_DIR"], "GGML_VK_VISIBLE_DEVICES="+conf["GGML_VK_VISIBLE_DEVICES"])
	logs, err := cmd.StderrPipe()
	if err != nil || cmd.Start() != nil {
		t.Fatal("cannot start isolated Go bridge")
	}
	addr, done := make(chan string, 1), make(chan error, 1)
	go func() {
		s := bufio.NewScanner(logs)
		for s.Scan() {
			if _, a, ok := strings.Cut(s.Text(), larkListeningPrefix); ok {
				addr <- a
			}
		}
		done <- cmd.Wait()
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("isolated bridge exited: %v", err)
			}
		case <-time.After(13 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("isolated bridge failed graceful shutdown")
		}
	})
	var c liveClient
	select {
	case a := <-addr:
		c = liveClient{client, "http://" + a, token}
	case <-time.After(5 * time.Second):
		t.Fatal("isolated bridge did not bind")
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if h := liveJSON[object](t, c, "/health", nil, nil, "", nil); h["tts"] == true {
			return c
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("isolated bridge TTS did not warm")
	return liveClient{}
}

func TestLive(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	data := env("XDG_DATA_HOME", filepath.Join(home, ".local/share"))
	conf := liveConfig(t, home)
	client := pooledClient()
	client.Timeout = 115 * time.Second
	defer client.CloseIdleConnections()
	port := conf["LARK_PORT"]
	if port == "" {
		port = "8765"
	}
	c := liveClient{client, env("LARK_TEST_URL", "http://"+net.JoinHostPort("127.0.0.1", port)), conf["LARK_TOKEN"]}
	liveHealthy(t, c)
	unauthorized := c
	unauthorized.token = ""
	unauthorized.read(t, "/api/devices", nil, "", nil, 401)
	unauthorized.read(t, "/api/chat", []byte("{}"), jsonType, nil, 401)
	unauthorized.read(t, "/ota/latest.bin", nil, "", nil, 401)
	run := "lark-live-" + hex.EncodeToString([]byte(strconv.FormatInt(time.Now().UnixNano(), 36)))

	liveDevices := func(t *testing.T) map[string]string {
		t.Helper()
		got := liveJSON[struct {
			Devices []struct {
				DeviceID string `json:"device_id"`
				IP       string `json:"ip"`
				Kind     string `json:"kind"`
				Port     int    `json:"port"`
				LastSeen int64  `json:"last_seen"`
			}
		}](t, c, "/api/devices", nil, nil, "", nil)
		devices := make(map[string]string, len(got.Devices))
		for _, p := range got.Devices {
			if p.Kind == "virtual" {
				continue
			}
			if p.DeviceID == "" || net.ParseIP(p.IP) == nil || p.Port <= 0 || p.LastSeen <= 0 || devices[p.DeviceID] != "" {
				t.Fatal("invalid or duplicate peer in live registry")
			}
			devices[p.DeviceID] = p.IP
		}
		return devices
	}
	var before map[string]string
	t.Run("Devices", func(t *testing.T) {
		before = liveDevices(t)
		t.Logf("%d registered peer addresses", len(before))
	})
	t.Run("OTA", func(t *testing.T) {
		for _, path := range []string{"/ota", "/ota/box3"} {
			manifest := liveJSON[struct {
				Exists bool
				Size   int64
				SHA256 string
				Mtime  int64
			}](t, c, path+"/version.json", nil, nil, "", nil)
			firmware, res := c.read(t, path+"/latest.bin", nil, "", nil, 200)
			digest := fmt.Sprintf("%x", sha256.Sum256(firmware))
			if !manifest.Exists || manifest.Mtime <= 0 || manifest.Size != int64(len(firmware)) ||
				manifest.Size != res.ContentLength || digest != manifest.SHA256 || digest != res.Header.Get("X-Firmware-Sha256") {
				t.Fatal("OTA image and manifest differ", path)
			}
			t.Logf("%s verified: %d bytes, SHA-256 %s", path, manifest.Size, digest)
		}
	})

	direct, res := c.read(t, "/api/tts", []byte(`{"text":"The Moon reflects sunlight."}`), jsonType, nil, 200)
	if res.Header.Get("Content-Type") != pcmType || !livePCMOK(direct) {
		t.Fatal("direct TTS returned invalid PCM")
	}
	t.Run("DeployedSTT", func(t *testing.T) {
		unauthorized.read(t, "/api/diagnostics/stt", []byte{}, "", nil, 401)
		probe := liveJSON[struct {
			Transcript string
			ClipMS     int `json:"clip_ms"`
			STTMS      int `json:"stt_ms"`
		}](t, c, "/api/diagnostics/stt", nil, []byte{}, "", nil)
		if !strings.EqualFold(probe.Transcript, "The moon reflects sunlight.") || probe.ClipMS < 500 {
			t.Fatal("deployed bridge did not transcribe the synthetic clip")
		}
		t.Logf("Parakeet decoded %d ms of synthetic speech in %d ms", probe.ClipMS, probe.STTMS)
	})
	t.Run("VoiceAndChat", func(t *testing.T) {
		settings := openCodeSettings("lark")
		settings.Model = ocModel{Provider: "test", ID: "deterministic"}
		stub, oc := liveModelStub(t, settings.Model)
		conf := maps.Clone(conf)
		conf["LARK_OPENCODE_URL"], conf["LARK_OPENCODE_PASSWORD"], conf["LARK_OPENCODE_AGENT"] = stub, "", settings.Agent
		iso := liveIsolated(t, data, conf, run, client)
		id := run + "-voice"
		board := testConnectWS(t, iso.base, iso.token, false)
		board.timeout = 25 * time.Second
		board.helloV2(t, id, "LiveVoice")
		viewer := testConnectWS(t, iso.base, iso.token, true)
		viewer.timeout = 25 * time.Second
		viewer.watch(t, id)
		input, _ := json.Marshal(object{"text": liveVoicePrompt})
		recording, _ := iso.read(t, "/api/tts", input, jsonType, nil, 200)
		board.recStart(t, "")
		var seq uint16
		for offset := 0; offset < len(recording); offset += rtpPCMBytes {
			chunk := recording[offset:min(offset+rtpPCMBytes, len(recording))]
			seq++
			board.sendRTP(t, rtpPTRecPCM, seq, uint32(seq)*rtpSamples, 1, chunk)
		}
		board.recEnd(t, "", false)
		board.until(t, func(f wsFrame) bool {
			p := framePayload(f)
			return hasProto(msgChime, "")(f) && len(p) >= 1 && p[0] == chimeThink
		})
		board.until(t, func(f wsFrame) bool {
			p := framePayload(f)
			return hasProto(msgTTSCtrl, "")(f) && len(p) >= 1 && p[0] == ttsOpStart
		})
		var spoken []byte
		end := board.until(t, func(f wsFrame) bool {
			if isPlayback(f) {
				spoken = append(spoken, framePayload(f)...)
			}
			p := framePayload(f)
			return hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == ttsOpEnd
		})
		if !livePCMOK(spoken) {
			t.Fatal("voice turn did not return playable PCM")
		}
		board.sendProto(t, msgTTSAck, encodeTTSAck(false, binary.LittleEndian.Uint64(framePayload(end)[1:9])))
		board.write(t, maskedFrame(wsClose, true, wsCloseFrame(1000).data))
		board.until(t, func(f wsFrame) bool { return f.op == wsClose })
		viewer.until(t, hasProto(msgPeers, ""))
		entries := liveJSON[struct{ Messages []chatEntry }](t, iso, "/api/devices/"+id+"/messages", nil, nil, "", nil).Messages
		if len(entries) != 2 || entries[0].Source != "voice" || !strings.Contains(strings.ToLower(entries[0].Text), "right triangle") {
			t.Fatal("voice question and answer not in canonical history")
		}

		follow := liveJSON[struct {
			Reply     string
			SessionID string `json:"session_id"`
			AudioURL  string `json:"audio_url"`
		}](t, iso, "/api/chat", object{"device_id": id, "text": "Repeat in one sentence, without tools: Paris is the capital of France."}, nil, "", nil)
		assertLiveActor(t, oc, follow.SessionID, settings)
		if !strings.Contains(strings.ToLower(follow.Reply), "paris") || !strings.HasPrefix(follow.AudioURL, "/api/audio/") {
			t.Fatal("remembered device could not chat and play a text reply")
		}
		pcm, res := iso.read(t, follow.AudioURL, nil, "", nil, 200)
		replay, fixed := iso.read(t, follow.AudioURL, nil, "", nil, 200)
		if !livePCMOK(pcm) || res.Header.Get("Content-Type") != pcmType || !bytes.Equal(pcm, replay) || fixed.ContentLength != int64(len(pcm)) {
			t.Fatal("live chat audio was silent, corrupt, or not byte-identical on replay")
		}
		messages, err := ocData[[]struct{ Text string }](t.Context(), oc, http.MethodGet, "/api/session/"+follow.SessionID+"/message?order=asc&limit=10&type=user", nil)
		if err != nil || len(messages) != 2 || !strings.HasPrefix(messages[0].Text, "[LiveVoice ("+id+") · hardware · voice]") {
			t.Fatal("HTTP chat did not reuse the WebSocket voice session or preserve provenance")
		}
		t.Logf("validated voice capture, playback, canonical history, HTTP follow-up and %d replayed PCM bytes", len(pcm))

		board = testConnectWS(t, iso.base, iso.token, false)
		board.helloV2(t, id, "LiveVoice")
		viewer = testConnectWS(t, iso.base, iso.token, true)
		viewer.watch(t, id)
		board.text(t, liveQuotaPrompt)
		viewer.until(t, hasSVG("Try again"))
		board.write(t, maskedFrame(wsPing, true, nil))
		board.until(t, func(f wsFrame) bool {
			p := framePayload(f)
			if (hasProto(msgTTSCtrl, "")(f) && len(p) >= 1 && p[0] == ttsOpStart) || isPlayback(f) {
				t.Fatal("model rejection unexpectedly started audio")
			}
			return f.op == wsPong
		})
		t.Log("stub model 429 produced the WebSocket error scene instead of TTS")
	})
	t.Run("DevicesAfter", func(t *testing.T) {
		if !maps.Equal(before, liveDevices(t)) {
			t.Fatal("live tests changed the deployed peer registry")
		}
		t.Logf("%d registered peer addresses unchanged", len(before))
	})
}

const liveQuotaPrompt = "Simulate a model usage limit."

// Exercise the production HTTP adapter using OpenCode V2's final projection.
// The untagged WS tests exercise the eager SSE path separately.
func liveModelStub(t *testing.T, model ocModel) (string, upstream) {
	t.Helper()
	const sid = "ses-live-model-stub"
	const path = "/api/session/" + sid
	var messages []object
	var mu sync.Mutex
	prompts := 0
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var data any
		switch r.Method + " " + r.URL.Path {
		case "GET /api/info":
			data = object{}
		case "GET /api/agent/lark":
			data = object{"id": "lark", "model": model}
		case "POST /api/session", "GET " + path:
			data = object{"id": sid, "agent": "lark", "model": model, "permissions": openCodeSettings("lark").Permissions}
		case "POST " + path + "/prompt":
			var input struct{ Text string }
			if json.NewDecoder(r.Body).Decode(&input) != nil {
				w.WriteHeader(400)
				return
			}
			prompts++
			id := strconv.Itoa(prompts)
			data = object{"id": "user-" + id}
			messages = append(messages, object{"id": "user-" + id, "type": "user", "text": input.Text})
			switch p := strings.ToLower(input.Text); {
			case strings.HasSuffix(input.Text, liveQuotaPrompt):
				messages = append(messages, object{"id": "idle-" + id, "type": "idle", "outcome": "failed", "error": object{"status": 429}})
				writeStubJSON(w, data)
				return
			case strings.Contains(p, "right triangle"):
				input.Text = "A right triangle has a ninety degree angle. It looks like a corner."
			case strings.Contains(p, "paris"):
				input.Text = "Paris is the capital of France."
			default:
				t.Error("stub model received an unexpected prompt")
				w.WriteHeader(400)
				return
			}
			messages = append(messages, object{"id": "assistant-" + id, "type": "assistant", "agent": "lark", "model": model,
				"finish": "stop", "content": []object{{"type": "text", "text": input.Text}}})
		case "POST /api/experimental/session/" + sid + "/wait", "POST " + path + "/interrupt":
			data = object{}
		case "GET " + path + "/message":
			rows := make([]object, 0, len(messages))
			for _, row := range messages {
				if kind := r.URL.Query().Get("type"); kind == "" || row["type"] == kind {
					rows = append(rows, row)
				}
			}
			if r.URL.Query().Get("order") == "desc" {
				slices.Reverse(rows)
			}
			if limit, _ := strconv.Atoi(r.URL.Query().Get("limit")); limit > 0 && limit < len(rows) {
				rows = rows[:limit]
			}
			data = rows
		default:
			w.WriteHeader(404)
			return
		}
		writeStubJSON(w, data)
	}))
	t.Cleanup(stub.Close)
	api, err := openCodeUpstream(stub.Client(), "", stub.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	return stub.URL, api
}

func writeStubJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", jsonType)
	_ = json.NewEncoder(w).Encode(object{"data": data})
}
