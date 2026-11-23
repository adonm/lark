package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestSplitNameToleratesSpeechNearMisses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		text, name, rest string
		ok               bool
	}{
		{"Cake, what time is it", "Cake", "what time is it", true},
		{"Kate tell me a story", "Cake", "tell me a story", true},
		{"hey Minty, play music", "Mint", "play music", true},
		{"cake", "Cake", "", true},
		{"the story please", "Cake", "", false},
		{"I want a story", "Cake", "", false},
		{"Bo, play music", "Bo", "play music", true},
		{"bo tell me a story", "Bo", "tell me a story", true},
		{"Al, what's up", "Al", "what's up", true},
		{"Bob, play music", "Bo", "", false},
		{"go to sleep", "Bo", "", false},
		{"I am old", "Al", "", false},
	} {
		rest, ok := splitName(tc.text, tc.name)
		if ok != tc.ok || rest != tc.rest {
			t.Fatalf("splitName(%q, %q) = %q, %v; want %q, %v", tc.text, tc.name, rest, ok, tc.rest, tc.ok)
		}
	}
	if editDistance("kate", "cake") != 2 || editDistance("minty", "mint") != 1 {
		t.Fatal("name edit distance changed")
	}
}

func openTestListeningWindow(t *testing.T, b *larkServer, termID string) {
	t.Helper()
	b.hub.mu.Lock()
	ok := b.hub.startListeningWindow(b.hub.terminals[termID], time.Now())
	b.hub.mu.Unlock()
	if !ok {
		t.Fatal("listening window did not open")
	}
}

func appendSpeech(t *testing.T, hw *testWS, seq *uint16, n int) {
	frame := bytes.Repeat([]byte{0xd0, 0x07}, endpointBytes/2)
	for range n {
		*seq = *seq + 1
		hw.sendRTP(t, rtpPTRecPCM, *seq, uint32(*seq)*320, frame)
	}
}

func appendQuiet(t *testing.T, hw *testWS, seq *uint16, n int) {
	frame := make([]byte, endpointBytes)
	for range n {
		*seq = *seq + 1
		hw.sendRTP(t, rtpPTRecPCM, *seq, uint32(*seq)*320, frame)
	}
}

func TestListenDidntCatchSpeaksAndRearms(t *testing.T) {
	t.Parallel()
	b, base, prompts := listenHarness(t, func(string) string { return "Okay." })
	hw, _ := setupListenTerminal(t, b, base, "box3-miss", "Greg", true)
	b.setSTT(func(context.Context, []byte) (string, error) { return "", nil })
	openTestListeningWindow(t, b, "box3-miss")
	hw.until(t, isListenRecStart)
	hw.recStart(t, "")

	var seq uint16
	appendSpeech(t, hw, &seq, 60) // 1.2 s
	appendQuiet(t, hw, &seq, 70)  // > 1.2 s silence
	hw.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpStop
	})
	hw.recEnd(t, "", false)
	end := hw.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == ttsOpEnd
	})
	hw.sendProto(t, msgTTSAck, encodeTTSAck(false, binary.LittleEndian.Uint64(framePayload(end)[1:9])))
	hw.until(t, isListenRecStart) // re-armed after the feedback line
	hw.barrier(t)
	if len(*prompts) != 0 {
		t.Fatalf("feedback line reached the agent: %v", *prompts)
	}
	b.diag.mu.Lock()
	didntCatch := b.diag.listen.didntCatch.Load()
	b.diag.mu.Unlock()
	if didntCatch != 1 {
		t.Fatalf("didn't-catch count = %d, want 1", didntCatch)
	}
}

