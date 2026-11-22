package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/chatmail/rpc-client-go/v2/deltachat"
)

const dcPanelVersion = "1"

//go:embed webxdc/panel/*
var panelAssets embed.FS

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
}

func (s *dcService) sendPanelXDC(ctx context.Context, c companionState, accID, chatID uint32, ct *deltachat.Contact) error {
	rpc := s.getRPC()
	if rpc == nil {
		return problem(503, "Delta Chat unavailable")
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
		_ = s.b.db.setMeta(ctx, "dc_panel:"+c.ID+":"+dcContactKey(*ct), dcPanelVersion)
	}
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
		break
	}
	if !verifiedHuman {
		log.Printf("Ignoring webxdc update from unverified or non-human sender (acc=%d msg=%d)", accID, msgID)
		return
	}

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
		if cmd.Op == "state" || cmd.Op == "ack" {
			continue
		}
		s.applyPanelCommand(ctx, accID, msgID, companionID, cmd)
	}
}

func (s *dcService) applyPanelCommand(ctx context.Context, accID, msgID uint32, companionID string, cmd panelCommand) {
	switch cmd.Op {
	case "sync":
		s.pushPanelState(ctx, accID, msgID, companionID, true)
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
	case "set_voice":
		if !s.b.voices.allowed(cmd.Voice) {
			s.pushPanelAck(accID, msgID, cmd.Op, false, "Unknown voice.")
			return
		}
		if _, err := s.b.patchCompanion(ctx, companionID, object{"voice": cmd.Voice}); err != nil {
			s.pushPanelAck(accID, msgID, cmd.Op, false, err.Error())
			return
		}
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

func (s *dcService) pushPanelState(ctx context.Context, accID, msgID uint32, companionID string, includePortrait bool) {
	rpc := s.getRPC()
	if rpc == nil || s.b == nil {
		return
	}
	c := s.b.companionFor(companionID)
	compObj := object{
		"id":             c.ID,
		"name":           c.Name,
		"form":           c.Form,
		"voice":          s.b.voiceFor(c.ID),
		"listen_seconds": c.ListenSeconds,
	}
	if includePortrait {
		if pngBytes, err := companionAvatarPNG(s.b, c); err == nil {
			compObj["portrait"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes)
		}
	}

	var termList []object
	if s.b.ws != nil {
		for _, t := range s.b.ws.companionTerminals(companionID) {
			termList = append(termList, t)
		}
	}

	var books, games []object
	if stories, err := s.b.listStories(ctx); err == nil {
		for _, st := range stories {
			books = append(books, object{"name": st.ID, "title": st.Title})
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
			"books":     books,
			"games":     games,
			"voices":    voices,
		},
	})
	if err != nil {
		return
	}
	_ = rpc.SendWebxdcStatusUpdate(accID, msgID, string(data), nil)
}
