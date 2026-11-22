package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/chatmail/rpc-client-go/v2/deltachat"
)

const (
	dcPanelVersion     = "3"
	panelAppName       = "Lark Control"
	panelActiveTTL     = 5 * time.Minute
	panelPushThrottle  = 1 * time.Second
	panelWatchInterval = 750 * time.Millisecond
)

//go:embed webxdc/panel/*
var panelAssets embed.FS

//go:embed webxdc/story/*
var storyAssets embed.FS

type panelWatchKey struct {
	accID uint32
	msgID uint32
}

type panelWatch struct {
	companionID string
	lastActive  time.Time
	lastState   string
	lastPush    time.Time
}

func buildStoryXDC(twee string) ([]byte, error) {
	entries, err := fs.ReadDir(storyAssets, "webxdc/story")
	if err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int {
		return strings.Compare(a.Name(), b.Name())
	})
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasSuffix(name, ".test.mjs") {
			continue
		}
		data, err := storyAssets.ReadFile("webxdc/story/" + name)
		if err != nil {
			return nil, err
		}
		w, err := zw.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
	}
	w, err := zw.Create("story.twee")
	if err != nil {
		return nil, err
	}
	if _, err := w.Write([]byte(twee)); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func buildPanelXDC() ([]byte, error) {
	entries, err := fs.ReadDir(panelAssets, "webxdc/panel")
	if err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int {
		return strings.Compare(a.Name(), b.Name())
	})
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasSuffix(name, ".test.mjs") {
			continue
		}
		data, err := panelAssets.ReadFile("webxdc/panel/" + name)
		if err != nil {
			_ = zw.Close()
			return nil, err
		}
		hdr := &zip.FileHeader{
			Name:     name,
			Method:   zip.Deflate,
			Modified: time.Unix(0, 0).UTC(),
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			_ = zw.Close()
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			_ = zw.Close()
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type panelUpdateEnvelope struct {
	Serial  uint32          `json:"serial"`
	Payload json.RawMessage `json:"payload"`
}

type panelCommand struct {
	V        int    `json:"v"`
	Op       string `json:"op"`
	App      string `json:"app,omitempty"`
	Voice    string `json:"voice,omitempty"`
	Terminal string `json:"terminal,omitempty"`
	Volume   int    `json:"volume,omitempty"`
	Seconds  int    `json:"seconds,omitempty"`
	Text     string `json:"text,omitempty"`
	Channel  string `json:"channel,omitempty"`
	Q        string `json:"q,omitempty"`
	BeforeTS int64  `json:"before_ts,omitempty"`
	BeforeID string `json:"before_id,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

func (s *dcService) sendPanelXDC(ctx context.Context, c companionState, accID, chatID uint32, ct *deltachat.Contact) error {
	rpc := s.getRPC()
	if rpc == nil {
		return problem(503, "Delta Chat unavailable")
	}
	var prevMsgID uint32
	var contactKey string
	if ct != nil {
		contactKey = dcContactKey(*ct)
		if s.b != nil && s.b.db != nil {
			if raw, err := s.b.db.getMeta(ctx, "dc_panel_msg:"+c.ID+":"+contactKey); err == nil {
				if parsed, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 32); err == nil {
					prevMsgID = uint32(parsed)
				}
			}
		}
	}

	xdcBytes, err := buildPanelXDC()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "lark-panel-*.xdc")
	if err != nil {
		return err
	}
	path := tmp.Name()
	_, werr := tmp.Write(xdcBytes)
	cerr := tmp.Close()
	defer os.Remove(path)
	if werr != nil || cerr != nil {
		return fmt.Errorf("write panel xdc: %v / %v", werr, cerr)
	}
	vt := deltachat.ViewtypeWebxdc
	msgID, err := rpc.SendMsg(accID, chatID, deltachat.MessageData{
		File:     &path,
		Viewtype: &vt,
	})
	if err != nil {
		return err
	}
	if ct != nil && s.b != nil && s.b.db != nil {
		_ = s.b.db.setMeta(ctx, "dc_panel:"+c.ID+":"+contactKey, dcPanelVersion)
		_ = s.b.db.setMeta(ctx, "dc_panel_msg:"+c.ID+":"+contactKey, strconv.FormatUint(uint64(msgID), 10))

		var toDelete []uint32
		if prevMsgID > 0 && prevMsgID != msgID {
			toDelete = append(toDelete, prevMsgID)
		} else if prevMsgID == 0 {
			// First run after upgrade: find any older panel webxdc messages in this chat and clean them up.
			if mediaIDs, err := rpc.GetChatMedia(accID, &chatID, deltachat.ViewtypeWebxdc, nil, nil); err == nil {
				for _, mid := range mediaIDs {
					if mid == msgID {
						continue
					}
					if info, err := rpc.GetWebxdcInfo(accID, mid); err == nil && info.IsAppSender && info.Name == panelAppName {
						toDelete = append(toDelete, mid)
					}
				}
			}
		}
		if len(toDelete) > 0 {
			if err := rpc.DeleteMessagesForAll(accID, toDelete); err != nil {
				log.Printf("Delta Chat delete old panel error (%s): %v", c.ID, err)
			}
		}
	}
	s.touchActivePanel(accID, msgID, c.ID, "")
	s.pushPanelState(ctx, accID, msgID, c.ID, true)
	return nil
}

func (s *dcService) sendPanelIfNeeded(ctx context.Context, c companionState, accID uint32, ct deltachat.Contact) {
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
	key := "dc_panel:" + c.ID + ":" + dcContactKey(ct)
	if seen, err := s.b.db.getMeta(ctx, key); err == nil && seen == dcPanelVersion {
		return
	}
	chatID, err := rpc.CreateChatByContactId(accID, ct.Id)
	if err != nil {
		return
	}
	if err := s.sendPanelXDC(ctx, c, accID, chatID, &ct); err != nil {
		log.Printf("Delta Chat panel SendMsg error (%s): %v", c.ID, err)
	}
}

func (s *dcService) handleWebxdcUpdate(ctx context.Context, accID, msgID uint32) {
	rpc := s.getRPC()
	if rpc == nil || s.b == nil {
		return
	}
	companionID := s.companionForAccount(ctx, accID)
	if companionID == "" {
		return
	}
	msg, err := rpc.GetMessage(accID, msgID)
	if err != nil {
		return
	}
	chat, err := rpc.GetBasicChatInfo(accID, msg.ChatId)
	if err != nil || chat.ChatType != deltachat.ChatTypeSingle || chat.IsContactRequest {
		log.Printf("Ignoring webxdc update in non-single or request chat (acc=%d msg=%d)", accID, msgID)
		return
	}
	peerIDs, err := rpc.GetChatContacts(accID, msg.ChatId)
	if err != nil {
		return
	}
	var verifiedHuman bool
	senderName := "Friend"
	for _, cid := range peerIDs {
		if cid == deltachat.ContactSelf {
			continue
		}
		ct, err := rpc.GetContact(accID, cid)
		if err != nil || !ct.IsVerified || ct.IsBlocked || ct.IsBot {
			continue
		}
		if _, isPeer := s.companionByAddr(ct.Address); isPeer {
			continue
		}
		verifiedHuman = true
		if name := strings.TrimSpace(ct.DisplayName); name != "" {
			senderName = name
		} else if name := strings.TrimSpace(ct.Name); name != "" {
			senderName = name
		}
		break
	}
	if !verifiedHuman {
		log.Printf("Ignoring webxdc update from unverified or non-human sender (acc=%d msg=%d)", accID, msgID)
		return
	}

	s.touchActivePanel(accID, msgID, companionID, "")

	serialKey := fmt.Sprintf("dc_panel_serial:%d:%d", accID, msgID)
	var lastSerial uint32
	if s.b.db != nil {
		if raw, err := s.b.db.getMeta(ctx, serialKey); err == nil {
			if parsed, err := strconv.ParseUint(raw, 10, 32); err == nil {
				lastSerial = uint32(parsed)
			}
		}
	}
	rawUpdates, err := rpc.GetWebxdcStatusUpdates(accID, msgID, lastSerial)
	if err != nil || rawUpdates == "" {
		return
	}
	var updates []panelUpdateEnvelope
	if err := json.Unmarshal([]byte(rawUpdates), &updates); err != nil {
		return
	}
	for _, u := range updates {
		if u.Serial <= lastSerial {
			continue
		}
		lastSerial = u.Serial
		if s.b.db != nil {
			_ = s.b.db.setMeta(ctx, serialKey, strconv.FormatUint(uint64(lastSerial), 10))
		}
		var cmd panelCommand
		if err := json.Unmarshal(u.Payload, &cmd); err != nil || cmd.V != 1 {
			continue
		}
		if cmd.Op == "state" || cmd.Op == "ack" || cmd.Op == "live_state" || cmd.Op == "history" && cmd.Limit == -1 {
			continue
		}
		s.applyPanelCommand(ctx, accID, msgID, companionID, senderName, cmd)
	}
}

func (s *dcService) sayOnCompanion(ctx context.Context, companionID, senderName, words string) {
	if s.b == nil {
		return
	}
	who := s.b.companionFor(companionID)
	s.b.chat(companionID, newChat("user", senderName, "/say "+words, "deltachat"), true)
	if s.b.ws != nil {
		s.b.ws.deliverNotice(ctx, terminalNotice{
			recipientIDs: []string{companionID},
			sender:       who,
			text:         words,
			speakText:    words,
			voice:        s.b.voiceFor(companionID),
			chime:        "intercom",
			showCard:     true,
			replyTo:      senderName,
		})
	}
}

func (s *dcService) previewVoiceOnCompanion(companionID, voiceID string) {
	if s.b == nil || s.b.ws == nil {
		return
	}
	h := s.b.ws
	previewText := voicePreviewText(voiceID)
	h.mu.Lock()
	var targets []*wsTerminal
	for _, d := range h.terminalsFor(companionID) {
		if h.active(d) {
			targets = append(targets, d)
		}
	}
	if len(targets) == 0 {
		h.mu.Unlock()
		return
	}
	if pcm := s.b.clips.peek(voiceID, previewText); len(pcm) > 0 {
		for _, d := range targets {
			h.playPCM(d, pcm)
		}
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()

	if w := s.b.worker; w != nil && w.ready.Load() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), ttsWait)
			defer cancel()
			pcm, err := s.b.clips.get(ctx, w, voiceID, previewText)
			if err != nil || len(pcm) == 0 {
				return
			}
			h.mu.Lock()
			for _, d := range h.terminalsFor(companionID) {
				if h.active(d) && h.b.voiceFor(companionID) == voiceID {
					h.playPCM(d, pcm)
				}
			}
			h.mu.Unlock()
		}()
		return
	}
	h.mu.Lock()
	for _, d := range targets {
		if h.active(d) {
			h.chime(d, "tap")
		}
	}
	h.mu.Unlock()
}

func (b *larkServer) clearStoryProgress(ctx context.Context, companionID, storyID string) error {
	if b == nil || b.db == nil {
		return nil
	}
	_, err := b.db.ExecContext(ctx, `DELETE FROM story_progress WHERE companion_id=? AND story_id=?`, companionID, storyID)
	return err
}

func (b *larkServer) companionLiveState(companionID string) string {
	if b == nil || b.ws == nil {
		return "offline"
	}
	b.ws.mu.Lock()
	defer b.ws.mu.Unlock()
	terms := b.ws.terminalsFor(companionID)
	if len(terms) == 0 {
		return "offline"
	}
	best := b.ws.voiceOwner(companionID)
	if best == nil {
		best = terms[0]
	}
	switch {
	case best.asleep:
		return "sleeping"
	case best.recording.active():
		return "listening"
	case best.ttsStarted || best.awaitAck:
		return "speaking"
	case best.busy:
		return "thinking"
	case best.pose.mood == "listening" || best.pose.mood == "thinking" || best.pose.mood == "speaking" || best.pose.mood == "sleepy":
		if best.pose.mood == "sleepy" {
			return "sleeping"
		}
		return best.pose.mood
	default:
		return "idle"
	}
}

func (s *dcService) applyPanelCommand(ctx context.Context, accID, msgID uint32, companionID, senderName string, cmd panelCommand) {
	switch cmd.Op {
	case "sync":
		s.pushPanelState(ctx, accID, msgID, companionID, true)
	case "say":
		words := strings.TrimSpace(cmd.Text)
		if words == "" {
			s.pushPanelAck(accID, msgID, cmd.Op, false, "Enter something to say.")
			return
		}
		clean, err := spokenText(words)
		if err != nil {
			s.pushPanelAck(accID, msgID, cmd.Op, false, err.Error())
			return
		}
		s.sayOnCompanion(ctx, companionID, senderName, clean)
		s.pushPanelAck(accID, msgID, cmd.Op, true, "Said it.")
		s.pushPanelState(ctx, accID, msgID, companionID, false)
	case "open_book", "open_story":
		if strings.TrimSpace(cmd.App) == "" {
			s.pushPanelAck(accID, msgID, cmd.Op, false, "Missing story ID.")
			return
		}
		st, err := s.b.loadStory(ctx, cmd.App)
		if err != nil {
			s.pushPanelAck(accID, msgID, cmd.Op, false, "Story not found.")
			return
		}
		s.b.ws.mu.Lock()
		for _, d := range s.b.ws.terminalsFor(companionID) {
			if s.b.ws.active(d) {
				s.b.ws.openStory(d, st)
			}
		}
		s.b.ws.mu.Unlock()
		s.pushPanelAck(accID, msgID, cmd.Op, true, "Opened on device.")
		s.pushPanelState(ctx, accID, msgID, companionID, false)
	case "restart_story":
		if strings.TrimSpace(cmd.App) == "" {
			s.pushPanelAck(accID, msgID, cmd.Op, false, "Missing story ID.")
			return
		}
		st, err := s.b.loadStory(ctx, cmd.App)
		if err != nil {
			s.pushPanelAck(accID, msgID, cmd.Op, false, "Story not found.")
			return
		}
		if err := s.b.clearStoryProgress(ctx, companionID, st.ID); err != nil {
			s.pushPanelAck(accID, msgID, cmd.Op, false, "Could not reset progress.")
			return
		}
		s.b.ws.mu.Lock()
		for _, d := range s.b.ws.terminalsFor(companionID) {
			if s.b.ws.active(d) {
				s.b.ws.openStory(d, st)
			}
		}
		s.b.ws.mu.Unlock()
		s.pushPanelAck(accID, msgID, cmd.Op, true, "Started over on device.")
		s.pushPanelState(ctx, accID, msgID, companionID, false)
	case "set_voice":
		if !s.b.voices.allowed(cmd.Voice) {
			s.pushPanelAck(accID, msgID, cmd.Op, false, "Unknown voice.")
			return
		}
		if _, err := s.b.patchCompanion(ctx, companionID, object{"voice": cmd.Voice}); err != nil {
			s.pushPanelAck(accID, msgID, cmd.Op, false, err.Error())
			return
		}
		s.previewVoiceOnCompanion(companionID, cmd.Voice)
		s.pushPanelAck(accID, msgID, cmd.Op, true, "Voice updated.")
		s.pushPanelState(ctx, accID, msgID, companionID, false)
	case "set_volume":
		if cmd.Volume < 0 || cmd.Volume > 100 || strings.TrimSpace(cmd.Terminal) == "" {
			s.pushPanelAck(accID, msgID, cmd.Op, false, "Invalid volume.")
			return
		}
		if err := s.b.setCompanionTerminalVolume(ctx, companionID, cmd.Terminal, cmd.Volume); err != nil {
			s.pushPanelAck(accID, msgID, cmd.Op, false, err.Error())
			return
		}
		s.pushPanelAck(accID, msgID, cmd.Op, true, fmt.Sprintf("Volume set to %d%%.", cmd.Volume))
		s.pushPanelState(ctx, accID, msgID, companionID, false)
	case "set_listen":
		if !slices.Contains([]int{0, 30, 60, 120, 300}, cmd.Seconds) {
			s.pushPanelAck(accID, msgID, cmd.Op, false, "Invalid listening duration.")
			return
		}
		if _, err := s.b.patchCompanion(ctx, companionID, object{"listen_seconds": cmd.Seconds}); err != nil {
			s.pushPanelAck(accID, msgID, cmd.Op, false, err.Error())
			return
		}
		s.pushPanelAck(accID, msgID, cmd.Op, true, "Listening mode updated.")
		s.pushPanelState(ctx, accID, msgID, companionID, false)
	case "history":
		s.pushPanelHistory(ctx, accID, msgID, companionID, cmd)
	}
}

func (s *dcService) pushPanelAck(accID, msgID uint32, reqOp string, ok bool, message string) {
	rpc := s.getRPC()
	if rpc == nil {
		return
	}
	data, err := json.Marshal(object{
		"payload": object{
			"v":       1,
			"op":      "ack",
			"req_op":  reqOp,
			"ok":      ok,
			"message": message,
		},
	})
	if err != nil {
		return
	}
	_ = rpc.SendWebxdcStatusUpdate(accID, msgID, string(data), nil)
}

func (s *dcService) pushPanelLiveState(accID, msgID uint32, liveState string) {
	rpc := s.getRPC()
	if rpc == nil {
		return
	}
	data, err := json.Marshal(object{
		"payload": object{
			"v":          1,
			"op":         "live_state",
			"live_state": liveState,
		},
	})
	if err != nil {
		return
	}
	_ = rpc.SendWebxdcStatusUpdate(accID, msgID, string(data), nil)
}

func (s *dcService) touchActivePanel(accID, msgID uint32, companionID, liveState string) {
	if s == nil {
		return
	}
	key := panelWatchKey{accID: accID, msgID: msgID}
	now := time.Now()
	s.mu.Lock()
	if s.activePanels == nil {
		s.activePanels = make(map[panelWatchKey]*panelWatch)
	}
	entry := s.activePanels[key]
	if entry == nil {
		entry = &panelWatch{companionID: companionID}
		s.activePanels[key] = entry
	}
	entry.companionID = companionID
	entry.lastActive = now
	if liveState != "" {
		entry.lastState = liveState
		entry.lastPush = now
	}
	startWatcher := !s.panelWatchRunning
	if startWatcher {
		s.panelWatchRunning = true
	}
	s.mu.Unlock()
	if startWatcher {
		go s.runPanelWatch()
	}
}

func (s *dcService) runPanelWatch() {
	ticker := time.NewTicker(panelWatchInterval)
	defer ticker.Stop()
	defer func() {
		s.mu.Lock()
		s.panelWatchRunning = false
		s.mu.Unlock()
	}()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
		}
		if s.b == nil || s.getRPC() == nil {
			return
		}
		now := time.Now()
		type pendingPush struct {
			accID uint32
			msgID uint32
			state string
		}
		var pushes []pendingPush
		s.mu.Lock()
		for k, w := range s.activePanels {
			if now.Sub(w.lastActive) > panelActiveTTL {
				delete(s.activePanels, k)
				continue
			}
			cur := s.b.companionLiveState(w.companionID)
			if cur != w.lastState && now.Sub(w.lastPush) >= panelPushThrottle {
				w.lastState = cur
				w.lastPush = now
				pushes = append(pushes, pendingPush{accID: k.accID, msgID: k.msgID, state: cur})
			}
		}
		empty := len(s.activePanels) == 0
		s.mu.Unlock()
		for _, p := range pushes {
			s.pushPanelLiveState(p.accID, p.msgID, p.state)
		}
		if empty {
			return
		}
	}
}

func (s *dcService) pushPanelState(ctx context.Context, accID, msgID uint32, companionID string, includePortrait bool) {
	rpc := s.getRPC()
	if rpc == nil || s.b == nil {
		return
	}
	c := s.b.companionFor(companionID)
	liveState := s.b.companionLiveState(companionID)
	compObj := object{
		"id":             c.ID,
		"name":           c.Name,
		"form":           c.Form,
		"voice":          s.b.voiceFor(c.ID),
		"listen_seconds": c.ListenSeconds,
		"live_state":     liveState,
	}
	if includePortrait {
		if pngBytes, err := companionAvatarPNG(s.b, c); err == nil {
			compObj["portrait"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes)
		}
	}

	var termList []object
	seenTerms := make(map[string]bool)
	if dbTerms, err := s.b.terminalsForCompanion(companionID); err == nil {
		for _, dt := range dbTerms {
			te := s.b.terminalEntry(dt, true)
			entry := object{
				"id":     dt.ID,
				"name":   dt.ID,
				"online": te["online"],
				"volume": te["volume"],
			}
			if pct, ok := te["battery_pct"]; ok {
				entry["battery_pct"] = pct
			}
			if chg, ok := te["charging"]; ok {
				entry["charging"] = chg
			}
			termList = append(termList, entry)
			seenTerms[strings.ToLower(dt.ID)] = true
		}
	}
	if s.b.ws != nil {
		for _, t := range s.b.ws.companionTerminals(companionID) {
			tid := strings.ToLower(str(t["id"]))
			if !seenTerms[tid] {
				termList = append(termList, t)
			}
		}
	}

	progressByStory := make(map[string]storyProgress)
	if plist, err := s.b.listStoryProgress(ctx, companionID); err == nil {
		for _, p := range plist {
			progressByStory[p.StoryID] = p
		}
	}

	var stories []object
	if list, err := s.b.listStories(ctx); err == nil {
		for _, st := range list {
			item := object{
				"name":     st.ID,
				"title":    st.Title,
				"passages": st.Passages,
				"progress": 0,
			}
			if prog, ok := progressByStory[st.ID]; ok {
				pct := 10
				if cs, err := parseTwee(st.Twee); err == nil && len(cs.Passages) > 0 {
					if idx, exists := cs.ByName[prog.Passage]; exists {
						if cs.Passages[idx].End {
							pct = 100
						} else {
							pct = max(5, min(95, (idx+1)*100/len(cs.Passages)))
						}
					}
				}
				item["progress"] = pct
				item["passage"] = prog.Passage
			}
			stories = append(stories, item)
		}
	}

	var voices []object
	for _, v := range voiceCatalog {
		voices = append(voices, object{"id": v.ID, "name": v.Name})
	}

	data, err := json.Marshal(object{
		"payload": object{
			"v":         1,
			"op":        "state",
			"companion": compObj,
			"terminals": termList,
			"stories":   stories,
			"voices":    voices,
		},
	})
	if err != nil {
		return
	}
	_ = rpc.SendWebxdcStatusUpdate(accID, msgID, string(data), nil)
	s.touchActivePanel(accID, msgID, companionID, liveState)
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func historySpeaker(role, sender, channel, companionName string) string {
	switch role {
	case "companion":
		if companionName != "" {
			return companionName
		}
		if sender != "" {
			return sender
		}
		return "Companion"
	case "user":
		if channel == "in_person" || sender == "" || sender == "You" {
			return "Someone in person"
		}
		return sender
	case "intercom":
		if sender != "" {
			return sender
		}
		return "Intercom"
	case "card":
		if companionName != "" {
			return companionName
		}
		return sender
	default:
		if sender != "" {
			return sender
		}
		return role
	}
}

func (s *dcService) pushPanelHistory(ctx context.Context, accID, msgID uint32, companionID string, cmd panelCommand) {
	rpc := s.getRPC()
	if rpc == nil || s.b == nil || s.b.db == nil {
		return
	}
	if s.b.history != nil {
		s.b.history.flush(companionID)
	}
	limit := cmd.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	ch := strings.TrimSpace(cmd.Channel)
	switch ch {
	case "", "all", "in_person", "deltachat", "intercom":
	default:
		s.pushPanelAck(accID, msgID, "history", false, "Unknown history filter.")
		return
	}
	q := strings.TrimSpace(cmd.Q)
	if len(q) > 100 {
		q = q[:100]
	}

	query := `SELECT id,role,sender,text,source,channel,ts_ms FROM messages WHERE companion_id=?`
	args := []any{strings.ToLower(companionID)}
	switch ch {
	case "in_person":
		query += ` AND (channel='in_person' OR (channel='' AND source IN ('voice','typed','tap','text')))`
	case "deltachat":
		query += ` AND (channel='deltachat' OR (channel='' AND source='deltachat'))`
	case "intercom":
		query += ` AND (channel='intercom' OR (channel='' AND source='intercom'))`
	}
	if q != "" {
		pat := "%" + escapeLike(strings.ToLower(q)) + "%"
		query += ` AND (LOWER(text) LIKE ? ESCAPE '\' OR LOWER(sender) LIKE ? ESCAPE '\')`
		args = append(args, pat, pat)
	}
	appendMode := cmd.BeforeTS > 0 && cmd.BeforeID != ""
	if appendMode {
		query += ` AND ts_ms<=? AND (ts_ms<? OR (ts_ms=? AND id<?))`
		args = append(args, cmd.BeforeTS, cmd.BeforeTS, cmd.BeforeTS, cmd.BeforeID)
	}
	query += ` ORDER BY ts_ms DESC, id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.b.db.readDB().QueryContext(ctx, query, args...)
	if err != nil {
		s.pushPanelAck(accID, msgID, "history", false, "Could not load history.")
		return
	}
	defer rows.Close()

	c := s.b.companionFor(companionID)
	items := []object{}
	for rows.Next() {
		var id, role, sender, text, source, rowCh string
		var ts int64
		if err := rows.Scan(&id, &role, &sender, &text, &source, &rowCh, &ts); err != nil {
			s.pushPanelAck(accID, msgID, "history", false, "Could not load history.")
			return
		}
		if rowCh == "" {
			rowCh = channelForSource(source)
		}
		clean := strings.TrimSpace(stripLarkHeader(text))
		items = append(items, object{
			"id":      id,
			"role":    role,
			"speaker": historySpeaker(role, sender, rowCh, c.Name),
			"channel": rowCh,
			"text":    clean,
			"ts_ms":   ts,
		})
	}
	if err := rows.Err(); err != nil {
		s.pushPanelAck(accID, msgID, "history", false, "Could not load history.")
		return
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	payload := object{
		"v":        1,
		"op":       "history",
		"items":    items,
		"has_more": hasMore,
		"append":   appendMode,
		"channel":  ch,
		"q":        q,
	}
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		payload["next_cursor"] = object{
			"ts_ms": last["ts_ms"],
			"id":    last["id"],
		}
	}
	data, err := json.Marshal(object{"payload": payload})
	if err != nil {
		return
	}
	_ = rpc.SendWebxdcStatusUpdate(accID, msgID, string(data), nil)
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
	var lines []string
	lines = append(lines, "Stories:")
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
	xdcBytes, err := buildStoryXDC(found.Twee)
	if err != nil {
		msg := "Could not package story player."
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

	var newIDs []uint32
	tweePath := filepath.Join(tmpDir, found.ID+".twee")
	if err := os.WriteFile(tweePath, []byte(found.Twee), 0o600); err == nil {
		vtFile := deltachat.ViewtypeFile
		caption := found.Title + " (.twee)"
		if mid, err := rpc.SendMsg(accID, chatID, deltachat.MessageData{Text: &caption, File: &tweePath, Viewtype: &vtFile}); err == nil && mid > 0 {
			newIDs = append(newIDs, mid)
		}
	}

	xdcPath := filepath.Join(tmpDir, found.ID+".xdc")
	if err := os.WriteFile(xdcPath, xdcBytes, 0o600); err == nil {
		vtXDC := deltachat.ViewtypeWebxdc
		caption := found.Title
		if mid, err := rpc.SendMsg(accID, chatID, deltachat.MessageData{Text: &caption, File: &xdcPath, Viewtype: &vtXDC}); err == nil && mid > 0 {
			newIDs = append(newIDs, mid)
		}
	}

	if s.b != nil && s.b.db != nil && len(newIDs) > 0 {
		parts := make([]string, len(newIDs))
		for i, id := range newIDs {
			parts[i] = strconv.FormatUint(uint64(id), 10)
		}
		_ = s.b.db.setMeta(ctx, metaKey, strings.Join(parts, ","))
	}
	var toDelete []uint32
	for _, oldID := range oldIDs {
		if !slices.Contains(newIDs, oldID) {
			toDelete = append(toDelete, oldID)
		}
	}
	if len(toDelete) > 0 {
		if err := rpc.DeleteMessagesForAll(accID, toDelete); err != nil {
			log.Printf("Delta Chat delete old story messages error (%s/%s): %v", companionID, found.ID, err)
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
