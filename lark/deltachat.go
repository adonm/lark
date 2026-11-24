package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chatmail/rpc-client-go/v2/deltachat"
)

const (
	defaultChatmailRelay = "nine.testrun.org"
	dcWelcomeVersion     = "3"
	dcRemovePending      = "dc_remove_pending:"
	// Restart the rpc-server after a crash without hot-looping.
	dcRestartDelay = 2 * time.Second
)

type dcRPC interface {
	AddAccount() (uint32, error)
	GetAllAccountIds() ([]uint32, error)
	AddTransportFromQr(accID uint32, qr string) error
	BatchSetConfig(accID uint32, cfg map[string]*string) error
	SetConfig(accID uint32, key string, val *string) error
	GetConfig(accID uint32, key string) (*string, error)
	StartIoForAllAccounts() error
	RemoveAccount(accID uint32) error
	GetChatSecurejoinQrCode(accID uint32, chatID *uint32) (string, error)
	CreateQrSvg(text string) (string, error)
	SecureJoin(accID uint32, qr string) (uint32, error)
	GetContacts(accID uint32, listFlags uint32, query *string) ([]deltachat.Contact, error)
	GetContact(accID uint32, contactID uint32) (deltachat.Contact, error)
	DeleteContact(accID uint32, contactID uint32) error
	GetChatIdByContactId(accID uint32, contactID uint32) (*uint32, error)
	CreateChatByContactId(accID uint32, contactID uint32) (uint32, error)
	DeleteChat(accID uint32, chatID uint32) error
	GetBasicChatInfo(accID uint32, chatID uint32) (deltachat.BasicChat, error)
	GetChatContacts(accID uint32, chatID uint32) ([]uint32, error)
	GetMessage(accID uint32, msgID uint32) (deltachat.Message, error)
	GetChatMedia(accID uint32, chatID *uint32, msgType deltachat.Viewtype, or2 *deltachat.Viewtype, or3 *deltachat.Viewtype) ([]uint32, error)
	SendMsg(accID uint32, chatID uint32, data deltachat.MessageData) (uint32, error)
	DeleteMessagesForAll(accID uint32, msgIDs []uint32) error
}

type dcContact struct {
	ID      uint32
	Name    string
	Address string
	IsBot   bool
}

type dcService struct {
	b           *larkServer
	relay       string
	bin         string
	accountsDir string
	ready       atomic.Bool

	mu        sync.Mutex
	rpc       dcRPC
	transport *deltachat.IOTransport
	bot       *deltachat.Bot
	addrs     map[string]string // companionID -> email address
	byAcc     map[uint32]string // dc_account_id -> companionID
	stopCh    chan struct{}
	doneCh    chan struct{}
}

func newDCService(b *larkServer, bin, accountsDir, relay string) *dcService {
	if relay == "" {
		relay = defaultChatmailRelay
	}
	return &dcService{
		b:           b,
		relay:       relay,
		bin:         bin,
		accountsDir: accountsDir,
		addrs:       make(map[string]string),
		byAcc:       make(map[uint32]string),
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
	}
}

func findDCRPCBinary(local string) string {
	if p := os.Getenv("LARK_DC_RPC_BIN"); p != "" {
		return p
	}
	pinned := filepath.Join(local, "bin", "deltachat-rpc-server")
	if _, err := os.Stat(pinned); err == nil {
		return pinned
	}
	if p, err := exec.LookPath("deltachat-rpc-server"); err == nil {
		return p
	}
	return ""
}

func (s *dcService) start(ctx context.Context) {
	defer close(s.doneCh)
	if s.bin == "" {
		log.Print("Delta Chat disabled: deltachat-rpc-server not found")
		return
	}
	if err := os.MkdirAll(s.accountsDir, 0o700); err != nil {
		log.Printf("Delta Chat accounts dir error: %v", err)
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		default:
		}
		err := s.runOnce(ctx)
		s.ready.Store(false)
		if ctx.Err() != nil {
			return
		}
		select {
		case <-s.stopCh:
			return
		default:
		}
		if err != nil {
			log.Printf("Delta Chat rpc-server exited: %v; restarting in 2s", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-time.After(dcRestartDelay):
		}
	}
}

