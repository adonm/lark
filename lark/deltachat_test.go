package main

import (
	"context"
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chatmail/rpc-client-go/v2/deltachat"
)

type stubDCSent struct {
	accID  uint32
	chatID uint32
	data   deltachat.MessageData
}

type stubDCRPC struct {
	mu       sync.Mutex
	nextAcc  uint32
	nextChat uint32
	configs  map[uint32]map[string]string
	contacts map[uint32]map[uint32]deltachat.Contact
	chats        map[uint32]map[uint32]deltachat.BasicChat
	chatContacts map[uint32]map[uint32][]uint32
	msgs         map[uint32]map[uint32]deltachat.Message
	xdcUpdates   map[uint32]map[uint32][]json.RawMessage
	sent         []stubDCSent
	qrJoined     []string
	deleted      []uint32
}

func newStubDCRPC() *stubDCRPC {
	return &stubDCRPC{
		nextAcc:      10,
		nextChat:     100,
		configs:      make(map[uint32]map[string]string),
		contacts:     make(map[uint32]map[uint32]deltachat.Contact),
		chats:        make(map[uint32]map[uint32]deltachat.BasicChat),
		chatContacts: make(map[uint32]map[uint32][]uint32),
		msgs:         make(map[uint32]map[uint32]deltachat.Message),
		xdcUpdates:   make(map[uint32]map[uint32][]json.RawMessage),
	}
}

func (s *stubDCRPC) AddAccount() (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextAcc++
	id := s.nextAcc
	s.configs[id] = make(map[string]string)
	s.contacts[id] = make(map[uint32]deltachat.Contact)
	s.chats[id] = make(map[uint32]deltachat.BasicChat)
	s.chatContacts[id] = make(map[uint32][]uint32)
	s.msgs[id] = make(map[uint32]deltachat.Message)
	return id, nil
}

func (s *stubDCRPC) AddTransportFromQr(accID uint32, qr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.configs[accID] == nil {
		s.configs[accID] = make(map[string]string)
	}
	s.configs[accID]["addr"] = strings.TrimPrefix(qr, "dcaccount:") + "-" + string(rune('a'+accID)) + "@test.example"
	return nil
}

func (s *stubDCRPC) BatchSetConfig(accID uint32, cfg map[string]*string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.configs[accID] == nil {
		s.configs[accID] = make(map[string]string)
	}
	for k, v := range cfg {
		if v != nil {
			s.configs[accID][k] = *v
		}
	}
	return nil
}

func (s *stubDCRPC) SetConfig(accID uint32, key string, val *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.configs[accID] == nil {
		s.configs[accID] = make(map[string]string)
	}
	if val != nil {
		s.configs[accID][key] = *val
	}
	return nil
}

func (s *stubDCRPC) GetConfig(accID uint32, key string) (*string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.configs[accID][key]
	return &v, nil
}

func (s *stubDCRPC) StartIoForAllAccounts() error { return nil }

func (s *stubDCRPC) GetChatSecurejoinQrCode(accID uint32, _ *uint32) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return "OPENPGP4FPR:TEST#a=" + s.configs[accID]["addr"], nil
}

func (s *stubDCRPC) CreateQrSvg(text string) (string, error) {
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512"><rect x="0" y="0" width="512" height="512" style="fill:#ffffff"/><g transform="translate(48,48)"><path style="fill:#000000" data-qr="` + text + `" d="M0,0h200v200h-200z"/></g></svg>`, nil
}

func (s *stubDCRPC) SecureJoin(accID uint32, qr string) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.qrJoined = append(s.qrJoined, qr)
	s.nextChat++
	return s.nextChat, nil
}

func (s *stubDCRPC) GetContacts(accID uint32, _ uint32, _ *string) ([]deltachat.Contact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []deltachat.Contact
	for _, c := range s.contacts[accID] {
		out = append(out, c)
	}
	return out, nil
}

func (s *stubDCRPC) GetContact(accID uint32, contactID uint32) (deltachat.Contact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.contacts[accID][contactID], nil
}

func (s *stubDCRPC) DeleteContact(accID uint32, contactID uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.contacts[accID], contactID)
	s.deleted = append(s.deleted, contactID)
	return nil
}

func (s *stubDCRPC) GetChatIdByContactId(accID uint32, contactID uint32) (*uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, ch := range s.chats[accID] {
		if ch.Id == contactID+1000 {
			return &id, nil
		}
	}
	return nil, nil
}

func (s *stubDCRPC) CreateChatByContactId(accID uint32, contactID uint32) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cid := contactID + 1000
	if s.chats[accID] == nil {
		s.chats[accID] = make(map[uint32]deltachat.BasicChat)
	}
	s.chats[accID][cid] = deltachat.BasicChat{Id: cid, ChatType: deltachat.ChatTypeSingle}
	if s.chatContacts[accID] == nil {
		s.chatContacts[accID] = make(map[uint32][]uint32)
	}
	s.chatContacts[accID][cid] = []uint32{deltachat.ContactSelf, contactID}
	return cid, nil
}

func (s *stubDCRPC) DeleteChat(accID uint32, chatID uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.chats[accID], chatID)
	return nil
}

func (s *stubDCRPC) GetBasicChatInfo(accID uint32, chatID uint32) (deltachat.BasicChat, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.chats[accID][chatID]; ok {
		return ch, nil
	}
	return deltachat.BasicChat{Id: chatID, ChatType: deltachat.ChatTypeSingle}, nil
}

func (s *stubDCRPC) GetChatContacts(accID uint32, chatID uint32) ([]uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if list, ok := s.chatContacts[accID][chatID]; ok {
		return slices.Clone(list), nil
	}
	var out []uint32
	for cid := range s.contacts[accID] {
		out = append(out, cid)
	}
	return out, nil
}

func (s *stubDCRPC) GetMessage(accID uint32, msgID uint32) (deltachat.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.msgs[accID][msgID], nil
}

func (s *stubDCRPC) SendMsg(accID uint32, chatID uint32, data deltachat.MessageData) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, stubDCSent{accID: accID, chatID: chatID, data: data})
	return uint32(len(s.sent)), nil
}

func (s *stubDCRPC) GetWebxdcStatusUpdates(accID uint32, instanceMsgID uint32, lastKnownSerial uint32) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.xdcUpdates[accID][instanceMsgID]
	var filtered []json.RawMessage
	for _, raw := range list {
		var env struct {
			Serial uint32 `json:"serial"`
		}
		if json.Unmarshal(raw, &env) == nil && env.Serial > lastKnownSerial {
			filtered = append(filtered, raw)
		}
	}
	b, _ := json.Marshal(filtered)
	return string(b), nil
}

