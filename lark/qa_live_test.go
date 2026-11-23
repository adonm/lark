//go:build live

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chatmail/rpc-client-go/v2/deltachat"
)

func qaLiveClient(t *testing.T) *cliClient {
	t.Helper()
	c, err := newCLIClient()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func qaTempCompanion(t *testing.T, c *cliClient, name, form string) string {
	t.Helper()
	res, err := c.do(t.Context(), http.MethodPost, "/api/companions", object{"name": name, "form": form})
	if err != nil {
		t.Fatalf("create temp companion %s: %v", name, err)
	}
	id := str(res["id"])
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		terms, err := c.do(ctx, http.MethodGet, "/api/terminals", nil)
		if err == nil {
			if list, ok := terms["terminals"].([]any); ok {
				for _, item := range list {
					m, _ := item.(object)
					if str(m["companion_id"]) == id {
						_, _ = c.do(ctx, http.MethodPost, "/api/terminals/"+str(m["id"])+"/detach", object{})
					}
				}
			}
		}
		_, _ = c.do(ctx, http.MethodDelete, "/api/companions/"+id, nil)
	})
	return id
}

func qaSimRun(t *testing.T, board, id, script string) []object {
	t.Helper()
	bin := filepath.Join(env("XDG_DATA_HOME", filepath.Join(os.Getenv("HOME"), ".local/share")), "lark", "lark")
	cmd := exec.CommandContext(t.Context(), bin, "sim", "--board", board, "--id", id)
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("lark sim failed: %v\n%s", err, out)
	}
	var res []object
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m object
		if err := json.Unmarshal([]byte(line), &m); err == nil {
			res = append(res, m)
		}
	}
	return res
}

func qaLoadSimScript(t *testing.T, name string, vars map[string]string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("qa", name))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for k, v := range vars {
		s = strings.ReplaceAll(s, "{{"+k+"}}", v)
	}
	return s
}

func TestQASimSuite(t *testing.T) {
	c := qaLiveClient(t)
	compID := qaTempCompanion(t, c, "qa-sim", "cat")
	vars := map[string]string{"COMP1": compID}

	t.Run("VoiceTurn", func(t *testing.T) {
		out := qaSimRun(t, "vocat", "sim-qa", qaLoadSimScript(t, "voice_turn.sim", vars))
		var hasTTS bool
		for _, row := range out {
			if row["cmd"] == "timeline" {
				if evs, ok := row["events"].([]any); ok {
					for _, e := range evs {
						if m, _ := e.(object); m["event"] == "tts_first_pcm" {
							hasTTS = true
						}
					}
				}
			}
		}
		if !hasTTS {
			t.Fatalf("expected tts_first_pcm in voice_turn timeline: %v", out)
		}
	})

	t.Run("InterruptStop", func(t *testing.T) {
		out := qaSimRun(t, "vocat", "sim-qa", qaLoadSimScript(t, "interrupt_stop.sim", vars))
		var lastState string
		for _, row := range out {
			if row["cmd"] == "wait" && row["state"] != nil {
				lastState = str(row["state"])
			}
		}
		if lastState != "idle" {
			t.Fatalf("expected final state idle after Stop, got %q", lastState)
		}
	})

	t.Run("MenusVocat", func(t *testing.T) {
		qaSimRun(t, "vocat", "sim-vocat", qaLoadSimScript(t, "menus_vocat.sim", vars))
	})

	t.Run("MenusBox3", func(t *testing.T) {
		qaSimRun(t, "box3", "sim-box3", qaLoadSimScript(t, "menus_box3.sim", vars))
	})
}

