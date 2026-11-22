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
	cake, err := b.createCompanion(t.Context(), object{"name": "Cake"})
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
	cake, err := b.createCompanion(t.Context(), object{"name": "Cake"})
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
	cake, err := b.createCompanion(t.Context(), object{"name": "Cake"})
	if err != nil {
		t.Fatal(err)
	}
	mint, err := b.createCompanion(t.Context(), object{"name": "Mint"})
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
	cake, err := b.createCompanion(t.Context(), object{"name": "Cake"})
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