func (s *stubDCRPC) SendWebxdcStatusUpdate(accID uint32, instanceMsgID uint32, updateStr string, _ *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.xdcUpdates[accID] == nil {
		s.xdcUpdates[accID] = make(map[uint32][]json.RawMessage)
	}
	serial := uint32(len(s.xdcUpdates[accID][instanceMsgID]) + 1)
	var obj map[string]any
	if err := json.Unmarshal([]byte(updateStr), &obj); err != nil {
		return err
	}
	obj["serial"] = serial
	raw, _ := json.Marshal(obj)
	s.xdcUpdates[accID][instanceMsgID] = append(s.xdcUpdates[accID][instanceMsgID], raw)
	return nil
}

func dcTestHarness(t *testing.T) (*larkServer, string, *stubDCRPC) {
	t.Helper()
	stub := newStubDCRPC()
	var prompts []string
	var mu sync.Mutex
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.oc = testOpenCodeOnDB(t, b.root, dir, b.db, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/api/session":
				_, _ = w.Write([]byte(`{"data":{"id":"ses-dc"}}`))
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
				_, _ = w.Write([]byte(`{"data":[{"id":"msg-2","type":"assistant","finish":"stop","content":[{"type":"text","text":"Hello Alice!"}]},{"id":"msg-1","type":"user"}]}`))
			default:
				w.WriteHeader(404)
			}
		})
		b.dc = newDCService(b, "", t.TempDir(), "nine.testrun.org")
		b.dc.rpc = stub
		b.dc.ready.Store(true)
	})
	_ = prompts
	return b, base, stub
}

func TestDeltaChatVerifiedTextAndNonContactIgnored(t *testing.T) {
	b, _, stub := dcTestHarness(t)
	cake, err := b.createCompanion(t.Context(), object{"name": "Cake", "form": "a cat"})
	if err != nil {
		t.Fatal(err)
	}
	accID := cake.DCAccountID
	if accID == 0 {
		t.Fatal("expected Delta Chat account to be provisioned")
	}

	// Verified human contact.
	stub.mu.Lock()
	stub.contacts[accID][10] = deltachat.Contact{Id: 10, DisplayName: "Alice", Address: "alice@example.org", IsVerified: true}
	stub.chats[accID][200] = deltachat.BasicChat{Id: 200, ChatType: deltachat.ChatTypeSingle}
	stub.msgs[accID][1] = deltachat.Message{Id: 1, ChatId: 200, FromId: 10, Text: "Hi Cake!", ViewType: deltachat.ViewtypeText}

	// Unverified stranger.
	stub.contacts[accID][11] = deltachat.Contact{Id: 11, DisplayName: "Stranger", Address: "stranger@example.org", IsVerified: false}
	stub.chats[accID][201] = deltachat.BasicChat{Id: 201, ChatType: deltachat.ChatTypeSingle, IsContactRequest: true}
	stub.msgs[accID][2] = deltachat.Message{Id: 2, ChatId: 201, FromId: 11, Text: "Spam!", ViewType: deltachat.ViewtypeText}
	stub.mu.Unlock()

	// Stranger is ignored: no turn, no reply, no history entry.
	b.dc.handleMessage(t.Context(), accID, 2)
	stub.mu.Lock()
	sentAfterStranger := len(stub.sent)
	stub.mu.Unlock()
	if sentAfterStranger != 0 {
		t.Fatalf("expected non-contact to be ignored, got %d sent", sentAfterStranger)
	}

	// Verified contact produces a turn, writes history, and sends a reply.
	b.dc.handleMessage(t.Context(), accID, 1)
	stub.mu.Lock()
	if len(stub.sent) != 1 || stub.sent[0].chatID != 200 || stub.sent[0].data.Text == nil || *stub.sent[0].data.Text != "Hello Alice!" {
		t.Fatalf("unexpected DC reply: %+v", stub.sent)
	}
	stub.mu.Unlock()
	entries, err := b.history.list(t.Context(), cake.ID)
	if err != nil || len(entries) != 2 || entries[0].Source != "deltachat" || entries[1].Text != "Hello Alice!" {
		t.Fatalf("unexpected history: %+v (%v)", entries, err)
	}
}

func TestDeltaChatPairMenuRendersQRAndUnpairRemovesContact(t *testing.T) {
	b, base, stub := dcTestHarness(t)
	cake, err := b.createCompanion(t.Context(), object{"name": "Cake", "form": "a cat"})
	if err != nil {
		t.Fatal(err)
	}
	accID := cake.DCAccountID
	stub.mu.Lock()
	stub.contacts[accID][42] = deltachat.Contact{Id: 42, DisplayName: "Alice", Address: "alice@example.org", IsVerified: true}
	stub.mu.Unlock()

	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, "vocat-33d8", cake.ID)
	bindMenu(dev, b.ws, "vocat-33d8")

	// Open Menu -> Next -> More -> Pair.
	tapMenu(t, dev, "menu", "menu-next")
	tapMenu(t, dev, "menu-next", "menu-more")
	tapMenu(t, dev, "menu-more", "menu-pair", "menu-unpair")
	pairSrc := tapMenu(t, dev, "menu-pair", "pair-close")
	if !strings.Contains(pairSrc, "pair-close") {
		t.Fatalf("expected pair drawer, got %q", pairSrc)
	}
	b.ws.mu.Lock()
	d := b.ws.terminals["vocat-33d8"]
	hasBlack := d != nil && slices.Contains(d.svg.fb, uint16(0x0000))
	b.ws.mu.Unlock()
	if !hasBlack {
		t.Fatal("Pair QR rendered without any black QR pixels")
	}

	// Back -> Unpair -> tap Alice -> removed.
	tapMenu(t, dev, "pair-close", "menu-unpair")
	tapMenu(t, dev, "menu-unpair", "Alice")
	tapMenu(t, dev, "unpair-0", "No paired contacts")
	stub.mu.Lock()
	removed := slices.Contains(stub.deleted, 42)
	stub.mu.Unlock()
	if !removed {
		t.Fatal("Unpair did not delete contact 42")
	}
}

