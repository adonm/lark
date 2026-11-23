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

	"github.com/chatmail/rpc-client-go/v2/deltachat"
)

// dcVoiceHarness is a Delta Chat server with a hardware terminal that can read
// out messages and take tap-to-talk replies.
func dcVoiceHarness(t *testing.T, replyFn func(prompt string) string) (*larkServer, string, *stubDCRPC, *[]string) {
	t.Helper()
	stub := newStubDCRPC()
	var mu sync.Mutex
	var prompts []string
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.worker = fakeTTS(b.root, nil)
		b.endpoint = testEndpointSettings()
		b.oc = testOpenCodeOnDB(t, b.root, dir, b.db, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/api/session":
				_, _ = w.Write([]byte(`{"data":{"id":"ses-dc"}}`))
			case r.Method == http.MethodPost && r.URL.Path == "/api/session/ses-dc/model":
				w.WriteHeader(204)
			case r.Method == http.MethodPost && r.URL.Path == "/api/session/ses-dc/prompt":
				var in struct{ Text string }
				_ = json.NewDecoder(r.Body).Decode(&in)
				mu.Lock()
				prompts = append(prompts, in.Text)
				mu.Unlock()
				_, _ = w.Write([]byte(`{"data":{"id":"msg-1"}}`))
			case r.Method == http.MethodPost && r.URL.Path == "/api/experimental/session/ses-dc/wait":
				w.WriteHeader(204)
			case r.Method == http.MethodGet && r.URL.Path == "/api/session/ses-dc/message":
				mu.Lock()
				last := ""
				if len(prompts) > 0 {
					last = prompts[len(prompts)-1]
				}
				mu.Unlock()
				reply := replyFn(last)
				_, _ = fmt.Fprintf(w, `{"data":[{"id":"msg-2","type":"assistant","finish":"stop","content":[{"type":"text","text":%q}]},{"id":"msg-1","type":"user"}]}`, reply)
			default:
				w.WriteHeader(404)
			}
		})
		b.dc = newDCService(b, "", t.TempDir(), "nine.testrun.org")
		b.dc.rpc = stub
		b.dc.ready.Store(true)
	})
	return b, base, stub, &prompts
}

func addDCHuman(stub *stubDCRPC, accID, contactID, chatID uint32, name, address string) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.contacts[accID][contactID] = deltachat.Contact{Id: contactID, DisplayName: name, Name: name, Address: address, IsVerified: true}
	stub.chats[accID][chatID] = deltachat.BasicChat{Id: chatID, ChatType: deltachat.ChatTypeSingle}
	stub.chatContacts[accID][chatID] = []uint32{deltachat.ContactSelf, contactID}
}

func addDCMessage(stub *stubDCRPC, accID, msgID, chatID, fromID uint32, text string) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.msgs[accID][msgID] = deltachat.Message{Id: msgID, ChatId: chatID, FromId: fromID, Text: text, ViewType: deltachat.ViewtypeText}
}

// ackNotice plays out the read-out and acknowledges it, as the device does.
func ackNotice(t *testing.T, hw *testWS) {
	t.Helper()
	end := hw.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == ttsOpEnd
	})
	audioID := binary.LittleEndian.Uint64(framePayload(end)[1:9])
	hw.sendProto(t, msgTTSAck, encodeTTSAck(false, audioID))
	hw.barrier(t)
}

func dcSent(stub *stubDCRPC) []stubDCSent {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return append([]stubDCSent(nil), stub.sent...)
}

// tapUtterance records a tap-to-talk reply without waiting for spoken audio.
func tapUtterance(t *testing.T, hw *testWS, b *larkServer, termID, sttText string) {
	t.Helper()
	b.stt = func(context.Context, []byte) (string, error) { return sttText, nil }
	b.ws.mu.Lock()
	d := b.ws.terminals[termID]
	b.ws.startRecording(d, "", time.Now(), true)
	b.ws.mu.Unlock()
	hw.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 2 && p[0] == recOpStart
	})
	hw.recStart(t, "")
	sendTestUtterance(t, hw)
	hw.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpStop
	})
	hw.recEnd(t, "", false)
}

func waitTurnDone(t *testing.T, b *larkServer, termID string) {
	t.Helper()
	b.ws.sig.wait(t, func() bool {
		b.ws.mu.Lock()
		defer b.ws.mu.Unlock()
		d := b.ws.terminals[termID]
		return d != nil && d.turn == 0 && !d.busy
	})
}

