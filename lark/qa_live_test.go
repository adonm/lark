//go:build live

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
		return m.ViewType == deltachat.ViewtypeText && strings.Contains(m.Text, "ready")
	})
	panel := waitMsg(15*time.Second, func(m deltachat.Message) bool {
		return m.ViewType == deltachat.ViewtypeWebxdc
	})

	sendText := func(txt string) {
		_, err := qadc.rpc.SendMsg(qadc.accID, chatID, deltachat.MessageData{Text: &txt})
		if err != nil {
			t.Fatalf("SendMsg(%q): %v", txt, err)
		}
	}

	sendText("/help")
	waitMsg(15*time.Second, func(m deltachat.Message) bool {
		return strings.Contains(m.Text, "/say") && strings.Contains(m.Text, "/stories")
	})

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
	waitMsg(15*time.Second, func(m deltachat.Message) bool {
		return m.ViewType == deltachat.ViewtypeWebxdc
	})

	_ = qadc.rpc.SendWebxdcStatusUpdate(qadc.accID, panel.Id, `{"payload":{"v":1,"op":"set_listen","seconds":60}}`, nil)
	deadline := time.Now().Add(15 * time.Second)
	listenOK := false
	for time.Now().Before(deadline) {
		comp, err := c.do(ctx, http.MethodGet, "/api/companions/"+compID, nil)
		if err == nil && fmt.Sprint(comp["listen_seconds"]) == "60" {
			listenOK = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !listenOK {
		t.Fatalf("webxdc set_listen=60 did not take effect")
	}
	_ = qadc.rpc.SendWebxdcStatusUpdate(qadc.accID, panel.Id, `{"payload":{"v":1,"op":"set_listen","seconds":0}}`, nil)

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
}
