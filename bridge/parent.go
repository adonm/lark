package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type parentSettings struct {
	Timezone      string `json:"timezone"`
	QuietStart    string `json:"quiet_start"`
	QuietEnd      string `json:"quiet_end"`
	DailyMinutes  int    `json:"daily_minutes"`
	PauseUntil    int64  `json:"pause_until_ms"`
	OverrideUntil int64  `json:"override_until_ms"`
}

type parentReason string

const (
	parentQuiet       parentReason = "quiet_hours"
	parentLimit       parentReason = "daily_limit"
	parentPause       parentReason = "paused"
	parentUnavailable parentReason = "unavailable"
)

func (r parentReason) notice() (title, text string) {
	switch r {
	case parentQuiet:
		return "Quiet hours", "It's rest time. Let's talk after quiet hours."
	case parentLimit:
		return "All done for today", "We've used today's time. Let's play again tomorrow."
	case parentPause:
		return "Taking a break", "I'm paused. A grown-up can unlock me."
	default:
		return "Taking a break", "Please ask a grown-up for help."
	}
}

func (r parentReason) err() error {
	_, text := r.notice()
	return problem(423, text)
}

func (s parentSettings) reason(now time.Time, loc *time.Location, used time.Duration) parentReason {
	at := now.UnixMilli()
	switch {
	case s.PauseUntil > at:
		return parentPause
	case s.OverrideUntil > at:
		return ""
	case s.QuietStart != "":
		wall := now.In(loc).Format("15:04")
		if s.QuietStart < s.QuietEnd && s.QuietStart <= wall && wall < s.QuietEnd ||
			s.QuietStart > s.QuietEnd && (s.QuietStart <= wall || wall < s.QuietEnd) {
			return parentQuiet
		}
	}
	if s.DailyMinutes > 0 && used >= time.Duration(s.DailyMinutes)*time.Minute {
		return parentLimit
	}
	return ""
}

func parentScene(reason parentReason) sceneView {
	title, _ := reason.notice()
	sub := "Ask a grown-up to unlock."
	switch reason {
	case parentQuiet:
		sub = "Let's talk after quiet hours."
	case parentLimit:
		sub = "Let's play again tomorrow."
	}
	return sceneView{bg: menuBG, title: title,
		art: svgRect(52, 76, 256, 166, uiCardRadius, menuCard, menuBorder, 2) +
			`<path d="M193 93a27 27 0 1 0 22 37 27 27 0 0 1-22-37z" fill="` + menuWarning + `"/>` +
			svgLabel(180, 183, 27, title, menuInk) + svgLabel(180, 220, 17, sub, menuMuted)}
}

// parentRuntime, like device activity, is owned by the hub lock.
type parentRuntime struct {
	settings   parentSettings
	loc        *time.Location
	configured bool
	day        string
	dayEnd     time.Time
	used       time.Duration
	savedMS    int64
	at         time.Time
	using      bool
	reason     parentReason
	offline    map[context.Context]context.CancelFunc
}

func (h *wsHub) parentLoad(key string) (*parentRuntime, error) {
	if p := h.parents[key]; p != nil {
		return p, nil
	}
	p := &parentRuntime{settings: parentSettings{Timezone: "UTC"}, loc: time.UTC}
	if h.b.db != nil {
		s := &p.settings
		err := h.b.db.QueryRow(`SELECT timezone,quiet_start,quiet_end,daily_minutes,pause_until_ms,override_until_ms
			FROM parent_settings WHERE device_id=?`, key).Scan(&s.Timezone, &s.QuietStart, &s.QuietEnd, &s.DailyMinutes, &s.PauseUntil, &s.OverrideUntil)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		p.configured = err == nil
		p.loc, err = time.LoadLocation(s.Timezone)
		if err != nil {
			return nil, err
		}
	}
	if h.parents == nil {
		h.parents = make(map[string]*parentRuntime)
	}
	h.parents[key] = p
	return p, nil
}