func (s *dcService) runOnce(ctx context.Context) error {
	tr := deltachat.NewIOTransport()
	tr.Cmd = s.bin
	tr.AccountsDir = s.accountsDir
	if err := tr.Open(); err != nil {
		return err
	}
	defer tr.Close()

	rpc := &deltachat.Rpc{Context: ctx, Transport: tr}
	bot := deltachat.NewBot(rpc)
	bot.OnNewMsg(func(_ *deltachat.Bot, accID, msgID uint32) {
		s.handleMessage(ctx, accID, msgID)
	})
	bot.On(&deltachat.EventTypeSecurejoinInviterProgress{}, func(_ *deltachat.Bot, accID uint32, ev deltachat.EventType) {
		p, ok := ev.(*deltachat.EventTypeSecurejoinInviterProgress)
		if !ok || p.Progress < 1000 {
			return
		}
		s.handleSecureJoinVerified(ctx, accID, p.ContactId)
	})
	s.mu.Lock()
	s.transport = tr
	s.rpc = rpc
	s.bot = bot
	s.mu.Unlock()

	if err := rpc.StartIoForAllAccounts(); err != nil {
		return err
	}
	s.ready.Store(true)
	log.Printf("Delta Chat rpc-server ready (relay=%s)", s.relay)

	go func() {
		if err := s.syncAccounts(ctx); err != nil && ctx.Err() == nil {
			log.Printf("Delta Chat account sync: %v", err)
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		errCh <- bot.Run()
	}()
	select {
	case <-ctx.Done():
		bot.Stop()
		return <-errCh
	case <-s.stopCh:
		bot.Stop()
		return <-errCh
	case err := <-errCh:
		return err
	}
}

func (s *dcService) shutdown() {
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
	s.mu.Lock()
	bot := s.bot
	tr := s.transport
	s.mu.Unlock()
	if bot != nil {
		bot.Stop()
	}
	if tr != nil {
		tr.Close()
	}
	<-s.doneCh
}

func (s *dcService) getRPC() dcRPC {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rpc
}

func (s *dcService) syncAccounts(ctx context.Context) error {
	s.retryPendingRemovals(ctx)
	comps, err := s.b.allCompanions(ctx)
	if err != nil {
		return err
	}
	for _, c := range comps {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := s.ensureCompanion(ctx, c); err != nil {
			log.Printf("Delta Chat account for %s: %v", c.ID, err)
		}
	}
	s.autoPairAll(ctx)
	s.syncWelcomes(ctx)
	return nil
}

func (s *dcService) ensureCompanion(ctx context.Context, c companionState) (uint32, error) {
	rpc := s.getRPC()
	if rpc == nil {
		return 0, errors.New("Delta Chat unavailable")
	}
	accID := c.DCAccountID
	if accID == 0 {
		var err error
		accID, err = rpc.AddAccount()
		if err != nil {
			return 0, err
		}
		if err := s.provisionAccount(ctx, rpc, c, accID); err != nil {
			// A partly provisioned account has no companion reference, so it
			// would leak forever. Remove it now, or record it for the next
			// rpc-server readiness if removal also fails.
			s.dropAccount(ctx, accID)
			return 0, err
		}
	}
	if err := s.syncProfile(c, accID); err != nil {
		log.Printf("Delta Chat profile for %s: %v", c.ID, err)
	}
	addrPtr, err := rpc.GetConfig(accID, "addr")
	s.mu.Lock()
	s.byAcc[accID] = c.ID
	if err == nil && addrPtr != nil && *addrPtr != "" {
		s.addrs[c.ID] = strings.ToLower(*addrPtr)
	}
	s.mu.Unlock()
	return accID, nil
}

func (s *dcService) provisionAccount(ctx context.Context, rpc dcRPC, c companionState, accID uint32) error {
	botVal := "1"
	nameVal := c.Name
	cfg := map[string]*string{
		"bot":         &botVal,
		"displayname": &nameVal,
	}
	if err := rpc.BatchSetConfig(accID, cfg); err != nil {
		return err
	}
	whoCalls := "nobody"
	if err := rpc.SetConfig(accID, "who_can_call_me", &whoCalls); err != nil {
		return err
	}
	if err := rpc.AddTransportFromQr(accID, "dcaccount:"+s.relay); err != nil {
		return err
	}
	if err := rpc.StartIoForAllAccounts(); err != nil {
		log.Printf("Delta Chat StartIo after provisioning %s: %v", c.ID, err)
	}
	if s.b != nil && s.b.db != nil {
		if _, err := s.b.db.ExecContext(ctx, `UPDATE companions SET dc_account_id=? WHERE id=?`, accID, c.ID); err != nil {
			return err
		}
	}
	return nil
}

// dropAccount removes an account and records a durable retry when the RPC is
// unavailable, so a failed create or delete cannot leak an account silently.
func (s *dcService) dropAccount(ctx context.Context, accID uint32) {
	if accID == 0 {
		return
	}
	if rpc := s.getRPC(); rpc != nil {
		if err := rpc.RemoveAccount(accID); err == nil {
			s.clearPendingRemoval(ctx, accID)
			return
		} else {
			log.Printf("Delta Chat could not remove account %d: %v", accID, err)
		}
	}
	if s.b == nil || s.b.db == nil {
		return
	}
	if err := s.b.db.setMeta(ctx, fmt.Sprintf("%s%d", dcRemovePending, accID), time.Now().UTC().Format(time.RFC3339)); err != nil {
		log.Printf("Delta Chat could not record pending removal of account %d: %v", accID, err)
	}
}

func (s *dcService) clearPendingRemoval(ctx context.Context, accID uint32) {
	if s.b == nil || s.b.db == nil {
		return
	}
	if _, err := s.b.db.ExecContext(ctx, `DELETE FROM meta WHERE key=?`, fmt.Sprintf("%s%d", dcRemovePending, accID)); err != nil {
		log.Printf("Delta Chat could not clear pending removal of account %d: %v", accID, err)
	}
}

// retryPendingRemovals runs whenever the rpc-server becomes ready.
func (s *dcService) retryPendingRemovals(ctx context.Context) {
	rpc := s.getRPC()
	if rpc == nil || s.b == nil || s.b.db == nil {
		return
	}
	rows, err := s.b.db.readDB().QueryContext(ctx, `SELECT key FROM meta WHERE key LIKE ?`, dcRemovePending+"%")
	if err != nil {
		return
	}
	var keys []string
	for rows.Next() {
		var key string
		if rows.Scan(&key) == nil {
			keys = append(keys, key)
		}
	}
	rows.Close()
	for _, key := range keys {
		id, err := strconv.ParseUint(strings.TrimPrefix(key, dcRemovePending), 10, 32)
		if err != nil {
			continue
		}
		if err := rpc.RemoveAccount(uint32(id)); err != nil {
			log.Printf("Delta Chat could not remove account %d yet: %v", id, err)
			continue
		}
		s.clearPendingRemoval(ctx, uint32(id))
	}
}

// reconcileAccounts compares the rpc-server's accounts with the companions
// table; orphaned accounts and queued removals are visible to doctor.
func (s *dcService) reconcileAccounts(ctx context.Context) object {
	out := object{"on_disk": 0, "linked": 0, "orphans": []uint32{}, "pending_removal": 0}
	if s == nil {
		return out
	}
	linked := map[uint32]bool{}
	if s.b != nil {
		if comps, err := s.b.allCompanions(ctx); err == nil {
			for _, c := range comps {
				if c.DCAccountID != 0 {
					linked[c.DCAccountID] = true
				}
			}
		}
	}
	out["linked"] = len(linked)
	if rpc := s.getRPC(); rpc != nil {
		if ids, err := rpc.GetAllAccountIds(); err == nil {
			slices.Sort(ids)
			out["on_disk"] = len(ids)
			// The full id set lets QA prove a run removed the accounts it
			// created; a crash before cleanup leaves them visible to doctor.
			out["ids"] = ids
			orphans := []uint32{}
			for _, id := range ids {
				if !linked[id] {
					orphans = append(orphans, id)
				}
			}
			out["orphans"] = orphans
		}
	}
	if s.b != nil && s.b.db != nil {
		var pending int
		if err := s.b.db.readDB().QueryRowContext(ctx, `SELECT count(*) FROM meta WHERE key LIKE ?`, dcRemovePending+"%").Scan(&pending); err == nil {
			out["pending_removal"] = pending
		}
	}
	return out
}

func (s *dcService) syncProfile(c companionState, accID uint32) error {
	rpc := s.getRPC()
	if rpc == nil || accID == 0 {
		return nil
	}
	name := c.Name
	if err := rpc.SetConfig(accID, "displayname", &name); err != nil {
		return err
	}
	pngBytes, err := companionAvatarPNG(s.b, c)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "lark-avatar-*.png")
	if err != nil {
		return err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.Write(pngBytes); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return rpc.SetConfig(accID, "selfavatar", &path)
}

func (s *dcService) autoPairAll(ctx context.Context) {
	rpc := s.getRPC()
	if rpc == nil {
		return
	}
	comps, err := s.b.allCompanions(ctx)
	if err != nil {
		return
	}
	type pairAcc struct {
		id    string
		accID uint32
		addr  string
	}
	var list []pairAcc
	for _, c := range comps {
		if c.DCAccountID == 0 {
			continue
		}
		addrPtr, err := rpc.GetConfig(c.DCAccountID, "addr")
		if err != nil || addrPtr == nil || *addrPtr == "" {
			continue
		}
		addr := strings.ToLower(*addrPtr)
		s.mu.Lock()
		// A create can race a delete; re-check under the mapping lock so a
		// deleted companion is never resurrected after forget.
		if _, exists := s.b.companionByID(c.ID); exists {
			s.byAcc[c.DCAccountID] = c.ID
			s.addrs[c.ID] = addr
		}
		s.mu.Unlock()
		list = append(list, pairAcc{c.ID, c.DCAccountID, addr})
	}
	for i := 0; i < len(list); i++ {
		a := list[i]
		contacts, err := rpc.GetContacts(a.accID, 0, nil)
		if err != nil {
			continue
		}
		verified := make(map[string]bool, len(contacts))
		for _, ct := range contacts {
			if ct.IsVerified {
				verified[strings.ToLower(ct.Address)] = true
			}
		}
		for j := i + 1; j < len(list); j++ {
			if ctx.Err() != nil {
				return
			}
			b := list[j]
			if verified[b.addr] {
				continue
			}
			qr, err := rpc.GetChatSecurejoinQrCode(a.accID, nil)
			if err != nil {
				log.Printf("Delta Chat QR for %s: %v", a.id, err)
				continue
			}
			if _, err := rpc.SecureJoin(b.accID, qr); err != nil {
				log.Printf("Delta Chat SecureJoin %s -> %s: %v", b.id, a.id, err)
			}
		}
	}
}

func (s *dcService) forget(companionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.addrs, companionID)
	for acc, cid := range s.byAcc {
		if cid == companionID {
			delete(s.byAcc, acc)
		}
	}
}

