package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

func stripLarkHeader(text string) string {
	first, rest, ok := strings.Cut(text, "\n")
	if ok && strings.HasPrefix(first, "[") && strings.HasSuffix(strings.TrimSuffix(first, "\r"), "]") {
		if strings.HasPrefix(first, "[Lark: ") || strings.Contains(first, ") · hardware · ") || strings.Contains(first, ") · web · ") {
			return rest
		}
	}
	return text
}

type chatEntry struct {
	ID          string    `json:"id"`
	Role        string    `json:"role"`
	Sender      string    `json:"sender"`
	Text        string    `json:"text"`
	Timestamp   time.Time `json:"timestamp"`
	Source      string    `json:"source"`
	SceneTitle  string    `json:"card_title,omitempty"`
	ToolSummary string    `json:"-"`
}

func newChat(role, sender, text, source string) chatEntry {
	return chatEntry{ID: "bridge-" + randomHex(12), Role: role, Sender: sender, Text: text,
		Source: source, Timestamp: time.Now().UTC()}
}

type chatHistory struct{ db *larkDB }

type historyCursor struct {
	TS int64  `json:"before_ts"`
	ID string `json:"before_id"`
}

type historyPage struct {
	CompanionID string         `json:"companion_id"`
	Messages    []chatEntry    `json:"messages"`
	HasMore     bool           `json:"has_more"`
	Next        *historyCursor `json:"next_cursor,omitempty"`
}

const insertMessage = `INSERT INTO messages (id,companion_id,role,sender,text,source,card_title,ts_ms)
	VALUES (?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`

func canonicalMessage(e chatEntry) (chatEntry, bool) {
	e.Text = strings.TrimSpace(stripLarkHeader(e.Text))
	switch e.Role {
	case "user":
		if e.Source == "timer" {
			return e, false
		}
	case "assistant", "companion":
		e.Role = "companion"
		e.Text = strings.TrimSpace(fenced.ReplaceAllString(e.Text, ""))
	case "intercom", "card":
	case "system":
		e.Role = "card"
		if e.Source != "timer" {
			if e.SceneTitle == "" || e.Source != "mcp" {
				return e, false
			}
			e.Text = "Drew: " + e.SceneTitle
		}
	default:
		return e, false
	}
	if e.Role != "card" {
		e.SceneTitle = ""
	}
	e.ToolSummary = ""
	return e, e.ID != "" && e.Text != ""
}

func messageValues(id string, e chatEntry) []any {
	return []any{e.ID, strings.ToLower(id), e.Role, e.Sender, e.Text, e.Source, e.SceneTitle, e.Timestamp.UnixMilli()}
}

func (h *chatHistory) add(id string, e chatEntry) {
	if e, ok := canonicalMessage(e); ok {
		if _, err := h.db.Exec(insertMessage, messageValues(id, e)...); err != nil {
			log.Printf("Lark could not persist message: %v", err)
		}
	}
}

func (h *chatHistory) list(ctx context.Context, id string) ([]chatEntry, error) {
	return h.read(ctx, id, 0)
}

func (h *chatHistory) read(ctx context.Context, id string, limit int) ([]chatEntry, error) {
	return h.readBefore(ctx, id, limit, nil)
}

func (h *chatHistory) readBefore(ctx context.Context, id string, limit int, before *historyCursor) ([]chatEntry, error) {
	query := `SELECT id,role,sender,text,source,card_title,ts_ms FROM messages WHERE companion_id=?`
	args := []any{strings.ToLower(id)}
	if before != nil {
		query += ` AND ts_ms<=? AND (ts_ms<? OR id<?)`
		args = append(args, before.TS, before.TS, before.ID)
	}
	if limit > 0 {
		query += ` ORDER BY ts_ms DESC,id DESC LIMIT ?`
		args = append(args, limit)
	} else {
		query += ` ORDER BY ts_ms,id`
	}
	rows, err := h.db.readDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, problem(502, "Cannot read chat events")
	}
	defer rows.Close()
	entries := []chatEntry{}
	for rows.Next() {
		var e chatEntry
		var ts int64
		if err := rows.Scan(&e.ID, &e.Role, &e.Sender, &e.Text, &e.Source, &e.SceneTitle, &ts); err != nil {
			return nil, problem(502, "Cannot read chat events")
		}
		e.Timestamp = time.UnixMilli(ts).UTC()
		entries = append(entries, e)
	}
	if rows.Err() != nil {
		return nil, problem(502, "Cannot read chat events")
	}
	if limit > 0 {
		slices.Reverse(entries)
	}
	return entries, nil
}

