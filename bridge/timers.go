package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	timerGrace  = 2 * time.Second
	timerExpiry = 2 * time.Minute
)

var timerIDRE = regexp.MustCompile(`^[a-f0-9]{24}$`)

type savedTimer struct {
	ID        string    `json:"id"`
	DeviceID  string    `json:"device_id"`
	Label     string    `json:"label"`
	Say       string    `json:"say"`
	Chime     string    `json:"chime"`
	WakeAgent bool      `json:"wake_agent"`
	Notes     []string  `json:"notes,omitempty"`
	AppName   string    `json:"app,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	FireAt    time.Time `json:"fire_at"`
}

type timerManager struct {
	b       *bridge
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	handles map[string]*timerHandle
	wg      sync.WaitGroup
	closed  bool
	// HTTP MCP calls do not carry a trusted OpenCode session ID. Reject
	// wake-agent scheduling on a device while its timer-triggered turn runs.
	fromTimerTurn map[string]int
	onFired       func(string)
}

type timerHandle struct{ timer *time.Timer }

const timerColumns = "id,device_id,label,say,chime,wake_agent,created_at,fire_at,notes_json,app_name"

func newTimerManager(b *bridge) *timerManager {
	ctx, cancel := context.WithCancel(b.root)
	return &timerManager{b: b, ctx: ctx, cancel: cancel, handles: make(map[string]*timerHandle), fromTimerTurn: make(map[string]int)}
}

func scanTimer(r rowScanner) (savedTimer, error) {
	var t savedTimer
	var wake int
	var created, fire, notes string
	err := r.Scan(&t.ID, &t.DeviceID, &t.Label, &t.Say, &t.Chime, &wake, &created, &fire, &notes, &t.AppName)
	if err != nil {
		return t, err
	}
	t.WakeAgent = wake != 0
	if err := json.Unmarshal([]byte(notes), &t.Notes); err != nil {
		return t, err
	}
	t.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err == nil {
		t.FireAt, err = time.Parse(time.RFC3339Nano, fire)
	}
	return t, err
}

func (m *timerManager) list(ctx context.Context, id string) ([]savedTimer, error) {
	rows, err := m.b.db.readDB().QueryContext(ctx, "SELECT "+timerColumns+" FROM timers WHERE (?='' OR device_id=?)", id, id)
	if err != nil {
		return nil, problem(502, "Cannot read timers")
	}
	defer rows.Close()
	out := []savedTimer{}
	for rows.Next() {
		t, err := scanTimer(rows)
		if err != nil {
			return nil, problem(502, "Cannot read timers")
		}
		out = append(out, t)
	}
	if rows.Err() != nil {
		return nil, problem(502, "Cannot read timers")
	}
	slices.SortFunc(out, func(a, b savedTimer) int { return a.FireAt.Compare(b.FireAt) })
	return out, nil
}

func (t savedTimer) state(now time.Time) object {
	remaining := max(time.Duration(0), t.FireAt.Sub(now))
	notes := t.Notes
	if notes == nil {
		notes = []string{}
	}
	return object{"id": t.ID, "device_id": t.DeviceID, "label": t.Label, "say": t.Say, "notes": notes,
		"app": t.AppName, "wake_agent": t.WakeAgent, "created_at": t.CreatedAt, "chime": t.Chime,
		"fire_at": t.FireAt, "remaining_s": (remaining + time.Second - 1) / time.Second}
}

func (b *bridge) timerState(ctx context.Context, id string, now time.Time) ([]object, error) {
	out := []object{}
	if b.timers == nil {
		return out, nil
	}
	timers, err := b.timers.list(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, t := range timers {
		out = append(out, t.state(now))
	}
	return out, nil
}

func (m *timerManager) activeCount(ctx context.Context, id string) int {
	var count int
	_ = m.b.db.readDB().QueryRowContext(ctx, "SELECT count(*) FROM timers WHERE device_id=?", id).Scan(&count)
	return count
}

func (m *timerManager) restore(now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	timers, err := m.list(m.ctx, "")
	if err != nil {
		return err
	}
	for _, t := range timers {
		delay := t.FireAt.Sub(now)
		switch {
		case delay < -timerExpiry:
			if _, err := deleteRow(m.ctx, m.b.db, "DELETE FROM timers WHERE id=?", t.ID); err != nil {
				return problem(502, "Cannot prune expired timers")
			}
		case delay <= 0:
			m.armLocked(t.ID, timerGrace)
		default:
			m.armLocked(t.ID, delay)
		}
	}
	return nil
}

func parseTimer(args object, now time.Time) (savedTimer, error) {
	t := savedTimer{ID: randomHex(12), DeviceID: str(args["device_id"]), CreatedAt: now.UTC(), Label: "Timer", Chime: "celebrate"}
	return editTimer(t, args, now, false)
}

func editTimer(t savedTimer, args object, now time.Time, update bool) (savedTimer, error) {
	seconds, err := mcpUint(args, "in_seconds")
	at, atSet := args["at"]
	if err != nil || atSet && seconds != nil || !update && !atSet && seconds == nil {
		return t, problem(400, "Provide exactly one of in_seconds or at")
	}
	if seconds != nil {
		if *seconds < 1 || *seconds > 86400 {
			return t, problem(400, "in_seconds must be 1–86400")
		}
		t.FireAt = now.Add(time.Duration(*seconds) * time.Second).UTC()
	} else if atSet {
		t.FireAt, err = time.Parse(time.RFC3339, str(at))
		delay := t.FireAt.Sub(now)
		if err != nil || delay <= 0 || delay > 24*time.Hour {
			return t, problem(400, "at must be RFC3339 within the next 24 hours")
		}
		t.FireAt = t.FireAt.UTC()
	}
	if v, ok := args["wake_agent"]; ok {
		if t.WakeAgent, ok = v.(bool); !ok {
			return t, problem(400, "Expected boolean wake_agent")
		}
	}
	if t.WakeAgent && (atSet || seconds != nil && *seconds < 30 ||
		seconds == nil && args["wake_agent"] == true && t.FireAt.Sub(now) < 30*time.Second) {
		return t, problem(400, "wake_agent requires in_seconds of at least 30")
	}
	if raw, set := args["label"]; set {
		if t.Label = shortText(raw, 60, true); t.Label == "" {
			t.Label = "Timer"
		}
	}
	if raw, present := args["say"]; present {
		s, ok := raw.(string)
		if !ok {
			return t, fieldError("timer.say", "expected a string")
		}
		t.Say = ""
		if s != "" || !update {
			t.Say, err = spokenText(s)
			if err != nil {
				return t, err
			}
		}
	}
	if _, present := args["chime"]; present {
		t.Chime = str(args["chime"])
		if !validChime(t.Chime) {
			return t, problem(400, "Unknown chime kind")
		}
	}
	if raw, present := args["notes"]; present {
		if a, ok := raw.([]any); update && ok && len(a) == 0 {
			t.Notes = nil
		} else {
			t.Notes, _, err = melodyInput(raw, "timer.notes")
			if err != nil {
				return t, err
			}
		}
	}
	if raw, present := args["app"]; present {
		var ok bool
		t.AppName, ok = raw.(string)
		if !ok || (!update || t.AppName != "") && !slugRE.MatchString(t.AppName) {
			return t, fieldError("timer.app", "expected a saved app name")
		}
	}
	return t, nil
}

func (m *timerManager) update(ctx context.Context, id, device string, args object, now time.Time) (savedTimer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var t savedTimer
	if m.closed {
		return t, problem(503, "Timers unavailable")
	}
	// A library rename can also change app_name; read and update atomically.
	tx, err := m.b.db.BeginTx(ctx, nil)
	unavailable := problem(502, "Cannot update timer")
	if err != nil {
		return t, unavailable
	}
	defer tx.Rollback()
	t, err = scanTimer(tx.QueryRowContext(ctx, "SELECT "+timerColumns+" FROM timers WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return t, problem(404, "Timer not found")
	}
	if err != nil {
		return t, unavailable
	}
	if device != t.DeviceID {
		return t, problem(403, "Device out of scope")
	}
	before := t.FireAt
	t, err = editTimer(t, args, now, true)
	if err != nil {
		return t, err
	}
	if t.WakeAgent && m.fromTimerTurn[strings.ToLower(t.DeviceID)] != 0 {
		return t, problem(409, "Cannot schedule a wake-agent timer during a timer-triggered turn")
	}
	if t.AppName != "" {
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM apps WHERE name=?)", t.AppName).Scan(&exists); err != nil {
			return t, unavailable
		}
		if !exists {
			return t, problem(404, "App not found")
		}
	}
	notes, _ := json.Marshal(t.Notes)
	if _, err := tx.ExecContext(ctx, `UPDATE timers SET label=?,say=?,chime=?,wake_agent=?,fire_at=?,notes_json=?,app_name=? WHERE id=?`,
		t.Label, t.Say, t.Chime, t.WakeAgent, t.FireAt.Format(time.RFC3339Nano), string(notes), t.AppName, id); err != nil {
		return t, unavailable
	}
	if err := tx.Commit(); err != nil {
		return t, unavailable
	}
	if !before.Equal(t.FireAt) || m.handles[id] == nil {
		m.stopLocked(id)
		m.armLocked(id, time.Until(t.FireAt))
	}
	return t, nil
}

func (m *timerManager) set(ctx context.Context, t savedTimer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return problem(503, "Timers unavailable")
	}
	if t.WakeAgent && m.fromTimerTurn[strings.ToLower(t.DeviceID)] != 0 {
		return problem(409, "Cannot schedule a wake-agent timer during a timer-triggered turn")
	}
	notes, _ := json.Marshal(t.Notes)
	result, err := m.b.db.ExecContext(ctx, `INSERT INTO timers (`+timerColumns+`)
		SELECT ?,?,?,?,?,?,?,?,?,? WHERE (SELECT count(*) FROM timers WHERE device_id=?) < 8
		AND (SELECT count(*) FROM timers) < 32 AND (?='' OR EXISTS(SELECT 1 FROM apps WHERE name=?))`,
		t.ID, t.DeviceID, t.Label, t.Say, t.Chime, t.WakeAgent, t.CreatedAt.Format(time.RFC3339Nano), t.FireAt.Format(time.RFC3339Nano),
		string(notes), t.AppName, t.DeviceID, t.AppName, t.AppName)
	if err != nil {
		return problem(502, "Cannot save timer")
	}
	n, err := result.RowsAffected()
	if err != nil {
		return problem(502, "Cannot save timer")
	}
	if n == 0 {
		if t.AppName != "" {
			if _, err := m.b.loadApp(ctx, t.AppName); err != nil {
				return err
			}
		}
		return problem(409, "Timer limit reached (8 per device, 32 total)")
	}
	m.armLocked(t.ID, time.Until(t.FireAt))
	return nil
}

func (m *timerManager) armLocked(id string, delay time.Duration) {
	m.wg.Add(1)
	handle := &timerHandle{}
	m.handles[id] = handle
	handle.timer = time.AfterFunc(delay, func() {
		defer m.wg.Done()
		m.fire(id, handle)
	})
}

func (m *timerManager) stopLocked(id string) {
	if handle := m.handles[id]; handle != nil {
		if handle.timer.Stop() {
			m.wg.Done()
		}
		delete(m.handles, id)
	}
}

func (m *timerManager) close() {
	m.mu.Lock()
	m.closed = true
	m.cancel()
	for id := range m.handles {
		m.stopLocked(id)
	}
	m.mu.Unlock()
	m.wg.Wait()
}

func (m *timerManager) claim(id string, expected *timerHandle) (savedTimer, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if handle := m.handles[id]; m.closed || handle == nil || expected != nil && expected != handle {
		return savedTimer{}, false
	}
	t, err := scanTimer(m.b.db.QueryRowContext(m.ctx, "DELETE FROM timers WHERE id=? RETURNING "+timerColumns, id))
	if err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			m.stopLocked(id)
		case m.ctx.Err() == nil:
			log.Print("Lark could not claim timer")
			// The callback may already have fired; a lone map entry cannot retry.
			m.stopLocked(id)
			m.armLocked(id, timerGrace)
		}
		return savedTimer{}, false
	}
	m.stopLocked(id)
	if t.WakeAgent {
		m.fromTimerTurn[strings.ToLower(t.DeviceID)]++
	}
	return t, true
}

func (m *timerManager) fire(id string, expected *timerHandle) {
	t, ok := m.claim(id, expected)
	if !ok {
		return
	}
	if t.WakeAgent {
		defer func() {
			m.mu.Lock()
			key := strings.ToLower(t.DeviceID)
			if m.fromTimerTurn[key]--; m.fromTimerTurn[key] == 0 {
				delete(m.fromTimerTurn, key)
			}
			m.mu.Unlock()
		}()
	}
	m.b.timerUpdate(t.DeviceID)
	m.b.deliverTimer(m.ctx, t)
	if m.onFired != nil {
		m.onFired(id)
	}
}

func (m *timerManager) cancelTimers(ctx context.Context, device, id string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, problem(503, "Timers unavailable")
	}
	ids := []string{}
	if id != "" {
		var owner string
		err := m.b.db.readDB().QueryRowContext(ctx, "SELECT device_id FROM timers WHERE id=?", id).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, problem(404, "Timer not found")
		}
		if err != nil {
			return nil, problem(502, "Cannot read timer")
		}
		if device != "" && device != owner {
			return nil, problem(403, "Device out of scope")
		}
		deleted, err := deleteRow(ctx, m.b.db, "DELETE FROM timers WHERE id=? AND device_id=?", id, owner)
		if err != nil {
			return nil, problem(502, "Cannot cancel timer")
		}
		if !deleted {
			return nil, problem(404, "Timer not found")
		}
		m.stopLocked(id)
		ids = append(ids, owner)
	} else {
		rows, err := m.b.db.QueryContext(ctx, "DELETE FROM timers WHERE device_id=? RETURNING id", device)
		if err != nil {
			return nil, problem(502, "Cannot cancel timers")
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, problem(502, "Cannot cancel timers")
			}
			m.stopLocked(id)
		}
		if rows.Err() != nil {
			return nil, problem(502, "Cannot cancel timers")
		}
		ids = append(ids, device)
	}
	return ids, nil
}

func (b *bridge) timerUpdate(id string) {
	if h := b.ws; h != nil {
		h.mu.Lock()
		defer h.mu.Unlock()
		key := strings.ToLower(id)
		frame := textFrame(object{"type": "timers", "device_id": id})
		for c := range h.clients {
			if c.watch == key || c.device != nil && c.device.key == key && c.device.virtual() {
				c.enqueue(frame)
			}
		}
	}
}

func (b *bridge) deliverTimer(ctx context.Context, t savedTimer) {
	who, err := b.lookupDevice(t.DeviceID)
	if err != nil || ctx.Err() != nil {
		return
	}
	h := b.ws
	if h == nil {
		return
	}
	message := t.Label
	if t.Say != "" {
		message += ": " + t.Say
	}
	var pcm []byte
	if !t.WakeAgent {
		pcm, _ = synthesizeMelody(t.Notes)
	}
	h.mu.Lock()
	key, now := strings.ToLower(t.DeviceID), time.Now()
	if h.parentRefresh(key, now) != "" {
		h.mu.Unlock()
		return
	}
	if !t.WakeAgent {
		defer func() {
			entry := newChat("card", who.Character, "Time's up! "+message, "timer")
			entry.SceneTitle = t.Label
			b.chat(t.DeviceID, entry, true)
		}()
	}
	d := h.devices[key]
	if d != nil && h.active(d) && d.callState == callNone {
		if t.AppName != "" {
			defer func() {
				if _, err := b.runSavedApp(ctx, t.DeviceID, t.AppName, nil); err != nil && ctx.Err() == nil {
					log.Printf("Lark could not launch timer app %q: %v", t.AppName, err)
				}
			}()
		}
		if t.WakeAgent {
			completed := make(chan struct{})
			started := h.startTurn(d, now, true, "timer", func(turnCtx context.Context, id uint64) {
				defer close(completed)
				turnCtx, cancel := context.WithCancel(turnCtx)
				defer cancel()
				stop := context.AfterFunc(ctx, cancel)
				defer stop()
				h.runTurn(turnCtx, d, id, "Timer fired: "+message, "timer", nil)
			})
			h.mu.Unlock()
			if started {
				<-completed
			}
			return
		}
		h.wake(d, now)
		if len(pcm) != 0 && t.Say == "" {
			h.playPCM(d, pcm)
		} else if len(pcm) == 0 {
			h.chime(d, t.Chime)
		}
		view := timerScene(message)
		d.card, d.drawer = &view, ""
		if t.Say != "" {
			h.speakLocalAfter(d, t.Say, pcm)
		}
		h.render(d, now, false)
		h.mu.Unlock()
		return
	}
	if t.WakeAgent && b.oc != nil {
		child, done, err := h.parentOffline(ctx, key, now)
		h.mu.Unlock()
		if err != nil {
			return
		}
		defer done()
		ctx = child
		ctx, cancel := context.WithTimeout(ctx, turnTimeout)
		defer cancel()
		_, _ = b.generateReply(ctx, ctx, who, "Timer fired: "+message, turnOptions{source: "timer"})
		return
	}
	h.mu.Unlock()
}

func (b *bridge) patchTimer(ctx context.Context, id string, args object) (object, error) {
	m := b.timers
	if m == nil {
		return nil, problem(503, "Timers unavailable")
	}
	if raw, exists := args["device_id"]; exists {
		id = str(raw)
	}
	if !deviceIDRE.MatchString(id) {
		return nil, problem(400, "Invalid timer device_id")
	}
	op, present := args["op"]
	_, secs := args["in_seconds"]
	_, at := args["at"]
	if !present && (secs || at) {
		op = "set"
	}
	switch op {
	case "set":
		who, err := b.lookupDevice(id)
		if err != nil {
			return nil, err
		}
		now := time.Now()
		t, err := parseTimer(withDevice(args, id), now)
		if err != nil {
			return nil, err
		}
		if err := b.parentAllowed(id); err != nil {
			return nil, err
		}
		if err := m.set(ctx, t); err != nil {
			return nil, err
		}
		b.timerUpdate(id)
		entry := newChat("card", who.Character, "Timer: "+t.Label, "timer")
		entry.SceneTitle = t.Label
		b.chat(id, entry, true)
		return t.state(now), nil
	case "cancel":
		timerID, all := str(args["id"]), args["all"] == true
		if all && timerID != "" || !all && !timerIDRE.MatchString(timerID) {
			return nil, problem(400, "Provide exactly one of id or all=true")
		}
		if _, err := m.cancelTimers(ctx, id, timerID); err != nil {
			return nil, err
		}
		b.timerUpdate(id)
		return object{"device_id": id, "id": timerID, "all": all, "cancelled": true}, nil
	case "update":
		timerID := str(args["id"])
		_, hasAll := args["all"]
		fields := len(args) - 2
		if _, present := args["device_id"]; present {
			fields--
		}
		if !timerIDRE.MatchString(timerID) || hasAll || fields < 1 {
			return nil, fieldError("timer", "update requires id and at least one field to change")
		}
		if err := b.parentAllowed(id); err != nil {
			return nil, err
		}
		now := time.Now()
		t, err := m.update(ctx, timerID, id, args, now)
		if err != nil {
			return nil, err
		}
		b.timerUpdate(id)
		return t.state(now), nil
	}
	return nil, problem(400, "Unknown timer operation")
}

func (b *bridge) timerAPI(w http.ResponseWriter, r *http.Request, path, scope string) (any, error) {
	m := b.timers
	if m == nil {
		return nil, problem(503, "Timers unavailable")
	}
	id := r.URL.Query().Get("device_id")
	if scope != "" {
		if id != "" && id != scope {
			return nil, problem(403, "Device out of scope")
		}
		id = scope
	}
	if path == "/api/timers" && r.Method == http.MethodGet {
		if id != "" && !deviceIDRE.MatchString(id) {
			return nil, problem(400, "Invalid timer device_id")
		}
		timers, err := m.list(r.Context(), id)
		return object{"timers": timers}, err
	}
	if timerID, ok := strings.CutPrefix(path, "/api/timers/"); ok && r.Method == http.MethodDelete && timerIDRE.MatchString(timerID) {
		ids, err := m.cancelTimers(r.Context(), scope, timerID)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			b.timerUpdate(id)
		}
		w.WriteHeader(204)
		return nil, nil
	}
	return nil, problem(404, "Not found")
}
