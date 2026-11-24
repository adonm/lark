package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func (b *larkServer) serveRemoteAPI(w http.ResponseWriter, r *http.Request, path string, tok remoteToken, raw string) {
	switch {
	case path == remotePath+"/api/capture" && r.Method == http.MethodPost:
		value, err := b.remoteCaptureStart(r, tok.CompanionID)
		sendResult(w, value, err)
	case path == remotePath+"/api/captures" && r.Method == http.MethodGet:
		value, err := b.remoteCaptureList(tok.CompanionID)
		sendResult(w, value, err)
	case strings.HasPrefix(path, remotePath+"/api/captures/"):
		name := strings.TrimPrefix(path, remotePath+"/api/captures/")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(name, "/transcribe"):
			value, err := b.remoteCaptureTranscribe(r, tok.CompanionID, strings.TrimSuffix(name, "/transcribe"))
			sendResult(w, value, err)
		case r.Method == http.MethodGet && strings.HasSuffix(name, ".wav"):
			b.remoteCaptureWAV(w, r, tok.CompanionID, strings.TrimSuffix(name, ".wav"))
		case r.Method == http.MethodDelete:
			b.remoteCaptureDelete(w, tok.CompanionID, name)
		default:
			sendError(w, problem(404, "Not found"))
		}
	case path == remotePath+"/api/history" && r.Method == http.MethodGet:
		page, err := b.remoteHistory(r.Context(), tok, r.URL.Query())
		sendResult(w, page, err)
	case path == remotePath+"/api/say" && r.Method == http.MethodPost:
		value, err := b.remoteSay(r, tok.CompanionID)
		sendResult(w, value, err)
	case path == remotePath+"/api/listen" && r.Method == http.MethodPost:
		value, err := b.remoteListen(r, tok.CompanionID)
		sendResult(w, value, err)
	case path == remotePath+"/api/volume" && r.Method == http.MethodPost:
		value, err := b.remoteVolume(r, tok.CompanionID)
		sendResult(w, value, err)
	case path == remotePath+"/api/voice" && r.Method == http.MethodPost:
		value, err := b.remoteVoice(r, tok.CompanionID)
		sendResult(w, value, err)
	case path == remotePath+"/api/story/open" && r.Method == http.MethodPost:
		value, err := b.remoteStory(r, tok.CompanionID, false)
		sendResult(w, value, err)
	case path == remotePath+"/api/story/restart" && r.Method == http.MethodPost:
		value, err := b.remoteStory(r, tok.CompanionID, true)
		sendResult(w, value, err)
	case path == remotePath+"/api/revoke" && r.Method == http.MethodPost:
		_, err := b.remote.revokeContact(r.Context(), tok.CompanionID, tok.Contact)
		if err == nil {
			http.SetCookie(w, &http.Cookie{Name: remoteCookieName, Value: "", Path: remotePath, MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
			sendJSON(w, 200, object{"ok": true, "message": "Remote controls off."})
			return
		}
		sendError(w, err)
	default:
		sendError(w, problem(404, "Not found"))
	}
}

func remoteStateResponse(b *larkServer, ctx context.Context, companionID, message string) object {
	return object{"ok": true, "message": message, "state": b.remoteSnapshot(ctx, companionID, false)}
}

func (b *larkServer) remoteSay(r *http.Request, companionID string) (any, error) {
	var req remoteSayRequest
	if err := decodeRequest(r, &req); err != nil {
		return nil, err
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return nil, problem(400, "Enter something to say")
	}
	clean, err := spokenText(text)
	if err != nil {
		return nil, problem(400, err.Error())
	}
	b.sayOnCompanion(r.Context(), companionID, "Remote", 0, clean)
	return remoteStateResponse(b, r.Context(), companionID, "Said it."), nil
}

func (b *larkServer) remoteListen(r *http.Request, companionID string) (any, error) {
	var req remoteListenRequest
	if err := decodeRequest(r, &req); err != nil {
		return nil, err
	}
	if req.Listening == nil {
		return nil, problem(400, "Expected listening to be true or false")
	}
	if err := b.setListening(r.Context(), companionID, *req.Listening); err != nil {
		return nil, err
	}
	return remoteStateResponse(b, r.Context(), companionID, "Listening mode updated."), nil
}

func (b *larkServer) remoteVolume(r *http.Request, companionID string) (any, error) {
	var req remoteVolumeRequest
	if err := decodeRequest(r, &req); err != nil {
		return nil, err
	}
	terminalID := strings.ToLower(strings.TrimSpace(req.Terminal))
	if terminalID == "" {
		return nil, problem(400, "Terminal is required")
	}
	t, ok := b.terminalByID(terminalID)
	if !ok {
		return nil, problem(404, "Terminal not found")
	}
	if !strings.EqualFold(t.CompanionID, companionID) {
		return nil, problem(403, "Terminal out of scope")
	}
	if req.Volume == nil {
		return nil, problem(400, `Expected {"volume":0..100}`)
	}
	if _, err := b.setVolume(r.Context(), terminalID, *req.Volume); err != nil {
		return nil, err
	}
	return remoteStateResponse(b, r.Context(), companionID, "Volume updated."), nil
}

func (b *larkServer) remoteVoice(r *http.Request, companionID string) (any, error) {
	var req remoteVoiceRequest
	if err := decodeRequest(r, &req); err != nil {
		return nil, err
	}
	if err := b.setVoice(r.Context(), companionID, req.Voice, true); err != nil {
		return nil, err
	}
	b.previewVoiceOnCompanion(companionID, req.Voice)
	return remoteStateResponse(b, r.Context(), companionID, "Voice updated."), nil
}

func (b *larkServer) remoteStory(r *http.Request, companionID string, restart bool) (any, error) {
	var req remoteStoryRequest
	if err := decodeRequest(r, &req); err != nil {
		return nil, err
	}
	storyID := strings.TrimSpace(req.Story)
	if storyID == "" {
		return nil, problem(400, "Missing story ID")
	}
	if restart {
		if _, err := b.restartStory(r.Context(), companionID, storyID); err != nil {
			return nil, err
		}
		return remoteStateResponse(b, r.Context(), companionID, "Started over on device."), nil
	}
	if _, err := b.openStory(r.Context(), companionID, storyID); err != nil {
		return nil, err
	}
	return remoteStateResponse(b, r.Context(), companionID, "Opened on device."), nil
}

func encodeHistoryCursor(c historyCursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(c.TS, 10) + ":" + c.ID))
}