func TestDeltaChatIntercomBetweenCompanions(t *testing.T) {
	b, base, stub := dcTestHarness(t)
	cake, err := b.createCompanion(t.Context(), object{"name": "Cake", "form": "a cat"})
	if err != nil {
		t.Fatal(err)
	}
	mint, err := b.createCompanion(t.Context(), object{"name": "Mint", "form": "a cat"})
	if err != nil {
		t.Fatal(err)
	}
	mintAddr := stub.configs[mint.DCAccountID]["addr"]
	cakeAddr := stub.configs[cake.DCAccountID]["addr"]

	// Wire verified peer contacts in both directions.
	stub.mu.Lock()
	stub.contacts[cake.DCAccountID][2] = deltachat.Contact{Id: 2, DisplayName: "Mint", Address: mintAddr, IsVerified: true, IsBot: true}
	stub.contacts[mint.DCAccountID][3] = deltachat.Contact{Id: 3, DisplayName: "Cake", Address: cakeAddr, IsVerified: true, IsBot: true}
	stub.mu.Unlock()

	mintDev := testConnectWS(t, base, "unit", false)
	mintDev.hello(t, "vocat-2cac", mint.ID)

	out := toolTest(t, base, "emit", object{"to": "Mint", "intercom": object{"from": cake.ID, "text": "Tea time!"}}, false)
	if !slices.Equal(out["intercom"].(object)["sent_to"].([]any), []any{mint.ID}) {
		t.Fatalf("unexpected emit intercom result: %v", out)
	}
	stub.mu.Lock()
	if len(stub.sent) != 1 || stub.sent[0].accID != cake.DCAccountID || *stub.sent[0].data.Text != "Tea time!" {
		t.Fatalf("intercom did not send via DC: %+v", stub.sent)
	}
	// Simulate Mint receiving that DC message.
	stub.chats[mint.DCAccountID][300] = deltachat.BasicChat{Id: 300, ChatType: deltachat.ChatTypeSingle}
	stub.msgs[mint.DCAccountID][77] = deltachat.Message{Id: 77, ChatId: 300, FromId: 3, Text: "Tea time!", ViewType: deltachat.ViewtypeText}
	stub.mu.Unlock()

	b.dc.handleMessage(t.Context(), mint.DCAccountID, 77)
	mintDev.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgChime, "")(f) && len(p) >= 1 && p[0] == chimeIntercom
	})
	waitSVG(t, b.ws, "vocat-2cac", "Tea time!")
}

func TestDeltaChatVoiceNoteTranscribedAndReplied(t *testing.T) {
	b, _, stub := dcTestHarness(t)
	var sttCalled bool
	b.stt = func(_ context.Context, pcm []byte) (string, error) {
		sttCalled = len(pcm) > 0
		return "Hello from voice note", nil
	}
	cake, err := b.createCompanion(t.Context(), object{"name": "Cake", "form": "a cat"})
	if err != nil {
		t.Fatal(err)
	}
	accID := cake.DCAccountID

	// Create a tiny valid WAV/OGG file if ffmpeg is available; otherwise test via a dummy file if ffmpeg is missing.
	tmpDir := t.TempDir()
	voicePath := filepath.Join(tmpDir, "note.ogg")
	wavPath := filepath.Join(tmpDir, "note.wav")
	// Generate 0.2s of 16kHz silence WAV.
	pcm := make([]byte, 16000*2/5)
	if err := encodePCMToOpus(t.Context(), pcm, sampleRate, voicePath); err != nil {
		// If ffmpeg is not installed in the test environment, skip the ffmpeg subprocess part cleanly.
		t.Skipf("ffmpeg not available for voice note decode test: %v", err)
	}
	_ = os.Remove(wavPath)

	stub.mu.Lock()
	stub.contacts[accID][10] = deltachat.Contact{Id: 10, DisplayName: "Alice", Address: "alice@example.org", IsVerified: true}
	stub.chats[accID][200] = deltachat.BasicChat{Id: 200, ChatType: deltachat.ChatTypeSingle}
	stub.msgs[accID][5] = deltachat.Message{
		Id:       5,
		ChatId:   200,
		FromId:   10,
		File:     &voicePath,
		ViewType: deltachat.ViewtypeVoice,
	}
	stub.mu.Unlock()

	b.dc.handleMessage(t.Context(), accID, 5)
	if !sttCalled {
		t.Fatal("voice note was not transcribed by STT")
	}
	stub.mu.Lock()
	if len(stub.sent) != 1 || *stub.sent[0].data.Text != "Hello Alice!" {
		t.Fatalf("voice note did not produce a reply: %+v", stub.sent)
	}
	stub.mu.Unlock()
	_ = httptest.NewServer
}

func TestDeltaChatHumanMessageDeliversCardChimeAndSpeech(t *testing.T) {
	b, base, stub := dcTestHarness(t)
	var spoken []string
	var spokenMu sync.Mutex
	b.worker = &synthWorker{requests: make(chan ttsRequest)}
	b.worker.ready.Store(true)
	go func() {
		for {
			select {
			case <-b.root.Done():
				return
			case req := <-b.worker.requests:
				spokenMu.Lock()
				spoken = append(spoken, req.voice+":"+req.text)
				spokenMu.Unlock()
				req.done <- ttsResult{pcm: []byte{1, 2, 3, 4}}
			}
		}
	}()

	greg, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "a cat"})
	if err != nil {
		t.Fatal(err)
	}
	dev1 := testConnectWS(t, base, "unit", false)
	dev1.hello(t, "box3-0001", "greg")
	dev2 := testConnectWS(t, base, "unit", false)
	dev2.hello(t, "vocat-0002", "greg")
	patchControl(t, base, "greg", object{"sleep": true})

	stub.mu.Lock()
	stub.contacts[greg.DCAccountID][10] = deltachat.Contact{Id: 10, DisplayName: "Mum", Address: "mum@example.org", IsVerified: true}
	stub.chats[greg.DCAccountID][200] = deltachat.BasicChat{Id: 200, ChatType: deltachat.ChatTypeSingle}
	stub.msgs[greg.DCAccountID][1] = deltachat.Message{Id: 1, ChatId: 200, FromId: 10, Text: "Dinner's ready", ViewType: deltachat.ViewtypeText}
	stub.mu.Unlock()

	b.dc.handleMessage(t.Context(), greg.DCAccountID, 1)

	for _, dev := range []*testWS{dev1, dev2} {
		dev.until(t, func(f wsFrame) bool {
			p := framePayload(f)
			return hasProto(msgChime, "")(f) && len(p) >= 1 && p[0] == chimeIntercom
		})
		end := dev.until(t, func(f wsFrame) bool {
			p := framePayload(f)
			return hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == ttsOpEnd
		})
		audioID := binary.LittleEndian.Uint64(framePayload(end)[1:9])
		dev.sendProto(t, msgTTSAck, encodeTTSAck(false, audioID))
	}
	waitSVG(t, b.ws, "box3-0001", "Hello Alice!")
	waitSVG(t, b.ws, "vocat-0002", "Hello Alice!")

	spokenMu.Lock()
	gotSpoken := slices.Clone(spoken)
	spokenMu.Unlock()
	v := b.voiceFor("greg")
	wantFirst := v + ":Message from Mum: Dinner's ready"
	wantReply := v + ":I told Mum: Hello Alice!"
	if !slices.Contains(gotSpoken, wantFirst) || !slices.Contains(gotSpoken, wantReply) {
		t.Fatalf("spoken = %v, want %q and %q", gotSpoken, wantFirst, wantReply)
	}
	stub.mu.Lock()
	if len(stub.sent) != 1 || stub.sent[0].chatID != 200 || *stub.sent[0].data.Text != "Hello Alice!" {
		t.Fatalf("unexpected DC reply: %+v", stub.sent)
	}
	stub.mu.Unlock()
}