func (b *larkServer) chat(id string, entry chatEntry, persist bool) {
	entry, ok := b.recordChat(id, entry, persist)
	if ok && b.ws != nil {
		b.ws.mu.Lock()
		defer b.ws.mu.Unlock()
		b.ws.broadcastChat(id, entry)
	}
}

func (b *larkServer) recordChat(id string, entry chatEntry, persist bool) (chatEntry, bool) {
	entry, ok := canonicalMessage(entry)
	if ok && persist && b.history != nil {
		b.history.add(id, entry)
	}
	return entry, ok
}

func (h *wsHub) broadcastChat(id string, entry chatEntry) {
	f := textFrame(object{"type": "chat", "companion_id": id, "entry": entry})
	key := strings.ToLower(id)
	for c := range h.clients {
		if c.watch == key || c.terminal != nil && c.terminal.companionID == key && c.terminal.virtual() {
			c.enqueue(f)
		}
	}
}

func (b *larkServer) messages(r *http.Request, id string) (any, error) {
	id = strings.ToLower(id)
	if !validCompanionID(id) {
		return nil, problem(400, "Invalid companion ID")
	}
	query := r.URL.Query()
	limit := 100
	if query.Has("limit") {
		n, err := strconv.Atoi(query.Get("limit"))
		if err != nil || n < 1 {
			return nil, problem(400, "Expected a positive history limit")
		}
		limit = min(n, 500)
	}
	var before *historyCursor
	if query.Has("before_ts") || query.Has("before_id") {
		ts, err := strconv.ParseInt(query.Get("before_ts"), 10, 64)
		key := query.Get("before_id")
		if err != nil || key == "" || len(key) > 512 {
			return nil, problem(400, "Expected before_ts (Unix milliseconds) and before_id")
		}
		before = &historyCursor{TS: ts, ID: key}
	}
	return b.readHistoryPage(r.Context(), id, limit, before)
}

func (b *larkServer) readHistoryPage(ctx context.Context, id string, limit int, before *historyCursor) (historyPage, error) {
	out := historyPage{CompanionID: id, Messages: []chatEntry{}}
	if b.history == nil {
		return out, nil
	}
	entries, err := b.history.readBefore(ctx, id, limit+1, before)
	if err != nil {
		return out, err
	}
	out.HasMore = len(entries) > limit
	if out.HasMore {
		entries = entries[1:] // The extra row is the oldest.
		out.Next = &historyCursor{TS: entries[0].Timestamp.UnixMilli(), ID: entries[0].ID}
	}
	out.Messages = entries
	return out, nil
}

