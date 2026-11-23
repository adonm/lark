package main

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"
)

type terminalRecord struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	CompanionID string `json:"companion_id,omitempty"`
	IP          string `json:"ip,omitempty"`
	Port        int    `json:"port,omitempty"`
	LastSeen    int64  `json:"last_seen,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
}

func (b *larkServer) terminalSnapshot() (map[string]terminalRecord, error) {
	if b.terminals == nil {
		return nil, nil
	}
	return b.terminals.all()
}

func (b *larkServer) terminalByID(id string) (terminalRecord, bool) {
	if b.terminals == nil {
		return terminalRecord{}, false
	}
	t, ok, _ := b.terminals.get(strings.ToLower(id))
	return t, ok
}

func (b *larkServer) terminalsForCompanion(companionID string) ([]terminalRecord, error) {
	all, err := b.terminalSnapshot()
	if err != nil {
		return nil, err
	}
	var out []terminalRecord
	for _, t := range all {
		if strings.EqualFold(t.CompanionID, companionID) {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b terminalRecord) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (b *larkServer) terminalEntry(t terminalRecord, full bool) object {
	auto, vol := b.volumePrefs(t.ID)
	entry := object{
		"id":           t.ID,
		"kind":         t.Kind,
		"companion_id": t.CompanionID,
		"auto_volume":  auto,
		"volume":       vol,
		"online":       false,
		"ws_connected": false,
		"mode":         "offline",
		"sleeping":     false,
	}
	if t.Kind == "hardware" {
		entry["ip"] = t.IP
		entry["port"] = t.Port
		entry["last_seen"] = t.LastSeen
	}
	if t.CreatedAt != "" {
		entry["created_at"] = t.CreatedAt
	}
	if b.ws != nil {
		b.ws.terminalStatus(entry, t.ID, full)
	}
	return entry
}

func (b *larkServer) terminalList(full bool) ([]object, error) {
	all, err := b.terminalSnapshot()
	if err != nil {
		return nil, err
	}
	list := make([]object, 0, len(all))
	for _, t := range all {
		list = append(list, b.terminalEntry(t, full))
	}
	slices.SortFunc(list, func(a, b object) int { return strings.Compare(str(a["id"]), str(b["id"])) })
	return list, nil
}

func (b *larkServer) terminalAPI(w http.ResponseWriter, r *http.Request, path string) (any, error) {
	if b.db == nil || b.terminals == nil {
		return nil, problem(503, "Terminal storage unavailable")
	}
	switch {
	case path == "/api/terminals" && r.Method == http.MethodGet:
		list, err := b.terminalList(false)
		if err != nil {
			return nil, err
		}
		return object{"terminals": list}, nil
	}
	rest, ok := strings.CutPrefix(path, "/api/terminals/")
	if !ok || rest == "" {
		return nil, problem(404, "Not found")
	}
	if id, ok := strings.CutSuffix(rest, "/attach"); ok && r.Method == http.MethodPost {
		obj, err := jsonBody(r)
		if err != nil {
			return nil, err
		}
		compID := strings.ToLower(strings.TrimSpace(str(obj["companion_id"])))
		if !validCompanionID(compID) {
			return nil, problem(400, "Invalid companion ID")
		}
		return b.attachTerminal(r.Context(), id, compID)
	}
	if id, ok := strings.CutSuffix(rest, "/detach"); ok && r.Method == http.MethodPost {
		return b.detachTerminal(r.Context(), id)
	}
	if id, ok := strings.CutSuffix(rest, "/volume"); ok && r.Method == http.MethodPost {
		return b.volumeAPI(r, id)
	}
	if id, ok := strings.CutSuffix(rest, "/pair"); ok && r.Method == http.MethodPost {
		if b.ws == nil || b.dc == nil {
			return nil, problem(503, "Pairing unavailable")
		}
		id = strings.ToLower(id)
		b.ws.mu.Lock()
		d := b.ws.terminals[id]
		if d == nil || !b.ws.active(d) || d.companionID == "" {
			b.ws.mu.Unlock()
			return nil, problem(409, "Terminal not connected to a companion")
		}
		compID, compName, geom := d.companionID, d.companion.Name, d.screen
		b.ws.mu.Unlock()
		invite, err := b.dc.pairInvite(compID)
		if err != nil {
			return nil, problem(502, err.Error())
		}
		pairImg, err := renderPixelPair(pixelSpecForGeometry(geom), compName, invite)
		if err != nil {
			return nil, problem(502, err.Error())
		}
		pngBytes, err := imageToPNG(pairImg)
		if err != nil {
			return nil, problem(502, err.Error())
		}
		b.ws.mu.Lock()
		if d = b.ws.terminals[id]; b.ws.active(d) && d.companionID == compID {
			now := time.Now()
			b.ws.wake(d, now)
			d.drawer = drawerPair
			d.menuPage = 0
			d.menuData = &menuContent{invite: invite}
			b.ws.pushPower(d, 100, true)
			b.ws.render(d, now, false)
		}
		b.ws.mu.Unlock()
		sendBytes(w, 200, "image/png", pngBytes)
		return nil, nil
	}
	if id, ok := strings.CutSuffix(rest, "/hits"); ok && r.Method == http.MethodGet {
		if b.ws == nil {
			return nil, problem(503, "Terminal hub unavailable")
		}
		id = strings.ToLower(id)
		if !terminalIDRE.MatchString(id) {
			return nil, problem(400, "Invalid terminal ID")
		}
		b.ws.mu.Lock()
		defer b.ws.mu.Unlock()
		d := b.ws.terminals[id]
		if d == nil || !b.ws.active(d) {
			return nil, problem(404, "Terminal not connected")
		}
		screen := "home"
		switch {
		case d.asleep:
			screen = "sleep"
		case d.drawer != "":
			screen = d.drawer
		case d.card != nil:
			screen = "card"
		case d.view != nil:
			screen = "view"
		}
		state := "idle"
		switch {
		case d.asleep:
			state = "sleeping"
		case d.recording.active():
			state = "listening"
		case d.pose.mood == "listening" || d.pose.mood == "thinking" || d.pose.mood == "speaking":
			state = d.pose.mood
		case d.busy:
			state = "thinking"
		}
		w, h := d.screen.width, d.screen.height
		if w == 0 || h == 0 {
			w, h = screenSize, screenSize
		}
		type hitEntry struct {
			ID     string `json:"id"`
			Label  string `json:"label,omitempty"`
			Action string `json:"action,omitempty"`
			Value  string `json:"value,omitempty"`
			X1     int    `json:"x1"`
			Y1     int    `json:"y1"`
			X2     int    `json:"x2"`
			Y2     int    `json:"y2"`
		}
		hits := make([]hitEntry, 0, len(d.svg.hits))
		for _, hb := range d.svg.hits {
			act := d.scene.actions[hb.id]
			hits = append(hits, hitEntry{
				ID:     hb.id,
				Label:  hb.label,
				Action: act.kind,
				Value:  act.value,
				X1:     hb.x1,
				Y1:     hb.y1,
				X2:     hb.x2,
				Y2:     hb.y2,
			})
		}
		return object{
			"terminal_id": d.id,
			"screen":      screen,
			"state":       state,
			"rev":         d.nativeRev,
			"width":       w,
			"height":      h,
			"hits":        hits,
		}, nil
	}
	if strings.Contains(rest, "/") {
		return nil, problem(404, "Not found")
	}
	id := strings.ToLower(rest)
	if !terminalIDRE.MatchString(id) {
		return nil, problem(400, "Invalid terminal ID")
	}
	switch r.Method {
	case http.MethodGet:
		t, ok := b.terminalByID(id)
		if !ok {
			return nil, problem(404, "Terminal not found")
		}
		return b.terminalEntry(t, false), nil
	case http.MethodPatch:
		obj, err := jsonBody(r)
		if err != nil {
			return nil, err
		}
		t, ok := b.terminalByID(id)
		if !ok {
			return nil, problem(404, "Terminal not found")
		}
		if raw, hasComp := obj["companion_id"]; hasComp {
			if raw == nil || str(raw) == "" {
				if _, err := b.detachTerminal(r.Context(), id); err != nil {
					return nil, err
				}
			} else {
				compID := strings.ToLower(strings.TrimSpace(str(raw)))
				if !validCompanionID(compID) {
					return nil, problem(400, "Invalid companion ID")
				}
				if _, err := b.attachTerminal(r.Context(), id, compID); err != nil {
					return nil, err
				}
			}
			t, _ = b.terminalByID(id)
		}
		if _, hasAuto := obj["auto"]; hasAuto {
			if b.ws == nil {
				return nil, problem(503, "Terminal hub unavailable")
			}
			volArgs := object{"auto_volume": obj["auto"]}
			if v, ok := obj["volume"]; ok {
				volArgs["volume"] = v
			}
			if _, err := b.ws.terminalVolumeControl(r.Context(), id, volArgs); err != nil {
				return nil, err
			}
		} else if v, hasVol := obj["volume"]; hasVol {
			if b.ws == nil {
				return nil, problem(503, "Terminal hub unavailable")
			}
			if _, err := b.ws.terminalVolumeControl(r.Context(), id, object{"auto_volume": false, "volume": v}); err != nil {
				return nil, err
			}
		}
		return b.terminalEntry(t, false), nil
	case http.MethodDelete:
		if _, ok := b.terminalByID(id); !ok {
			return nil, problem(404, "Terminal not found")
		}
		if err := b.terminals.save(id, terminalRecord{}, true); err != nil {
			return nil, problem(502, "Cannot delete terminal")
		}
		_, _ = b.db.ExecContext(r.Context(), `DELETE FROM terminal_prefs WHERE terminal_id=?`, id)
		if b.ws != nil {
			b.ws.disconnectTerminal(id)
		}
		w.WriteHeader(204)
		return nil, nil
	}
	return nil, problem(404, "Not found")
}

func (b *larkServer) attachTerminal(ctx context.Context, termID, compID string) (object, error) {
	termID = strings.ToLower(termID)
	compID = strings.ToLower(compID)
	if !terminalIDRE.MatchString(termID) {
		return nil, problem(400, "Invalid terminal ID")
	}
	if !validCompanionID(compID) {
		return nil, problem(400, "Invalid companion ID")
	}
	t, ok := b.terminalByID(termID)
	if !ok {
		return nil, problem(404, "Terminal not found")
	}
	if _, ok := b.companionByID(compID); !ok {
		return nil, problem(404, "Companion not found")
	}
	t.CompanionID = compID
	if err := b.terminals.save(t.ID, t, false); err != nil {
		return nil, problem(502, "Cannot attach terminal")
	}
	if b.ws != nil {
		b.ws.attachTerminal(t.ID, compID)
	}
	return b.terminalEntry(t, false), nil
}

func (b *larkServer) detachTerminal(ctx context.Context, termID string) (object, error) {
	termID = strings.ToLower(termID)
	if !terminalIDRE.MatchString(termID) {
		return nil, problem(400, "Invalid terminal ID")
	}
	t, ok := b.terminalByID(termID)
	if !ok {
		return nil, problem(404, "Terminal not found")
	}
	t.CompanionID = ""
	if err := b.terminals.save(t.ID, t, false); err != nil {
		return nil, problem(502, "Cannot detach terminal")
	}
	if b.ws != nil {
		b.ws.attachTerminal(t.ID, "")
	}
	return b.terminalEntry(t, false), nil
}