func TestDeltaChatSilentReplyProducesNeitherChatNorSpeech(t *testing.T) {
	b, base, _ := listenHarness(t, func(string) string { return "[silent]" })
	dcStub := newStubDCRPC()
	b.dc = newDCService(b, "", t.TempDir(), "nine.testrun.org")
	b.dc.rpc = dcStub
	b.dc.ready.Store(true)

	greg, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "a cat"})
	if err != nil {
		t.Fatal(err)
	}
	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, "box3-silent", "greg")

	var spoken []string
	var spokenMu sync.Mutex
	b.worker = &synthWorker{requests: make(chan ttsRequest)}
	b.worker.ready.Store(true)
	go func() {
		for {
			select {
			case <-b.root.Done():
				return
			case req := <-b.worker.requests:
				spokenMu.Lock()
				spoken = append(spoken, req.text)
				spokenMu.Unlock()
				req.done <- ttsResult{pcm: []byte{1, 2, 3, 4}}
			}
		}
	}()

	dcStub.mu.Lock()
	dcStub.contacts[greg.DCAccountID][10] = deltachat.Contact{Id: 10, DisplayName: "Mum", Address: "mum@example.org", IsVerified: true}
	dcStub.chats[greg.DCAccountID][200] = deltachat.BasicChat{Id: 200, ChatType: deltachat.ChatTypeSingle}
	dcStub.msgs[greg.DCAccountID][1] = deltachat.Message{Id: 1, ChatId: 200, FromId: 10, Text: "Okay", ViewType: deltachat.ViewtypeText}
	dcStub.mu.Unlock()

	b.dc.handleMessage(t.Context(), greg.DCAccountID, 1)
	waitSVG(t, b.ws, "box3-silent", "Okay")

	spokenMu.Lock()
	gotSpoken := slices.Clone(spoken)
	spokenMu.Unlock()
	if len(gotSpoken) != 1 || gotSpoken[0] != "Message from Mum: Okay" {
		t.Fatalf("silent reply should only speak incoming message, got %v", gotSpoken)
	}
	dcStub.mu.Lock()
	sentCount := len(dcStub.sent)
	dcStub.mu.Unlock()
	if sentCount != 0 {
		t.Fatalf("silent reply should not send DC message, got %d", sentCount)
	}
}

func TestDeltaChatMessageQueuedDuringVoiceTurn(t *testing.T) {
	b, base, stub := dcTestHarness(t)
	ttsCh := make(chan string, 4)
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
	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, "box3-0003", "greg")

	b.ws.mu.Lock()
	term := b.ws.terminals["box3-0003"]
	turnDone := make(chan struct{})
	b.ws.startTurn(term, time.Now(), false, "voice", func(_ context.Context, id uint64) {
		defer close(turnDone)
		stub.mu.Lock()
		stub.contacts[greg.DCAccountID][10] = deltachat.Contact{Id: 10, DisplayName: "Mum", Address: "mum@example.org", IsVerified: true}
		stub.chats[greg.DCAccountID][200] = deltachat.BasicChat{Id: 200, ChatType: deltachat.ChatTypeSingle}
		stub.msgs[greg.DCAccountID][2] = deltachat.Message{Id: 2, ChatId: 200, FromId: 10, Text: "Bedtime soon", ViewType: deltachat.ViewtypeText}
		stub.mu.Unlock()

		b.dc.handleMessage(t.Context(), greg.DCAccountID, 2)
		select {
		case got := <-ttsCh:
			t.Fatalf("TTS started during active voice turn: %q", got)
		default:
		}
		b.ws.finishTurn(term, id, nil, nil)
	})
	b.ws.mu.Unlock()
	<-turnDone

	select {
	case got := <-ttsCh:
		if got != "Message from Mum: Bedtime soon" {
			t.Fatalf("queued TTS = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued DC message did not speak after voice turn ended")
	}
	waitSVG(t, b.ws, "box3-0003", "Bedtime soon")
}

func TestEmitSaySpeaksOnTerminal(t *testing.T) {
	b, base, _ := dcTestHarness(t)
	ttsCh := make(chan string, 4)
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

	if _, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, "box3-0004", "greg")

	out := toolTest(t, base, "emit", object{"to": "greg", "say": "Dinner is ready!"}, false)
	if out["said"] != true {
		t.Fatalf("emit say = %+v", out)
	}
	select {
	case got := <-ttsCh:
		if got != "Dinner is ready!" {
			t.Fatalf("emit say TTS = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("emit say did not trigger TTS")
	}
}

func TestHouseholdVoiceLoopTellMumOK(t *testing.T) {
	b, base, stub := dcTestHarness(t)
	b.worker = &synthWorker{requests: make(chan ttsRequest)}
	b.worker.ready.Store(true)
	go func() {
		for {
			select {
			case <-b.root.Done():
				return
			case req := <-b.worker.requests:
				req.done <- ttsResult{pcm: []byte{1, 2, 3, 4}}
			}
		}
	}()

	greg, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "a cat"})
	if err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	stub.contacts[greg.DCAccountID][10] = deltachat.Contact{Id: 10, DisplayName: "Mum", Address: "mum@example.org", IsVerified: true}
	stub.mu.Unlock()

	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, "box3-0005", "greg")

	b.ws.mu.Lock()
	term := b.ws.terminals["box3-0005"]
	done := make(chan struct{})
	b.ws.startTurn(term, time.Now(), false, "voice", func(ctx context.Context, id uint64) {
		defer close(done)
		b.ws.runTurn(ctx, term, id, "tell Mum OK", "voice", nil)
	})
	b.ws.mu.Unlock()
	<-done

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.sent) != 1 || stub.sent[0].accID != greg.DCAccountID || *stub.sent[0].data.Text != "OK" {
		t.Fatalf("tell Mum OK did not send DC message: %+v", stub.sent)
	}
	waitSVG(t, b.ws, "box3-0005", "Sent to Mum!")
}