func promptCount(prompts *[]string) int {
	return len(*prompts)
}

// TestDeviceReplyToDeltaChatHuman covers the reported bug: a message is read
// out, the child taps and answers by voice, and the reply must reach the human
// sender instead of only being spoken in the room.
func TestDeviceReplyToDeltaChatHuman(t *testing.T) {
	b, base, stub, prompts := dcVoiceHarness(t, func(prompt string) string {
		if strings.Contains(prompt, "via Delta Chat") {
			return "[silent]"
		}
		return "Coming, Mum!"
	})
	greg, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "a cat"})
	if err != nil {
		t.Fatal(err)
	}
	addDCHuman(stub, greg.DCAccountID, 10, 200, "Mum", "mum@example.org")
	addDCMessage(stub, greg.DCAccountID, 1, 200, 10, "Dinner at six")

	hw := testConnectWS(t, base, "unit", false)
	hw.helloHardware(t, "box3-dc", "greg", capDrag|capBridgeEndpoint)

	b.dc.handleMessage(t.Context(), greg.DCAccountID, 1)
	ackNotice(t, hw)
	if sent := dcSent(stub); len(sent) != 0 {
		t.Fatalf("silent read-out turn sent %+v", sent)
	}

	sendSpeechAndWaitTTS(t, hw, b, "box3-dc", "Yes, I'll be there")
	if sent := dcSent(stub); len(sent) != 1 || sent[0].accID != greg.DCAccountID || sent[0].chatID != 200 ||
		sent[0].data.Text == nil || *sent[0].data.Text != "Coming, Mum!" {
		t.Fatalf("read-out reply did not reach Mum in chat 200: %+v", sent)
	}
	if len(*prompts) < 2 || !strings.Contains((*prompts)[1], "replying to Mum (Delta Chat)") {
		t.Fatalf("in-person reply lost its Delta Chat context: %v", *prompts)
	}

	// The read-out context covers one reply only.
	sendSpeechAndWaitTTS(t, hw, b, "box3-dc", "What time is it?")
	if sent := dcSent(stub); len(sent) != 1 {
		t.Fatalf("a follow-up turn repeated the Delta Chat delivery: %+v", sent)
	}
	if len(*prompts) < 3 || strings.Contains((*prompts)[2], "replying to Mum") {
		t.Fatalf("read-out context leaked into a later turn: %v", *prompts)
	}
}

// TestDeviceReplyToDeltaChatCompanion covers a companion-to-companion read-out.
func TestDeviceReplyToDeltaChatCompanion(t *testing.T) {
	b, base, stub, prompts := dcVoiceHarness(t, func(string) string { return "See you soon, Cake!" })
	greg, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "a cat"})
	if err != nil {
		t.Fatal(err)
	}
	cake, err := b.createCompanion(t.Context(), object{"id": "cake", "name": "Cake", "form": "a fox"})
	if err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	stub.contacts[greg.DCAccountID][12] = deltachat.Contact{Id: 12, Address: stub.configs[cake.DCAccountID]["addr"], DisplayName: "cake", IsVerified: true}
	stub.chats[greg.DCAccountID][300] = deltachat.BasicChat{Id: 300, ChatType: deltachat.ChatTypeSingle}
	stub.chatContacts[greg.DCAccountID][300] = []uint32{deltachat.ContactSelf, 12}
	stub.mu.Unlock()
	addDCMessage(stub, greg.DCAccountID, 2, 300, 12, "Snacks at mine?")

	hw := testConnectWS(t, base, "unit", false)
	hw.helloHardware(t, "box3-dc2", "greg", capDrag|capBridgeEndpoint)

	b.dc.handleMessage(t.Context(), greg.DCAccountID, 2)
	ackNotice(t, hw)
	if got := promptCount(prompts); got != 0 {
		t.Fatalf("read-out of a companion message should not prompt, got %d", got)
	}

	sendSpeechAndWaitTTS(t, hw, b, "box3-dc2", "Yes, see you soon")
	if sent := dcSent(stub); len(sent) != 1 || sent[0].accID != greg.DCAccountID || sent[0].chatID != 300 ||
		sent[0].data.Text == nil || *sent[0].data.Text != "See you soon, Cake!" {
		t.Fatalf("companion read-out reply did not reach Cake: %+v", sent)
	}
	if !strings.Contains((*prompts)[0], "replying to Cake (Delta Chat)") {
		t.Fatalf("missing companion Delta Chat context: %v", *prompts)
	}
}

