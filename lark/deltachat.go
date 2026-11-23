package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	imagecolor "image/color"
	"image/png"
	"io"
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
)

type dcRPC interface {
	AddAccount() (uint32, error)
	AddTransportFromQr(accID uint32, qr string) error
	BatchSetConfig(accID uint32, cfg map[string]*string) error
	SetConfig(accID uint32, key string, val *string) error
	GetConfig(accID uint32, key string) (*string, error)
	StartIoForAllAccounts() error
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
		case <-time.After(2 * time.Second):
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
		botVal := "1"
		nameVal := c.Name
		cfg := map[string]*string{
			"bot":         &botVal,
			"displayname": &nameVal,
		}
		if err := rpc.BatchSetConfig(accID, cfg); err != nil {
			return 0, err
		}
		whoCalls := "nobody"
		_ = rpc.SetConfig(accID, "who_can_call_me", &whoCalls)
		if err := rpc.AddTransportFromQr(accID, "dcaccount:"+s.relay); err != nil {
			return 0, err
		}
		if err := rpc.StartIoForAllAccounts(); err != nil {
			log.Printf("Delta Chat StartIo after provisioning %s: %v", c.ID, err)
		}
		if s.b != nil && s.b.db != nil {
			if _, err := s.b.db.ExecContext(ctx, `UPDATE companions SET dc_account_id=? WHERE id=?`, accID, c.ID); err != nil {
				return 0, err
			}
		}
	}
	s.syncProfile(c, accID)
	addrPtr, err := rpc.GetConfig(accID, "addr")
	s.mu.Lock()
	s.byAcc[accID] = c.ID
	if err == nil && addrPtr != nil && *addrPtr != "" {
		s.addrs[c.ID] = strings.ToLower(*addrPtr)
	}
	s.mu.Unlock()
	return accID, nil
}

func (s *dcService) syncProfile(c companionState, accID uint32) {
	rpc := s.getRPC()
	if rpc == nil || accID == 0 {
		return
	}
	name := c.Name
	_ = rpc.SetConfig(accID, "displayname", &name)
	if pngBytes, err := companionAvatarPNG(s.b, c); err == nil {
		tmp, err := os.CreateTemp("", "lark-avatar-*.png")
		if err == nil {
			path := tmp.Name()
			_, werr := tmp.Write(pngBytes)
			cerr := tmp.Close()
			if werr == nil && cerr == nil {
				_ = rpc.SetConfig(accID, "selfavatar", &path)
			}
			_ = os.Remove(path)
		}
	}
}

func companionAvatarPNG(b *larkServer, c companionState) ([]byte, error) {
	var bg *image.RGBA
	var poses [4]*image.RGBA
	loaded := false
	if b != nil && b.portraits != nil && c.PortraitID != "" {
		dir := filepath.Join(b.portraits.dir, c.PortraitID)
		if p, err := loadOrNormalizePoses(dir, pixelPortraitHeight); err == nil {
			poses = p
			loaded = true
		}
		if bgImg, err := loadBackgroundPNG(dir); err == nil {
			bg = bgImg
		}
	}
	if !loaded {
		poses = pixelPlaceholderPoses()
	}
	img := renderPixelHome(pixelSpecVoCat, bg, poses[0], gradeDay, "", "")
	return imageToPNG(img)
}

