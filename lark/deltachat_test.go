package main

import (
	"context"
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
	chats    map[uint32]map[uint32]deltachat.BasicChat
	msgs     map[uint32]map[uint32]deltachat.Message
	sent     []stubDCSent
	qrJoined []string
	deleted  []uint32
}

func newStubDCRPC() *stubDCRPC {
	return &stubDCRPC{
		nextAcc:  10,
		nextChat: 100,
		configs:  make(map[uint32]map[string]string),
		contacts: make(map[uint32]map[uint32]deltachat.Contact),
		chats:    make(map[uint32]map[uint32]deltachat.BasicChat),
		msgs:     make(map[uint32]map[uint32]deltachat.Message),
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

	// Open Menu -> Next -> Pair.
	tapMenu(t, dev, "menu", "menu-next")
	tapMenu(t, dev, "menu-next", "menu-pair", "menu-unpair")
	qrSVG := tapMenu(t, dev, "menu-pair", "Pair")
	if !strings.Contains(qrSVG, "Pair") {
		t.Fatalf("missing Pair title in SVG: %s", qrSVG)
	}
	// Rasterize the Pair QR scene to confirm SVG->RGB565 renders black QR pixels.
	scr, err := buildScreen(qrSVG, false, roundScreen)
	if err != nil {
		t.Fatalf("Pair QR SVG failed to rasterize: %v", err)
	}
	if !slices.Contains(scr.fb, uint16(0x0000)) {
		t.Fatal("Pair QR SVG rendered without any black QR pixels")
	}

	// Back -> Unpair -> tap Alice -> removed.
	tapMenu(t, dev, "menu-back", "menu-unpair")
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
		dev.until(t, func(f wsFrame) bool {
			p := framePayload(f)
			return hasProto(msgTTSCtrl, "")(f) && len(p) >= 1 && p[0] == ttsOpStart
		})
	}
	waitSVG(t, b.ws, "box3-0001", "From Mum")
	waitSVG(t, b.ws, "vocat-0002", "From Mum")

	spokenMu.Lock()
	gotSpoken := slices.Clone(spoken)
	spokenMu.Unlock()
	wantSpeech := b.voiceFor("greg") + ":Message from Mum: Dinner's ready"
	if len(gotSpoken) != 1 || gotSpoken[0] != wantSpeech {
		t.Fatalf("spoken = %v, want [%q] (remote reply must not be spoken aloud)", gotSpoken, wantSpeech)
	}
	stub.mu.Lock()
	if len(stub.sent) != 1 || stub.sent[0].chatID != 200 || *stub.sent[0].data.Text != "Hello Alice!" {
		t.Fatalf("unexpected DC reply: %+v", stub.sent)
	}
	stub.mu.Unlock()
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
					if strings.Contains(last, "Delta Chat from Mum") {
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

	excerpt, err := b.oc.recentDialogue(t.Context(), "greg", baseTS.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(excerpt, "[Delta Chat from Mum (contact)") || !strings.Contains(excerpt, "[In person at Greg's device") {
		t.Fatalf("excerpt missing channel/sender format: %s", excerpt)
	}
}
