package main

// In-memory Delta Chat RPC used by the offline tests and by the e2e stub
// server. The product binary contains the type but the linker drops it unless
// a test or the e2e build reaches it.

import (
	"slices"
	"strings"
	"sync"

	"github.com/chatmail/rpc-client-go/v2/deltachat"
)

type stubDCSent struct {
	accID  uint32
	chatID uint32
	data   deltachat.MessageData
}

type stubDCRPC struct {
	mu            sync.Mutex
	nextAcc       uint32
	nextChat      uint32
	configs       map[uint32]map[string]string
	contacts      map[uint32]map[uint32]deltachat.Contact
	chats         map[uint32]map[uint32]deltachat.BasicChat
	chatContacts  map[uint32]map[uint32][]uint32
	msgs          map[uint32]map[uint32]deltachat.Message
	sent          []stubDCSent
	qrJoined      []string
	deleted       []uint32
	deletedForAll []uint32
	removed       []uint32
	sig           condSignal
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

func (s *stubDCRPC) GetAllAccountIds() ([]uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]uint32, 0, len(s.configs))
	for id := range s.configs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids, nil
}

func (s *stubDCRPC) RemoveAccount(accID uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.configs, accID)
	delete(s.contacts, accID)
	delete(s.chats, accID)
	delete(s.chatContacts, accID)
	delete(s.msgs, accID)
	s.removed = append(s.removed, accID)
	return nil
}

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

func (s *stubDCRPC) GetChatMedia(accID uint32, chatID *uint32, msgType deltachat.Viewtype, _ *deltachat.Viewtype, _ *deltachat.Viewtype) ([]uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []uint32
	for mid, m := range s.msgs[accID] {
		if (chatID == nil || m.ChatId == *chatID) && m.ViewType == msgType {
			ids = append(ids, mid)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

func (s *stubDCRPC) DeleteMessagesForAll(_ uint32, msgIDs []uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletedForAll = append(s.deletedForAll, msgIDs...)
	return nil
}

func (s *stubDCRPC) SendMsg(accID uint32, chatID uint32, data deltachat.MessageData) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, stubDCSent{accID: accID, chatID: chatID, data: data})
	s.sig.notify()
	return uint32(len(s.sent)), nil
}