func decodeHistoryCursor(raw string) (*historyCursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	tsRaw, id, ok := strings.Cut(string(data), ":")
	if !ok || id == "" {
		return nil, fmt.Errorf("invalid cursor")
	}
	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return nil, err
	}
	return &historyCursor{TS: ts, ID: id}, nil
}

func (b *larkServer) remoteHistory(ctx context.Context, tok remoteToken, query url.Values) (object, error) {
	if b.db == nil {
		return nil, problem(503, "History unavailable")
	}
	companionID := tok.CompanionID
	if b.history != nil {
		b.history.flush(companionID)
	}
	limit := remoteHistoryLimit
	if raw := query.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return nil, problem(400, "Expected a positive history limit")
		}
		limit = min(n, remoteHistoryLimitMax)
	}
	channel := strings.TrimSpace(query.Get("channel"))
	switch channel {
	case "", "all", "in_person", "deltachat":
	default:
		return nil, problem(400, "Unknown history filter")
	}
	q := strings.TrimSpace(query.Get("q"))
	if len(q) > remoteHistoryQueryMax {
		q = q[:remoteHistoryQueryMax]
	}
	var cursor *historyCursor
	if raw := query.Get("cursor"); raw != "" {
		var err error
		cursor, err = decodeHistoryCursor(raw)
		if err != nil {
			return nil, problem(400, "Invalid history cursor")
		}
	}

	// A scoped link shows in-person notes and only the Delta Chat messages
	// this contact sent. Companion replies and intercom traffic stay private.
	querySQL := `SELECT id,role,sender,text,channel,ts_ms FROM messages WHERE companion_id=?`
	args := []any{strings.ToLower(companionID)}
	switch channel {
	case "in_person":
		querySQL += ` AND channel='in_person'`
	case "deltachat":
		querySQL += ` AND channel='deltachat' AND role='user' AND LOWER(sender) IN (?,?)`
		args = append(args, tok.Contact, strings.ToLower(strings.TrimSpace(tok.ContactName)))
	default:
		querySQL += ` AND (channel='in_person' OR (channel='deltachat' AND role='user' AND LOWER(sender) IN (?,?)))`
		args = append(args, tok.Contact, strings.ToLower(strings.TrimSpace(tok.ContactName)))
	}
	if q != "" {
		pat := "%" + escapeLike(strings.ToLower(q)) + "%"
		querySQL += ` AND (LOWER(text) LIKE ? ESCAPE '\' OR LOWER(sender) LIKE ? ESCAPE '\')`
		args = append(args, pat, pat)
	}
	if cursor != nil {
		querySQL += ` AND ts_ms<=? AND (ts_ms<? OR (ts_ms=? AND id<?))`
		args = append(args, cursor.TS, cursor.TS, cursor.TS, cursor.ID)
	}
	querySQL += ` ORDER BY ts_ms DESC,id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := b.db.readDB().QueryContext(ctx, querySQL, args...)
	if err != nil {
		return nil, problem(502, "Cannot read history")
	}
	defer rows.Close()
	c := b.companionFor(companionID)
	items := []object{}
	for rows.Next() {
		var id, role, sender, text, rowChannel string
		var ts int64
		if err := rows.Scan(&id, &role, &sender, &text, &rowChannel, &ts); err != nil {
			return nil, problem(502, "Cannot read history")
		}
		items = append(items, object{
			"id":      id,
			"role":    role,
			"speaker": historySpeaker(role, sender, rowChannel, c.Name),
			"channel": rowChannel,
			"text":    strings.TrimSpace(stripLarkHeader(text)),
			"ts_ms":   ts,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, problem(502, "Cannot read history")
	}
	out := object{"items": items, "has_more": false}
	if len(items) > limit {
		items = items[:limit]
		out["items"] = items
		out["has_more"] = true
		oldest := items[len(items)-1]
		ts, _ := oldest["ts_ms"].(int64)
		out["next_cursor"] = encodeHistoryCursor(historyCursor{TS: ts, ID: str(oldest["id"])})
	}
	return out, nil
}

// sayOnCompanion and previewVoiceOnCompanion are shared by the Delta Chat
// commands and the remote page.
func (b *larkServer) sayOnCompanion(ctx context.Context, companionID, senderName string, chatID uint32, words string) {
	if b == nil {
		return
	}
	who := b.companionFor(companionID)
	b.chat(companionID, newChat("user", senderName, "/say "+words, "deltachat"), true)
	if b.hub != nil {
		n := intercomNotice(who, []string{companionID}, words, nil, b.voiceFor(companionID))
		n.intro = ""
		if chatID != 0 {
			n.reply = &turnReply{name: senderName, companionID: companionID, chatID: chatID}
			n.cardReply = senderName
		}
		b.hub.deliverNotice(ctx, n)
	}
}

func (b *larkServer) previewVoiceOnCompanion(companionID, voiceID string) {
	if b == nil || b.hub == nil {
		return
	}
	h := b.hub
	previewText := voicePreviewText(voiceID)
	h.mu.Lock()
	var targets []*terminalSession
	for _, d := range h.terminalsFor(companionID) {
		if h.active(d) {
			targets = append(targets, d)
		}
	}
	if len(targets) == 0 {
		h.mu.Unlock()
		return
	}
	if pcm := b.clips.peek(voiceID, previewText); len(pcm) > 0 {
		for _, d := range targets {
			h.playPCM(d, pcm)
		}
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()

	if w := b.worker; w != nil && w.ready.Load() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), ttsWait)
			defer cancel()
			pcm, err := b.clips.get(ctx, w, voiceID, previewText)
			if err != nil || len(pcm) == 0 {
				return
			}
			h.mu.Lock()
			for _, d := range h.terminalsFor(companionID) {
				if h.active(d) && b.voiceFor(companionID) == voiceID {
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
