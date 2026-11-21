package main

import (
	"context"
	"database/sql"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

var virtualID = regexp.MustCompile(`^virt-[0-9a-f]{8}$`)

func syncVirtualCharacter(ctx context.Context, tx *sql.Tx, id, name string) error {
	if !virtualID.MatchString(id) {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE virtual_clients SET character=? WHERE id=? AND character<>?`, name, id, name)
	return err
}

type virtualClient struct {
	ID        string    `json:"id"`
	Character string    `json:"character"`
	Voice     string    `json:"voice,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func (b *larkServer) virtualSnapshot() (map[string]virtualClient, error) {
	if b.virtual == nil {
		return nil, nil
	}
	return b.virtual.all()
}

func (b *larkServer) virtualByID(id string) (virtualClient, bool) {
	if b.virtual == nil {
		return virtualClient{}, false
	}
	v, ok, _ := b.virtual.get(id)
	return v, ok
}

func (b *larkServer) virtualAPI(w http.ResponseWriter, r *http.Request, path string) (any, error) {
	if b.virtual == nil {
		return nil, problem(503, "Virtual clients unavailable")
	}
	switch {
	case path == "/api/virtual-clients" && r.Method == http.MethodGet:
		clients, err := b.virtualSnapshot()
		if err != nil {
			return nil, err
		}
		out := make([]object, 0, len(clients))
		for _, id := range slices.Sorted(maps.Keys(clients)) {
			out = append(out, b.publicVirtual(clients[id]))
		}
		return object{"virtual_clients": out}, nil
	case path == "/api/virtual-clients" && r.Method == http.MethodPost:
		obj, err := jsonBody(r)
		if err != nil {
			return nil, err
		}
		name := strings.TrimSpace(str(obj["character"]))
		if value, supplied := obj["character"]; supplied {
			if _, ok := value.(string); !ok || name != "" && !characterRE.MatchString(name) {
				return nil, problem(400, "Invalid character name")
			}
		}
		voice := str(obj["voice"])
		if _, supplied := obj["voice"]; supplied && !b.voices.allowed(voice) {
			return nil, problem(400, "Unsupported voice")
		}
		companion := blankCompanion()
		if name != "" {
			companion.Name = name
			companion.Configured = true
			companion.FaceSVG = companion.defaultFace()
		}
		v := virtualClient{ID: "virt-" + randomHex(4), Character: companion.Name, Voice: voice, CreatedAt: time.Now().UTC()}
		ctx := r.Context()
		tx, err := b.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, problem(502, "Cannot create virtual client")
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `INSERT INTO virtual_clients (id,character,voice,created_at) VALUES (?,?,?,?)`, b.virtual.values(v.ID, v)...); err != nil {
			return nil, problem(502, "Cannot create virtual client")
		}
		if err := writeCompanion(ctx, tx, v.ID, companion); err != nil || tx.Commit() != nil {
			return nil, problem(502, "Cannot create virtual client")
		}
		sendJSON(w, 201, b.publicVirtual(v))
		return nil, nil
	case strings.HasPrefix(path, "/api/virtual-clients/") && r.Method == http.MethodDelete:
		id := strings.TrimPrefix(path, "/api/virtual-clients/")
		if b.tunnels != nil {
			b.tunnels.mu.Lock()
			defer b.tunnels.mu.Unlock()
		}
		if _, ok := b.virtualByID(id); !ok {
			return nil, problem(404, "Virtual client not found")
		}
		if b.tunnels != nil {
			if err := b.tunnels.removeLocked(r.Context(), id, false); err != nil {
				return nil, err
			}
		}
		ctx := r.Context()
		tx, err := b.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, problem(502, "Cannot delete virtual client")
		}
		defer tx.Rollback()
		for _, query := range []string{"DELETE FROM virtual_clients WHERE id=?", "DELETE FROM companions WHERE device_id=?", "DELETE FROM voices WHERE device_id=?"} {
			if _, err := tx.ExecContext(ctx, query, id); err != nil {
				return nil, problem(502, "Cannot delete virtual client")
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, problem(502, "Cannot delete virtual client")
		}
		if b.ws != nil {
			b.ws.disconnectDevice(id)
		}
		w.WriteHeader(204)
		return nil, nil
	}
	return nil, problem(404, "Not found")
}

func (b *larkServer) publicVirtual(v virtualClient) object {
	connected := false
	if b.ws != nil {
		b.ws.mu.Lock()
		d := b.ws.devices[v.ID]
		connected = d != nil && b.ws.active(d)
		b.ws.mu.Unlock()
	}
	companion := b.companionFor(peer{DeviceID: v.ID, Character: v.Character})
	out := object{"id": v.ID, "character": companion.Name, "companion": companion.companionLook, "created_at": v.CreatedAt,
		"online": connected, "ws_connected": connected, "voice": b.voices.resolve(companion.Voice, deviceTheme(companion.Name))}
	if details := b.tunnels.info(v.ID); details != nil {
		out["tunnel"] = details
	}
	return out
}

func deviceTheme(character string) string {
	if strings.EqualFold(character, "mint") {
		return "mint"
	}
	return "cake"
}

func (b *larkServer) deviceList() ([]object, error) {
	return b.deviceEntries(false)
}

func (b *larkServer) deviceEntries(full bool) ([]object, error) {
	others, err := b.peers.snapshot("")
	if err != nil {
		return nil, err
	}
	clients, err := b.virtualSnapshot()
	if err != nil {
		return nil, err
	}
	peers := make(map[string]peer, len(others)+len(clients))
	for _, p := range others {
		peers[strings.ToLower(p.DeviceID)] = p
	}
	if h := b.ws; h != nil {
		h.mu.Lock()
		for id, d := range h.devices {
			if h.active(d) {
				peers[id] = d.who
			}
		}
		h.mu.Unlock()
	}
	for id, v := range clients {
		peers[id] = peer{DeviceID: id, Character: v.Character}
	}
	list := make([]object, 0, len(peers))
	for id, p := range peers {
		list = append(list, b.deviceEntry(p, clients[id], full))
	}
	slices.SortFunc(list, func(a, b object) int { return strings.Compare(str(a["device_id"]), str(b["device_id"])) })
	return list, nil
}

func (b *larkServer) findDevice(id string) (peer, virtualClient, error) {
	if v, ok := b.virtualByID(id); ok {
		return peer{DeviceID: v.ID, Character: v.Character}, v, nil
	}
	if !virtualID.MatchString(strings.ToLower(id)) {
		if h := b.ws; h != nil {
			h.mu.Lock()
			if d := h.devices[strings.ToLower(id)]; d != nil && d.who.DeviceID == id && h.active(d) {
				p := d.who
				h.mu.Unlock()
				return p, virtualClient{}, nil
			}
			h.mu.Unlock()
		}
		if b.peers.store != nil {
			p, ok, err := b.peers.get(id)
			if err != nil {
				return peer{}, virtualClient{}, err
			}
			if ok {
				return p, virtualClient{}, nil
			}
		}
	}
	return peer{}, virtualClient{}, problem(404, "Device not found")
}

func (b *larkServer) deviceEntry(who peer, v virtualClient, full bool) object {
	id := who.DeviceID
	entry := publicPeer(who)
	entry["kind"], entry["theme"] = "physical", deviceTheme(who.Character)
	if webDeviceID(strings.ToLower(id)) {
		entry["kind"] = "virtual"
		delete(entry, "ip")
	}
	if v.ID != "" {
		delete(entry, "port")
		delete(entry, "last_seen")
	}
	companion := b.companionFor(who)
	companion.Voice = b.voices.resolve(companion.Voice, str(entry["theme"]))
	entry["character"], entry["companion"], entry["voice"] = companion.Name, companion.companionLook, companion.Voice
	if full {
		entry["companion"] = companion
	} else if details := b.tunnels.info(id); details != nil {
		entry["tunnel"] = details
	}
	if b.ws != nil {
		b.ws.deviceStatus(entry, id, full)
	} else {
		entry["auto_volume"], entry["volume"] = b.volumePrefs(id)
		entry["sleeping"], entry["ws_connected"], entry["mode"] = false, false, "offline"
	}
	return entry
}