func (b *larkServer) chatAPI(r *http.Request, scope string) (any, error) {
	obj, err := jsonBody(r)
	if err != nil {
		return nil, err
	}
	if scope != "" && !strings.EqualFold(str(obj["companion_id"]), scope) {
		return nil, problem(403, "Companion out of scope")
	}
	id, err := companionIDArg(r, obj, true)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), turnTimeout)
	defer cancel()
	text := strings.TrimSpace(str(obj["text"]))
	if text == "" || len(text) > 4096 {
		return nil, problem(400, "Expected nonempty text of at most 4096 bytes")
	}
	sender, ok := b.companionByID(id)
	if !ok {
		return nil, problem(404, "Companion not found")
	}
	minutes := childPauseMinutes(text)
	if b.ws != nil {
		b.ws.mu.Lock()
		key, now := strings.ToLower(id), time.Now()
		if r := b.ws.parentRefresh(key, now); r != "" {
			b.ws.mu.Unlock()
			return nil, r.err()
		}
		if t := b.ws.audioTerminal(key); t != nil {
			if b.oc == nil && minutes == 0 {
				b.ws.mu.Unlock()
				return nil, problem(503, "Chat unavailable")
			}
			if t.callState != callNone {
				b.ws.mu.Unlock()
				return nil, problem(409, "Companion is in a call")
			}
			b.ws.startTurn(t, now, false, "typed", func(ctx context.Context, turn uint64) {
				b.ws.runTurn(ctx, t, turn, text, "typed", nil)
			})
			b.ws.mu.Unlock()
			return object{"ok": true, "companion_id": sender.ID}, nil
		}
		if minutes > 0 {
			err := b.ws.childPause(key, minutes, now)
			b.ws.mu.Unlock()
			_, reply := parentPause.notice()
			return object{"ok": true, "companion_id": sender.ID, "reply": reply}, err
		}
		if b.oc == nil {
			b.ws.mu.Unlock()
			return nil, problem(503, "Chat unavailable")
		}
		child, done, err := b.ws.parentOffline(ctx, key, now)
		b.ws.mu.Unlock()
		if err != nil {
			return nil, err
		}
		defer done()
		ctx = child
	}
	if b.oc == nil {
		return nil, problem(503, "Chat unavailable")
	}
	out, err := b.turn(ctx, sender, text)
	if ctx.Err() != nil {
		if policyErr := b.parentAllowed(id); policyErr != nil {
			return nil, policyErr
		}
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	result := object{"ok": true, "companion_id": sender.ID, "reply": out.Reply, "session_id": out.SessionID}
	if scope == "" {
		result["audio_url"] = out.AudioURL
	}
	return result, nil
}

func (b *larkServer) resetCompanion(r *http.Request, id string) (any, error) {
	var archiveHistory bool
	if r.ContentLength != 0 {
		args, err := jsonBody(r)
		if err != nil {
			return nil, err
		}
		archiveHistory, err = companionBool(args, "archive_history", false)
		if err != nil {
			return nil, err
		}
	}
	if b.oc == nil {
		return nil, problem(503, "Chat unavailable")
	}
	if _, ok := b.companionByID(id); !ok {
		return nil, problem(404, "Companion not found")
	}
	ctx, cancel := context.WithTimeout(r.Context(), turnTimeout)
	defer cancel()
	return object{"ok": true}, b.startNewCompanionSession(ctx, id, archiveHistory)
}

func resetCompanionSession(ctx context.Context, tx *sql.Tx, id string, archiveHistory bool) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE companion_id=?`, id); err != nil {
		return problem(502, "Cannot save sessions")
	}
	if !archiveHistory {
		return nil
	}
	key := strings.ToLower(id)
	// Keep consecutive resets in distinct archives, including within one millisecond.
	for ts := time.Now().UnixMilli(); ; ts++ {
		archive := fmt.Sprintf("archive-%s-%d", key, ts)
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM messages WHERE companion_id=?)`, archive).Scan(&exists); err != nil {
			return problem(502, "Cannot archive chat history")
		}
		if !exists {
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET companion_id=? WHERE companion_id=?`, archive, key); err != nil {
				return problem(502, "Cannot archive chat history")
			}
			return nil
		}
	}
}

func (b *larkServer) startNewCompanionSession(ctx context.Context, id string, archiveHistory bool) error {
	id = strings.ToLower(id)
	run := b.oc.prepareMappingChange(id)
	if b.ws != nil {
		b.ws.stopCompanionTurns(id)
	}
	return run(ctx, func() error {
		b.companionMu.Lock()
		defer b.companionMu.Unlock()
		companion, err := b.changeCompanion(ctx, id, "new_session", object{"archive_history": archiveHistory})
		if err != nil {
			return err
		}
		if b.ws != nil {
			b.ws.mu.Lock()
			defer b.ws.mu.Unlock()
			for _, t := range b.ws.terminalsFor(id) {
				b.ws.stopCompanionTurn(t)
			}
			b.ws.companionChanged(id, companion)
			if archiveHistory {
				b.ws.historyArchived(id)
			}
		}
		return nil
	})
}

func (h *wsHub) historyArchived(id string) {
	key := strings.ToLower(id)
	f := textFrame(object{"op": "history_archived", "companion_id": id})
	for c := range h.clients {
		if c.role == "web" && (c.scope == nil || strings.EqualFold(c.scope.ID, key)) {
			c.enqueue(f)
		}
	}
}