func TestStateExposesContacts(t *testing.T) {
	b, base, stub := dcTestHarness(t)
	greg, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "a cat"})
	if err != nil {
		t.Fatal(err)
	}
	cake, err := b.createCompanion(t.Context(), object{"id": "cake", "name": "Cake", "form": "a cat"})
	if err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	stub.contacts[greg.DCAccountID][10] = deltachat.Contact{Id: 10, DisplayName: "Mum", Address: "mum@example.org", IsVerified: true}
	stub.contacts[greg.DCAccountID][11] = deltachat.Contact{Id: 11, DisplayName: "Unverified Bob", Address: "bob@example.org", IsVerified: false}
	stub.contacts[greg.DCAccountID][12] = deltachat.Contact{Id: 12, DisplayName: "Cake", Address: stub.configs[cake.DCAccountID]["addr"], IsVerified: true}
	stub.mu.Unlock()

	me := stateMe(t, base, "greg")
	contacts, ok := me["contacts"].([]any)
	if !ok || len(contacts) != 3 {
		t.Fatalf("me.contacts = %#v", me["contacts"])
	}
	byName := map[string]object{}
	for _, raw := range contacts {
		c := raw.(object)
		byName[str(c["name"])] = c
	}
	if byName["Mum"]["kind"] != "human" || byName["Mum"]["verified"] != true {
		t.Fatalf("Mum contact = %+v", byName["Mum"])
	}
	if byName["Unverified Bob"]["kind"] != "human" || byName["Unverified Bob"]["verified"] != false {
		t.Fatalf("Bob contact = %+v", byName["Unverified Bob"])
	}
	if byName["Cake"]["kind"] != "companion" || byName["Cake"]["verified"] != true {
		t.Fatalf("Cake contact = %+v", byName["Cake"])
	}
}

func TestOverlappingRemoteAndLocalTurnsRouteReplies(t *testing.T) {
	stub := newStubDCRPC()
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	var prompts []string
	var msgID int
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.oc = testOpenCodeOnDB(t, b.root, dir, b.db, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/api/session":
				_, _ = w.Write([]byte(`{"data":{"id":"ses-overlap"}}`))
			case r.Method == http.MethodGet && r.URL.Path == "/api/session/ses-overlap":
				_, _ = w.Write([]byte(`{"data":{"id":"ses-overlap","agent":"lark","model":{"providerID":"test","id":"model"}}}`))
			case strings.HasPrefix(r.URL.Path, "/api/session/ses-overlap/"):
				if r.URL.Path == "/api/session/ses-overlap/agent" || r.URL.Path == "/api/session/ses-overlap/model" {
					w.WriteHeader(204)
					return
				}
				switch r.URL.Path {
				case "/api/session/ses-overlap/prompt":
					var in struct{ Text string }
					_ = json.NewDecoder(r.Body).Decode(&in)
					mu.Lock()
					prompts = append(prompts, in.Text)
					n := len(prompts)
					msgID++
					id := msgID
					mu.Unlock()
					if n == 1 {
						close(firstStarted)
						<-releaseFirst
					}
					_, _ = w.Write([]byte(`{"data":{"id":"msg-` + string(rune('0'+id)) + `"}}`))
				case "/api/session/ses-overlap/message":
					mu.Lock()
					n := len(prompts)
					id := msgID
					last := ""
					if n > 0 {
						last = prompts[n-1]
					}
					mu.Unlock()
					reply := "Local reply"
					if strings.Contains(last, "Mum via Delta Chat") {
						reply = "Remote reply to Mum"
					}
					uid := "msg-" + string(rune('0'+id))
					aid := "msg-a" + string(rune('0'+id))
					_, _ = w.Write([]byte(`{"data":[{"id":"` + aid + `","type":"assistant","finish":"stop","content":[{"type":"text","text":"` + reply + `"}]},{"id":"` + uid + `","type":"user"}]}`))
				default:
					w.WriteHeader(404)
				}
			case r.Method == http.MethodPatch && r.URL.Path == "/api/session/ses-overlap":
				w.WriteHeader(204)
			case r.Method == http.MethodPost && r.URL.Path == "/api/experimental/session/ses-overlap/wait":
				w.WriteHeader(204)
			default:
				w.WriteHeader(404)
			}
		})
		b.dc = newDCService(b, "", t.TempDir(), "nine.testrun.org")
		b.dc.rpc = stub
		b.dc.ready.Store(true)
	})

	b.worker = &synthWorker{requests: make(chan ttsRequest)}
	b.worker.ready.Store(true)
	localTTS := make(chan string, 8)
	go func() {
		for {
			select {
			case <-b.root.Done():
				return
			case req := <-b.worker.requests:
				localTTS <- req.text
				req.done <- ttsResult{pcm: []byte{1, 2, 3, 4}}
			}
		}
	}()

	greg, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "a cat"})
	if err != nil {
		t.Fatal(err)
	}
	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, "box3-0006", "greg")
	b.ws.mu.Lock()
	term := b.ws.terminals["box3-0006"]
	b.ws.mu.Unlock()

	stub.mu.Lock()
	stub.contacts[greg.DCAccountID][10] = deltachat.Contact{Id: 10, DisplayName: "Mum", Address: "mum@example.org", IsVerified: true}
	stub.chats[greg.DCAccountID][200] = deltachat.BasicChat{Id: 200, ChatType: deltachat.ChatTypeSingle}
	stub.msgs[greg.DCAccountID][1] = deltachat.Message{Id: 1, ChatId: 200, FromId: 10, Text: "Remote question", ViewType: deltachat.ViewtypeText}
	stub.mu.Unlock()

	dcDone := make(chan struct{})
	go func() {
		defer close(dcDone)
		b.dc.handleMessage(t.Context(), greg.DCAccountID, 1)
	}()
	<-firstStarted

	b.ws.mu.Lock()
	localDone := make(chan struct{})
	b.ws.startTurn(term, time.Now(), false, "voice", func(ctx context.Context, id uint64) {
		defer close(localDone)
		b.ws.runTurn(ctx, term, id, "Local question", "voice", nil)
	})
	b.ws.mu.Unlock()

	// Release the serialized DC turn; local turn must run second and get its own reply.
	close(releaseFirst)
	<-dcDone
	<-localDone

	stub.mu.Lock()
	if len(stub.sent) != 1 || *stub.sent[0].data.Text != "Remote reply to Mum" {
		t.Fatalf("DC reply mixed up: %+v", stub.sent)
	}
	stub.mu.Unlock()

	var gotLocal bool
	deadline := time.After(3 * time.Second)
	for !gotLocal {
		select {
		case s := <-localTTS:
			if s == "Remote reply to Mum" {
				t.Fatalf("remote reply leaked to local TTS")
			}
			if s == "Local reply" {
				gotLocal = true
			}
		case <-deadline:
			t.Fatal("local reply did not play on terminal")
		}
	}
}