func imageToPNG(img image.Image) ([]byte, error) {
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func fbToPNG(fb []uint16, geom screenGeometry) ([]byte, error) {
	w, h := geom.width, geom.height
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			v := fb[y*w+x]
			r5 := uint8((v >> 11) & 0x1f)
			g6 := uint8((v >> 5) & 0x3f)
			b5 := uint8(v & 0x1f)
			img.SetNRGBA(x, y, imagecolor.NRGBA{
				R: (r5 << 3) | (r5 >> 2),
				G: (g6 << 2) | (g6 >> 4),
				B: (b5 << 3) | (b5 >> 2),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
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
		s.byAcc[c.DCAccountID] = c.ID
		s.addrs[c.ID] = addr
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

// shortenDeltaChatInvite strips optional display-name and version query parameters
// from a Delta Chat invite URL so the QR version and module count stay as small as possible.
func shortenDeltaChatInvite(raw string) string {
	idx := strings.IndexByte(raw, '#')
	if idx < 0 {
		return raw
	}
	prefix := raw[:idx+1]
	frag := raw[idx+1:]
	parts := strings.Split(frag, "&")
	if len(parts) <= 1 {
		return raw
	}
	kept := []string{parts[0]}
	for _, p := range parts[1:] {
		if strings.HasPrefix(p, "n=") || strings.HasPrefix(p, "v=") {
			continue
		}
		kept = append(kept, p)
	}
	return prefix + strings.Join(kept, "&")
}

func (s *dcService) pairInvite(companionID string) (string, error) {
	rpc := s.getRPC()
	if rpc == nil {
		return "", errors.New("Delta Chat unavailable")
	}
	accID := s.accountForCompanion(companionID)
	if accID == 0 {
		return "", errors.New("Companion has no Delta Chat account")
	}
	qr, err := rpc.GetChatSecurejoinQrCode(accID, nil)
	if err != nil {
		return "", err
	}
	return shortenDeltaChatInvite(qr), nil
}

func (s *dcService) pairQR(companionID string) (string, error) {
	rpc := s.getRPC()
	if rpc == nil {
		return "", errors.New("Delta Chat unavailable")
	}
	accID := s.accountForCompanion(companionID)
	if accID == 0 {
		return "", errors.New("Companion has no Delta Chat account")
	}
	qr, err := rpc.GetChatSecurejoinQrCode(accID, nil)
	if err != nil {
		return "", err
	}
	return rpc.CreateQrSvg(qr)
}

func (s *dcService) listPairedContacts(companionID string) ([]dcContact, error) {
	rpc := s.getRPC()
	if rpc == nil {
		return nil, errors.New("Delta Chat unavailable")
	}
	accID := s.accountForCompanion(companionID)
	if accID == 0 {
		return nil, errors.New("Companion has no Delta Chat account")
	}
	contacts, err := rpc.GetContacts(accID, 0, nil)
	if err != nil {
		return nil, err
	}
	var out []dcContact
	for _, ct := range contacts {
		if ct.Id == deltachat.ContactSelf || !ct.IsVerified || ct.IsBlocked {
			continue
		}
		if _, isPeer := s.companionByAddr(ct.Address); isPeer {
			continue
		}
		name := strings.TrimSpace(ct.DisplayName)
		if name == "" {
			name = strings.TrimSpace(ct.Name)
		}
		if name == "" {
			name = ct.Address
		}
		out = append(out, dcContact{ID: ct.Id, Name: name, Address: ct.Address, IsBot: ct.IsBot})
	}
	slices.SortFunc(out, func(a, b dcContact) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return out, nil
}

type dcContactInfo struct {
	ID       string
	Name     string
	Kind     string // "human" or "companion"
	Verified bool
}

func (s *dcService) listAllContacts(companionID string) ([]dcContactInfo, error) {
	rpc := s.getRPC()
	if rpc == nil {
		return nil, errors.New("Delta Chat unavailable")
	}
	accID := s.accountForCompanion(companionID)
	if accID == 0 {
		return nil, errors.New("Companion has no Delta Chat account")
	}
	contacts, err := rpc.GetContacts(accID, 0, nil)
	if err != nil {
		return nil, err
	}
	var out []dcContactInfo
	for _, ct := range contacts {
		if ct.Id == deltachat.ContactSelf || ct.IsBlocked {
			continue
		}
		name := strings.TrimSpace(ct.DisplayName)
		if name == "" {
			name = strings.TrimSpace(ct.Name)
		}
		if name == "" {
			name = ct.Address
		}
		kind := "human"
		id := ""
		if peerID, isPeer := s.companionByAddr(ct.Address); isPeer {
			kind = "companion"
			id = peerID
			if peer := s.b.companionFor(peerID); peer.Name != "" {
				name = peer.Name
			}
		}
		out = append(out, dcContactInfo{ID: id, Name: name, Kind: kind, Verified: ct.IsVerified})
	}
	slices.SortFunc(out, func(a, b dcContactInfo) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out, nil
}

func (s *dcService) unpairContact(companionID string, contactID uint32) error {
	rpc := s.getRPC()
	if rpc == nil {
		return errors.New("Delta Chat unavailable")
	}
	accID := s.accountForCompanion(companionID)
	if accID == 0 {
		return errors.New("Companion has no Delta Chat account")
	}
	if chatID, err := rpc.GetChatIdByContactId(accID, contactID); err == nil && chatID != nil {
		_ = rpc.DeleteChat(accID, *chatID)
	}
	return rpc.DeleteContact(accID, contactID)
}

func (s *dcService) sendIntercom(ctx context.Context, fromCompanionID, targetName, text string, pcm16k []byte) ([]string, error) {
	rpc := s.getRPC()
	if rpc == nil {
		return nil, problem(503, "Delta Chat unavailable")
	}
	accID := s.accountForCompanion(fromCompanionID)
	if accID == 0 {
		return nil, problem(503, "Companion has no Delta Chat account")
	}
	contacts, err := rpc.GetContacts(accID, 0, nil)
	if err != nil {
		return nil, problem(502, "Cannot read contacts")
	}
	targetLower := strings.ToLower(strings.TrimSpace(targetName))
	var matched []deltachat.Contact
	for _, ct := range contacts {
		if ct.Id == deltachat.ContactSelf || !ct.IsVerified || ct.IsBlocked {
			continue
		}
		peerID, isPeer := s.companionByAddr(ct.Address)
		if targetLower == "all" {
			if isPeer {
				matched = append(matched, ct)
			}
			continue
		}
		name := strings.ToLower(strings.TrimSpace(ct.DisplayName))
		alt := strings.ToLower(strings.TrimSpace(ct.Name))
		peerName := ""
		if isPeer && s.b != nil {
			if pc, ok := s.b.companionByID(peerID); ok {
				peerName = strings.ToLower(strings.TrimSpace(pc.Name))
			}
		}
		if (isPeer && (peerID == targetLower || peerName == targetLower)) || name == targetLower || alt == targetLower {
			matched = append(matched, ct)
		}
	}
	if len(matched) == 0 && targetLower != "all" {
		for _, ct := range contacts {
			if ct.Id == deltachat.ContactSelf || !ct.IsVerified || ct.IsBlocked {
				continue
			}
			name := strings.ToLower(strings.TrimSpace(ct.DisplayName))
			if strings.Contains(name, targetLower) {
				matched = append(matched, ct)
			}
		}
	}
	if len(matched) == 0 {
		return nil, problem(404, "No matching paired contact found")
	}

	var opusPath string
	if len(pcm16k) > 0 {
		tmp, err := os.CreateTemp("", "lark-voice-*.ogg")
		if err == nil {
			opusPath = tmp.Name()
			_ = tmp.Close()
			defer os.Remove(opusPath)
			if err := encodePCMToOpus(ctx, pcm16k, sampleRate, opusPath); err != nil {
				opusPath = ""
			}
		}
	}

	var sentTo []string
	sender := s.b.companionFor(fromCompanionID)
	for _, ct := range matched {
		chatID, err := rpc.CreateChatByContactId(accID, ct.Id)
		if err != nil {
			continue
		}
		var msg deltachat.MessageData
		if text != "" {
			msg.Text = &text
		}
		if opusPath != "" {
			vt := deltachat.ViewtypeVoice
			msg.File = &opusPath
			msg.Viewtype = &vt
		}
		if _, err := rpc.SendMsg(accID, chatID, msg); err == nil {
			label := ct.DisplayName
			if peerID, isPeer := s.companionByAddr(ct.Address); isPeer {
				label = peerID
			}
			sentTo = append(sentTo, label)
		}
	}
	if len(sentTo) > 0 && s.b != nil {
		s.b.chat(fromCompanionID, newChat("companion", sender.Name, text, "intercom"), true)
	}
	return sentTo, nil
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

func (s *dcService) syncWelcomes(ctx context.Context) {
	rpc := s.getRPC()
	if rpc == nil || s.b == nil {
		return
	}
	comps, err := s.b.allCompanions(ctx)
	if err != nil {
		return
	}
	for _, c := range comps {
		if ctx.Err() != nil {
			return
		}
		if c.DCAccountID == 0 {
			continue
		}
		contacts, err := rpc.GetContacts(c.DCAccountID, 0, nil)
		if err != nil {
			continue
		}
		for _, ct := range contacts {
			s.sendWelcomeIfNeeded(ctx, c, c.DCAccountID, ct)
		}
	}
}

func (s *dcService) handleSecureJoinVerified(ctx context.Context, accID, contactID uint32) {
	rpc := s.getRPC()
	if rpc == nil || s.b == nil || contactID == 0 {
		return
	}
	companionID := s.companionForAccount(ctx, accID)
	if companionID == "" {
		return
	}
	ct, err := rpc.GetContact(accID, contactID)
	if err != nil {
		return
	}
	c := s.b.companionFor(companionID)
	s.sendWelcomeIfNeeded(ctx, c, accID, ct)
}

func (s *dcService) sendWelcomeIfNeeded(ctx context.Context, c companionState, accID uint32, ct deltachat.Contact) {
	rpc := s.getRPC()
	if rpc == nil || s.b == nil || s.b.db == nil {
		return
	}
	if ct.Id == deltachat.ContactSelf || !ct.IsVerified || ct.IsBlocked || ct.IsBot {
		return
	}
	if _, isPeer := s.companionByAddr(ct.Address); isPeer {
		return
	}
	key := "dc_welcome:" + c.ID + ":" + dcContactKey(ct)
	if seen, err := s.b.db.getMeta(ctx, key); err == nil && seen == dcWelcomeVersion {
		return
	}
	chatID, err := rpc.CreateChatByContactId(accID, ct.Id)
	if err != nil {
		return
	}
	txt := welcomeText(c)
	if _, err := rpc.SendMsg(accID, chatID, deltachat.MessageData{Text: &txt}); err != nil {
		log.Printf("Delta Chat welcome SendMsg error (%s): %v", c.ID, err)
		return
	}
	_ = s.b.db.setMeta(ctx, key, dcWelcomeVersion)
}

func (s *dcService) handleMessage(ctx context.Context, accID, msgID uint32) {
	rpc := s.getRPC()
	if rpc == nil {
		return
	}
	companionID := s.companionForAccount(ctx, accID)
	if companionID == "" {
		return
	}
	msg, err := rpc.GetMessage(accID, msgID)
	if err != nil || msg.IsInfo || msg.FromId == deltachat.ContactSelf || msg.FromId == 0 {
		return
	}
	contact, err := rpc.GetContact(accID, msg.FromId)
	if err != nil || !contact.IsVerified || contact.IsBlocked {
		return
	}
	chat, err := rpc.GetBasicChatInfo(accID, msg.ChatId)
	if err != nil || chat.IsContactRequest {
		return
	}

	isVoice := (msg.ViewType == deltachat.ViewtypeVoice || msg.ViewType == deltachat.ViewtypeAudio) && msg.File != nil && *msg.File != ""
	if peerID, isPeer := s.companionByAddr(contact.Address); isPeer {
		var pcm []byte
		if isVoice {
			pcm, _ = decodeAudioToPCM16k(ctx, *msg.File)
		}
		sender := s.b.companionFor(peerID)
		if s.b.ws != nil {
			s.b.ws.deliverDCIntercom(ctx, sender, companionID, msg.Text, pcm)
		}
		return
	}

	text := strings.TrimSpace(msg.Text)
	if isVoice {
		pcm, err := decodeAudioToPCM16k(ctx, *msg.File)
		if err != nil {
			log.Printf("Delta Chat voice decode error: %v", err)
			return
		}
		if s.b.stt == nil {
			return
		}
		spoken, err := s.b.stt(ctx, pcm)
		if err != nil {
			log.Printf("Delta Chat voice STT error: %v", err)
			return
		}
		text = strings.TrimSpace(spoken)
	}
	senderName := strings.TrimSpace(contact.DisplayName)
	if senderName == "" {
		senderName = strings.TrimSpace(contact.Name)
	}
	if senderName == "" {
		senderName = "Friend"
	}
	if !isVoice && msg.File != nil && strings.HasSuffix(strings.ToLower(*msg.File), ".twee") {
		s.importTweeAttachment(ctx, rpc, accID, msg.ChatId, senderName, *msg.File)
		return
	}
	if text == "" {
		return
	}
	who := s.b.companionFor(companionID)

	private := false
	if !isVoice && strings.HasPrefix(text, "/") {
		cmd, arg := splitSlashCommand(text)
		switch cmd {
		case "/say":
			if arg == "" {
				hint := "Usage: /say <words>"
				_, _ = rpc.SendMsg(accID, msg.ChatId, deltachat.MessageData{Text: &hint})
				return
			}
			s.b.sayOnCompanion(ctx, companionID, senderName, arg)
			ack := "Said it."
			if _, err := rpc.SendMsg(accID, msg.ChatId, deltachat.MessageData{Text: &ack}); err != nil {
				log.Printf("Delta Chat SendMsg error: %v", err)
			}
			return
		case "/private":
			if arg == "" {
				hint := "Usage: /private <words>"
				_, _ = rpc.SendMsg(accID, msg.ChatId, deltachat.MessageData{Text: &hint})
				return
			}
			private = true
			text = arg
		case "/help":
			help := welcomeText(who)
			if _, err := rpc.SendMsg(accID, msg.ChatId, deltachat.MessageData{Text: &help}); err != nil {
				log.Printf("Delta Chat SendMsg error: %v", err)
			}
			return
		case "/remote":
			if s.b.remote == nil {
				hint := "Remote controls unavailable."
				_, _ = rpc.SendMsg(accID, msg.ChatId, deltachat.MessageData{Text: &hint})
				return
			}
			contactKey := dcContactKey(contact)
			if strings.EqualFold(arg, "off") {
				_, err := s.b.remote.revokeContact(ctx, companionID, contactKey)
				reply := "Remote controls off."
				if err != nil && !errors.Is(err, context.Canceled) {
					reply = err.Error()
				}
				_, _ = rpc.SendMsg(accID, msg.ChatId, deltachat.MessageData{Text: &reply})
				return
			}
			if arg != "" {
				hint := "Usage: /remote [off]"
				_, _ = rpc.SendMsg(accID, msg.ChatId, deltachat.MessageData{Text: &hint})
				return
			}
			link, err := s.b.remote.issue(ctx, companionID, contactKey)
			if err != nil {
				errText := err.Error()
				_, _ = rpc.SendMsg(accID, msg.ChatId, deltachat.MessageData{Text: &errText})
				return
			}
			_, _ = rpc.SendMsg(accID, msg.ChatId, deltachat.MessageData{Text: &link})
			return
		case "/stories":
			s.sendStoriesList(ctx, rpc, accID, msg.ChatId)
			return
		case "/story":
			if arg == "" {
				hint := "Usage: /story <name>"
				_, _ = rpc.SendMsg(accID, msg.ChatId, deltachat.MessageData{Text: &hint})
				return
			}
			s.sendStoryFiles(ctx, rpc, companionID, accID, msg.ChatId, arg)
			return
		default:
			hint := "Unknown command. Try /say <words>, /private <words>, /stories, /story <name>, /remote, or /help."
			if _, err := rpc.SendMsg(accID, msg.ChatId, deltachat.MessageData{Text: &hint}); err != nil {
				log.Printf("Delta Chat SendMsg error: %v", err)
			}
			return
		}
	}

	if s.b.oc == nil {
		return
	}

	turnCtx, cancel := context.WithTimeout(ctx, turnTimeout)
	defer cancel()
	turnCtx = s.b.traceTurn(turnCtx, companionID, who.Name, "deltachat", time.Now())

	s.b.chat(companionID, newChat("user", senderName, text, "deltachat"), true)
	if !private && s.b.ws != nil {
		cardSender := blankCompanion()
		cardSender.ID = senderName
		cardSender.Name = senderName
		s.b.ws.deliverNotice(ctx, terminalNotice{
			recipientIDs: []string{companionID},
			sender:       cardSender,
			text:         text,
			speakText:    "Message from " + senderName + ": " + text,
			voice:        s.b.voiceFor(companionID),
			chime:        "intercom",
			showCard:     true,
			replyTo:      senderName,
			trace:        traceFrom(turnCtx),
		})
	}

	prompt := s.b.turnPromptWithState(turnCtx, companionID, "deltachat", text, who, turnChannel{
		channel:    "deltachat",
		sender:     senderName,
		senderKind: "contact",
		private:    private,
	}, time.Now().In(s.b.location()))
	raw, _, err := s.b.oc.askPrepared(turnCtx, companionID, func() (string, string) {
		return who.Name, prompt
	}, turnStream{}, "deltachat", nil, nil)
	if err != nil {
		return
	}
	reply := strings.TrimSpace(fenced.ReplaceAllString(raw, ""))
	if isSilentReply(reply) {
		return
	}
	s.b.chat(companionID, newChat("companion", who.Name, reply, "deltachat"), true)

	outMsg := deltachat.MessageData{Text: &reply}
	var opusPath string
	if isVoice && s.b.worker != nil && s.b.worker.ready.Load() {
		if clean, err := spokenText(reply); err == nil {
			if pcm, err := s.b.worker.synthesize(turnCtx, s.b.voiceFor(companionID), clean); err == nil && len(pcm) > 0 {
				if tmp, err := os.CreateTemp("", "lark-reply-*.ogg"); err == nil {
					opusPath = tmp.Name()
					_ = tmp.Close()
					defer os.Remove(opusPath)
					if err := encodePCMToOpus(turnCtx, pcm, sampleRate, opusPath); err == nil {
						vt := deltachat.ViewtypeVoice
						outMsg.File = &opusPath
						outMsg.Viewtype = &vt
					}
				}
			}
		}
	}
	if _, err := rpc.SendMsg(accID, msg.ChatId, outMsg); err != nil {
		log.Printf("Delta Chat SendMsg error: %v", err)
	}
	if !private && s.b.ws != nil {
		s.b.ws.deliverNotice(ctx, terminalNotice{
			recipientIDs: []string{companionID},
			sender:       who,
			text:         reply,
			speakText:    "I told " + senderName + ": " + reply,
			voice:        s.b.voiceFor(companionID),
			showCard:     true,
			replyTo:      senderName,
			trace:        traceFrom(turnCtx),
		})
	}
}

func splitSlashCommand(text string) (cmd, arg string) {
	text = strings.TrimSpace(text)
	for i, r := range text {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return strings.ToLower(text[:i]), strings.TrimSpace(text[i:])
		}
	}
	return strings.ToLower(text), ""
}

func welcomeText(c companionState) string {
	name := strings.TrimSpace(c.Name)
	if name == "" {
		name = "Lark"
	}
	intro := fmt.Sprintf("Hi! I'm %s.", name)
	if form := strings.TrimSpace(c.Form); form != "" {
		intro = fmt.Sprintf("Hi! I'm %s, %s.", name, form)
	}
	return strings.Join([]string{
		intro + " Text me anytime — I'll read it out at home and reply.",
		"/say <words> — I'll say it out loud exactly",
		"/private <words> — just between us",
		"/stories — list interactive stories",
		"/story <name> — download a story as a .twee file",
		"/remote — open my remote controls",
		"/help — this message",
		"Send a .twee file to import a story. When listening mode is on, people at home can answer you out loud.",
	}, "\n")
}

func (s *dcService) sendStoriesList(ctx context.Context, rpc dcRPC, accID, chatID uint32) {
	stories, err := s.b.listStories(ctx)
	if err != nil {
		msg := "Could not load stories."
		_, _ = rpc.SendMsg(accID, chatID, deltachat.MessageData{Text: &msg})
		return
	}
	if len(stories) == 0 {
		msg := "No stories yet."
		_, _ = rpc.SendMsg(accID, chatID, deltachat.MessageData{Text: &msg})
		return
	}
	lines := []string{"Stories:"}
	for _, st := range stories {
		lines = append(lines, fmt.Sprintf("- %s (%s, %d passages)", st.Title, st.ID, st.Passages))
	}
	out := strings.Join(lines, "\n")
	_, _ = rpc.SendMsg(accID, chatID, deltachat.MessageData{Text: &out})
}

func (s *dcService) sendStoryFiles(ctx context.Context, rpc dcRPC, companionID string, accID, chatID uint32, query string) {
	stories, err := s.b.listStories(ctx)
	if err != nil {
		msg := "Could not load stories."
		_, _ = rpc.SendMsg(accID, chatID, deltachat.MessageData{Text: &msg})
		return
	}
	q := strings.ToLower(strings.TrimSpace(query))
	var found *savedStory
	for i := range stories {
		st := &stories[i]
		if st.ID == q || strings.EqualFold(st.Title, query) {
			found = st
			break
		}
	}
	if found == nil {
		msg := fmt.Sprintf("Story %q not found. Try /stories to see available stories.", query)
		_, _ = rpc.SendMsg(accID, chatID, deltachat.MessageData{Text: &msg})
		return
	}
	tmpDir, err := os.MkdirTemp("", "lark-story-*")
	if err != nil {
		return
	}
	defer os.RemoveAll(tmpDir)

	var oldIDs []uint32
	metaKey := fmt.Sprintf("dc_story_msgs:%s:%d:%s", companionID, chatID, found.ID)
	if s.b != nil && s.b.db != nil {
		if raw, err := s.b.db.getMeta(ctx, metaKey); err == nil {
			for _, part := range strings.Split(raw, ",") {
				if parsed, err := strconv.ParseUint(strings.TrimSpace(part), 10, 32); err == nil && parsed > 0 {
					oldIDs = append(oldIDs, uint32(parsed))
				}
			}
		}
	}

	tweePath := filepath.Join(tmpDir, found.ID+".twee")
	var newID uint32
	if err := os.WriteFile(tweePath, []byte(found.Twee), 0o600); err == nil {
		vtFile := deltachat.ViewtypeFile
		caption := found.Title + " (.twee)"
		if mid, err := rpc.SendMsg(accID, chatID, deltachat.MessageData{Text: &caption, File: &tweePath, Viewtype: &vtFile}); err == nil {
			newID = mid
		}
	}
	if newID > 0 && s.b != nil && s.b.db != nil {
		_ = s.b.db.setMeta(ctx, metaKey, strconv.FormatUint(uint64(newID), 10))
	}
	var toDelete []uint32
	for _, oldID := range oldIDs {
		if oldID != newID {
			toDelete = append(toDelete, oldID)
		}
	}
	if len(toDelete) > 0 {
		if err := rpc.DeleteMessagesForAll(accID, toDelete); err != nil {
			log.Printf("Delta Chat delete old story files error (%s/%s): %v", companionID, found.ID, err)
		}
	}
}

func (s *dcService) importTweeAttachment(ctx context.Context, rpc dcRPC, accID, chatID uint32, senderName, filePath string) {
	f, err := os.Open(filePath)
	if err != nil {
		msg := "Could not read .twee attachment."
		_, _ = rpc.SendMsg(accID, chatID, deltachat.MessageData{Text: &msg})
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 64*1024+1))
	if err != nil {
		msg := "Could not read .twee attachment."
		_, _ = rpc.SendMsg(accID, chatID, deltachat.MessageData{Text: &msg})
		return
	}
	if len(raw) > 64*1024 {
		msg := "Story file exceeds 64 KiB limit."
		_, _ = rpc.SendMsg(accID, chatID, deltachat.MessageData{Text: &msg})
		return
	}
	cs, err := parseTwee(string(raw))
	if err != nil {
		msg := fmt.Sprintf("Could not import story:\n%s", err.Error())
		_, _ = rpc.SendMsg(accID, chatID, deltachat.MessageData{Text: &msg})
		return
	}
	slug := s.b.nextAvailableStorySlug(ctx, cs.Title)
	saved, _, err := s.b.saveStory(ctx, slug, string(raw), senderName, true)
	if err != nil {
		msg := fmt.Sprintf("Could not save story: %s", err.Error())
		_, _ = rpc.SendMsg(accID, chatID, deltachat.MessageData{Text: &msg})
		return
	}
	msg := fmt.Sprintf("Imported story %q (%s, %d passages).", saved.Title, saved.ID, saved.Passages)
	_, _ = rpc.SendMsg(accID, chatID, deltachat.MessageData{Text: &msg})
}

func decodeAudioToPCM16k(ctx context.Context, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-i", path, "-f", "s16le", "-acodec", "pcm_s16le", "-ac", "1", "-ar", strconv.Itoa(sampleRate), "-")
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg decode: %v (%s)", err, strings.TrimSpace(errBuf.String()))
	}
	return out.Bytes(), nil
}

func encodePCMToOpus(ctx context.Context, pcm []byte, rate int, outPath string) error {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-y", "-f", "s16le", "-ar", strconv.Itoa(rate), "-ac", "1", "-i", "-", "-c:a", "libopus", "-b:a", "32k", outPath)
	cmd.Stdin = bytes.NewReader(pcm)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg encode: %v (%s)", err, strings.TrimSpace(errBuf.String()))
	}
	return nil
}

func (h *wsHub) deliverDCIntercom(ctx context.Context, sender companionState, recipientID, text string, pcm []byte) {
	if h == nil || h.b == nil {
		return
	}
	intro := "Message from " + sender.Name + "!"
	if pcm != nil {
		intro = "From " + sender.Name + "!"
	}
	h.deliverNotice(ctx, terminalNotice{
		recipientIDs: []string{recipientID},
		sender:       sender,
		text:         text,
		speakText:    text,
		intro:        intro,
		voice:        h.b.voiceFor(sender.ID),
		pcm:          pcm,
		chime:        "intercom",
		showCard:     true,
		trace:        traceFrom(ctx),
	})
	if text != "" {
		h.b.chat(recipientID, newChat("intercom", sender.Name, text, "intercom"), true)
	}
}
