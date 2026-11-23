package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func listenHarness(t *testing.T, replyFn func(string) string) (*larkServer, string, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var prompts []string
	idleCh := make(chan struct{}, 16)
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.worker = fakeTTS(b.root, nil)
		b.endpoint = testEndpointSettings()
		b.oc = testOpenCodeOnDB(t, b.root, dir, b.db, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/api/session":
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"data":{"id":"ses-listen"}}`)
			case r.Method == http.MethodGet && r.URL.Path == "/api/session/ses-listen":
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"data":{"id":"ses-listen"}}`)
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/model"):
				w.WriteHeader(204)
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/prompt"):
				var body struct {
					Text string `json:"text"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				mu.Lock()
				prompts = append(prompts, body.Text)
				n := len(prompts)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"data":{"id":"prompt%d"}}`, n)
				select {
				case idleCh <- struct{}{}:
				default:
				}
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/message"):
				mu.Lock()
				n := len(prompts)
				last := ""
				if n > 0 {
					last = prompts[n-1]
				}
				reply := replyFn(last)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"data":[{"id":"ans%d","type":"assistant","finish":"stop","content":[{"type":"text","text":%q}]},{"id":"prompt%d","type":"user"}]}`, n, reply, n)
			case r.Method == http.MethodGet && r.URL.Path == "/api/event":
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				fmt.Fprint(w, ": connected\n\n")
				flusher.Flush()
				for {
					select {
					case <-idleCh:
						fmt.Fprint(w, "data: {\"type\":\"session.idle\",\"data\":{\"sessionID\":\"ses-listen\"}}\n\n")
						flusher.Flush()
					case <-r.Context().Done():
						return
					}
				}
			default:
				http.NotFound(w, r)
			}
		})
	})
	b.oc.feed.sig.wait(t, func() bool {
		b.oc.feed.mu.Lock()
		defer b.oc.feed.mu.Unlock()
		return b.oc.feed.connected
	})
	return b, base, &prompts
}

func setupListenTerminal(t *testing.T, b *larkServer, base, termID, compName string, listenSeconds int) (*testWS, string) {
	t.Helper()
	created := testObject(t, testRequest(t, http.MethodPost, base+"/api/companions", "unit", object{
		"name": compName,
		"form": "a mossy chameleon",
	}, 201))
	cid := str(created["id"])
	if listenSeconds > 0 {
		toolTest(t, base, "patch", object{
			"companion_id": cid,
			"companion":    object{"listen_seconds": listenSeconds},
		}, false)
	}
	hw := testConnectWS(t, base, "unit", false)
	hw.helloHardware(t, termID, cid, capDrag|capBridgeEndpoint)
	b.hub.mu.Lock()
	owner := b.hub.voiceOwners[cid]
	b.hub.mu.Unlock()
	if owner != termID {
		t.Fatalf("voice owner = %q, want %q", owner, termID)
	}
	return hw, cid
}

func sendTestUtterance(t *testing.T, hw *testWS) {
	t.Helper()
	voiced := make([]byte, endpointBytes)
	voiced[0] = 1
	var seq uint16 = 1
	var ts uint32 = 320
	for range 5 {
		hw.sendRTP(t, rtpPTRecPCM, seq, ts, voiced)
		seq++
		ts += 320
	}
	silent := make([]byte, endpointBytes)
	for range 235 {
		hw.sendRTP(t, rtpPTRecPCM, seq, ts, silent)
		seq++
		ts += 320
	}
}

func sendSpeechAndWaitTTS(t *testing.T, hw *testWS, b *larkServer, termID, sttText string) uint64 {
	t.Helper()
	b.setSTT(func(context.Context, []byte) (string, error) { return sttText, nil })
	b.hub.mu.Lock()
	d := b.hub.terminals[termID]
	b.hub.startRecording(d, "", time.Now(), true)
	b.hub.mu.Unlock()
	hw.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 2 && p[0] == recOpStart
	})
	hw.recStart(t, "")
	sendTestUtterance(t, hw)
	hw.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpStop
	})
	hw.recEnd(t, "", false)
	end := hw.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == ttsOpEnd
	})
	return binary.LittleEndian.Uint64(framePayload(end)[1:9])
}

func isListenRecStart(f protocolFrame) bool {
	p := framePayload(f)
	return hasProto(msgRecCtrl, "")(f) && len(p) >= 2 && p[0] == recOpStart && p[1]&recTargetListen != 0
}

func TestListenModeOffDoesNotOpenWindow(t *testing.T) {
	b, base, _ := listenHarness(t, func(string) string { return "Hello there." })
	hw, _ := setupListenTerminal(t, b, base, "box3-off", "Greg", 0)
	audioID := sendSpeechAndWaitTTS(t, hw, b, "box3-off", "Hi Greg")
	hw.sendProto(t, msgTTSAck, encodeTTSAck(false, audioID))
	time.Sleep(80 * time.Millisecond)
	b.hub.mu.Lock()
	d := b.hub.terminals["box3-off"]
	active := d != nil && d.recording.active()
	b.hub.mu.Unlock()
	if active {
		t.Fatal("listening window opened when listen_seconds=0")
	}
}

func TestListenModeOpensAfterPlaybackAckAndRearms(t *testing.T) {
	b, base, prompts := listenHarness(t, func(string) string { return "Got it." })
	hw, _ := setupListenTerminal(t, b, base, "box3-on", "Greg", 30)
	audioID := sendSpeechAndWaitTTS(t, hw, b, "box3-on", "Hi Greg")

	b.hub.mu.Lock()
	d := b.hub.terminals["box3-on"]
	beforeAck := d.recording.active()
	b.hub.mu.Unlock()
	if beforeAck {
		t.Fatal("listening window opened before playback ack")
	}

	hw.sendProto(t, msgTTSAck, encodeTTSAck(false, audioID))
	hw.until(t, isListenRecStart)
	hw.recStart(t, "")

	b.setSTT(func(context.Context, []byte) (string, error) { return "Tell me a story", nil })
	sendTestUtterance(t, hw)
	hw.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpStop
	})
	hw.recEnd(t, "", false)

	end2 := hw.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == ttsOpEnd
	})
	audioID2 := binary.LittleEndian.Uint64(framePayload(end2)[1:9])
	hw.sendProto(t, msgTTSAck, encodeTTSAck(false, audioID2))

	hw.until(t, isListenRecStart)
	if len(*prompts) < 2 {
		t.Fatalf("expected 2 prompts, got %d", len(*prompts))
	}
}

func TestListenModeSilenceTimesOut(t *testing.T) {
	b, base, prompts := listenHarness(t, func(string) string { return "Okay." })
	hw, _ := setupListenTerminal(t, b, base, "box3-sil", "Greg", 1)
	audioID := sendSpeechAndWaitTTS(t, hw, b, "box3-sil", "Hi Greg")
	hw.sendProto(t, msgTTSAck, encodeTTSAck(false, audioID))
	hw.until(t, isListenRecStart)
	hw.recStart(t, "")

	hw.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpCancel
	})
	if len(*prompts) != 1 {
		t.Fatalf("silence timeout should not trigger a new turn, got %d prompts", len(*prompts))
	}
}

func TestListenModeTapEndsWindowWithoutMenu(t *testing.T) {
	b, base, _ := listenHarness(t, func(string) string { return "Okay." })
	hw, _ := setupListenTerminal(t, b, base, "box3-tap", "Greg", 30)
	audioID := sendSpeechAndWaitTTS(t, hw, b, "box3-tap", "Hi Greg")
	hw.sendProto(t, msgTTSAck, encodeTTSAck(false, audioID))
	hw.until(t, isListenRecStart)
	hw.recStart(t, "")

	hw.tap(t, 100, 100, 0)
	hw.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpCancel
	})
	b.hub.mu.Lock()
	d := b.hub.terminals["box3-tap"]
	drawer := d.drawer
	active := d.recording.active()
	b.hub.mu.Unlock()
	if active || drawer != "" {
		t.Fatalf("tap should end window without opening menu: active=%v drawer=%q", active, drawer)
	}
}

func TestListenModeReplyToDeltaChatNotice(t *testing.T) {
	b, base, prompts := listenHarness(t, func(string) string { return "Sent to Mum." })
	hw, cid := setupListenTerminal(t, b, base, "box3-dc", "Greg", 30)

	cardSender := blankCompanion()
	cardSender.ID = "Mum"
	cardSender.Name = "Mum"
	b.hub.deliverNotice(t.Context(), terminalNotice{
		recipientIDs: []string{cid},
		sender:       cardSender,
		text:         "Dinner is ready",
		speakText:    "Message from Mum: Dinner is ready",
		voice:        b.voiceFor(cid),
		chime:        "intercom",
		showCard:     true,
		reply:        &turnReply{name: "Mum"},
	})

	end := hw.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == ttsOpEnd
	})
	audioID := binary.LittleEndian.Uint64(framePayload(end)[1:9])
	hw.sendProto(t, msgTTSAck, encodeTTSAck(false, audioID))

	hw.until(t, isListenRecStart)
	hw.recStart(t, "")

	b.setSTT(func(context.Context, []byte) (string, error) { return "Tell her I'm coming", nil })
	sendTestUtterance(t, hw)
	hw.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpStop
	})
	hw.recEnd(t, "", false)

	hw.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == ttsOpEnd
	})
	if len(*prompts) == 0 || !strings.Contains((*prompts)[0], "replying to Mum (Delta Chat)") {
		t.Fatalf("expected replying to Mum (Delta Chat) header, got %v", *prompts)
	}
}

func TestListenModeDropsFillerAndEmptyTranscripts(t *testing.T) {
	for _, filler := range []string{"", "   ", "...", "uh", "um", "hmm.", "uh-huh"} {
		if !isFillerTranscript(filler) {
			t.Fatalf("expected %q to be filler", filler)
		}
	}
	for _, real := range []string{"yes", "no", "hi Greg", "uh wait a second"} {
		if isFillerTranscript(real) {
			t.Fatalf("expected %q to be real speech", real)
		}
	}

	b, base, prompts := listenHarness(t, func(string) string { return "Okay." })
	hw, _ := setupListenTerminal(t, b, base, "box3-fill", "Greg", 1)
	audioID := sendSpeechAndWaitTTS(t, hw, b, "box3-fill", "Hi Greg")
	hw.sendProto(t, msgTTSAck, encodeTTSAck(false, audioID))
	hw.until(t, isListenRecStart)
	hw.recStart(t, "")

	b.setSTT(func(context.Context, []byte) (string, error) { return "um", nil })
	sendTestUtterance(t, hw)
	hw.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpCancel
	})
	if len(*prompts) != 1 {
		t.Fatalf("filler should not trigger LLM prompt, got %d prompts", len(*prompts))
	}
}

func TestListenModeSilentReplyProducesNoTTS(t *testing.T) {
	b, base, _ := listenHarness(t, func(text string) string {
		if strings.Contains(text, "Overheard") {
			return "[silent]"
		}
		return "Hello!"
	})
	hw, _ := setupListenTerminal(t, b, base, "box3-silrep", "Greg", 30)

	audioID := sendSpeechAndWaitTTS(t, hw, b, "box3-silrep", "Hi Greg")
	hw.sendProto(t, msgTTSAck, encodeTTSAck(false, audioID))
	hw.until(t, isListenRecStart)
	hw.recStart(t, "")

	b.setSTT(func(context.Context, []byte) (string, error) { return "Overheard chatter", nil })
	sendTestUtterance(t, hw)
	hw.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpStop
	})
	hw.recEnd(t, "", false)

	// Re-arms directly without any TTS_CTRL start/end.
	hw.until(t, isListenRecStart)
	b.hub.mu.Lock()
	d := b.hub.terminals["box3-silrep"]
	hasView := d.view != nil || d.card != nil
	b.hub.mu.Unlock()
	if hasView {
		t.Fatal("silent reply should not show a card")
	}
}

func TestListenModeOnlyOpensOnVoiceOwner(t *testing.T) {
	b, base, _ := listenHarness(t, func(string) string { return "Hello." })
	hw1, cid := setupListenTerminal(t, b, base, "box3-owner", "Greg", 30)
	hw2 := testConnectWS(t, base, "unit", false)
	hw2.helloHardware(t, "vocat-other", cid, capDrag|capBridgeEndpoint)
	b.hub.mu.Lock()
	if owner := b.hub.voiceOwners[cid]; owner != "box3-owner" {
		b.hub.mu.Unlock()
		t.Fatalf("voice owner = %q, want box3-owner", owner)
	}
	b.hub.mu.Unlock()

	audioID := sendSpeechAndWaitTTS(t, hw1, b, "box3-owner", "Hi Greg")
	hw1.sendProto(t, msgTTSAck, encodeTTSAck(false, audioID))

	hw1.until(t, isListenRecStart)
	time.Sleep(60 * time.Millisecond)
	b.hub.mu.Lock()
	otherActive := b.hub.terminals["vocat-other"].recording.active()
	b.hub.mu.Unlock()
	if otherActive {
		t.Fatal("listening window opened on non-voice-owner terminal")
	}
}

func TestListenModeCountdownIndicator(t *testing.T) {
	for _, spec := range []pixelDisplaySpec{pixelSpecVoCat, pixelSpecBox3} {
		idle := renderPixelHomeListen(spec, nil, nil, gradeDay, "", "", false)
		listen := renderPixelHomeListen(spec, nil, nil, gradeDay, "listening", "", true)
		if idle.Bounds() != listen.Bounds() {
			t.Fatalf("unexpected bounds mismatch for %dx%d", spec.logicalW, spec.logicalH)
		}
		if string(idle.Pix) == string(listen.Pix) {
			t.Fatalf("listening indicator did not change from idle for %dx%d", spec.logicalW, spec.logicalH)
		}
	}
}
