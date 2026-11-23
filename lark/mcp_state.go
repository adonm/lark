package main

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"
)

var stateSlices = []string{"companions", "terminals", "timers", "stories", "memory", "messages", "diagnostics"}

func stringList(raw any, key string) ([]string, error) {
	items, ok := raw.([]any)
	if !ok {
		return nil, fieldError(key, "expected an array of strings")
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, fieldError(key, "expected an array of strings")
		}
		out = append(out, s)
	}
	return out, nil
}

func (b *larkServer) mcpState(ctx context.Context, args object) (object, error) {
	loc := b.location()
	now := time.Now().In(loc)
	id := strings.ToLower(str(args["companion_id"]))
	include := stateSlices[:3]
	var err error
	out := object{"now": object{"local": formatLocalTimeIn(now, loc), "rfc3339": now.Format(time.RFC3339), "unix_ms": now.UnixMilli()}}
	if raw, ok := args["include"]; ok {
		include, err = stringList(raw, "include")
		if err != nil {
			return nil, err
		}
	}
	for _, key := range include {
		if !slices.Contains(stateSlices, key) {
			return nil, fieldError("include", "unknown slice %q", key)
		}
	}
	if id != "" || slices.Contains(include, "companions") || slices.Contains(include, "terminals") {
		if err := b.mcpCompanionState(ctx, out, id, include); err != nil {
			return nil, err
		}
	}
	for _, key := range include {
		if _, exists := out[key]; exists {
			continue
		}
		switch key {
		case "stories":
			out[key], err = b.storiesState(ctx)
		case "timers":
			out[key], err = b.timerState(ctx, id, now)
		case "memory":
			scopes := []string{"shared"}
			if id != "" && id != "shared" {
				scopes = append(scopes, "companion:"+id)
			}
			if raw, ok := args["memory_scopes"]; ok {
				scopes, err = stringList(raw, "memory_scopes")
				if err != nil {
					return nil, err
				}
			}
			out[key], err = b.memoryState(ctx, scopes)
		case "messages":
			if id == "" {
				return nil, fieldError("companion_id", "required for messages")
			}
			limit := 10
			n, nErr := mcpUint(args, "messages_limit")
			if nErr != nil || n != nil && (*n < 1 || *n > 30) {
				return nil, fieldError("messages_limit", "expected 1–30")
			}
			if n != nil {
				limit = int(*n)
			}
			var before *historyCursor
			if raw, exists := args["messages_cursor"]; exists {
				cursor, _ := raw.(object)
				ts, ok := cursor["before_ts"].(json.Number)
				msg, valid := cursor["before_id"].(string)
				n, parseErr := ts.Int64()
				if !ok || parseErr != nil || !valid || msg == "" || len(msg) > 512 {
					return nil, fieldError("messages_cursor", "expected before_ts (Unix milliseconds) and before_id")
				}
				before = &historyCursor{TS: n, ID: msg}
			}
			page, readErr := b.readHistoryPage(ctx, id, limit, before)
			out[key], out["messages_has_more"], out["messages_next_cursor"], err = page.Messages, page.HasMore, page.Next, readErr
		case "diagnostics":
			out[key] = b.diagnosticReport()
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (b *larkServer) mcpCompanionState(ctx context.Context, out object, id string, include []string) error {
	wantCompanions, wantTerminals := slices.Contains(include, "companions"), slices.Contains(include, "terminals")
	var list []companionState
	if wantCompanions {
		var err error
		list, err = b.allCompanions(ctx)
		if err != nil {
			return err
		}
	} else if id != "" {
		c, ok := b.companionByID(id)
		if !ok {
			return problem(404, "Companion not found")
		}
		list = []companionState{c}
	}
	companions := object{}
	for _, c := range list {
		entry := b.companionEntry(c, true)
		if wantCompanions {
			companions[c.ID] = entry
		}
		if strings.EqualFold(c.ID, id) {
			compObj := object{
				"id":             c.ID,
				"name":           c.Name,
				"configured":     c.Configured,
				"personality":    c.Personality,
				"form":           c.Form,
				"voice":          entry["voice"],
				"portrait_id":    c.PortraitID,
				"listen_seconds": c.ListenSeconds,
				"online":         entry["online"],
				"dormant":        entry["dormant"],
			}
			termList := []object{}
			if b.ws != nil {
				b.ws.mu.Lock()
				compTerminals := b.ws.terminalsFor(c.ID)
				best := b.ws.voiceOwner(c.ID)
				mode, sleeping := "offline", false
				for _, t := range compTerminals {
					if t == best {
						mode, sleeping = t.pose.mood, t.asleep
					}
				}
				b.ws.mu.Unlock()
				compObj["mode"] = mode
				compObj["sleeping"] = sleeping
			}
			dbTerms, _ := b.terminalsForCompanion(c.ID)
			for _, dt := range dbTerms {
				te := b.terminalEntry(dt, true)
				termList = append(termList, object{
					"id":     dt.ID,
					"kind":   dt.Kind,
					"online": te["online"],
					"caps":   te["caps"],
					"geometry": object{
						"width":  te["width"],
						"height": te["height"],
						"shape":  te["shape"],
						"board":  te["board"],
					},
					"battery": object{
						"battery_pct": te["battery_pct"],
						"charging":    te["charging"],
						"has_battery": te["has_battery"],
					},
					"wifi": object{
						"wifi_rssi": te["wifi_rssi"],
					},
					"volume": object{
						"volume":      te["volume"],
						"auto_volume": te["auto_volume"],
					},
				})
			}
			contacts := []object{}
			seenPeers := make(map[string]bool)
			if b.dc != nil && b.dc.ready.Load() {
				if list, err := b.dc.listAllContacts(c.ID); err == nil {
					for _, ct := range list {
						entry := object{
							"name":     ct.Name,
							"kind":     ct.Kind,
							"verified": ct.Verified,
						}
						if ct.ID != "" {
							entry["id"] = ct.ID
							seenPeers[ct.ID] = true
						}
						contacts = append(contacts, entry)
					}
				}
			}
			if peers, err := b.peerSnapshot(c.ID); err == nil {
				for _, p := range peers {
					if !seenPeers[p.ID] {
						contacts = append(contacts, object{
							"id":       p.ID,
							"name":     p.Name,
							"kind":     "companion",
							"verified": true,
						})
					}
				}
			}
			out["me"] = object{
				"companion": compObj,
				"terminals": termList,
				"contacts":  contacts,
			}
		}
	}
	if id != "" && out["me"] == nil {
		return problem(404, "Companion not found")
	}
	if wantCompanions {
		out["companions"] = companions
	}
	if wantTerminals {
		terms, err := b.terminalList(true)
		if err != nil {
			return err
		}
		termMap := object{}
		for _, t := range terms {
			termMap[str(t["id"])] = t
		}
		out["terminals"] = termMap
	}
	return nil
}

func (b *larkServer) mcpPatch(ctx context.Context, args object) (object, error) {
	id := strings.ToLower(str(args["companion_id"]))
	out := object{"ok": true}
	// Apply appearance before changing sleep, and leave sleep alone on appearance-only edits.
	for _, key := range []string{"companion", "terminal", "memory", "timer"} {
		raw, ok := args[key]
		if !ok {
			continue
		}
		var err error
		if key == "memory" {
			out[key], err = b.patchMemory(ctx, raw)
		} else {
			edit, ok := raw.(object)
			if !ok {
				return nil, fieldError(key, "expected an object")
			}
			switch key {
			case "companion":
				if id == "" {
					return nil, fieldError("companion_id", "required; pass the companion ID from the turn header")
				}
				out[key], err = b.patchCompanion(ctx, id, edit)
			case "terminal":
				if id == "" && strings.TrimSpace(str(edit["terminal_id"])) == "" {
					return nil, fieldError("companion_id", "required; pass the companion ID from the turn header")
				}
				out[key], err = b.patchTerminal(ctx, id, edit)
			case "timer":
				if id == "" && strings.TrimSpace(str(edit["companion_id"])) == "" {
					return nil, fieldError("companion_id", "required; pass the companion ID from the turn header")
				}
				out[key], err = b.patchTimer(ctx, id, edit)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if len(out) == 1 {
		return nil, problem(400, "Expected a terminal, companion, memory, or timer patch")
	}
	return out, nil
}

func (b *larkServer) patchTerminal(ctx context.Context, companionID string, args object) (object, error) {
	termID := strings.ToLower(str(args["terminal_id"]))
	if termID == "" {
		if !validCompanionID(companionID) {
			return nil, problem(400, "Invalid companion ID")
		}
		if b.ws != nil {
			b.ws.mu.Lock()
			if t := b.ws.voiceOwner(companionID); t != nil {
				termID = t.id
			}
			b.ws.mu.Unlock()
		}
		if termID == "" {
			terms, _ := b.terminalsForCompanion(companionID)
			if len(terms) > 0 {
				termID = terms[0].ID
			}
		}
		if termID == "" {
			return nil, problem(409, "Companion has no attached terminal")
		}
	}
	if b.ws == nil {
		return nil, problem(503, "Terminal hub unavailable")
	}
	ctrl := maps.Clone(args)
	delete(ctrl, "terminal_id")
	if len(ctrl) == 0 {
		return nil, problem(400, "Expected a terminal setting")
	}
	return b.ws.terminalVolumeControl(ctx, termID, ctrl)
}
