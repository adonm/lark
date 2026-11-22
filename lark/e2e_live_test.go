//go:build live

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type e2eSuite struct {
	liveClient
	parent, workspace string
	prefix            string
	model             *e2eModel
	real              bool
}

func e2eWrite(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil || os.WriteFile(path, data, mode) != nil {
		t.Fatal("cannot write isolated fixture:", filepath.Base(path))
	}
}

func startE2E(t *testing.T, real bool) *e2eSuite {
	t.Helper()
	bin := os.Getenv("LARK_TEST_BIN")
	if !filepath.IsAbs(bin) {
		t.Fatal("use scripts/check.sh --live or --e2e-real to build the native bridge")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	data := env("XDG_DATA_HOME", filepath.Join(home, ".local/share"))
	root := t.TempDir()
	local := filepath.Join(root, "data/lark")
	client := pooledClient()
	client.Timeout = 100 * time.Second
	t.Cleanup(client.CloseIdleConnections)
	s := &e2eSuite{liveClient: liveClient{client: client, token: randomHex(32)},
		parent: randomHex(32), workspace: filepath.Join(root, "workspace"), prefix: "E2E-" + randomHex(5), real: real}
	config := filepath.Join(root, "config")
	e2eWrite(t, filepath.Join(config, "lark/parent.token"), []byte(s.parent), 0o600)
	e2eWrite(t, filepath.Join(root, "bin/opentunnel"), []byte(e2eTunnelCLI), 0o700)
	for _, name := range []string{"stt-models", "tts-models"} {
		if err := os.Symlink(filepath.Join(data, "lark", name), filepath.Join(local, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := checkEndpointModel(filepath.Join(local, "stt-models", sileroModel)); err != nil {
		t.Fatal(err)
	}
	repo, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	s.model = startE2EModel(t, s, home)
	cmd := exec.Command(bin, "serve")
	// Never inherit cloud credentials, the installed bridge token/DB, or the actual CLI home.
	cmd.Env = []string{
		"HOME=" + root, "PATH=" + filepath.Join(root, "bin") + ":/usr/bin:/bin",
		"XDG_CONFIG_HOME=" + config, "XDG_DATA_HOME=" + filepath.Join(root, "data"),
		"XDG_STATE_HOME=" + filepath.Join(root, "state"), "XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
		"LARK_HOST=127.0.0.1", "LARK_PORT=0", "LARK_TOKEN=" + s.token,
		"LARK_DB_PATH=" + filepath.Join(root, "lark.db"), "LARK_REPO_DIR=" + repo,
		"LARK_OTA_DIR=" + root, "LARK_BOX3_OTA_DIR=" + root,
		"LARK_OPENCODE_URL=" + s.model.url, "LARK_OPENCODE_DIR=" + s.workspace,
	}
	prefs := e2eInstallation(t, home, false)
	for _, key := range []string{"TTS_MODEL_DIR", "GGML_VK_VISIBLE_DEVICES"} {
		if v := env(key, prefs[key]); v != "" {
			cmd.Env = append(cmd.Env, key+"="+v)
		}
	}
	log, err := cmd.StderrPipe()
	if err != nil || cmd.Start() != nil {
		t.Fatal("cannot start isolated bridge")
	}
	addr, done := make(chan string, 1), make(chan error, 1)
	go func() {
		lines := bufio.NewScanner(log)
		for lines.Scan() {
			if _, a, ok := strings.Cut(lines.Text(), larkListeningPrefix); ok {
				addr <- a
			}
		}
		done <- cmd.Wait() // Never emit raw process logs: they may contain URLs or model output.
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-done:
			if err != nil {
				t.Error("isolated bridge did not exit cleanly")
			}
		case <-time.After(13 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("isolated bridge did not shut down")
		}
	})
	select {
	case a := <-addr:
		host, port, err := net.SplitHostPort(a)
		if err != nil || host != "127.0.0.1" || port == "8765" {
			t.Fatal("bridge did not bind to an isolated loopback port")
		}
		s.base = "http://" + a
	case err := <-done:
		done <- err
		t.Fatal("isolated bridge exited before listening; check local native model assets")
	case <-time.After(25 * time.Second):
		t.Fatal("isolated bridge did not listen")
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		h := liveJSON[object](t, s.liveClient, "/health", nil, nil, "", nil)
		if h["tts"] == true {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("isolated Kokoro did not warm")
		}
		time.Sleep(100 * time.Millisecond)
	}
	liveHealthy(t, s.liveClient)
	if s.real {
		s.model.connectMCP(t, s)
	}
	deadline = time.Now().Add(20 * time.Second)
	for {
		report := liveJSON[diagReport](t, s.liveClient, "/api/diagnostics", nil, nil, "", nil)
		if report.Refresh.State == "current" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("isolated MCP catalog did not refresh")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return s
}

func e2eInstallation(t *testing.T, home string, required bool) map[string]string {
	t.Helper()
	path := filepath.Join(env("XDG_CONFIG_HOME", filepath.Join(home, ".config")), "lark/lark.env")
	raw, err := os.ReadFile(path)
	if required && err != nil {
		t.Fatal("cannot read private dedicated OpenCode configuration")
	}
	values := map[string]string{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		k, v, _ := strings.Cut(line, "=")
		switch strings.TrimSpace(k) {
		case "LARK_OPENCODE_PASSWORD":
			if !required {
				continue
			}
			fallthrough
		case "TTS_MODEL_DIR", "GGML_VK_VISIBLE_DEVICES":
			values[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return values
}

const e2eTunnelCLI = `#!/bin/sh
case "$1 $2" in
  "status ") printf 'Hostname: test.opentunnel.example\nService: running\n';;
  "route add") case "$3" in 127.0.0.1:*) printf 'Route added: https://scoped.test.opentunnel.example → %s\n' "$3";; *) exit 2;; esac;;
  "route remove") [ "$3" = scoped ];;
  *) exit 2;;
esac
`

func (s *e2eSuite) request(t *testing.T, method, path string, body any, status int) []byte {
	t.Helper()
	return e2eRequest(t, s.liveClient, method, path, "", body, status)
}

func e2eRequest(t *testing.T, c liveClient, method, path, parent string, body any, status int) []byte {
	t.Helper()
	var buf io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		buf = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, c.base+path, buf)
	if err != nil {
		t.Fatal("cannot construct isolated API request")
	}
	req.Header.Set("X-Lark-Token", c.token)
	if parent != "" {
		req.Header.Set("X-Lark-Parent-Token", parent)
	}
	if body != nil {
		req.Header.Set("Content-Type", jsonType)
	}
	res, err := c.client.Do(req)
	if err != nil {
		t.Fatalf("%s: request failed", req.URL.Path)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != status {
		t.Fatalf("%s %s: got HTTP %d, want %d", method, req.URL.Path, res.StatusCode, status)
	}
	return raw
}

func (s *e2eSuite) tool(t *testing.T, name string, args object, failed bool) object {
	t.Helper()
	payload := object{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": object{"name": name, "arguments": args}}
	v := testObject(t, s.request(t, "POST", "/mcp", payload, 200))
	result, _ := v["result"].(object)
	if result == nil || (result["isError"] == true) != failed {
		t.Fatalf("MCP %s: unexpected result status (expected error: %v)", name, failed)
	}
	out, _ := result["structuredContent"].(object)
	return out
}

type e2eCompanion struct {
	*testWS
	id, name    string
	mirror      *testWS
	width, high int
	revision    uint16
	pixels      []uint16
	codecs      [2]int
	svg         string
}

func (s *e2eSuite) browser(t *testing.T, name string) *e2eCompanion {
	t.Helper()
	name = s.prefix + " " + name
	v := testObject(t, s.request(t, "POST", "/api/companions", object{"name": name}, 201))
	id := str(v["id"])
	term := testObject(t, s.request(t, "POST", "/api/terminals", object{"kind": "web", "companion_id": id}, 201))
	termID := str(term["id"])
	if !webTerminalID.MatchString(termID) {
		t.Fatal("missing web terminal ID")
	}
	ws := testConnectWS(t, s.base, s.token, true)
	ws.json(t, object{"ev": "hello", "role": "web", "terminal_id": termID, "companion_id": id, "firmware": "web",
		"volume": 65, "caps": object{"call": true, "call_pcm": true, "drag": true, "bridge_endpoint": true}})
	c := &e2eCompanion{testWS: ws, id: id, name: name}
	c.scene(t, "companion-ring")
	return c
}

func (s *e2eSuite) board(t *testing.T, variant, label string) *e2eCompanion {
	t.Helper()
	name := s.prefix + " " + label
	id := "lark-" + strings.ToLower(s.prefix) + "-" + strings.ToLower(label)
	width, height, shape, battery := 360, 360, "round", variant == "esp-vocat"
	var pct, mv, charging any
	if battery {
		pct, mv, charging = 74, 3930, false
	} else {
		id = "box3-" + strings.ToLower(s.prefix) + "-" + strings.ToLower(label)
		width, height, shape = 320, 240, "rect"
	}
	ws := testConnectWS(t, s.base, s.token, false)
	ws.json(t, object{"ev": "hello", "role": "device", "device_id": id, "character": name,
		"firmware": "0.9.5", "proto": 2, "ip": "127.0.0.1", "width": width, "height": height, "shape": shape, "board": variant,
		"caps": object{"battery": battery, "head_pads": false, "call": true, "call_pcm": true, "drag": true,
			"pointer_rev": true, "bridge_endpoint": true, "panel_sleep": battery, "wake_word": true},
		"free_heap": 124000, "min_free_heap": 104000, "free_psram": 2000000,
		"wifi_rssi": -43, "rssi": -43, "uptime_s": 412, "cpu_pct": 13,
		"battery_pct": pct, "battery_mv": mv, "charging": charging, "volume": 65})
	c := &e2eCompanion{testWS: ws, id: id, name: name, width: width, high: height, pixels: make([]uint16, width*height)}
	ws.until(t, hasWS("hello_ok", ""))
	first := c.expect(t, "full initial framebuffer", isDisplay)
	r, _, _ := decodeDisplay(t, first)
	if r != image.Rect(0, 0, width, height) {
		t.Fatal("board did not receive full native framebuffer:", r)
	}
	return c
}

func (c *e2eCompanion) mirrorOn(t *testing.T, s *e2eSuite) {
	t.Helper()
	c.mirror = testConnectWS(t, s.base, s.token, true)
	c.updateScene(t, c.mirror.watch(t, c.id))
	c.syncRevision(t)
}

func (c *e2eCompanion) apply(t *testing.T, f wsFrame) {
	t.Helper()
	if !isDisplay(f) {
		return
	}
	r, rev, pixels := decodeDisplay(t, f)
	if r.Max.X > c.width || r.Max.Y > c.high {
		t.Fatal("dirty rectangle exceeds advertised geometry:", r)
	}
	c.revision = rev
	c.codecs[f.data[9]]++
	for y := range r.Dy() {
		at := (r.Min.Y+y)*c.width + r.Min.X
		copy(c.pixels[at:at+r.Dx()], pixels[y*r.Dx():(y+1)*r.Dx()])
	}
}

func (c *e2eCompanion) expect(t *testing.T, label string, matches func(wsFrame) bool) wsFrame {
	t.Helper()
	deadline := time.Now().Add(max(6*time.Second, c.timeout))
	for time.Now().Before(deadline) {
		f := c.read(t)
		c.apply(t, f)
		c.updateScene(t, f)
		if f.op == wsClose {
			t.Fatalf("%s closed waiting for %s", c.name, label)
		}
		if matches(f) {
			return f
		}
		if f.op == wsPing {
			c.write(t, maskedFrame(wsPong, true, f.data))
		}
	}
	t.Fatalf("%s did not receive %s before the deadline", c.name, label)
	return wsFrame{}
}

func (c *e2eCompanion) updateScene(t *testing.T, f wsFrame) {
	t.Helper()
	if hasWS("svg", "")(f) {
		c.svg = str(testObject(t, f.data)["svg"])
	}
}

func (c *e2eCompanion) scene(t *testing.T, text ...string) string {
	t.Helper()
	return c.sceneWhere(t, strings.Join(text, ", "), func(f wsFrame) bool {
		for _, part := range text {
			if !bytes.Contains(f.data, []byte(part)) {
				return false
			}
		}
		return true
	})
}

func (c *e2eCompanion) sceneWhere(t *testing.T, label string, filter func(wsFrame) bool) string {
	t.Helper()
	match := func(f wsFrame) bool { return hasWS("svg", "")(f) && filter(f) }
	if c.mirror != nil {
		defer func() {
			if t.Failed() {
				t.Logf("%s: wanted mirror scene %q", c.name, label)
			}
		}()
		f := c.mirror.until(t, match)
		c.updateScene(t, f)
		c.syncTo(t, uint16(e2eNumber(testObject(t, f.data)["rev"])))
	} else {
		c.expect(t, label, match)
	}
	return c.svg
}

func e2eNumber(v any) int {
	n, _ := strconv.Atoi(fmt.Sprint(v))
	return n
}

func (c *e2eCompanion) syncTo(t *testing.T, rev uint16) {
	t.Helper()
	if int16(c.revision-rev) < 0 {
		c.expect(t, "framebuffer matching mirror revision", func(f wsFrame) bool {
			return isDisplay(f) && int16(c.revision-rev) >= 0
		})
	}
}

func (c *e2eCompanion) syncRevision(t *testing.T) {
	t.Helper()
	c.json(t, object{"ev": "status", "rssi": -44})
	c.syncFrames(t)
}

func (c *e2eCompanion) syncFrames(t *testing.T) {
	t.Helper()
	c.write(t, maskedFrame(wsPing, true, []byte("sync")))
	c.expect(t, "frame barrier", func(f wsFrame) bool { return f.op == wsPong && string(f.data) == "sync" })
}

func (c *e2eCompanion) input(t *testing.T, ev string, x, y int, revision uint16) {
	t.Helper()
	if c.width != 0 {
		side := min(c.width, c.high)
		x, y = (c.width-side)/2+x*side/360, (c.high-side)/2+y*side/360
	}
	c.json(t, object{"ev": ev, "x": x, "y": y, "rev_lo": revision})
}

func (c *e2eCompanion) tap(t *testing.T, id string) {
	t.Helper()
	if c.width == 0 {
		c.json(t, object{"ev": "tap", "id": id})
		return
	}
	// Derive a coordinate from the public SVG hit-box, but submit only native
	// pixels and firmware's revision to the board socket.
	match := regexp.MustCompile(`data-hit="(\d+),(\d+),(\d+),(\d+)" data-id="` + regexp.QuoteMeta(id) + `"`).FindStringSubmatch(c.svg)
	if len(match) != 5 {
		t.Fatalf("%s has no native hitbox for %s", c.name, id)
	}
	c.syncFrames(t)
	c.input(t, "tap", e2eNumber(match[1])+e2eNumber(match[3])/2, e2eNumber(match[2])+e2eNumber(match[4])/2, c.revision)
}

func (c *e2eCompanion) audio(t *testing.T, prefix byte, pcm []byte) {
	t.Helper()
	for offset := 0; offset < len(pcm); offset += pcmPacket {
		part := pcm[offset:min(offset+pcmPacket, len(pcm))]
		c.write(t, maskedFrame(wsBinary, true, append([]byte{prefix}, part...)))
	}
}

func (c *e2eCompanion) ack(t *testing.T, end wsFrame) {
	t.Helper()
	audio := testObject(t, end.data)["audio"]
	if _, ok := audio.(json.Number); !ok {
		t.Fatal("missing numeric playback ID")
	}
	c.json(t, object{"ev": "tts_ack", "state": "done", "audio": audio})
}

func (s *e2eSuite) history(t *testing.T, id, query string) []chatEntry {
	t.Helper()
	var out struct{ Messages []chatEntry }
	if err := json.Unmarshal(s.request(t, "GET", "/api/devices/"+id+"/messages"+query, nil, 200), &out); err != nil {
		t.Fatal("cannot decode canonical history")
	}
	return out.Messages
}

func (s *e2eSuite) synth(t *testing.T, text string) []byte {
	t.Helper()
	pcm := s.request(t, "POST", "/api/tts", object{"text": text}, 200)
	if !livePCMOK(pcm) {
		t.Fatal("Kokoro did not produce non-silent PCM")
	}
	return pcm
}

func (s *e2eSuite) scope(t *testing.T, id string) liveClient {
	t.Helper()
	v := testObject(t, s.request(t, "POST", "/api/tunnels", object{"device_id": id}, 201))
	u, err := url.Parse(str(v["share_url"]))
	if err != nil || u.Host != "scoped.test.opentunnel.example" || v["active"] != true {
		t.Fatal("fake tunnel CLI did not provision the scoped local listener")
	}
	port, token := e2eNumber(v["local_port"]), u.Query().Get("token")
	if port <= 0 || !scopeKey.MatchString(token) {
		t.Fatal("invalid scoped listener or credential")
	}
	t.Cleanup(func() {
		// The subtest context is already canceled when Cleanup runs.
		req, _ := http.NewRequest("DELETE", s.base+"/api/tunnels/"+id, nil)
		req.Header.Set("X-Lark-Token", s.token)
		res, err := s.client.Do(req)
		if err != nil {
			t.Error("could not remove isolated scoped listener")
			return
		}
		res.Body.Close()
		if res.StatusCode != 204 && res.StatusCode != 200 {
			t.Errorf("could not remove isolated scoped listener: HTTP %d", res.StatusCode)
		}
	})
	return liveClient{s.client, "http://127.0.0.1:" + strconv.Itoa(port), token}
}
