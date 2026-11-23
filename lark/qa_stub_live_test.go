//go:build live

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestQAStubSuite is the fast end-to-end loop: one isolated bridge built with
// the e2e tag, scripted STT/TTS/Delta Chat, and lark sim as the firmware. It
// asserts product behaviour (state transitions, routing, frames, DB rows,
// Delta Chat sends), never model quality.

type stubSuite struct {
	*e2eSuite
	bin, device string
	port        int
	model       *e2eModel
	fixtures    map[string]voiceFixture

	logMu sync.Mutex
	logs  []string
}

func TestQAStubSuite(t *testing.T) {
	if os.Getenv("LARK_E2E_MODE") != "stub" {
		t.Skip("use scripts/check.sh --e2e-stub")
	}
	s := startStubE2E(t)
	s.model.setRules([]e2eStubRule{
		// "Gus" owns the timer scenario. The 30 s timer is still registered
		// when the turn ends, so the assertion does not race its firing.
		{Match: "timer for ninety seconds", Reply: "I set a timer.",
			Tool: "patch", Args: object{"companion_id": "gus", "timer": object{
				"op": "set", "in_seconds": 30, "label": "Stub timer", "say": "Timer done."}}},
		{Match: "little lamp", Reply: "[silent]"},
		{Match: "Dinner at six", Reply: "See you at six, Mum."},
	})
	scenarios := []struct {
		name string
		run  func(s *stubSuite, t *testing.T)
	}{
		{"VoiceTurn", (*stubSuite).voiceTurn},
		{"InterruptStop", (*stubSuite).interruptStop},
		{"ListeningMode", (*stubSuite).listeningMode},
		{"Menus", (*stubSuite).menus},
		{"Stories", (*stubSuite).stories},
		{"MemoriesVoiceVolume", (*stubSuite).memoriesVoiceVolume},
		{"Timer", (*stubSuite).timer},
		{"DeltaChat", (*stubSuite).deltaChat},
		{"Intercom", (*stubSuite).intercom},
		{"MultiTerminal", (*stubSuite).multiTerminal},
		{"Robustness", (*stubSuite).robustness},
	}
	for _, sc := range scenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			started := time.Now()
			sc.run(s, t)
			t.Logf("scenario %s: %s", sc.name, time.Since(started).Round(time.Millisecond))
		})
	}
}