func (h *wsHub) parentSet(key string, s parentSettings, loc *time.Location, now time.Time) error {
	p, err := h.parentLoad(key)
	if err != nil {
		return err
	}
	if err := h.parentAccount(key, p, now, true); err != nil {
		return err
	}
	_, err = h.b.db.Exec(`INSERT INTO parent_settings VALUES (?,?,?,?,?,?,?) ON CONFLICT(device_id) DO UPDATE SET
		timezone=excluded.timezone,quiet_start=excluded.quiet_start,quiet_end=excluded.quiet_end,
		daily_minutes=excluded.daily_minutes,pause_until_ms=excluded.pause_until_ms,override_until_ms=excluded.override_until_ms`,
		key, s.Timezone, s.QuietStart, s.QuietEnd, s.DailyMinutes, s.PauseUntil, s.OverrideUntil)
	if err != nil {
		return err
	}
	if p.settings.Timezone != s.Timezone {
		p.day, p.dayEnd = "", time.Time{}
	}
	p.settings, p.loc, p.configured = s, loc, true
	h.parentRefresh(key, now)
	return nil
}

func (h *wsHub) parentFlush(key string, p *parentRuntime) error {
	ms := p.used.Milliseconds()
	if h.b.db != nil && ms != p.savedMS {
		if _, err := h.b.db.Exec(`INSERT INTO parent_usage VALUES (?,?,?) ON CONFLICT(device_id,day)
			DO UPDATE SET used_ms=excluded.used_ms`, key, p.day, ms); err != nil {
			return err
		}
	}
	p.savedMS = ms
	return nil
}

// Stop at offset transitions as well as midnight: some zones change offset
// at midnight, making that local time ambiguous or nonexistent.
func parentDayEnd(local time.Time) time.Time {
	y, m, d := local.Date()
	_, offset := local.Zone()
	end := time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC).Add(-time.Duration(offset) * time.Second)
	if _, zoneEnd := local.ZoneBounds(); !zoneEnd.IsZero() && zoneEnd.Before(end) {
		return zoneEnd
	}
	return end
}

func (h *wsHub) parentAccount(key string, p *parentRuntime, now time.Time, flush bool) error {
	if now.Before(p.at) {
		now = p.at
	}
	if p.at.IsZero() || !p.using {
		p.at = now
	}
	for {
		if p.dayEnd.IsZero() || !p.at.Before(p.dayEnd) {
			local := p.at.In(p.loc)
			day := local.Format(time.DateOnly)
			if day != p.day {
				if err := h.parentFlush(key, p); err != nil {
					return err
				}
				var ms int64
				if h.b.db != nil {
					err := h.b.db.QueryRow(`SELECT used_ms FROM parent_usage WHERE device_id=? AND day=?`, key, day).Scan(&ms)
					if err != nil && !errors.Is(err, sql.ErrNoRows) {
						return err
					}
				}
				p.day, p.used, p.savedMS = day, time.Duration(ms)*time.Millisecond, ms
			}
			p.dayEnd = parentDayEnd(local)
		}
		end := now
		if p.dayEnd.Before(end) {
			end = p.dayEnd
		}
		if p.using {
			p.used += end.Sub(p.at)
		}
		p.at = end
		if now.Before(p.dayEnd) {
			break
		}
	}
	if flush || p.used.Milliseconds()-p.savedMS >= 1000 {
		return h.parentFlush(key, p)
	}
	return nil
}

func (h *wsHub) parentUsing(d *wsDevice, p *parentRuntime) bool {
	return p.reason == "" && (len(p.offline) != 0 || d != nil && h.active(d) &&
		(d.recording.active() || d.busy || d.ttsStarted || d.awaitAck || d.app != nil && !d.asleep || d.callState == callActive))
}

func (h *wsHub) parentRefresh(key string, now time.Time) parentReason {
	if h.b == nil {
		return ""
	}
	p, err := h.parentLoad(key)
	r := parentUnavailable
	if err == nil {
		err = h.parentAccount(key, p, now, false)
		if err == nil {
			r = p.settings.reason(p.at, p.loc, p.used)
		}
		p.reason = r
		if r != "" {
			for _, cancel := range p.offline {
				cancel()
			}
			clear(p.offline)
		}
	}
	if err != nil {
		log.Printf("Lark parent controls unavailable for %s: %v", key, err)
	}
	d := h.devices[key]
	if d != nil && h.active(d) && d.parentReason != r {
		h.applyParent(d, r, now)
	}
	if p != nil {
		p.using = h.parentUsing(d, p)
	}
	return r
}

