package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/chatmail/rpc-client-go/v2/deltachat"
)

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
		if s.b.hub != nil {
			s.b.hub.deliverDCIntercom(ctx, sender, companionID, msg.ChatId, msg.Text, pcm)
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
		transcribe := s.b.sttFunc()
		if transcribe == nil {
			return
		}
		spoken, err := transcribe(ctx, pcm)
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
			s.b.sayOnCompanion(ctx, companionID, senderName, msg.ChatId, arg)
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
			link, err := s.b.remote.issue(ctx, companionID, contactKey, contact.Name)
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
	if !private && s.b.hub != nil {
		cardSender := blankCompanion()
		cardSender.ID = senderName
		cardSender.Name = senderName
		n := intercomNotice(cardSender, []string{companionID}, text, nil, s.b.voiceFor(companionID))
		n.intro = ""
		n.speakText = "Message from " + senderName + ": " + text
		n.reply = &turnReply{name: senderName, companionID: companionID, chatID: msg.ChatId}
		n.trace = traceFrom(turnCtx)
		s.b.hub.deliverNotice(ctx, n)
	}
	gen, err := s.b.generateReply(turnCtx, turnCtx, who, text, turnOptions{
		source: "deltachat",
		channel: turnChannel{
			channel:    "deltachat",
			sender:     senderName,
			senderKind: "contact",
			private:    private,
		},
		textOnly: true,
	})
	if err != nil {
		return
	}
	reply := gen.reply
	if reply == "" {
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
	if !private && s.b.hub != nil {
		s.b.hub.deliverNotice(ctx, terminalNotice{
			recipientIDs: []string{companionID},
			sender:       who,
			text:         reply,
			speakText:    "I told " + senderName + ": " + reply,
			voice:        s.b.voiceFor(companionID),
			showCard:     true,
			cardReply:    senderName,
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
		out = append(out, dcContact{ID: ct.Id, Name: s.contactName(ct), Address: ct.Address, IsBot: ct.IsBot})
	}
	slices.SortFunc(out, func(a, b dcContact) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return out, nil
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
		name := s.contactName(ct)
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

// contactMatches reports whether target names this contact exactly as
// me.contacts shows it: display name, address-book name, address, or for a
// companion peer its current ID and renamed name.
func (s *dcService) contactMatches(ct deltachat.Contact, target string) bool {
	names := []string{
		strings.ToLower(strings.TrimSpace(ct.DisplayName)),
		strings.ToLower(strings.TrimSpace(ct.Name)),
		strings.ToLower(strings.TrimSpace(ct.Address)),
	}
	if peerID, isPeer := s.companionByAddr(ct.Address); isPeer {
		names = append(names, strings.ToLower(peerID))
		if s.b != nil {
			if peer, ok := s.b.companionByID(peerID); ok {
				names = append(names, strings.ToLower(strings.TrimSpace(peer.Name)))
			}
		}
	}
	return slices.ContainsFunc(names, func(name string) bool { return name != "" && name == target })
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

// contactName is the name shown to agents in me.contacts for a Delta Chat
// contact, so emit targets and read-out labels always match it.
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

func (s *dcService) contactName(ct deltachat.Contact) string {
	if name := strings.TrimSpace(ct.DisplayName); name != "" {
		return name
	}
	if name := strings.TrimSpace(ct.Name); name != "" {
		return name
	}
	return strings.TrimSpace(ct.Address)
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