func startStubE2E(t *testing.T) *stubSuite {
	t.Helper()
	bin := os.Getenv("LARK_TEST_BIN")
	if !filepath.IsAbs(bin) {
		t.Fatal("use scripts/check.sh --e2e-stub to build the stub binary")
	}
	root := t.TempDir()
	for _, dir := range []string{"data", "state", "cache", "config", "workspace"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	client := pooledClient()
	client.Timeout = 60 * time.Second
	t.Cleanup(client.CloseIdleConnections)
	s := &stubSuite{e2eSuite: &e2eSuite{
		liveClient: liveClient{client: client, token: randomHex(32)},
		workspace:  filepath.Join(root, "workspace"),
		prefix:     "STUB-" + randomHex(4),
	}, bin: bin}
	s.model = startE2EModel(t, s.e2eSuite, root)

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.port = probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	s.device = net.JoinHostPort("127.0.0.1", strconv.Itoa(s.port))

	cmd := exec.Command(bin, "serve")
	// Never inherit installed credentials, the real database, or the CLI home.
	cmd.Env = []string{
		"HOME=" + root, "PATH=" + filepath.Join(root, "bin") + ":/usr/bin:/bin",
		"XDG_CONFIG_HOME=" + filepath.Join(root, "config"),
		"XDG_DATA_HOME=" + filepath.Join(root, "data"),
		"XDG_STATE_HOME=" + filepath.Join(root, "state"),
		"XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
		"LARK_HOST=127.0.0.1", "LARK_PORT=0", "LARK_DEVICE_PORT=" + strconv.Itoa(s.port),
		"LARK_TOKEN=" + s.token, "LARK_DB_PATH=" + filepath.Join(root, "lark.db"),
		"LARK_REPO_DIR=" + repo, "LARK_OTA_DIR=" + root, "LARK_BOX3_OTA_DIR=" + root,
		"LARK_OPENCODE_URL=" + s.model.url, "LARK_OPENCODE_DIR=" + s.workspace,
		"LARK_STUB=1", "LARK_DEBUG_ENDPOINT=1",
		"LARK_STUB_FIXTURES=" + filepath.Join(repo, "lark", "qa", "fixtures", "voice"),
	}
	logs, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal("cannot start the stub bridge")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-done:
			if err != nil {
				t.Error("stub bridge did not exit cleanly")
			}
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("stub bridge did not shut down")
		}
	})
	addr := make(chan string, 1)
	go func() {
		lines := bufio.NewScanner(logs)
		for lines.Scan() {
			line := lines.Text()
			s.logMu.Lock()
			s.logs = append(s.logs, line)
			if len(s.logs) > 400 {
				s.logs = s.logs[len(s.logs)-400:]
			}
			s.logMu.Unlock()
			if _, a, ok := strings.Cut(line, larkListeningPrefix); ok {
				select {
				case addr <- a:
				default:
				}
			}
		}
	}()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		s.logMu.Lock()
		defer s.logMu.Unlock()
		t.Logf("stub bridge log (last %d lines):", len(s.logs))
		for _, line := range s.logs {
			t.Logf("  %s", line)
		}
	})
	select {
	case a := <-addr:
		s.base = "http://" + a
	case err := <-done:
		done <- err
		t.Fatal("stub bridge exited before listening")
	case <-time.After(20 * time.Second):
		t.Fatal("stub bridge did not listen")
	}
	health := liveJSON[object](t, s.liveClient, "/health", nil, nil, "", nil)
	for _, key := range []string{"ok", "stt", "tts", "deltachat"} {
		if health[key] != true {
			t.Fatalf("stub bridge is not ready: %s", key)
		}
	}
	if health["stt_engine"] != "stub" || health["tts_engine"] != "stub" {
		t.Fatalf("bridge did not enter stub mode: stt=%v tts=%v", health["stt_engine"], health["tts_engine"])
	}
	m := readVoiceManifest(t, filepath.Join("qa", "fixtures", "voice"))
	s.fixtures = map[string]voiceFixture{}
	for _, f := range m.Fixtures {
		s.fixtures[f.Name] = f
	}
	return s
}

func (s *stubSuite) companion(t *testing.T, name string) string {
	t.Helper()
	raw := s.request(t, http.MethodPost, "/api/companions", object{"name": name, "form": "a small cat"}, 201)
	return str(testObject(t, raw)["id"])
}

func (s *stubSuite) fixture(t *testing.T, name string) voiceFixture {
	t.Helper()
	f, ok := s.fixtures[name]
	if !ok {
		t.Fatalf("no voice fixture %q", name)
	}
	return f
}

func (s *stubSuite) sim(t *testing.T, board, id, script string) []object {
	t.Helper()
	rows, out, err := s.trySim(t.Context(), board, id, script)
	if err != nil {
		t.Fatalf("lark sim %s failed: %v\n%s", id, err, out)
	}
	return rows
}

// trySim is safe to call from a goroutine: it returns the error instead of
// calling t.Fatalf, which would abandon the caller's channel send.
func (s *stubSuite) trySim(ctx context.Context, board, id, script string) ([]object, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.bin, "sim", "--board", board, "--id", id, "--addr", s.device)
	cmd.Env = []string{
		"HOME=" + filepath.Dir(filepath.Dir(s.workspace)), "PATH=/usr/bin:/bin",
		"LARK_TOKEN=" + s.token, "LARK_URL=" + s.base,
		"LARK_PORT=" + strconv.Itoa(s.port), "XDG_CONFIG_HOME=/nonexistent",
	}
	cmd.Stdin = strings.NewReader(script)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil || errb.Len() != 0 {
		return nil, out.String() + errb.String(), fmt.Errorf("%v %s", err, errb.String())
	}
	if ctx.Err() != nil {
		return nil, out.String(), fmt.Errorf("timed out")
	}
	var rows []object
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		var row object
		if err := dec.Decode(&row); err != nil {
			return nil, out.String(), fmt.Errorf("invalid JSON %q", line)
		}
		rows = append(rows, row)
	}
	return rows, out.String(), nil
}

