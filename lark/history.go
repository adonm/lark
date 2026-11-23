package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

func stripLarkHeader(text string) string {
	first, rest, ok := strings.Cut(text, "\n")
	if ok && strings.HasPrefix(first, "[") && strings.HasSuffix(strings.TrimSuffix(first, "\r"), "]") {
		// Current headers carry only the live speaker; "You are" is stored legacy.
		for _, prefix := range []string{"[Speaker: ", "[Event: ", "[You are "} {
			if strings.HasPrefix(first, prefix) {
				text = rest
				break
			}
		}
	}
	if i := strings.LastIndex(text, "\n[state: "); i >= 0 && strings.HasSuffix(strings.TrimSuffix(text, "\r"), "]") {
		text = text[:i]
	}
	for _, hint := range []string{"\n[library: ", "\n[story: "} {
		if i := strings.LastIndex(text, hint); i >= 0 && strings.HasSuffix(strings.TrimSuffix(text, "\r"), "]") {
			text = text[:i]
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
	Channel     string    `json:"channel,omitempty"`
	SceneTitle  string    `json:"card_title,omitempty"`
	ToolSummary string    `json:"-"`
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

func channelForSource(source string) string {
	switch source {
	case "deltachat":
		return "deltachat"
	case "intercom":
		return "intercom"
	case "timer":
		return "timer"
	case "voice", "typed", "tap", "text":
		return "in_person"
	default:
		return source
	}
}

func newChat(role, sender, text, source string) chatEntry {
	// Message IDs only need to be unique and unguessable; rand.Text panics
	// instead of degrading if the system entropy source fails.
	return chatEntry{ID: "bridge-" + rand.Text(), Role: role, Sender: sender, Text: text,
		Source: source, Channel: channelForSource(source), Timestamp: time.Now().In(time.Local)}
}

type chatWrite struct {
	id    string
	entry chatEntry
	sync  chan struct{}
}

type chatQueue struct {
	ch   chan chatWrite
	quit chan struct{}
	done chan struct{}
}

type chatHistory struct {
	db     *larkDB
	mu     sync.Mutex
	queues map[string]*chatQueue
	closed bool
}

func (h *chatHistory) queueFor(id string) *chatQueue {
	key := strings.ToLower(id)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	if h.queues == nil {
		h.queues = make(map[string]*chatQueue)
	}
	q := h.queues[key]
	if q == nil {
		q = &chatQueue{ch: make(chan chatWrite, 64), quit: make(chan struct{}), done: make(chan struct{})}
		h.queues[key] = q
		go h.runQueue(q)
	}
	return q
}

func (h *chatHistory) runQueue(q *chatQueue) {
	defer close(q.done)
	for {
		select {
		case <-q.quit:
			// Persist what is already queued, then stop.
			for {
				select {
				case w := <-q.ch:
					h.write(q, w)
				default:
					return
				}
			}
		case w := <-q.ch:
			h.write(q, w)
		}
	}
}

func (h *chatHistory) write(q *chatQueue, w chatWrite) {
	if w.sync != nil {
		close(w.sync)
		return
	}
	if _, err := h.db.Exec(insertMessage, messageValues(w.id, w.entry)...); err != nil {
		log.Printf("Lark could not persist message: %v", err)
	}
}

func (h *chatHistory) flush(id string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	q := h.queues[strings.ToLower(id)]
	h.mu.Unlock()
	if q == nil {
		return
	}
	done := make(chan struct{})
	select {
	case q.ch <- chatWrite{sync: done}:
		select {
		case <-done:
		case <-q.quit:
		}
	case <-q.quit:
	}
}

func (h *chatHistory) close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	queues := make([]*chatQueue, 0, len(h.queues))
	for _, q := range h.queues {
		queues = append(queues, q)
	}
	clear(h.queues)
	h.mu.Unlock()
	for _, q := range queues {
		close(q.quit)
		<-q.done
	}
}

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

const insertMessage = `INSERT INTO messages (id,companion_id,role,sender,text,source,channel,card_title,ts_ms)
	VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`

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
	ch := e.Channel
	if ch == "" {
		ch = channelForSource(e.Source)
	}
	return []any{e.ID, strings.ToLower(id), e.Role, e.Sender, e.Text, e.Source, ch, e.SceneTitle, e.Timestamp.UnixMilli()}
}

func (h *chatHistory) add(id string, e chatEntry) {
	if e, ok := canonicalMessage(e); ok {
		if q := h.queueFor(id); q != nil {
			select {
			case q.ch <- chatWrite{id: strings.ToLower(id), entry: e}:
			case <-q.quit:
			}
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
	h.flush(id)
	query := `SELECT id,role,sender,text,source,channel,card_title,ts_ms FROM messages WHERE companion_id=?`
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
		if err := rows.Scan(&e.ID, &e.Role, &e.Sender, &e.Text, &e.Source, &e.Channel, &e.SceneTitle, &ts); err != nil {
			return nil, problem(502, "Cannot read chat events")
		}
		e.Timestamp = time.UnixMilli(ts).In(time.Local)
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
	b.recordChat(id, entry, persist)
}

func (b *larkServer) recordChat(id string, entry chatEntry, persist bool) (chatEntry, bool) {
	entry, ok := canonicalMessage(entry)
	if ok && persist && b.history != nil {
		b.history.add(id, entry)
	}
	return entry, ok
}

func (h *terminalHub) broadcastChat(id string, entry chatEntry) {}

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
	if b.hub != nil {
		b.hub.stopCompanionTurns(id)
	}
	return run(ctx, func() error {
		b.companionMu.Lock()
		defer b.companionMu.Unlock()
		companion, err := b.changeCompanion(ctx, id, "new_session", object{"archive_history": archiveHistory})
		if err != nil {
			return err
		}
		if b.hub != nil {
			b.hub.mu.Lock()
			defer b.hub.mu.Unlock()
			for _, t := range b.hub.terminalsFor(id) {
				b.hub.stopCompanionTurn(t)
			}
			b.hub.companionChanged(id, companion)
			if archiveHistory {
				b.hub.historyArchived(id)
			}
		}
		return nil
	})
}

func (h *terminalHub) historyArchived(id string) {}
