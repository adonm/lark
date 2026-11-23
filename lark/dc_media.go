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
	"strconv"
	"strings"

	"github.com/chatmail/rpc-client-go/v2/deltachat"
)

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
		if targetLower == "all" {
			if _, isPeer := s.companionByAddr(ct.Address); isPeer {
				matched = append(matched, ct)
			}
			continue
		}
		if s.contactMatches(ct, targetLower) {
			matched = append(matched, ct)
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
			label := s.contactName(ct)
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

// sendDeviceReply delivers a reply spoken at the device back into the Delta
// Chat chat its message was read from. The chat ID is captured at read-out so
// this keeps working for group chats and renamed contacts.
func (s *dcService) sendDeviceReply(ctx context.Context, companionID string, chatID uint32, text string) error {
	rpc := s.getRPC()
	if rpc == nil || chatID == 0 {
		return errors.New("Delta Chat unavailable")
	}
	accID := s.accountForCompanion(companionID)
	if accID == 0 {
		return errors.New("Companion has no Delta Chat account")
	}
	text = shortText(text, maxMessage, true)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	out := deltachat.MessageData{Text: &text}
	if _, err := rpc.SendMsg(accID, chatID, out); err != nil {
		return err
	}
	if s.b != nil {
		s.b.chat(companionID, newChat("companion", s.b.companionFor(companionID).Name, text, "deltachat"), true)
	}
	return nil
}

func (h *terminalHub) deliverDCIntercom(ctx context.Context, sender companionState, recipientID string, chatID uint32, text string, pcm []byte) {
	if h == nil || h.b == nil {
		return
	}
	n := intercomNotice(sender, []string{recipientID}, text, pcm, h.b.voiceFor(sender.ID))
	n.reply = &turnReply{name: sender.Name, companionID: recipientID, chatID: chatID}
	n.trace = traceFrom(ctx)
	h.deliverNotice(ctx, n)
	if text != "" {
		h.b.chat(recipientID, newChat("intercom", sender.Name, text, "intercom"), true)
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