func (s *dcService) accountForCompanion(companionID string) uint32 {
	if s.b != nil {
		c := s.b.companionFor(companionID)
		if c.DCAccountID != 0 {
			return c.DCAccountID
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for acc, cid := range s.byAcc {
		if cid == companionID {
			return acc
		}
	}
	return 0
}

func (s *dcService) companionByAddr(addr string) (string, bool) {
	addr = strings.ToLower(strings.TrimSpace(addr))
	if addr == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for cid, a := range s.addrs {
		if a == addr {
			return cid, true
		}
	}
	return "", false
}

type dcContactInfo struct {
	ID       string
	Name     string
	Kind     string // "human" or "companion"
	Verified bool
}

func (s *dcService) companionForAccount(ctx context.Context, accID uint32) string {
	s.mu.Lock()
	companionID := s.byAcc[accID]
	s.mu.Unlock()
	if companionID != "" || s.b == nil {
		return companionID
	}
	comps, err := s.b.allCompanions(ctx)
	if err != nil {
		return ""
	}
	for _, c := range comps {
		if c.DCAccountID == accID {
			s.mu.Lock()
			s.byAcc[accID] = c.ID
			s.mu.Unlock()
			return c.ID
		}
	}
	return ""
}

func dcContactKey(ct deltachat.Contact) string {
	if addr := strings.ToLower(strings.TrimSpace(ct.Address)); addr != "" {
		return addr
	}
	return strconv.FormatUint(uint64(ct.Id), 10)
}
