package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"
)

func loadParentToken(path, bridgeToken string) (string, error) {
	stat, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", errors.New("cannot open Lark parent.token")
	}
	if !stat.Mode().IsRegular() || stat.Mode().Perm()&0o077 != 0 || stat.Size() > 256 {
		return "", errors.New("Lark parent.token must be a private file (0600)")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("cannot open Lark parent.token")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 257))
	if err != nil {
		return "", errors.New("cannot read Lark parent.token")
	}
	token := strings.TrimSpace(string(raw))
	if len(raw) > 256 || len(token) < 32 || !tokenRE.MatchString(token) || token == bridgeToken {
		return "", errors.New("Lark parent.token must contain a distinct ASCII token of at least 32 characters without whitespace")
	}
	return token, nil
}

func parentLAN(r *http.Request) bool {
	remote, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	// The scoped listener never routes here. Don't honor client-supplied proxy addresses.
	_, err = lanIP(remote.Addr().Unmap().String(), true)
	return err == nil
}

func (b *larkServer) parentAPI(r *http.Request, id string) (any, error) {
	if !parentLAN(r) {
		return nil, problem(403, "Open parent controls directly on your LAN")
	}
	if b.parentToken == "" || b.ws == nil || b.db == nil {
		return nil, problem(503, "Parent controls aren't configured on this bridge")
	}
	if !tokenHeaderMatches(r.Header, "Parent-Token", b.parentToken) {
		return nil, problem(403, "Parent token not accepted")
	}
	if r.Method == http.MethodGet && id == "" {
		return b.parentOverview(r.Context())
	}
	if r.Method != http.MethodPost || id == "" {
		return nil, problem(405, "Use GET for the overview or POST to change a companion")
	}
	c, ok := b.companionByID(id)
	if !ok {
		return nil, problem(404, "Companion not found")
	}
	readArgs := jsonBody
	if parentFragmentRequest(r) {
		readArgs = parentForm
	}
	args, err := readArgs(r)
	if err != nil {
		return nil, err
	}
	h := b.ws
	h.mu.Lock()
	defer h.mu.Unlock()
	key := strings.ToLower(c.ID)
	p, err := h.parentLoad(key)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	s, loc, err := editParent(p.settings, p.loc, args, now)
	if err != nil {
		return nil, err
	}
	if err := h.parentSet(key, s, loc, now); err != nil {
		return nil, err
	}
	return parentRow(c.ID, c.Name, p), nil
}

func editParent(s parentSettings, loc *time.Location, args object, now time.Time) (parentSettings, *time.Location, error) {
	switch args["action"] {
	case "settings":
		zone, zOK := args["timezone"].(string)
		start, sOK := args["quiet_start"].(string)
		end, eOK := args["quiet_end"].(string)
		minutes, err := mcpUint(args, "daily_minutes")
		if len(args) != 5 || !zOK || !sOK || !eOK || err != nil || minutes == nil || *minutes > 1440 {
			return s, loc, problem(400, "Enter a time zone, both quiet-hour fields, and a daily limit from 0 to 1440 minutes")
		}
		if zone == "" || zone == "Local" || len(zone) > 64 {
			return s, loc, problem(400, "Use an IANA time zone such as America/New_York or UTC")
		}
		newLoc, err := time.LoadLocation(zone)
		if err != nil {
			return s, loc, problem(400, "Unknown IANA time zone")
		}
		if start != "" || end != "" {
			valid := func(clock string) bool {
				t, err := time.Parse("15:04", clock)
				return err == nil && t.Format("15:04") == clock
			}
			if !valid(start) || !valid(end) || start == end {
				return s, loc, problem(400, "Quiet hours need different start and end times in HH:MM, or leave both blank")
			}
		}
		s.Timezone, s.QuietStart, s.QuietEnd, s.DailyMinutes = zone, start, end, int(*minutes)
		return s, newLoc, nil
	case "pause", "unlock":
		minutes, err := mcpUint(args, "minutes")
		if len(args) != 2 || err != nil || minutes == nil || *minutes < 1 || *minutes > 1440 {
			return s, loc, problem(400, "Choose 1 to 1440 minutes")
		}
		until := now.Add(time.Duration(*minutes) * time.Minute).UnixMilli()
		if args["action"] == "pause" {
			s.PauseUntil, s.OverrideUntil = until, 0
		} else {
			s.PauseUntil, s.OverrideUntil = 0, until
		}
	case "end_override":
		if len(args) != 1 {
			return s, loc, problem(400, "end_override takes no other fields")
		}
		s.OverrideUntil = 0
	default:
		return s, loc, problem(400, "Unknown parent action")
	}
	return s, loc, nil
}

func parentRow(id, name string, p *parentRuntime) object {
	return object{"companion_id": id, "name": name, "settings": p.settings,
		"reason": p.reason, "used_seconds": int(p.used / time.Second), "day": p.day}
}

func (b *larkServer) parentOverview(ctx context.Context) (object, error) {
	companions, err := b.allCompanions(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]object, 0, len(companions))
	for _, c := range companions {
		id, name := c.ID, c.Name
		h, key := b.ws, strings.ToLower(id)
		h.mu.Lock()
		reason := h.parentRefresh(key, now)
		var row object
		if p := h.parents[key]; p != nil {
			row = parentRow(id, name, p)
		}
		h.mu.Unlock()
		if reason == parentUnavailable {
			return nil, reason.err()
		}
		var talk, apps, calls, intercom, timers int
		err = b.db.readDB().QueryRowContext(ctx, `SELECT
			COUNT(CASE WHEN role='user' THEN 1 END),
			COUNT(CASE WHEN source='app' THEN 1 END),
			COUNT(CASE WHEN source='call' THEN 1 END),
			COUNT(CASE WHEN role='intercom' THEN 1 END),
			COUNT(CASE WHEN source='timer' THEN 1 END)
			FROM messages WHERE companion_id=? AND ts_ms>=?`, key, now.Add(-7*24*time.Hour).UnixMilli()).Scan(&talk, &apps, &calls, &intercom, &timers)
		if err != nil {
			return nil, err
		}
		row["recent"] = object{"talk_turns": talk, "app_launches": apps, "calls": calls, "intercom": intercom, "timer_cards": timers}
		out = append(out, row)
	}
	return object{"companions": out}, nil
}
