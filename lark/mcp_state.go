package main

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"
)

var stateSlices = []string{"companions", "terminals", "apps", "timers", "memory", "messages", "diagnostics"}

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
	now := time.Now()
	id := strings.ToLower(str(args["companion_id"]))
	include := stateSlices[:4]
	var err error
	out := object{"now": object{"local": now.Format(time.RFC3339), "unix_ms": now.UnixMilli()}}
	if raw, exists := args["app_name"]; exists {
		name, ok := raw.(string)
		if !ok {
			return nil, fieldError("app_name", "expected a string")
		}
		if name != "" {
			name, err = normalizeAppName(name, "app_name")
			if err != nil {
				return nil, err
			}
			out["app"], err = b.savedAppState(ctx, name)
			if err != nil {
				return nil, err
			}
			include = nil
		}
	}
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
		case "apps":
			code, flagErr := companionBool(args, "include_code", false)
			if flagErr != nil {
				return nil, flagErr
			}
			out[key], err = b.appState(ctx, code)
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
				"id":           c.ID,
				"name":         c.Name,
				"configured":   c.Configured,
				"face_bg":      c.FaceBG,
				"mark_color":   c.MarkColor,
				"accent_color": c.AccentColor,
				"personality":  c.Personality,
				"voice":        entry["voice"],
				"face_svg":     c.FaceSVG,
				"online":       entry["online"],
				"dormant":      entry["dormant"],
				"parent_policy": b.parentState(c.ID, time.Now()),
			}
			var activeApp any
			termList := []object{}
			if b.ws != nil {
				b.ws.mu.Lock()
				compTerminals := b.ws.terminalsFor(c.ID)
				best := b.ws.audioTerminal(c.ID)
				canCall := false
				var callSnap object
				var lastAppErr object
				var activeAppState any
				mode, sleeping := "offline", false
				for _, t := range compTerminals {
					if t.caps.Call {
						canCall = true
					}
					if t.callState != callNone && callSnap == nil {
						callSnap = b.ws.callSnapshot(t)
					}
					if t.lastAppError != nil && lastAppErr == nil {
						lastAppErr = t.lastAppError
					}
					if t.app != nil && activeApp == nil {
						activeApp = b.ws.activeAppState(t.app)
						activeAppState = t.app.snapshot
					}
					if t == best {
						mode, sleeping = t.pose.mood, t.asleep
					}
				}
				b.ws.mu.Unlock()
				compObj["can_call"] = canCall
				compObj["call"] = callSnap
				compObj["last_app_error"] = lastAppErr
				compObj["active_app"] = activeApp
				if activeAppState != nil {
					compObj["active_app_state"] = activeAppState
				}
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
						"battery_mv":  te["battery_mv"],
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
			out["me"] = object{
				"companion":  compObj,
				"terminals":  termList,
				"active_app": activeApp,
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
	for _, key := range []string{"companion", "terminal", "memory", "timer", "apps"} {
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
				out[key], err = b.patchCompanion(ctx, id, edit)
			case "terminal":
				out[key], err = b.patchTerminal(ctx, id, edit)
			case "timer":
				out[key], err = b.patchTimer(ctx, id, edit)
			case "apps":
				out[key], err = b.patchSavedApps(ctx, edit)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if len(out) == 1 {
		return nil, problem(400, "Expected a terminal, companion, memory, timer, or saved app patch")
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
			if t := b.ws.audioTerminal(companionID); t != nil {
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