func TestRecentDialogueIncludesChannelAndSender(t *testing.T) {
	b, _, _ := dcTestHarness(t)
	if _, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	baseTS := time.Now().Add(-time.Minute)
	addAt := func(offset time.Duration, role, sender, text, source string) {
		e := newChat(role, sender, text, source)
		e.Timestamp = baseTS.Add(offset)
		b.history.add("greg", e)
	}
	addAt(1*time.Second, "user", "Mum", "Dinner at 6", "deltachat")
	addAt(2*time.Second, "companion", "Greg", "Got it!", "deltachat")
	addAt(3*time.Second, "user", "You", "Who texted?", "voice")
	addAt(4*time.Second, "companion", "Greg", "Mum did.", "voice")

	b.history.flush("greg")
	excerpt, err := b.oc.recentDialogue(t.Context(), "greg", baseTS.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(excerpt, "[You are Greg. Speaker: Mum via Delta Chat (contact)") || !strings.Contains(excerpt, "[You are Greg. Speaker: someone in person at your device (name unknown)") {
		t.Fatalf("excerpt missing channel/sender format: %s", excerpt)
	}
}

func TestDeltaChatSlashCommands(t *testing.T) {
	var prompts []string
	var promptsMu sync.Mutex
	b, base, _ := listenHarness(t, func(prompt string) string {
		promptsMu.Lock()
		prompts = append(prompts, prompt)
		promptsMu.Unlock()
		return "Just between us."
	})
	stub := newStubDCRPC()
	b.dc = newDCService(b, "", t.TempDir(), "nine.testrun.org")
	b.dc.rpc = stub
	b.dc.ready.Store(true)

	greg, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "an old chameleon"})
	if err != nil {
		t.Fatal(err)
	}
	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, "box3-cmd", "greg")
	patchControl(t, base, "greg", object{"sleep": true})

	var spoken []string
	var spokenMu sync.Mutex
	b.worker = &synthWorker{requests: make(chan ttsRequest)}
	b.worker.ready.Store(true)
	go func() {
		for {
			select {
			case <-b.root.Done():
				return
			case req := <-b.worker.requests:
				spokenMu.Lock()
				spoken = append(spoken, req.voice+":"+req.text)
				spokenMu.Unlock()
				req.done <- ttsResult{pcm: []byte{1, 2, 3, 4}}
			}
		}
	}()

	stub.mu.Lock()
	stub.contacts[greg.DCAccountID][10] = deltachat.Contact{Id: 10, DisplayName: "Mum", Address: "mum@example.org", IsVerified: true}
	stub.chats[greg.DCAccountID][200] = deltachat.BasicChat{Id: 200, ChatType: deltachat.ChatTypeSingle}
	stub.msgs[greg.DCAccountID][1] = deltachat.Message{Id: 1, ChatId: 200, FromId: 10, Text: "/say Bedtime!", ViewType: deltachat.ViewtypeText}
	stub.msgs[greg.DCAccountID][2] = deltachat.Message{Id: 2, ChatId: 200, FromId: 10, Text: "/private Secret plan", ViewType: deltachat.ViewtypeText}
	stub.msgs[greg.DCAccountID][3] = deltachat.Message{Id: 3, ChatId: 200, FromId: 10, Text: "/help", ViewType: deltachat.ViewtypeText}
	stub.msgs[greg.DCAccountID][4] = deltachat.Message{Id: 4, ChatId: 200, FromId: 10, Text: "/bogus", ViewType: deltachat.ViewtypeText}
	stub.mu.Unlock()

	// 1. /say: spoken word for word, no agent turn, "Said it." reply, recorded in messages.
	b.dc.handleMessage(t.Context(), greg.DCAccountID, 1)
	end := dev.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == ttsOpEnd
	})
	dev.sendProto(t, msgTTSAck, encodeTTSAck(false, binary.LittleEndian.Uint64(framePayload(end)[1:9])))
	waitSVG(t, b.ws, "box3-cmd", "Bedtime!")

	// 2. /private: agent turn with private label, reply in chat, nothing spoken.
	b.dc.handleMessage(t.Context(), greg.DCAccountID, 2)

	// 3. /help: sends welcome instructions, no agent turn, nothing spoken.
	b.dc.handleMessage(t.Context(), greg.DCAccountID, 3)

	// 4. /bogus: sends short help hint, no agent turn, nothing spoken.
	b.dc.handleMessage(t.Context(), greg.DCAccountID, 4)

	spokenMu.Lock()
	gotSpoken := slices.Clone(spoken)
	spokenMu.Unlock()
	wantSay := b.voiceFor("greg") + ":Bedtime!"
	if len(gotSpoken) != 1 || gotSpoken[0] != wantSay {
		t.Fatalf("spoken = %v, want [%q]", gotSpoken, wantSay)
	}

	promptsMu.Lock()
	gotPrompts := slices.Clone(prompts)
	promptsMu.Unlock()
	if len(gotPrompts) != 1 || !strings.Contains(gotPrompts[0], "[You are Greg (greg). Speaker: Mum via Delta Chat (contact) · private") || !strings.Contains(gotPrompts[0], "\nSecret plan\n[state: ") {
		t.Fatalf("unexpected agent prompts: %v", gotPrompts)
	}

	stub.mu.Lock()
	sent := slices.Clone(stub.sent)
	stub.mu.Unlock()
	if len(sent) != 4 {
		t.Fatalf("expected 4 chat replies, got %d: %+v", len(sent), sent)
	}
	if *sent[0].data.Text != "Said it." {
		t.Fatalf("/say reply = %q, want Said it.", *sent[0].data.Text)
	}
	if *sent[1].data.Text != "Just between us." {
		t.Fatalf("/private reply = %q", *sent[1].data.Text)
	}
	if !strings.Contains(*sent[2].data.Text, "Hi! I'm Greg, an old chameleon.") || !strings.Contains(*sent[2].data.Text, "/panel") {
		t.Fatalf("/help reply = %q", *sent[2].data.Text)
	}
	if !strings.Contains(*sent[3].data.Text, "Unknown command") {
		t.Fatalf("unknown command reply = %q", *sent[3].data.Text)
	}

	entries, err := b.history.list(t.Context(), "greg")
	if err != nil {
		t.Fatal(err)
	}
	var foundSay bool
	for _, e := range entries {
		if e.Source == "deltachat" && e.Text == "/say Bedtime!" {
			foundSay = true
		}
	}
	if !foundSay {
		t.Fatalf("/say not recorded in messages with deltachat channel: %+v", entries)
	}
}