func TestListenNameRequiredAfterPause(t *testing.T) {
	t.Parallel()
	b, base, prompts := listenHarness(t, func(string) string { return "Okay." })
	hw, _ := setupListenTerminal(t, b, base, "box3-name", "Cake", true)
	b.setSTT(func(context.Context, []byte) (string, error) { return "tell me a story", nil })
	b.hub.mu.Lock()
	b.hub.terminals["box3-name"].nameFreeUntil = time.Now().Add(-time.Second)
	b.hub.mu.Unlock()

	openTestListeningWindow(t, b, "box3-name")
	hw.until(t, isListenRecStart)
	hw.recStart(t, "")
	var seq uint16
	appendSpeech(t, hw, &seq, 60)
	appendQuiet(t, hw, &seq, 70)
	hw.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpStop
	})
	hw.recEnd(t, "", false)
	hw.until(t, isListenRecStart) // ignored silently, then re-armed
	hw.barrier(t)
	if len(*prompts) != 0 {
		t.Fatalf("nameless speech after the pause reached the agent: %v", *prompts)
	}
	if got := b.diag.listen.nameRequired.Load(); got != 1 {
		t.Fatalf("name-required count = %d, want 1", got)
	}

	// A near miss re-opens the turn, and the name is stripped before the prompt.
	b.setSTT(func(context.Context, []byte) (string, error) { return "Kate, tell me a story", nil })
	hw.recStart(t, "")
	appendSpeech(t, hw, &seq, 60)
	appendQuiet(t, hw, &seq, 70)
	hw.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpStop
	})
	hw.recEnd(t, "", false)
	hw.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == ttsOpEnd
	})
	hw.barrier(t)
	if len(*prompts) != 1 || !bytes.Contains([]byte((*prompts)[0]), []byte("tell me a story")) {
		t.Fatalf("named turn missing: %v", *prompts)
	}
	if bytes.Contains([]byte((*prompts)[0]), []byte("Kate,")) {
		t.Fatalf("name leaked into the prompt: %q", (*prompts)[0])
	}
}

func TestListenNamePresentButUnusableSaysDidntCatch(t *testing.T) {
	t.Parallel()
	for i, stt := range []string{"Cake", "Cake um"} {
		b, base, prompts := listenHarness(t, func(string) string { return "Okay." })
		term := fmt.Sprintf("box3-nu%d", i)
		hw, _ := setupListenTerminal(t, b, base, term, "Cake", true)
		b.setSTT(func(context.Context, []byte) (string, error) { return stt, nil })
		b.hub.mu.Lock()
		b.hub.terminals[term].nameFreeUntil = time.Now().Add(-time.Second)
		b.hub.mu.Unlock()
		openTestListeningWindow(t, b, term)
		hw.until(t, isListenRecStart)
		hw.recStart(t, "")
		var seq uint16
		appendSpeech(t, hw, &seq, 60)
		appendQuiet(t, hw, &seq, 70)
		hw.until(t, func(f protocolFrame) bool {
			p := framePayload(f)
			return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpStop
		})
		hw.recEnd(t, "", false)
		hw.until(t, func(f protocolFrame) bool {
			p := framePayload(f)
			return hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == ttsOpEnd
		})
		hw.barrier(t)
		if len(*prompts) != 0 {
			t.Fatalf("%q reached the agent: %v", stt, *prompts)
		}
		if got := b.diag.listen.didntCatch.Load(); got != 1 {
			t.Fatalf("%q didn't-catch count = %d, want 1", stt, got)
		}
		if got := b.diag.listen.nameRequired.Load(); got != 0 {
			t.Fatalf("%q counted as missing name", stt)
		}
	}
}

// freeFollowUps is called from the turn goroutine and from playback under the
// hub lock; the clock must not be written without it. Run with -race.
func TestFollowUpGraceConcurrentAccess(t *testing.T) {
	t.Parallel()
	h := &terminalHub{}
	d := &terminalSession{}
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			h.freeFollowUps(d, now.Add(time.Duration(i)*time.Millisecond))
		}(i)
		go func() {
			defer wg.Done()
			h.nameRequired(d, now.Add(5*time.Second))
		}()
	}
	wg.Wait()
	// Refreshing an existing grace must move it forward, not keep the first.
	d.nameFreeUntil = now
	h.freeFollowUps(d, now.Add(time.Second))
	if got := d.nameFreeUntil; !got.Equal(now.Add(time.Second + followUpGrace)) {
		t.Fatalf("nameFreeUntil = %v, want a refreshed %v", got, now.Add(time.Second+followUpGrace))
	}
}