func (h *wsHub) applyParent(d *wsDevice, r parentReason, now time.Time) {
	old := d.parentReason
	d.parentReason = r
	if r != "" {
		h.endCall(d, "Cat is resting")
		pendingRec := d.recording.active() && !d.recording.open()
		h.abortCallRecording(d)
		d.recording.ignoreStart = pendingRec
		h.cancelTurn(d, "parent controls")
		h.stopPlayback(d, true)
		h.clearView(d)
		d.drawer = ""
		d.asleep = true
	} else {
		h.stopPlayback(d, true)
		d.last, d.asleep = now, false
	}
	h.push(d, object{"op": "power", "brightness": d.brightness()}, true)
	h.push(d, parentFrame(d), true)
	if d.caps.WakeWord {
		h.push(d, object{"op": "wake_word", "enabled": r == "" && d.wakeWord, "device_id": d.who.DeviceID}, false)
	}
	h.render(d, now, true)
	if r == parentLimit && old == "" && !d.announcing {
		_, text := r.notice()
		h.speakLocal(d, text)
	}
}

func parentFrame(d *wsDevice) object {
	f := object{"op": "parent_policy", "device_id": d.who.DeviceID, "reason": d.parentReason, "message": ""}
	if d.parentReason != "" {
		_, f["message"] = d.parentReason.notice()
	}
	return f
}

func (h *wsHub) parentFinishActivity(key string, now time.Time) {
	if p := h.parents[key]; p != nil {
		if err := h.parentAccount(key, p, now, true); err != nil {
			log.Printf("Lark parent usage checkpoint for %s: %v", key, err)
		}
		p.using = h.parentUsing(h.devices[key], p)
	}
}

func (h *wsHub) parentObserve(d *wsDevice, now time.Time) {
	if p := h.parents[d.key]; p != nil {
		using := h.parentUsing(d, p)
		if p.using != using {
			if err := h.parentAccount(d.key, p, now, false); err != nil {
				log.Printf("Lark parent usage for %s: %v", d.key, err)
			}
			p.using = using
		}
	}
}

func (h *wsHub) parentOffline(ctx context.Context, key string, now time.Time) (context.Context, func(), error) {
	if r := h.parentRefresh(key, now); r != "" {
		return nil, nil, r.err()
	}
	p := h.parents[key]
	child, cancel := context.WithCancel(ctx)
	if p.offline == nil {
		p.offline = make(map[context.Context]context.CancelFunc)
	}
	p.offline[child], p.using = cancel, true
	return child, func() {
		cancel()
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(p.offline, child)
		h.parentFinishActivity(key, time.Now())
	}, nil
}

func (b *bridge) parentAllowed(id string) error {
	if h := b.ws; h != nil {
		h.mu.Lock()
		defer h.mu.Unlock()
		if r := h.parentRefresh(strings.ToLower(id), time.Now()); r != "" {
			return r.err()
		}
	}
	return nil
}

var childPauseRE = regexp.MustCompile(`^(?:pause|stop working) for ([a-z]+|[0-9]{1,2}) minutes?(?: so i can(?:not|'t) use you)?[.!]?$`)

func childPauseMinutes(text string) int {
	m := childPauseRE.FindStringSubmatch(strings.ToLower(strings.TrimSpace(strings.ReplaceAll(text, "’", "'"))))
	if m == nil {
		return 0
	}
	for i, word := range strings.Fields("one two three four five six seven eight nine ten") {
		if word == m[1] {
			return i + 1
		}
	}
	n, _ := strconv.Atoi(m[1])
	if n <= 30 {
		return n
	}
	return 0
}

func (h *wsHub) childPause(key string, minutes int, now time.Time) error {
	if r := h.parentRefresh(key, now); r != "" {
		return r.err()
	}
	if h.b.db == nil || h.b.parentToken == "" || minutes < 1 || minutes > 30 {
		return problem(409, "Can't start a break right now")
	}
	p := h.parents[key]
	s := p.settings
	s.PauseUntil = now.Add(time.Duration(minutes) * time.Minute).UnixMilli()
	return h.parentSet(key, s, p.loc, now)
}

func (b *bridge) parentState(id string, now time.Time) object {
	out := object{"restricted": false}
	if h := b.ws; h != nil {
		h.mu.Lock()
		defer h.mu.Unlock()
		key := strings.ToLower(id)
		r := h.parentRefresh(key, now)
		out["restricted"], out["reason"] = r != "", r
		if r != "" {
			_, out["message"] = r.notice()
		}
		if p := h.parents[key]; p != nil {
			out["used_seconds"] = int(p.used / time.Second)
			out["daily_minutes"] = p.settings.DailyMinutes
			out["day"] = p.day
		}
	}
	return out
}