// TestDeviceReplyExplicitEmitWins proves an agent-chosen target is honoured.
func TestDeviceReplyExplicitEmitWins(t *testing.T) {
	var b *larkServer
	b, base, stub, _ := dcVoiceHarness(t, func(prompt string) string {
		if strings.Contains(prompt, "via Delta Chat") {
			return "[silent]"
		}
		if _, err := b.callMCP(context.Background(), "emit", object{
			"to":       "Mum",
			"intercom": object{"from": "greg", "text": "Dinner is at six"},
		}); err != nil {
			return "I couldn't send that."
		}
		return "Told her."
	})
	greg, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "a cat"})
	if err != nil {
		t.Fatal(err)
	}
	addDCHuman(stub, greg.DCAccountID, 10, 200, "Mum", "mum@example.org")
	addDCMessage(stub, greg.DCAccountID, 1, 200, 10, "Dinner at six")

	hw := testConnectWS(t, base, "unit", false)
	hw.helloHardware(t, "box3-dc3", "greg", capDrag|capBridgeEndpoint)
	b.dc.handleMessage(t.Context(), greg.DCAccountID, 1)
	ackNotice(t, hw)

	sendSpeechAndWaitTTS(t, hw, b, "box3-dc3", "Tell her six")
	sent := dcSent(stub)
	if len(sent) != 1 || sent[0].data.Text == nil || *sent[0].data.Text != "Dinner is at six" {
		t.Fatalf("explicit emit should be the only delivery: %+v", sent)
	}
}

// TestDeviceReplySilentSendsNothing: [silent] on the in-person reply is quiet.
func TestDeviceReplySilentSendsNothing(t *testing.T) {
	b, base, stub, _ := dcVoiceHarness(t, func(string) string { return "[silent]" })
	greg, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "a cat"})
	if err != nil {
		t.Fatal(err)
	}
	addDCHuman(stub, greg.DCAccountID, 10, 200, "Mum", "mum@example.org")
	addDCMessage(stub, greg.DCAccountID, 1, 200, 10, "Dinner at six")

	hw := testConnectWS(t, base, "unit", false)
	hw.helloHardware(t, "box3-dc4", "greg", capDrag|capBridgeEndpoint)
	b.dc.handleMessage(t.Context(), greg.DCAccountID, 1)
	ackNotice(t, hw)

	tapUtterance(t, hw, b, "box3-dc4", "Nothing to say")
	waitTurnDone(t, b, "box3-dc4")
	if sent := dcSent(stub); len(sent) != 0 {
		t.Fatalf("silent reply sent %+v", sent)
	}
}

// TestDeviceTurnWithoutReadoutNotForwarded guards normal in-person turns.
func TestDeviceTurnWithoutReadoutNotForwarded(t *testing.T) {
	b, base, stub, prompts := dcVoiceHarness(t, func(string) string { return "It's four o'clock." })
	greg, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "a cat"})
	if err != nil {
		t.Fatal(err)
	}
	addDCHuman(stub, greg.DCAccountID, 10, 200, "Mum", "mum@example.org")

	hw := testConnectWS(t, base, "unit", false)
	hw.helloHardware(t, "box3-dc5", "greg", capDrag|capBridgeEndpoint)
	sendSpeechAndWaitTTS(t, hw, b, "box3-dc5", "What time is it?")
	if sent := dcSent(stub); len(sent) != 0 {
		t.Fatalf("plain in-person turn was forwarded: %+v", sent)
	}
	if strings.Contains((*prompts)[0], "Delta Chat") {
		t.Fatalf("plain turn carried Delta Chat context: %v", *prompts)
	}
}