func TestQADeltaChatLive(t *testing.T) {
	if os.Getenv("LARK_QA_DC") == "" {
		t.Skip("set LARK_QA_DC=1 to run live Delta Chat QA")
	}
	c := qaLiveClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()

	compID := qaTempCompanion(t, c, "qa-dc", "cat")

	pairPNG := filepath.Join(t.TempDir(), "pair.png")
	qaSimRun(t, "vocat", "sim-qa", fmt.Sprintf("attach %s\ntap-label Menu\ntap-label Next\ntap-label More\ntap-label Pair\nscreen %s\ntap-label pair-close\ntap-label Back\ntap-label Back\n", compID, pairPNG))

	qadc, err := startQADCClient(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(qadc.close)

	unpaired, err := startQADCClient(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unpaired.close)

	chatID, err := qadc.pairFromScreenPNG(pairPNG)
	if err != nil {
		t.Fatalf("pairFromScreenPNG: %v", err)
	}

	waitMsg := func(timeout time.Duration, pred func(deltachat.Message) bool) deltachat.Message {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			ids, err := qadc.rpc.GetMessageIds(qadc.accID, chatID, false, false)
			if err == nil {
				for i := len(ids) - 1; i >= 0; i-- {
					m, err := qadc.rpc.GetMessage(qadc.accID, ids[i])
					if err == nil && m.FromId != deltachat.ContactSelf && pred(m) {
						return m
					}
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for Delta Chat message")
		return deltachat.Message{}
	}

	waitMsg(25*time.Second, func(m deltachat.Message) bool {
		return m.ViewType == deltachat.ViewtypeText && strings.Contains(m.Text, "/help")
	})
	sendText := func(txt string) {
		_, err := qadc.rpc.SendMsg(qadc.accID, chatID, deltachat.MessageData{Text: &txt})
		if err != nil {
			t.Fatalf("SendMsg(%q): %v", txt, err)
		}
	}

	sendText("/help")
	help := waitMsg(15*time.Second, func(m deltachat.Message) bool {
		return strings.Contains(m.Text, "/say") && strings.Contains(m.Text, "/remote")
	})
	if strings.Contains(help.Text, "/panel") {
		t.Fatalf("welcome still advertises the removed panel: %q", help.Text)
	}

	// /remote replies with one tappable link and nothing else.
	sendText("/remote")
	remoteMsg := waitMsg(20*time.Second, func(m deltachat.Message) bool {
		return m.ViewType == deltachat.ViewtypeText && strings.HasPrefix(strings.TrimSpace(m.Text), "https://") && strings.Contains(m.Text, "/remote?token=")
	})
	firstLink := strings.TrimSpace(remoteMsg.Text)
	if strings.ContainsAny(firstLink, " \n") {
		t.Fatalf("/remote reply is not a bare link: %q", firstLink)
	}
	parsed, err := url.Parse(firstLink)
	if err != nil || parsed.Query().Get("token") == "" {
		t.Fatalf("invalid /remote link: %q (%v)", firstLink, err)
	}
	remoteBase := parsed.Scheme + "://" + parsed.Host
	oldToken := parsed.Query().Get("token")

	client := &http.Client{Timeout: 20 * time.Second}
	waitPage := func(link string) []byte {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			res, err := client.Get(link)
			if err == nil {
				body, _ := io.ReadAll(res.Body)
				res.Body.Close()
				if res.StatusCode == 200 {
					return body
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatal("remote page did not load over the tunnel")
		return nil
	}
	page := waitPage(firstLink)
	if !bytes.Contains(page, []byte("/remote/app.js")) {
		t.Fatal("remote page did not include its app")
	}

	remoteReq := func(method, path, token string, body any) (int, []byte) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			reader = bytes.NewReader(raw)
		}
		req, err := http.NewRequestWithContext(ctx, method, remoteBase+path+"?token="+url.QueryEscape(token), reader)
		if err != nil {
			t.Fatal(err)
		}
		if body != nil {
			req.Header.Set("Content-Type", jsonType)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		return res.StatusCode, data
	}

	if status, _ := remoteReq(http.MethodPost, "/remote/api/listen", oldToken, object{"seconds": 60}); status != 200 {
		t.Fatalf("remote listen status = %d", status)
	}
	deadline := time.Now().Add(10 * time.Second)
	listenOK := false
	for time.Now().Before(deadline) {
		comp, err := c.do(ctx, http.MethodGet, "/api/companions/"+compID, nil)
		if err == nil && fmt.Sprint(comp["listen_seconds"]) == "60" {
			listenOK = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !listenOK {
		t.Fatal("remote listen=60 did not take effect")
	}
	if status, _ := remoteReq(http.MethodPost, "/remote/api/listen", oldToken, object{"seconds": 0}); status != 200 {
		t.Fatalf("remote listen off status = %d", status)
	}

	// Voice and history round trips.
	if status, body := remoteReq(http.MethodPost, "/remote/api/voice", oldToken, object{"voice": "bf_lily"}); status != 200 || !bytes.Contains(body, []byte("bf_lily")) {
		t.Fatalf("remote voice failed: status=%d body=%s", status, body)
	}
	if status, body := remoteReq(http.MethodGet, "/remote/api/history", oldToken, object{"limit": 5}); status != 200 || !bytes.Contains(body, []byte("items")) {
		t.Fatalf("remote history failed: status=%d body=%s", status, body)
	}

	// A fresh /remote link invalidates the old token.
	sendText("/remote")
	secondMsg := waitMsg(20*time.Second, func(m deltachat.Message) bool {
		return m.ViewType == deltachat.ViewtypeText && strings.HasPrefix(strings.TrimSpace(m.Text), "https://") && strings.Contains(m.Text, "/remote?token=") && strings.TrimSpace(m.Text) != firstLink
	})
	secondURL, _ := url.Parse(strings.TrimSpace(secondMsg.Text))
	newToken := secondURL.Query().Get("token")
	if status, _ := remoteReq(http.MethodGet, "/remote/api/history", oldToken, nil); status != 401 {
		t.Fatalf("old remote token still works: status=%d", status)
	}
	if status, _ := remoteReq(http.MethodGet, "/remote/api/history", newToken, nil); status != 200 {
		t.Fatalf("new remote token failed: status=%d", status)
	}

	// REST command latency versus the old chatmail command path.
	restSamples := make([]time.Duration, 0, 5)
	for i := 0; i < 5; i++ {
		seconds := 30
		if i%2 == 1 {
			seconds = 0
		}
		start := time.Now()
		if status, _ := remoteReq(http.MethodPost, "/remote/api/listen", newToken, object{"seconds": seconds}); status != 200 {
			t.Fatalf("latency listen status = %d", status)
		}
		restSamples = append(restSamples, time.Since(start))
	}
	chatSamples := make([]time.Duration, 0, 5)
	for i := 0; i < 5; i++ {
		before := map[uint32]bool{}
		if ids, err := qadc.rpc.GetMessageIds(qadc.accID, chatID, false, false); err == nil {
			for _, id := range ids {
				before[id] = true
			}
		}
		start := time.Now()
		sendText(fmt.Sprintf("/say latency probe %d", i))
		measured := false
		probeDeadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(probeDeadline) {
			ids, err := qadc.rpc.GetMessageIds(qadc.accID, chatID, false, false)
			if err == nil {
				for _, id := range ids {
					if before[id] {
						continue
					}
					m, err := qadc.rpc.GetMessage(qadc.accID, id)
					if err == nil && m.FromId == deltachat.ContactSelf && m.ViewType == deltachat.ViewtypeText && m.Text == "Said it." {
						chatSamples = append(chatSamples, time.Since(start))
						measured = true
						break
					}
				}
			}
			if measured {
				break
			}
			// Bound each poll so a slow relay cannot stall the whole QA run.
			time.Sleep(100 * time.Millisecond)
		}
		if !measured {
			t.Fatal("chatmail /say latency probe timed out")
		}
	}
	slices.Sort(restSamples)
	slices.Sort(chatSamples)
	t.Logf("remote latency: REST listen p50=%s; chatmail /say p50=%s", restSamples[len(restSamples)/2], chatSamples[len(chatSamples)/2])

	// /remote off revokes the current token and stops the tunnel.
	sendText("/remote off")
	waitMsg(20*time.Second, func(m deltachat.Message) bool {
		return strings.Contains(m.Text, "Remote controls off.")
	})
	if status, _ := remoteReq(http.MethodGet, "/remote/api/history", newToken, nil); status != 401 {
		t.Fatalf("revoked remote token still works: status=%d", status)
	}

	sendText("/say Hello from Delta Chat!")
	waitMsg(15*time.Second, func(m deltachat.Message) bool {
		return strings.Contains(m.Text, "Said it.")
	})

	sendText("/private What is two plus three?")
	waitMsg(20*time.Second, func(m deltachat.Message) bool {
		return strings.Contains(strings.ToLower(m.Text), "five") || strings.Contains(m.Text, "5")
	})

	sendText("What is four plus four?")
	waitMsg(20*time.Second, func(m deltachat.Message) bool {
		return strings.Contains(strings.ToLower(m.Text), "eight") || strings.Contains(m.Text, "8")
	})

	sendText("/stories")
	waitMsg(15*time.Second, func(m deltachat.Message) bool {
		return strings.Contains(m.Text, "The Clockwork Owl")
	})
	sendText("/story clockwork_owl")
	waitMsg(15*time.Second, func(m deltachat.Message) bool {
		return m.ViewType == deltachat.ViewtypeFile && m.FileName != nil && strings.HasSuffix(*m.FileName, ".twee")
	})

	ttsReq, _ := json.Marshal(object{"text": "What is one plus two?"})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/tts", bytes.NewReader(ttsReq))
	req.Header.Set("X-Lark-Token", c.token)
	req.Header.Set("Content-Type", jsonType)
	ttsRes, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	wavPath := filepath.Join(t.TempDir(), "in.wav")
	oggPath := filepath.Join(t.TempDir(), "in.ogg")
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(ttsRes.Body)
	ttsRes.Body.Close()
	if err := os.WriteFile(wavPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-y", "-i", wavPath, "-c:a", "libopus", oggPath).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
	vt := deltachat.ViewtypeVoice
	if _, err := qadc.rpc.SendMsg(qadc.accID, chatID, deltachat.MessageData{File: &oggPath, Viewtype: &vt}); err != nil {
		t.Fatal(err)
	}
	waitMsg(25*time.Second, func(m deltachat.Message) bool {
		return m.ViewType == deltachat.ViewtypeVoice
	})

	sendText("/unknowncmd")
	waitMsg(15*time.Second, func(m deltachat.Message) bool {
		return strings.Contains(strings.ToLower(m.Text), "unknown") || strings.Contains(m.Text, "/help")
	})

	qaSimRun(t, "vocat", "sim-qa", fmt.Sprintf("attach %s\ntap-label Menu\ntap-label Next\ntap-label More\ntap-label Unpair\ntap-label QA Client\ntap-label Back\ntap-label Back\ntap-label Back\n", compID))
}

func TestQAHardwareSuite(t *testing.T) {
	if os.Getenv("LARK_QA_HW") == "" {
		t.Skip("set LARK_QA_HW=1 to run hardware QA")
	}
	c := qaLiveClient(t)
	compID := qaTempCompanion(t, c, "qa-hw", "cat")
	bin := filepath.Join(env("XDG_DATA_HOME", filepath.Join(os.Getenv("HOME"), ".local/share")), "lark", "lark")

	orig := map[string]string{"vocat-33d8": "cake", "box3-b324": "greg"}
	for dev, origComp := range orig {
		t.Cleanup(func() {
			_, _ = c.do(context.Background(), http.MethodPost, "/api/terminals/"+dev+"/attach", object{"companion_id": origComp})
		})
		if _, err := c.do(t.Context(), http.MethodPost, "/api/terminals/"+dev+"/attach", object{"companion_id": compID}); err != nil {
			t.Fatalf("attach %s: %v", dev, err)
		}
	}

	for _, dev := range []string{"vocat-33d8", "box3-b324"} {
		t.Run(dev, func(t *testing.T) {
			out, err := exec.CommandContext(t.Context(), bin, "device", dev, "status").CombinedOutput()
			if err != nil {
				t.Fatalf("device %s status: %v\n%s", dev, err, out)
			}
			var st object
			if err := json.Unmarshal(out, &st); err != nil || st["connected"] != true {
				t.Fatalf("device %s not connected: %s", dev, out)
			}
			png := filepath.Join(t.TempDir(), dev+".png")
			if out, err := exec.CommandContext(t.Context(), bin, "device", dev, "screen", png).CombinedOutput(); err != nil {
				t.Fatalf("device %s screen: %v\n%s", dev, err, out)
			}
		})
	}

	t.Run("box3-b324/AutoCommitNoisyMic", func(t *testing.T) {
		body, _ := json.Marshal(object{"text": "What is two plus two?"})
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, c.base+"/api/tts", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Lark-Token", c.token)
		req.Header.Set("Content-Type", jsonType)
		res, err := c.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		wavRaw := make([]byte, 0, 1<<20)
		buf := make([]byte, 32<<10)
		for {
			n, rerr := res.Body.Read(buf)
			if n > 0 {
				wavRaw = append(wavRaw, buf[:n]...)
			}
			if rerr != nil {
				break
			}
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("TTS HTTP %d", res.StatusCode)
		}
		speechPCM, err := parseWAVPCM16k(wavRaw)
		if err != nil {
			t.Fatal(err)
		}
		// Append 4 seconds of low-level ambient noise (~350 RMS) after speech.
		noisePCM := make([]byte, sampleRate*2*4)
		var seed uint32 = 12345
		for i := 0; i < len(noisePCM); i += 2 {
			seed = seed*1664525 + 1013904223
			v := int16(int32(seed>>16)%1201 - 600)
			noisePCM[i] = byte(v)
			noisePCM[i+1] = byte(v >> 8)
		}
		noisyPath := filepath.Join(t.TempDir(), "noisy.wav")
		if err := writeWAV16k(noisyPath, append(speechPCM, noisePCM...)); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.CommandContext(t.Context(), bin, "device", "box3-b324", "mic", noisyPath).CombinedOutput(); err != nil {
			t.Fatalf("device mic: %v\n%s", err, out)
		}
		// Wake if asleep, then tap Home to start recording.
		for range 2 {
			if out, err := exec.CommandContext(t.Context(), bin, "device", "box3-b324", "tap", "160", "120").CombinedOutput(); err != nil {
				t.Fatalf("device tap: %v\n%s", err, out)
			}
			time.Sleep(250 * time.Millisecond)
			hits, err := c.do(t.Context(), http.MethodGet, "/api/terminals/box3-b324/hits", nil)
			if err == nil && str(hits["state"]) == "listening" {
				break
			}
		}
		// Assert auto-commit without tapping Send.
		deadline := time.Now().Add(10 * time.Second)
		committed := false
		for time.Now().Before(deadline) {
			hits, err := c.do(t.Context(), http.MethodGet, "/api/terminals/box3-b324/hits", nil)
			if err == nil {
				st := str(hits["state"])
				if st == "thinking" || st == "speaking" {
					committed = true
					break
				}
			}
			time.Sleep(150 * time.Millisecond)
		}
		if !committed {
			t.Fatal("box3-b324 did not auto-commit under speech + noise without Send")
		}
		// Wait for turn to finish so cleanup can restore greg cleanly.
		doneDeadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(doneDeadline) {
			hits, err := c.do(t.Context(), http.MethodGet, "/api/terminals/box3-b324/hits", nil)
			if err == nil && str(hits["state"]) == "idle" {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
	})
}
