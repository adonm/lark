package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	mcpProtocol = "2025-11-25"
	mcpLimit    = 128 << 10
	exitNode    = "lark-exit"

	mcpInstructions = "Use exact companion IDs. state=snapshot, patch=update companion/terminal/memory/timer, story=Twee 3, library=instant books/facts, emit=Delta Chat/say/audio. Only final prose is spoken."
)

type rpcRequest struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func rpcError(id json.RawMessage, code int, message string) object {
	return object{"jsonrpc": "2.0", "id": id, "error": object{"code": code, "message": message}}
}

func validRPCID(id json.RawMessage) bool {
	if len(id) == 0 {
		return true
	}
	switch id[0] {
	case '"', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return true
	}
	return false
}

func (b *larkServer) serveMCP(w http.ResponseWriter, r *http.Request) {
	if b.token == "" || !tokenHeaderMatches(r.Header, "Token", b.token) &&
		!tokenMatches(r.Header.Values("Authorization"), "Bearer "+b.token) {
		sendError(w, problem(401, "Unauthorized"))
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" ||
			(u.Scheme != "http" && u.Scheme != "https") || !strings.EqualFold(u.Host, r.Host) {
			sendError(w, problem(403, "Unexpected MCP origin"))
			return
		}
	}
	switch r.Method {
	case http.MethodDelete:
		w.WriteHeader(204) // Stateless transport.
		return
	case http.MethodPost:
	default:
		w.Header().Set("Allow", "POST, DELETE")
		sendError(w, problem(405, "Method not allowed"))
		return
	}
	if version := r.Header.Get("MCP-Protocol-Version"); version != "" && !supportedMCP(version) {
		sendError(w, problem(400, "Unsupported MCP protocol version"))
		return
	}
	kind, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if kind != jsonType {
		sendError(w, problem(415, "Expected application/json"))
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mcpLimit))
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			sendError(w, problem(413, "MCP request too large"))
		} else {
			sendError(w, problem(400, "Cannot read MCP request"))
		}
		return
	}
	if !json.Valid(raw) {
		sendJSON(w, 400, rpcError(nil, -32700, "Parse error"))
		return
	}
	var req rpcRequest
	if json.Unmarshal(raw, &req) != nil || req.Version != "2.0" || req.Method == "" || !validRPCID(req.ID) {
		sendJSON(w, 400, rpcError(nil, -32600, "Invalid request"))
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	params, err := mcpObject(req.Params)
	if err != nil {
		sendJSON(w, 200, rpcError(req.ID, -32602, "Invalid params"))
		return
	}
	var result any
	switch req.Method {
	case "initialize":
		version := str(params["protocolVersion"])
		if !supportedMCP(version) {
			version = mcpProtocol
		}
		result = object{"protocolVersion": version, "capabilities": object{"tools": object{}},
			"serverInfo":   object{"name": "lark", "version": "2.0.0"},
			"instructions": mcpInstructions}
	case "ping":
		result = object{}
	case "tools/list":
		result = object{"tools": mcpTools}
	case "tools/call":
		name := str(params["name"])
		args, ok := params["arguments"].(object)
		if params["arguments"] == nil {
			args, ok = object{}, true
		}
		if !ok || mcpInputSchema(name) == nil {
			sendJSON(w, 200, rpcError(req.ID, -32602, "Unknown tool or invalid arguments"))
			return
		}
		out, err := b.callMCP(r.Context(), name, args)
		if err != nil {
			out = object{"error": err.Error()}
			var version *versionConflict
			if errors.As(err, &version) {
				out["version"] = version.version
			}
		}
		wire, _ := json.Marshal(out)
		result = object{"content": []object{{"type": "text", "text": string(wire)}}, "structuredContent": out, "isError": err != nil}
	default:
		sendJSON(w, 200, rpcError(req.ID, -32601, "Method not found"))
		return
	}
	sendJSON(w, 200, object{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func supportedMCP(version string) bool {
	return version == mcpProtocol || version == "2025-06-18" || version == "2025-03-26" || version == "2024-11-05"
}

func mcpObject(raw []byte) (object, error) {
	if len(raw) == 0 {
		return object{}, nil
	}
	return decodeObject(raw)
}

func closedObject(props object, required ...string) object {
	if required == nil {
		required = []string{}
	}
	return object{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

func propertySchema(schema object, key string) (object, bool) {
	if props, ok := schema["properties"].(object); ok {
		if child, ok := props[key].(object); ok {
			return child, true
		}
	}
	return nil, false
}

func screenHitIDs(s svgScreen) []string {
	ids := make([]string, 0, len(s.hits))
	seen := map[string]bool{}
	for _, h := range s.hits {
		if h.id != "" && !seen[h.id] {
			seen[h.id] = true
			ids = append(ids, h.id)
		}
	}
	return ids
}

var basicChimes = []string{"tap", "think", "intercom", "celebrate"}

var mcpTools = func() []object {
	text, number, flag := object{"type": "string"}, object{"type": "integer"}, object{"type": "boolean"}
	enum := func(items ...string) object { return object{"type": "string", "enum": items} }
	array := func(items object) object { return object{"type": "array", "items": items} }
	schema := closedObject
	tool := func(name, description string, in object) object {
		return object{"name": name, "description": description, "inputSchema": in}
	}
	notes := object{"anyOf": []object{text, array(text)}, "description": "PITCH:MS separated by spaces or an array, e.g. C5:150 E5:150"}
	chimes := append(slices.Clone(basicChimes), slices.Sorted(maps.Keys(sfxPresets))...)
	voices := make([]string, 0, len(voiceCatalog))
	for _, voice := range voiceCatalog {
		voices = append(voices, voice.ID)
	}
	voice := enum(voices...)
	memory := schema(object{"op": enum("set", "delete"), "scope": text, "key": text, "value": object{},
		"expected_version": object{"type": "integer", "minimum": 0, "description": "Require this version; 0 means absent"}}, "scope", "key")
	return []object{
		tool("state", "Read live state slice. companion_id adds me.companion, me.terminals, me.contacts.",
			schema(object{"companion_id": text, "include": array(enum(stateSlices...)), "memory_scopes": array(text),
				"messages_limit":  object{"type": "integer", "minimum": 1, "maximum": 30, "default": 10},
				"messages_cursor": schema(object{"before_ts": number, "before_id": text}, "before_ts", "before_id")})),
		tool("patch", "Update companion, terminal, timer, or atomic memory. Portraits use companion.portrait with form.",
			schema(object{"companion_id": text,
				"terminal": schema(object{"terminal_id": text, "volume": object{"type": "integer", "minimum": 0, "maximum": 100}, "auto_volume": flag, "sleep": flag}),
				"companion": schema(object{"op": enum("update", "reset_blank"), "name": text, "form": text,
					"personality": text, "voice": voice, "portrait": portraitSchema, "configured": flag,
					"listen_seconds": object{"type": "integer", "minimum": 0, "maximum": 300}}),
				"memory": object{"anyOf": []object{memory, array(memory)}},
				"timer":  schema(object{"op": enum("set", "update", "cancel"), "companion_id": text, "id": text, "all": flag, "in_seconds": number, "at": text, "label": text, "say": text, "chime": enum(basicChimes...), "notes": notes, "wake_agent": flag})})),
		tool("story", "Create, update, get, or delete a Twee 3 story (validates links, reachability, <=3 choices, <=600 chars).",
			schema(object{"op": enum("create", "update", "delete", "get"), "id": text, "twee": object{"type": "string", "maxLength": 65536}, "author": text}, "op", "id")),
		tool("library", "ALWAYS search first for a fact, story, or book question (Simple Wikipedia, Wikipedia, Gutenberg); instant, no page fetch. Then read or list; for a requested story, import it to open on the reader.",
			schema(object{"op": enum("search", "read", "list", "import"), "query": text,
				"source": enum("auto", "simple", "enwiki", "gutenberg"), "title": text, "ref": text,
				"section": object{"type": "integer", "minimum": 0}, "companion_id": text, "open": flag,
				"limit": object{"type": "integer", "minimum": 1, "maximum": 200}}, "op")),
		tool("emit", "Send Delta Chat intercom, speak aloud on terminals (say), or play notes/sfx/chime.",
			schema(object{"to": text, "say": text, "intercom": schema(object{"from": text, "text": text}, "from", "text"),
				"notes": notes, "sfx": enum(chimes...), "chime": enum(chimes...)}, "to")),
	}
}()

func mcpInputSchema(name string) object {
	for _, tool := range mcpTools {
		if tool["name"] == name {
			return tool["inputSchema"].(object)
		}
	}
	return nil
}

// Check closed objects against the published schema before dispatch. Leave
// value constraints and operation-specific fields to the runtime decoders.
func checkMCPFields(value any, schema object, path string) error {
	if branches, ok := schema["anyOf"].([]object); ok {
		for _, branch := range branches {
			if err := checkMCPFields(value, branch, path); err != nil {
				return err
			}
		}
	}
	switch v := value.(type) {
	case object:
		if schema["additionalProperties"] == false {
			for _, key := range slices.Sorted(maps.Keys(v)) {
				child, ok := propertySchema(schema, key)
				if !ok {
					return fieldError(path+key, "unknown argument")
				}
				if err := checkMCPFields(v[key], child, path+key+"."); err != nil {
					return err
				}
			}
		}
	case []any:
		if item, ok := schema["items"].(object); ok {
			for _, value := range v {
				if err := checkMCPFields(value, item, path); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (h *wsHub) terminalStatus(entry object, id string, full bool) {
	key := strings.ToLower(id)
	entry["ws_connected"], entry["ws"], entry["mode"] = false, false, "offline"
	entry["battery_pct"], entry["volume"], entry["scene_rev"] = nil, nil, uint64(0)
	entry["sleeping"] = false
	h.mu.Lock()
	if d := h.terminals[key]; d != nil && h.active(d) {
		entry["ws_connected"], entry["ws"], entry["online"], entry["mode"] = true, true, true, d.pose.mood
		entry["volume"], entry["scene_rev"], entry["firmware"] = d.vol, d.rev, d.firmware
		entry["caps"] = d.caps
		entry["auto_volume"], entry["sleeping"] = d.autoVol, d.asleep
		if d.story != nil {
			entry["story"] = d.story.title
		}
		if d.board != "" {
			entry["board"], entry["shape"], entry["width"], entry["height"] = d.board, d.screen.shape, d.screen.width, d.screen.height
		}
		if d.caps.Battery && d.pct >= 0 {
			entry["battery_pct"] = d.pct
		}
		if full {
			entry["battery_mv"], entry["charging"], entry["has_battery"] = d.telemetry.MV, d.telemetry.Charge, d.telemetry.HasBattery
			entry["wifi_rssi"], entry["cpu_pct"] = d.telemetry.WiFi, d.telemetry.CPU
			title := d.scene.title
			if title == "" {
				title = "Companion"
			}
			entry["screen"] = object{"title": title, "shape": d.screen.shape, "rev": d.rev, "hit_ids": screenHitIDs(d.svg)}
			entry["width"], entry["height"] = d.screen.width, d.screen.height
		}
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()
	auto, level := h.b.volumePrefs(id)
	entry["auto_volume"] = auto
	if !auto {
		entry["volume"] = level
	}
}

func mcpUint(args object, key string) (*uint64, error) {
	if v, ok := args[key]; ok {
		if n, valid := v.(json.Number); valid {
			if num, err := strconv.ParseUint(string(n), 10, 64); err == nil {
				return &num, nil
			}
		}
		// Direct unit tests and internal callers may pass native integers.
		switch n := v.(type) {
		case int:
			if n >= 0 {
				num := uint64(n)
				return &num, nil
			}
		case int64:
			if n >= 0 {
				num := uint64(n)
				return &num, nil
			}
		case uint64:
			return &n, nil
		}
		return nil, problem(400, "Invalid "+key)
	}
	return nil, nil
}

func (b *larkServer) callMCP(ctx context.Context, name string, args object) (object, error) {
	if err := checkMCPFields(args, mcpInputSchema(name), ""); err != nil {
		return nil, err
	}
	switch name {
	case "state":
		return b.mcpState(ctx, args)
	case "patch":
		return b.mcpPatch(ctx, args)
	case "story":
		return b.mcpStory(ctx, args)
	case "library":
		return b.mcpLibrary(ctx, args)
	case "emit":
		return b.mcpEmit(ctx, args)
	default:
		return nil, problem(400, "Unknown tool")
	}
}

func (b *larkServer) mcpEmit(ctx context.Context, args object) (object, error) {
	toRaw := strings.TrimSpace(str(args["to"]))
	to := strings.ToLower(toRaw)
	if toRaw == "" {
		return nil, problem(400, "Expected recipient name, companion ID, or all")
	}
	if _, hasIntercom := args["intercom"]; !hasIntercom && !validCompanionID(to) && to != "all" {
		return nil, problem(400, "Expected exact companion ID or all")
	}
	out := object{"ok": true, "to": to}
	var pcm []byte
	var kind string
	var audioField string
	for _, key := range []string{"notes", "sfx", "chime"} {
		raw, ok := args[key]
		if !ok {
			continue
		}
		if audioField != "" {
			return nil, problem(400, "Use only one of notes, sfx, or chime")
		}
		audioField = key
		if key == "notes" {
			var err error
			_, pcm, err = melodyInput(raw, "notes")
			if err != nil {
				return nil, err
			}
		} else if kind = str(raw); !validChime(kind) {
			var err error
			pcm, err = sfxPCM(kind)
			if err != nil {
				return nil, fieldError(key, "unknown kind")
			}
		}
	}
	receipts := []object{}
	if raw, ok := args["intercom"]; ok {
		i, ok := raw.(object)
		if !ok {
			return nil, fieldError("intercom", "expected from and text")
		}
		r, delivered, err := b.mcpIntercom(ctx, object{"from_companion_id": i["from"], "to_companion_id": to, "text": i["text"]})
		if err != nil {
			return nil, err
		}
		out["intercom"] = r
		receipts = append(receipts, delivered...)
	}
	if raw, ok := args["say"]; ok {
		sayText, err := spokenText(str(raw))
		if err != nil {
			return nil, err
		}
		delivered, err := b.emitSay(ctx, to, sayText, kind)
		if err != nil {
			return nil, err
		}
		receipts = append(receipts, delivered...)
		out["said"] = slices.ContainsFunc(delivered, func(receipt object) bool { return receipt["status"] == "queued" })
		if audioField == "chime" || audioField == "sfx" {
			// Chime/sfx was already delivered alongside speech.
			audioField = ""
			out["played"] = out["said"]
		}
	}
	if audioField != "" {
		delivered, err := b.emitSignal(to, audioField, kind, pcm)
		if err != nil {
			return nil, err
		}
		receipts = append(receipts, delivered...)
		out["played"] = slices.ContainsFunc(delivered, func(receipt object) bool { return receipt["status"] == "queued" })
	}
	if len(out) == 2 {
		return nil, problem(400, "Expected an action")
	}
	out["receipts"] = receipts
	return out, nil
}

func volumeCommand(args object, key string) (auto bool, level int, set bool, err error) {
	volume, err := mcpUint(args, "volume")
	if err != nil || volume != nil && *volume > 100 {
		return false, 0, false, problem(400, "Volume must be an integer from 0 to 100")
	}
	if _, ok := args[key]; ok {
		auto, err = companionBool(args, key, false)
		if err != nil {
			return false, 0, false, err
		}
		if auto && volume != nil {
			return false, 0, false, problem(400, "Use auto without manual volume")
		}
	}
	if auto {
		return true, initialAutoVolume, true, nil
	}
	if volume != nil {
		return false, int(*volume), true, nil
	}
	if _, present := args[key]; present {
		return false, -1, true, nil // Hold the current level when disabling auto-volume.
	}
	return false, 0, false, nil
}

func (h *wsHub) terminalVolumeControl(ctx context.Context, id string, args object) (object, error) {
	auto, level, setVolume, err := volumeCommand(args, "auto_volume")
	if err != nil {
		return nil, err
	}
	sleep, setSleep := args["sleep"]
	asleep, isBool := sleep.(bool)
	if setSleep && !isBool {
		return nil, problem(400, "Sleep must be a boolean")
	}
	if !setVolume && !setSleep {
		return nil, problem(400, "Expected volume, auto_volume, or sleep")
	}
	id = strings.ToLower(id)
	if !setSleep {
		if _, ok := h.b.terminalByID(id); !ok {
			return nil, problem(404, "Terminal not found")
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.terminals[id]
	if d == nil || !h.active(d) {
		if setSleep {
			return nil, problem(409, "Terminal is not connected")
		}
		h.mu.Unlock()
		h.settingsMu.Lock()
		if level < 0 {
			_, level = h.b.volumePrefs(id)
		}
		err := h.b.saveVolumePrefs(ctx, id, auto, level)
		h.settingsMu.Unlock()
		h.mu.Lock()
		if err != nil {
			return nil, err
		}
		return object{"ok": true, "terminal_id": id, "volume": level, "auto_volume": auto, "sleeping": false}, nil
	}
	if setVolume {
		if err := h.setVolume(ctx, d, auto, level, ""); err != nil {
			return nil, err
		}
	}
	if setSleep {
		now := time.Now()
		d.last = now
		h.setSleeping(d, asleep)
		h.render(d, now, false)
	}
	return object{"ok": true, "terminal_id": d.id, "companion_id": d.companionID, "volume": d.vol, "auto_volume": d.autoVol, "sleeping": d.asleep}, nil
}

func validChime(kind string) bool {
	return slices.Contains(basicChimes, kind)
}

func (h *wsHub) chime(d *wsTerminal, kind string) bool {
	if kind == "celebrate" {
		kind = "intercom" // Existing firmware's happy tone.
	}
	if code, ok := encodeChimePayload(kind); ok {
		return d.client.enqueueProto(msgChime, []byte{code})
	}
	return false
}

func deliveryReceipt(action, id, reason string) object {
	r := object{"companion_id": id, "action": action, "status": "queued"}
	if reason != "" {
		r["status"], r["reason"] = "skipped", reason
	}
	return r
}

func (h *wsHub) signalTarget(companionID string) (*wsTerminal, string) {
	d := h.voiceOwner(companionID)
	if d == nil {
		return nil, "not_connected"
	}
	return d, ""
}

func (b *larkServer) emitSay(ctx context.Context, id, text, chime string) ([]object, error) {
	if b.ws == nil {
		return nil, problem(503, "Terminal hub unavailable")
	}
	ids := []string{id}
	if id == "all" {
		list, err := b.peerSnapshot("")
		if err != nil {
			return nil, err
		}
		ids = make([]string, 0, len(list))
		for _, p := range list {
			ids = append(ids, p.ID)
		}
	}
	receipts := make([]object, 0, len(ids))
	for _, cid := range ids {
		delivered := b.ws.deliverNotice(ctx, terminalNotice{
			recipientIDs: []string{cid},
			speakText:    text,
			voice:        b.voiceFor(cid),
			chime:        chime,
			showCard:     false,
		})
		if len(delivered) == 0 {
			receipts = append(receipts, deliveryReceipt("say", cid, "not_connected"))
		} else {
			receipts = append(receipts, deliveryReceipt("say", cid, ""))
		}
	}
	return receipts, nil
}

func (b *larkServer) emitSignal(id, action, kind string, pcm []byte) ([]object, error) {
	if b.ws == nil {
		return nil, problem(503, "Terminal hub unavailable")
	}
	ids := []string{id}
	if id == "all" {
		list, err := b.peerSnapshot("")
		if err != nil {
			return nil, err
		}
		ids = make([]string, 0, len(list))
		for _, p := range list {
			ids = append(ids, p.ID)
		}
	}
	receipts := make([]object, 0, len(ids))
	h := b.ws
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, id := range ids {
		d, reason := h.signalTarget(id)
		if d != nil && !h.playSignal(d, kind, pcm) {
			reason = "queue_unavailable"
		}
		receipts = append(receipts, deliveryReceipt(action, id, reason))
	}
	return receipts, nil
}

func (h *wsHub) playSignal(d *wsTerminal, kind string, pcm []byte) bool {
	if len(pcm) > 0 {
		return h.playPCM(d, pcm)
	}
	return h.chime(d, kind)
}

func (b *larkServer) mcpIntercom(ctx context.Context, args object) (object, []object, error) {
	fromID := strings.ToLower(str(args["from_companion_id"]))
	sender, ok := b.companionByID(fromID)
	if !ok {
		return nil, nil, problem(404, "Companion not found")
	}
	text := shortText(args["text"], maxMessage, true)
	if _, err := spokenText(text); err != nil {
		return nil, nil, err
	}
	to := strings.TrimSpace(str(args["to_companion_id"]))
	if b.dc != nil && b.dc.ready.Load() {
		sentTo, err := b.dc.sendIntercom(ctx, sender.ID, to, text, nil)
		if err != nil {
			return nil, nil, err
		}
		receipts := make([]object, 0, len(sentTo))
		for _, target := range sentTo {
			receipts = append(receipts, deliveryReceipt("intercom", target, ""))
		}
		return object{"ok": len(sentTo) > 0, "sent_to": sentTo}, receipts, nil
	}
	to = strings.ToLower(to)
	others, err := b.peerSnapshot(sender.ID)
	if err != nil {
		return nil, nil, err
	}
	recipients := others
	if to != "all" {
		recipients = slices.DeleteFunc(others, func(p companionTarget) bool { return p.ID != to })
	}
	if len(recipients) == 0 && to != "all" {
		return nil, nil, problem(404, "Recipient not found")
	}
	out := b.deliver(ctx, sender, recipients, text, nil)
	receipts := make([]object, 0, len(recipients))
	for _, p := range recipients {
		reason := ""
		if !slices.Contains(out.sent, p.ID) {
			reason = "not_connected"
			if h := b.ws; h != nil {
				h.mu.Lock()
				_, reason = h.signalTarget(p.ID)
				h.mu.Unlock()
				if reason == "" {
					reason = "queue_unavailable"
				}
			}
		}
		receipts = append(receipts, deliveryReceipt("intercom", p.ID, reason))
	}
	return object{"ok": len(out.sent) > 0, "sent_to": out.sent, "delivered_to": out.delivered, "failed_to": out.failed}, receipts, nil
}

func fieldError(path, format string, args ...any) error {
	return problem(400, path+": "+fmt.Sprintf(format, args...))
}
