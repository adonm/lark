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

	companionText := func() []string {
		var out []string
		ids, err := qadc.rpc.GetMessageIds(qadc.accID, chatID, false, false)
		if err != nil {
			return out
		}
		for _, id := range ids {
			if m, err := qadc.rpc.GetMessage(qadc.accID, id); err == nil && m.FromId == deltachat.ContactSelf {
				out = append(out, m.Text)
			}
		}
		return out
	}
	waitCount := func(n int, timeout time.Duration) []string {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if got := companionText(); len(got) >= n {
				return got
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %d companion messages", n)
		return nil
	}
	// Wait for the welcome so pairing has settled before the read-out.
	waitCount(1, 30*time.Second)
	settled := len(companionText())

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
	if _, err := qadc.rpc.SendMsg(qadc.accID, chatID, deltachat.MessageData{Text: &text}); err != nil {
		t.Fatal(err)
	}
	before := len(waitCount(settled+1, 40*time.Second))

	// Reply by voice at the device, as the child does after a read-out.
	run("say Thanks, see you at six.")
	run("wait idle 60s")

	got := waitCount(before+1, 40*time.Second)
	reply := strings.TrimSpace(got[len(got)-1])
	if reply == "" {
		t.Fatalf("device reply was delivered with empty text: %v", got)
	}
	if strings.Contains(strings.ToLower(reply), "couldn't reach") || strings.Contains(strings.ToLower(reply), "can't reach") {
		t.Fatalf("device reply claimed it could not reach the sender: %q", reply)
	}
}