func simEvents(rows []object) []object {
	var out []object
	for _, row := range rows {
		if row["cmd"] == "timeline" {
			if events, ok := row["events"].([]any); ok {
				for _, event := range events {
					if m, ok := event.(object); ok {
						out = append(out, m)
					}
				}
			}
		}
	}
	return out
}

func simHitsScreen(rows []object) string {
	screen := ""
	for _, row := range rows {
		if row["cmd"] == "hits" {
			screen = str(row["screen"])
		}
	}
	return screen
}

func sawEvent(rows []object, name string) bool {
	for _, event := range simEvents(rows) {
		if event["event"] == name {
			return true
		}
	}
	return false
}

func countEvents(rows []object, name string) int {
	n := 0
	for _, event := range simEvents(rows) {
		if event["event"] == name {
			n++
		}
	}
	return n
}

func lastSimState(rows []object) string {
	state := ""
	for _, row := range rows {
		if row["cmd"] == "wait-current" || row["cmd"] == "state" {
			state = str(row["state"])
		}
	}
	return state
}

func (s *stubSuite) waitCompanion(t *testing.T, id string, pred func(object) bool) object {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		entry := testObject(t, s.request(t, http.MethodGet, "/api/companions/"+id, nil, 200))
		if pred(entry) {
			return entry
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("companion %s did not reach the expected state", id)
	return nil
}

func (s *stubSuite) tool(t *testing.T, name string, args object) object {
	t.Helper()
	raw := s.request(t, http.MethodPost, "/mcp", object{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": object{"name": name, "arguments": args}}, 200)
	res, _ := testObject(t, raw)["result"].(object)
	if res == nil || res["isError"] == true {
		t.Fatalf("MCP %s failed: %s", name, raw)
	}
	out, _ := res["structuredContent"].(object)
	return out
}

func (s *stubSuite) voiceTurn(t *testing.T) {
	id := s.companion(t, "Ada")
	f := s.fixture(t, "short")
	rows := s.sim(t, "vocat", "stub-voice", fmt.Sprintf(`attach %s
tap 180 180
say %s
sleep 250
wait-current done 10s
timeline
`, id, filepath.Join("qa", "fixtures", "voice", f.File)))
	if !sawEvent(rows, "tts_first_pcm") {
		t.Fatalf("voice turn produced no TTS: %v", simEvents(rows))
	}
	if last := lastSimState(rows); last != "idle" && last != "done" {
		t.Fatalf("voice turn ended in %q", last)
	}
	if !s.model.sawUserText(f.Transcript) {
		t.Fatalf("scripted transcript %q never reached the model (prompts=%d texts=%q)", f.Transcript, s.model.prompts.Load(), s.model.userTexts())
	}
}

func (s *stubSuite) interruptStop(t *testing.T) {
	id := s.companion(t, "Bo")
	rows := s.sim(t, "vocat", "stub-stop", fmt.Sprintf(`attach %s
tap 180 180
say %s
wait-current speaking 8s
hits
tap-label Stop
sleep 300
state
timeline
`, id, filepath.Join("qa", "fixtures", "voice", "trailing-um.wav")))
	if !sawEvent(rows, "tts_start") {
		t.Fatal("reply never started")
	}
	if last := lastSimState(rows); last != "idle" && last != "done" {
		t.Fatalf("Stop left the terminal in %q", last)
	}
	events := simEvents(rows)
	start, end := -1, -1
	for i, event := range events {
		switch event["event"] {
		case "tts_start":
			start = i
		case "tts_end":
			end = i
		}
	}
	if start < 0 || end < start {
		t.Fatalf("no tts_end after Stop: %v", events)
	}
}

func (s *stubSuite) listeningMode(t *testing.T) {
	id := s.companion(t, "Cy")
	s.request(t, http.MethodPatch, "/api/companions/"+id, object{"listening": true}, 200)
	prime := s.fixture(t, "short")
	rows := s.sim(t, "vocat", "stub-listen", fmt.Sprintf(`attach %s
tap 180 180
say %s
wait-rec 10s
say %s
sleep 1800
timeline
`, id, filepath.Join("qa", "fixtures", "voice", prime.File),
		filepath.Join("qa", "fixtures", "voice", "function-word.wav")))
	if !s.model.sawUserText("put his") {
		t.Fatal("listening follow-up never reached the model")
	}
	diag := liveJSON[diagReport](t, s.liveClient, "/api/diagnostics", nil, nil, "", nil)
	if diag.Listening.Windows == 0 || diag.Listening.Committed == 0 {
		t.Fatalf("listening counters did not move: %+v", diag.Listening)
	}
	if countEvents(rows, "rec_start") < 2 {
		t.Fatalf("expected a tap capture and a listening capture: %v", simEvents(rows))
	}
}

func (s *stubSuite) menus(t *testing.T) {
	id := s.companion(t, "Di")
	for i := range 4 {
		s.tool(t, "story", object{"op": "create", "id": fmt.Sprintf("menu_story_%d", i),
			"twee": fmt.Sprintf(":: StoryTitle\nMenu Story %d\n:: Start {\"end\":true}\nPage one.\n", i)})
	}
	for _, board := range []string{"vocat", "box3"} {
		board := board
		t.Run(board, func(t *testing.T) {
			t.Parallel()
			out := s.sim(t, board, "stub-menu-"+board,
				qaLoadSimScript(t, "menus_"+board+".sim", map[string]string{"COMP1": id}))
			if len(out) == 0 {
				t.Fatal("menu script printed nothing")
			}
		})
	}
}

func (s *stubSuite) stories(t *testing.T) {
	id := s.companion(t, "Eve")
	s.tool(t, "story", object{"op": "create", "id": "stub_story",
		"twee": ":: StoryTitle\nStub Story\n:: Start {\"end\":true}\nOnce upon a time a clockwork owl woke up.\n"})
	rows := s.sim(t, "vocat", "stub-story", fmt.Sprintf(`attach %s
tap-label Menu
tap-label Stories
tap-label story-0
hits
timeline
`, id))
	if !sawEvent(rows, "status") {
		t.Fatal("story screen produced no frames")
	}
	if screen := simHitsScreen(rows); screen != "story-reader" {
		t.Fatalf("story did not open: screen %q", screen)
	}
}

func (s *stubSuite) memoriesVoiceVolume(t *testing.T) {
	id := s.companion(t, "Fay")
	s.tool(t, "patch", object{"companion_id": id, "memory": object{"op": "set", "scope": "companion:" + id,
		"key": "favourite", "value": object{"text": "bluebells"}}})
	s.sim(t, "vocat", "stub-memory", fmt.Sprintf(`attach %s
tap-label Menu
tap-label Memories
tap-label memory-0
hits
timeline
`, id))
	rows := s.sim(t, "vocat", "stub-voice-menu", fmt.Sprintf(`attach %s
tap-label Menu
tap-label Next
tap-label Voice
tap-label Next
tap-label Bella
hits
timeline
`, id))
	s.waitCompanion(t, id, func(o object) bool { return str(o["voice"]) == "af_bella" })
	rows = s.sim(t, "vocat", "stub-volume", fmt.Sprintf(`attach %s
tap-label Menu
tap-label Next
tap-label More
tap-label Volume
hits
timeline
`, id))
	if screen := simHitsScreen(rows); screen != "volume" {
		t.Fatalf("volume menu did not open: screen %q", screen)
	}
}

func (s *stubSuite) timer(t *testing.T) {
	id := s.companion(t, "Gus")
	s.sim(t, "vocat", "stub-timer", fmt.Sprintf(`attach %s
tap 180 180
say %s
sleep 250
wait-current done 10s
timeline
`, id, filepath.Join("qa", "fixtures", "voice", "timer.wav")))
	state := s.tool(t, "state", object{"companion_id": id, "include": []string{"timers"}})
	timers, _ := state["timers"].([]any)
	if len(timers) == 0 {
		t.Fatalf("timer tool call did not create a row: %v", state)
	}
	found := false
	for _, raw := range timers {
		if m, ok := raw.(object); ok && str(m["label"]) == "Stub timer" {
			found = true
		}
	}
	if !found {
		t.Fatalf("timer row missing label: %v", timers)
	}
}

func (s *stubSuite) deltaChat(t *testing.T) {
	id := s.companion(t, "Hal")
	s.request(t, http.MethodPost, "/api/e2e/stub/dc/message", object{
		"companion_id": id, "name": "Mum", "address": "mum@test.example", "text": "Dinner at six"}, 200)
	sent := testObject(t, s.request(t, http.MethodGet, "/api/e2e/stub/dc/sent", nil, 200))
	messages, _ := sent["messages"].([]any)
	found := false
	for _, raw := range messages {
		if m, ok := raw.(object); ok && strings.Contains(str(m["text"]), "See you at six") {
			found = true
		}
	}
	if !found {
		t.Fatalf("Delta Chat reply not sent: %v", messages)
	}
}

func (s *stubSuite) intercom(t *testing.T) {
	cake := s.companion(t, "Cake")
	mint := s.companion(t, "Mint")
	accounts := testObject(t, s.request(t, http.MethodGet, "/api/e2e/stub/dc/accounts", nil, 200))["accounts"].(object)
	mintAddr := str(accounts[mint])
	if mintAddr == "" {
		t.Fatalf("no stub Delta Chat address for Mint: %v", accounts)
	}
	done := make(chan struct {
		rows []object
		out  string
		err  error
	}, 1)
	go func() {
		rows, out, err := s.trySim(t.Context(), "vocat", "stub-intercom", fmt.Sprintf(`attach %s
wait-current done 15s
timeline
`, cake))
		done <- struct {
			rows []object
			out  string
			err  error
		}{rows, out, err}
	}()
	time.Sleep(300 * time.Millisecond)
	// A paired companion speaking over Delta Chat reaches the local terminal.
	s.request(t, http.MethodPost, "/api/e2e/stub/dc/message", object{
		"companion_id": cake, "name": "Mint", "address": mintAddr, "text": "Come play!"}, 200)
	result := <-done
	if result.err != nil {
		t.Fatalf("intercom sim failed: %v\n%s", result.err, result.out)
	}
	rows := result.rows
	if !sawEvent(rows, "tts_first_pcm") && !sawEvent(rows, "tts_start") {
		t.Fatalf("intercom did not reach the terminal: %v", simEvents(rows))
	}
}

func (s *stubSuite) multiTerminal(t *testing.T) {
	cake := s.companion(t, "Ivy")
	mint := s.companion(t, "Jon")
	var wg sync.WaitGroup
	results := make([][]object, 2)
	errs := make([]error, 2)
	outs := make([]string, 2)
	scripts := []struct{ id, comp, fixture string }{
		{"stub-multi-a", cake, "short"},
		{"stub-multi-b", mint, "list"},
	}
	for i, sc := range scripts {
		wg.Add(1)
		go func(i int, id, comp, fixture string) {
			defer wg.Done()
			results[i], outs[i], errs[i] = s.trySim(t.Context(), "vocat", id, fmt.Sprintf(`attach %s
tap 180 180
say %s
sleep 250
wait-current done 10s
timeline
`, comp, filepath.Join("qa", "fixtures", "voice", fixture+".wav")))
		}(i, sc.id, sc.comp, sc.fixture)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("multi-terminal sim %d failed: %v\n%s", i, errs[i], outs[i])
		}
		if !sawEvent(results[i], "tts_first_pcm") {
			t.Fatalf("a terminal produced no reply: %v", simEvents(results[i]))
		}
	}
}

func (s *stubSuite) robustness(t *testing.T) {
	id := s.companion(t, "Kim")
	t.Run("NoiseOnly", func(t *testing.T) {
		t.Parallel()
		rows := s.sim(t, "vocat", "stub-noise", fmt.Sprintf(`attach %s
tap 180 180
say %s
sleep 1500
state
timeline
`, id, filepath.Join("qa", "fixtures", "voice", "noise-only.wav")))
		if sawEvent(rows, "tts_start") {
			t.Fatal("noise-only turn spoke a reply")
		}
	})
	t.Run("SilentReply", func(t *testing.T) {
		t.Parallel()
		rows := s.sim(t, "vocat", "stub-silent", fmt.Sprintf(`attach %s
tap 180 180
say %s
sleep 1500
state
timeline
`, id, filepath.Join("qa", "fixtures", "voice", "quiet-over-noise.wav")))
		if sawEvent(rows, "tts_start") {
			t.Fatal("[silent] produced speech")
		}
		if last := lastSimState(rows); last != "idle" && last != "done" {
			t.Fatalf("[silent] ended in %q", last)
		}
	})
}