// TestEmitIntercomMatchesContactAddress covers me.contacts names falling back
// to the address, and rejects unknown names instead of partially matching.
func TestEmitIntercomMatchesContactAddress(t *testing.T) {
	b, base, stub := dcTestHarness(t)
	greg, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "a cat"})
	if err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	stub.contacts[greg.DCAccountID][11] = deltachat.Contact{Id: 11, Address: "dad@example.org", IsVerified: true}
	stub.mu.Unlock()

	out := toolTest(t, base, "emit", object{"to": "dad@example.org", "intercom": object{"from": "greg", "text": "Hi Dad"}}, false)
	intercom, _ := out["intercom"].(object)
	sent, _ := intercom["sent_to"].([]any)
	if len(sent) != 1 || sent[0] != "dad@example.org" {
		t.Fatalf("address-name emit = %+v", out)
	}
	if got := dcSent(stub); len(got) != 1 || got[0].data.Text == nil || *got[0].data.Text != "Hi Dad" {
		t.Fatalf("address-name emit did not send: %+v", got)
	}

	toolTest(t, base, "emit", object{"to": "d", "intercom": object{"from": "greg", "text": "Hi"}}, true)
	if got := dcSent(stub); len(got) != 1 {
		t.Fatalf("partial name must not send: %+v", got)
	}
}

// TestSayReadoutCardRepliesToHumanSender covers the card Reply button after a
// /say read-out: it used to target the companion itself, so the voice note
// failed and the device said it could not reach greg.
func TestSayReadoutCardRepliesToHumanSender(t *testing.T) {
	b, base, stub, _ := dcVoiceHarness(t, func(string) string { return "ok" })
	ttsCh := make(chan string, 8)
	b.worker = &synthWorker{requests: make(chan ttsRequest)}
	b.worker.ready.Store(true)
	go func() {
		for {
			select {
			case <-b.root.Done():
				return
			case req := <-b.worker.requests:
				ttsCh <- req.text
				req.done <- ttsResult{pcm: []byte{1, 2, 3, 4}}
			}
		}
	}()
	greg, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "a cat"})
	if err != nil {
		t.Fatal(err)
	}
	addDCHuman(stub, greg.DCAccountID, 10, 200, "Mum", "mum@example.org")
	hw := testConnectWS(t, base, "unit", false)
	hw.helloHardware(t, "box3-card", "greg", capDrag|capBridgeEndpoint)

	b.sayOnCompanion(t.Context(), "greg", "Mum", 200, "Dinner is ready")
	b.ws.mu.Lock()
	d := b.ws.terminals["box3-card"]
	target := ""
	if d.view != nil {
		target = d.view.actions["reply"].value
	}
	b.ws.mu.Unlock()
	if target != "mum" {
		t.Fatalf("card reply target = %q, want mum", target)
	}
	ackNotice(t, hw)

	b.stt = func(context.Context, []byte) (string, error) { return "See you at six", nil }
	b.ws.mu.Lock()
	b.ws.startRecording(d, target, time.Now(), true)
	b.ws.mu.Unlock()
	hw.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 2 && p[0] == recOpStart
	})
	hw.recStart(t, "")
	sendTestUtterance(t, hw)
	hw.recEnd(t, "", false)

	var sent []stubDCSent
	sendDeadline := time.Now().Add(3 * time.Second)
	for len(sent) == 0 && time.Now().Before(sendDeadline) {
		sent = dcSent(stub)
		time.Sleep(10 * time.Millisecond)
	}
	if len(sent) != 1 || sent[0].accID != greg.DCAccountID || sent[0].data.Viewtype == nil ||
		*sent[0].data.Viewtype != deltachat.ViewtypeVoice {
		t.Fatalf("card reply did not send a voice note to Mum: %+v", sent)
	}
	spoken := ""
	ttsDeadline := time.After(3 * time.Second)
loop:
	for {
		select {
		case got := <-ttsCh:
			spoken = got
			if strings.Contains(got, "Sent to") {
				break loop
			}
		case <-ttsDeadline:
			break loop
		}
	}
	if spoken != "Sent to Mum!" {
		t.Fatalf("confirmation = %q, want %q", spoken, "Sent to Mum!")
	}
}

func TestIntercomCardReplyTargetsConversationPartner(t *testing.T) {
	if got := (intercomCard{companion: "Greg", reply: "Mum"}).scene().actions["reply"].value; got != "mum" {
		t.Fatalf("card reply target = %q, want mum", got)
	}
	if got := (intercomCard{companion: "Cake"}).scene().actions["reply"].value; got != "cake" {
		t.Fatalf("card without an explicit target = %q, want cake", got)
	}
}
