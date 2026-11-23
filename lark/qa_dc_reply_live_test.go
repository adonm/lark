//go:build live

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chatmail/rpc-client-go/v2/deltachat"
)

// TestQADeltaChatDeviceReplyLive checks the reported bug against a throwaway
// companion and a temporary Delta Chat account: a message is read out, the
// device answers by voice, and the answer must arrive back in the sender's chat.
func TestQADeltaChatDeviceReplyLive(t *testing.T) {
	if os.Getenv("LARK_QA_DC") == "" {
		t.Skip("set LARK_QA_DC=1 to run live Delta Chat QA")
	}
	c := qaLiveClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()
	compID := qaTempCompanion(t, c, "qa-dc-reply", "cat")

	pairPNG := filepath.Join(t.TempDir(), "pair.png")
	qaSimRun(t, "vocat", "sim-qa-reply-pair", fmt.Sprintf("attach %s\ntap-label Menu\ntap-label Next\ntap-label More\ntap-label Pair\nscreen %s\ntap-label pair-close\ntap-label Back\ntap-label Back\n", compID, pairPNG))

	qadc, err := startQADCClient(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(qadc.close)
	chatID, err := qadc.pairFromScreenPNG(pairPNG)
	if err != nil {
		t.Fatalf("pairFromScreenPNG: %v", err)
	}

	incoming := func() []deltachat.Message {
		var out []deltachat.Message
		ids, err := qadc.rpc.GetMessageIds(qadc.accID, chatID, false, false)
		if err != nil {
			return out
		}
		for _, id := range ids {
			if m, err := qadc.rpc.GetMessage(qadc.accID, id); err == nil && m.FromId != deltachat.ContactSelf {
				out = append(out, m)
			}
		}
		return out
	}
	waitCount := func(n int, timeout time.Duration) []deltachat.Message {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if got := incoming(); len(got) >= n {
				return got
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %d incoming messages", n)
		return nil
	}
	// Wait for the welcome so pairing and key exchange have settled.
	deadline := time.Now().Add(40 * time.Second)
	for {
		ok := slices.ContainsFunc(incoming(), func(m deltachat.Message) bool { return strings.Contains(m.Text, "/say") })
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the welcome")
		}
		time.Sleep(500 * time.Millisecond)
	}
	settled := len(incoming())

	bin := filepath.Join(env("XDG_DATA_HOME", filepath.Join(os.Getenv("HOME"), ".local/share")), "lark", "lark")
	sim := exec.CommandContext(ctx, bin, "sim", "--board", "vocat", "--id", "sim-qa-reply")
	stdin, err := sim.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := sim.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sim.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = sim.Process.Kill()
		_ = sim.Wait()
	})
	lines := bufio.NewScanner(stdout)
	run := func(line string) map[string]any {
		t.Helper()
		if _, err := fmt.Fprintln(stdin, line); err != nil {
			t.Fatalf("sim %q: %v", line, err)
		}
		if !lines.Scan() {
			t.Fatalf("sim %q produced no result", line)
		}
		var m map[string]any
		if err := json.Unmarshal(lines.Bytes(), &m); err != nil {
			t.Fatalf("sim %q: %v (%s)", line, err, lines.Text())
		}
		if m["ok"] != true {
			t.Fatalf("sim %q failed: %v", line, m)
		}
		return m
	}
	run("attach " + compID)

	// Send from Delta Chat. The companion answers over Delta Chat and reads the
	// message out on the attached device.
	text := "Dinner at six?"
	for sendErr := error(nil); ; time.Sleep(500 * time.Millisecond) {
		_, sendErr = qadc.rpc.SendMsg(qadc.accID, chatID, deltachat.MessageData{Text: &text})
		if sendErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("SendMsg: %v", sendErr)
		}
	}
	replied := waitCount(settled+1, 40*time.Second)
	lastID := replied[len(replied)-1].Id

	// The read-out and the companion's reply leave a card with a Reply button.
	// Tap it (or Home when no card is up) and answer by voice, as the child does.
	replyTap := ""
	replyDeadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(replyDeadline) {
		hits, err := c.do(ctx, "GET", "/api/terminals/sim-qa-reply/hits", nil)
		if err == nil {
			if list, ok := hits["hits"].([]any); ok {
				for _, item := range list {
					hit, _ := item.(object)
					if str(hit["action"]) != "reply" {
						continue
					}
					x1, _ := numInt(hit["x1"])
					x2, _ := numInt(hit["x2"])
					y1, _ := numInt(hit["y1"])
					y2, _ := numInt(hit["y2"])
					replyTap = fmt.Sprintf("%d %d", (x1+x2)/2, (y1+y2)/2)
				}
			}
		}
		if replyTap != "" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if replyTap != "" {
		run("tap " + replyTap)
	} else {
		run("tap 180 180")
	}
	run("say Thanks, see you at six.")
	run("wait idle 60s")

	var last deltachat.Message
	deadline = time.Now().Add(40 * time.Second)
	for {
		for _, m := range incoming() {
			if m.Id > lastID {
				last = m
			}
		}
		if last.Id != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("device reply did not arrive in the Delta Chat chat")
		}
		time.Sleep(500 * time.Millisecond)
	}
	spoke := strings.TrimSpace(last.Text)
	if spoke == "" && (last.File == nil || *last.File == "") {
		t.Fatalf("device reply arrived empty: %+v", last)
	}
	if strings.Contains(strings.ToLower(spoke), "couldn't reach") || strings.Contains(strings.ToLower(spoke), "can't reach") {
		t.Fatalf("device reply claimed it could not reach the sender: %q", spoke)
	}

}