func TestDeltaChatWelcomeOnVerifyAndStartup(t *testing.T) {
	b, _, stub := dcTestHarness(t)
	greg, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "an old chameleon"})
	if err != nil {
		t.Fatal(err)
	}
	cake, err := b.createCompanion(t.Context(), object{"id": "cake", "name": "Cake", "form": "a cat"})
	if err != nil {
		t.Fatal(err)
	}

	stub.mu.Lock()
	// Existing verified human contact.
	stub.contacts[greg.DCAccountID][10] = deltachat.Contact{Id: 10, DisplayName: "Mum", Address: "mum@example.org", IsVerified: true}
	// Unverified human contact.
	stub.contacts[greg.DCAccountID][11] = deltachat.Contact{Id: 11, DisplayName: "Stranger", Address: "stranger@example.org", IsVerified: false}
	// Companion-to-companion contact (should never receive welcome).
	stub.contacts[greg.DCAccountID][12] = deltachat.Contact{Id: 12, DisplayName: "Cake", Address: stub.configs[cake.DCAccountID]["addr"], IsVerified: true, IsBot: true}
	stub.mu.Unlock()

	// Startup sync sends welcome + panel once to Mum, skips Stranger and Cake.
	b.dc.syncWelcomes(t.Context())
	b.dc.syncWelcomes(t.Context())

	stub.mu.Lock()
	if len(stub.sent) != 2 {
		t.Fatalf("expected 2 messages (welcome + panel) on startup sync, got %d: %+v", len(stub.sent), stub.sent)
	}
	if !strings.Contains(*stub.sent[0].data.Text, "Hi! I'm Greg, an old chameleon.") {
		t.Fatalf("unexpected welcome text: %q", *stub.sent[0].data.Text)
	}
	if stub.sent[1].data.Viewtype == nil || *stub.sent[1].data.Viewtype != deltachat.ViewtypeWebxdc {
		t.Fatalf("expected panel webxdc message, got %+v", stub.sent[1].data)
	}
	// New human completes SecureJoin.
	stub.contacts[greg.DCAccountID][13] = deltachat.Contact{Id: 13, DisplayName: "Dad", Address: "dad@example.org", IsVerified: true}
	stub.mu.Unlock()

	b.dc.handleSecureJoinVerified(t.Context(), greg.DCAccountID, 13)
	b.dc.handleSecureJoinVerified(t.Context(), greg.DCAccountID, 13)

	stub.mu.Lock()
	if len(stub.sent) != 4 {
		t.Fatalf("expected 4 total messages (2 welcomes + 2 panels) after SecureJoin, got %d: %+v", len(stub.sent), stub.sent)
	}
	stub.mu.Unlock()
}

func TestWebxdcPanelDeliveryAndUpdates(t *testing.T) {
	xdcBytes, err := buildPanelXDC()
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(xdcBytes), int64(len(xdcBytes)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]bool{}
	for _, f := range zr.File {
		files[f.Name] = true
	}
	for _, want := range []string{"index.html", "manifest.toml", "icon.png", "oat.min.css", "oat.min.js", "codec.js", "app.js"} {
		if !files[want] {
			t.Fatalf("panel xdc missing %s (got %v)", want, files)
		}
	}
	if files["codec.test.mjs"] {
		t.Fatal("panel xdc should not include test files")
	}

	b, base, stub := dcTestHarness(t)
	greg, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "an old chameleon"})
	if err != nil {
		t.Fatal(err)
	}
	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, "box3-xdc", "greg")
	if _, _, err := b.saveStory(t.Context(), "lost_island", ":: StoryTitle\nLost Island\n\n:: Start\nPage one.\n\n[[Next->Page2]]\n\n:: Page2 {\"end\":true}\nPage two.\n", "", true); err != nil {
		t.Fatal(err)
	}

	stub.mu.Lock()
	stub.contacts[greg.DCAccountID][10] = deltachat.Contact{Id: 10, DisplayName: "Mum", Address: "mum@example.org", IsVerified: true}
	stub.chats[greg.DCAccountID][200] = deltachat.BasicChat{Id: 200, ChatType: deltachat.ChatTypeSingle}
	stub.chatContacts[greg.DCAccountID][200] = []uint32{deltachat.ContactSelf, 10}
	stub.msgs[greg.DCAccountID][1] = deltachat.Message{Id: 1, ChatId: 200, FromId: 10, Text: "/panel", ViewType: deltachat.ViewtypeText}

	// Unverified stranger chat.
	stub.contacts[greg.DCAccountID][11] = deltachat.Contact{Id: 11, DisplayName: "Stranger", Address: "stranger@example.org", IsVerified: false}
	stub.chats[greg.DCAccountID][201] = deltachat.BasicChat{Id: 201, ChatType: deltachat.ChatTypeSingle}
	stub.chatContacts[greg.DCAccountID][201] = []uint32{deltachat.ContactSelf, 11}
	stub.msgs[greg.DCAccountID][99] = deltachat.Message{Id: 99, ChatId: 201, FromId: 11, ViewType: deltachat.ViewtypeWebxdc}
	stub.mu.Unlock()

	// 1. /panel sends .xdc and initial state update (with portrait).
	b.dc.handleMessage(t.Context(), greg.DCAccountID, 1)
	stub.mu.Lock()
	if len(stub.sent) != 1 || stub.sent[0].data.Viewtype == nil || *stub.sent[0].data.Viewtype != deltachat.ViewtypeWebxdc {
		t.Fatalf("expected webxdc message from /panel, got %+v", stub.sent)
	}
	// Map sent msgID 1 in stub.msgs so handleWebxdcUpdate can resolve its chat.
	panelMsgID := uint32(1)
	stub.msgs[greg.DCAccountID][panelMsgID] = deltachat.Message{Id: panelMsgID, ChatId: 200, FromId: deltachat.ContactSelf, ViewType: deltachat.ViewtypeWebxdc}
	initialUpdates := len(stub.xdcUpdates[greg.DCAccountID][panelMsgID])
	stub.mu.Unlock()
	if initialUpdates != 1 || !strings.Contains(string(stub.xdcUpdates[greg.DCAccountID][panelMsgID][0]), "data:image/png;base64,") {
		t.Fatalf("expected initial state update with portrait, got %d updates", initialUpdates)
	}

	// 2. Drive verified client updates: set_listen, set_voice, set_volume, open_book.
	sendClientUpdate := func(msgID uint32, payload string) {
		_ = stub.SendWebxdcStatusUpdate(greg.DCAccountID, msgID, payload, nil)
		b.dc.handleWebxdcUpdate(t.Context(), greg.DCAccountID, msgID)
	}
	sendClientUpdate(panelMsgID, `{"payload":{"v":1,"op":"set_listen","seconds":120}}`)
	sendClientUpdate(panelMsgID, `{"payload":{"v":1,"op":"set_voice","voice":"bf_lily"}}`)
	sendClientUpdate(panelMsgID, `{"payload":{"v":1,"op":"set_volume","terminal":"box3-xdc","volume":45}}`)
	sendClientUpdate(panelMsgID, `{"payload":{"v":1,"op":"open_story","app":"lost_island"}}`)

	if got := b.companionFor("greg").ListenSeconds; got != 120 {
		t.Fatalf("listen_seconds = %d, want 120", got)
	}
	if got := b.voiceFor("greg"); got != "bf_lily" {
		t.Fatalf("voice = %q, want bf_lily", got)
	}
	b.ws.mu.Lock()
	d := b.ws.terminals["box3-xdc"]
	vol := d.vol
	drawer := d.drawer
	storyID := ""
	if d.story != nil {
		storyID = d.story.id
	}
	b.ws.mu.Unlock()
	if vol != 45 {
		t.Fatalf("terminal volume = %d, want 45", vol)
	}
	if drawer != drawerStoryReader || storyID != "lost_island" {
		t.Fatalf("expected story reader with lost_island, got drawer=%q story=%q", drawer, storyID)
	}

	// 3. Unverified sender update is ignored.
	sendClientUpdate(99, `{"payload":{"v":1,"op":"set_listen","seconds":300}}`)
	if got := b.companionFor("greg").ListenSeconds; got != 120 {
		t.Fatalf("unverified update mutated listen_seconds to %d", got)
	}
	stub.mu.Lock()
	unverifiedReplies := len(stub.xdcUpdates[greg.DCAccountID][99])
	stub.mu.Unlock()
	if unverifiedReplies != 1 { // only the injected client update itself, no ack/state reply
		t.Fatalf("expected no reply updates for unverified sender, got %d", unverifiedReplies)
	}
}

