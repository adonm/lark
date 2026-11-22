package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	imagecolor "image/color"
	"image/png"
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

const defaultChatmailRelay = "nine.testrun.org"

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
	GetMessage(accID uint32, msgID uint32) (deltachat.Message, error)
	SendMsg(accID uint32, chatID uint32, data deltachat.MessageData) (uint32, error)
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
	img := renderPixelHome(pixelSpecVoCat, bg, poses[0], gradeDay, "", 0, "", false)
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
		if (isPeer && peerID == targetLower) || name == targetLower || alt == targetLower {
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

func (s *dcService) handleMessage(ctx context.Context, accID, msgID uint32) {
	rpc := s.getRPC()
	if rpc == nil {
		return
	}
	s.mu.Lock()
	companionID := s.byAcc[accID]
	s.mu.Unlock()
	if companionID == "" {
		comps, err := s.b.allCompanions(ctx)
		if err == nil {
			for _, c := range comps {
				if c.DCAccountID == accID {
					companionID = c.ID
					s.mu.Lock()
					s.byAcc[accID] = companionID
					s.mu.Unlock()
					break
				}
			}
		}
	}
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
	if text == "" {
		return
	}
	if s.b.oc == nil {
		return
	}
	who := s.b.companionFor(companionID)
	senderName := strings.TrimSpace(contact.DisplayName)
	if senderName == "" {
		senderName = strings.TrimSpace(contact.Name)
	}
	if senderName == "" {
		senderName = "Friend"
	}

	turnCtx, cancel := context.WithTimeout(ctx, turnTimeout)
	defer cancel()
	turnCtx = s.b.traceTurn(turnCtx, companionID, who.Name, "deltachat", time.Now())

	s.b.chat(companionID, newChat("user", senderName, text, "deltachat"), true)
	if s.b.ws != nil {
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

	prompt := turnPromptChannelAt(companionID, "deltachat", text, who, turnChannel{
		channel:    "deltachat",
		sender:     senderName,
		senderKind: "contact",
	}, time.Now().In(s.b.location()))
	s.b.setDCBuildTarget(companionID, accID, msg.ChatId)
	defer s.b.setDCBuildTarget(companionID, 0, 0)
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
	if s.b.ws != nil {
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
