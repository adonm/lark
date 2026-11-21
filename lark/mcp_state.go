package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"
)

var stateSlices = []string{"devices", "companions", "apps", "timers", "memory", "saved_companions", "messages", "diagnostics"}

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
	id := str(args["device_id"])
	include := stateSlices[:5]
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
	if id != "" || slices.Contains(include, "devices") || slices.Contains(include, "companions") {
		if err := b.mcpDeviceState(out, id, include); err != nil {
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
		case "saved_companions":
			out[key], err = b.savedCompanionState(ctx)
		case "memory":
			scopes := []string{"shared"}
			if id != "" && id != "shared" {
				scopes = append(scopes, id)
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
				return nil, fieldError("device_id", "required for messages")
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

func (b *larkServer) mcpDeviceState(out object, id string, include []string) error {
	wantDevices, wantCompanions := slices.Contains(include, "devices"), slices.Contains(include, "companions")
	var list []object
	if wantDevices || wantCompanions {
		var err error
		list, err = b.deviceEntries(true)
		if err != nil {
			return err
		}
	} else if id != "" {
		p, v, err := b.findDevice(id)
		if err != nil {
			return err
		}
		list = []object{b.deviceEntry(p, v, true)}
	}
	devices, companions := object{}, object{}
	for _, entry := range list {
		key := str(entry["device_id"])
		p := object{"device_id": key, "name": entry["character"]}
		for _, field := range []string{"kind", "board", "online", "ws_connected", "width", "height", "mode", "sleeping", "volume", "auto_volume", "voice", "battery_pct",
			"screen", "active_app", "last_app_error", "can_call", "call", "wifi_rssi", "cpu_pct"} {
			p[field] = entry[field]
		}
		if state, ok := entry["active_app_state"]; ok {
			p["active_app_state"] = state
		}
		if wantDevices {
			devices[key] = p
		}
		if wantCompanions {
			companions[key] = entry["companion"]
		}
		if strings.EqualFold(key, id) {
			p["parent_policy"] = b.parentState(key, time.Now())
			out["me"] = object{"device": p, "companion": entry["companion"], "active_app": p["active_app"]}
		}
	}
	if id != "" && out["me"] == nil {
		return problem(404, "Device not found")
	}
	if wantDevices {
		out["devices"] = devices
	}
	if wantCompanions {
		out["companions"] = companions
	}
	return nil
}

func (b *larkServer) mcpPatch(ctx context.Context, args object) (object, error) {
	id := str(args["device_id"])
	out := object{"ok": true}
	// Apply appearance before changing sleep, and leave sleep alone on appearance-only edits.
	for _, key := range []string{"companion", "device", "memory", "timer", "apps"} {
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
			case "device":
				out[key], err = b.patchDevice(ctx, id, edit)
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
		return nil, problem(400, "Expected a device, companion, memory, timer, or saved app patch")
	}
	return out, nil
}

func (b *larkServer) patchDevice(ctx context.Context, id string, args object) (object, error) {
	if _, err := b.lookupDevice(id); err != nil {
		return nil, err
	}
	voice, hasVoice := args["voice"]
	if hasVoice && !b.voices.allowed(str(voice)) {
		return nil, problem(400, "Unsupported voice")
	}
	if b.ws == nil {
		return nil, problem(503, "Device hub unavailable")
	}
	if len(args) == 0 {
		return nil, problem(400, "Expected a device setting")
	}
	if !hasVoice || len(args) > 1 {
		if _, err := b.ws.mcpControl(ctx, id, args); err != nil {
			return nil, err
		}
	}
	if hasVoice {
		if err := b.ws.saveVoice(id, str(voice)); err != nil {
			return nil, err
		}
	}
	out := object{}
	if err := b.mcpDeviceState(out, id, nil); err != nil {
		return nil, err
	}
	return out["me"].(object)["device"].(object), nil
}