func TestDeltaChatStoryImportAndExport(t *testing.T) {
	b, _, stub := dcTestHarness(t)
	greg, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "an old chameleon"})
	if err != nil {
		t.Fatal(err)
	}
	dc := b.dc
	stub.mu.Lock()
	stub.contacts[greg.DCAccountID][10] = deltachat.Contact{Id: 10, DisplayName: "Mum", Address: "mum@example.org", IsVerified: true}
	stub.chats[greg.DCAccountID][200] = deltachat.BasicChat{Id: 200, ChatType: deltachat.ChatTypeSingle}
	stub.chatContacts[greg.DCAccountID][200] = []uint32{deltachat.ContactSelf, 10}
	stub.mu.Unlock()

	tweePath := filepath.Join(t.TempDir(), "woods.twee")
	if err := os.WriteFile(tweePath, []byte(sampleBranchingTwee), 0o600); err != nil {
		t.Fatal(err)
	}

	// Import .twee attachment.
	stub.mu.Lock()
	stub.msgs[greg.DCAccountID][1] = deltachat.Message{Id: 1, ChatId: 200, FromId: 10, ViewType: deltachat.ViewtypeFile, File: &tweePath}
	stub.mu.Unlock()
	dc.handleMessage(t.Context(), greg.DCAccountID, 1)

	// Second import of same title gets -2 suffix.
	stub.mu.Lock()
	stub.msgs[greg.DCAccountID][2] = deltachat.Message{Id: 2, ChatId: 200, FromId: 10, ViewType: deltachat.ViewtypeFile, File: &tweePath}
	stub.mu.Unlock()
	dc.handleMessage(t.Context(), greg.DCAccountID, 2)

	stories, err := b.listStories(t.Context())
	if err != nil || len(stories) != 2 {
		t.Fatalf("expected 2 imported stories, got %d (%v)", len(stories), err)
	}

	// /stories lists them; /story sends .twee + .xdc.
	stub.mu.Lock()
	stub.msgs[greg.DCAccountID][3] = deltachat.Message{Id: 3, ChatId: 200, FromId: 10, Text: "/stories", ViewType: deltachat.ViewtypeText}
	stub.msgs[greg.DCAccountID][4] = deltachat.Message{Id: 4, ChatId: 200, FromId: 10, Text: "/story the-whispering-woods", ViewType: deltachat.ViewtypeText}
	stub.mu.Unlock()
	dc.handleMessage(t.Context(), greg.DCAccountID, 3)
	dc.handleMessage(t.Context(), greg.DCAccountID, 4)

	stub.mu.Lock()
	sent := slices.Clone(stub.sent)
	stub.mu.Unlock()
	if len(sent) < 5 {
		t.Fatalf("expected import replies, /stories list, and .twee + .xdc sends, got %d messages", len(sent))
	}
	var sawList, sawTwee, sawXDC bool
	for _, m := range sent {
		if m.data.Text != nil && strings.Contains(*m.data.Text, "The Whispering Woods") {
			sawList = true
		}
		if m.data.Viewtype != nil && *m.data.Viewtype == deltachat.ViewtypeFile {
			sawTwee = true
		}
		if m.data.Viewtype != nil && *m.data.Viewtype == deltachat.ViewtypeWebxdc {
			sawXDC = true
		}
	}
	if !sawList || !sawTwee || !sawXDC {
		t.Fatalf("missing expected sends: list=%v twee=%v xdc=%v", sawList, sawTwee, sawXDC)
	}
}

func TestDCIntercomMatchesRenamedPeer(t *testing.T) {
	b, _, stub := dcTestHarness(t)
	qa1, err := b.createCompanion(t.Context(), object{"id": "qa1", "name": "qa1", "form": "cat"})
	if err != nil {
		t.Fatal(err)
	}
	qa2, err := b.createCompanion(t.Context(), object{"id": "qa2", "name": "Milo", "form": "owl"})
	if err != nil {
		t.Fatal(err)
	}
	dc := b.dc
	stub.mu.Lock()
	stub.contacts[qa1.DCAccountID][10] = deltachat.Contact{Id: 10, Address: stub.configs[qa2.DCAccountID]["addr"], DisplayName: "qa2", Name: "qa2", IsVerified: true}
	stub.mu.Unlock()

	sentTo, err := dc.sendIntercom(t.Context(), "qa1", "Milo", "Hello Milo!", nil)
	if err != nil {
		t.Fatalf("sendIntercom failed for renamed peer: %v", err)
	}
	if len(sentTo) != 1 || sentTo[0] != "qa2" {
		t.Fatalf("sentTo = %v, want [qa2]", sentTo)
	}
}
