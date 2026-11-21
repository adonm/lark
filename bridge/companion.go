package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
)

type companionLook struct {
	Name        string `json:"name"`
	Configured  bool   `json:"configured"`
	FaceBG      string `json:"face_bg"`
	MarkColor   string `json:"mark_color"`
	AccentColor string `json:"accent_color"`
	Personality string `json:"personality"`
}

type companionState struct {
	companionLook
	Voice   string `json:"voice"`
	FaceSVG string `json:"face_svg"`
}

type savedCompanion struct {
	companionState
	Slug      string `json:"slug"`
	UpdatedAt int64  `json:"updated_at"`
}

func blankCompanion() companionState {
	p := storybookPalettes["peach"]
	c := companionState{companionLook: companionLook{Name: "New Cat", FaceBG: p.bg, MarkColor: p.furShadow, AccentColor: p.blush}}
	c.FaceSVG = c.defaultFace()
	return c
}

func (c companionState) defaultFace() string {
	return defaultFaceSVG(c.FaceBG, c.MarkColor, c.AccentColor, c.Configured)
}

var (
	companionSlugRE    = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	companionSlugSpace = regexp.MustCompile(`[^a-z0-9]+`)
)

func companionSlug(name string) string {
	return strings.Trim(companionSlugSpace.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

const activeCompanionColumns = `name,configured,face_bg,mark_color,accent_color,personality,face_svg`
const savedCompanionColumns = `slug,name,face_bg,mark_color,accent_color,personality,voice,face_svg,updated_at`
const companionVoiceSQL = `COALESCE((SELECT voice FROM voices WHERE device_id=?),(SELECT voice FROM virtual_clients WHERE id=?),'')`

func readCompanion(row rowScanner) (companionState, error) {
	var c companionState
	var configured int
	err := row.Scan(&c.Name, &configured, &c.FaceBG, &c.MarkColor, &c.AccentColor, &c.Personality, &c.FaceSVG, &c.Voice)
	c.Configured = configured != 0
	if err == nil && c.FaceSVG == "" {
		c.FaceSVG = c.defaultFace()
	}
	return c, err
}

func readSavedCompanion(row rowScanner) (savedCompanion, error) {
	var p savedCompanion
	p.Configured = true
	err := row.Scan(&p.Slug, &p.Name, &p.FaceBG, &p.MarkColor, &p.AccentColor, &p.Personality, &p.Voice, &p.FaceSVG, &p.UpdatedAt)
	p.Voice = storedVoice(p.Voice)
	if err == nil && p.FaceSVG == "" {
		p.FaceSVG = p.defaultFace()
	}
	return p, err
}

func writeCompanion(ctx context.Context, tx *sql.Tx, id string, c companionState) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO companions (device_id,`+activeCompanionColumns+`,updated_at) VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(device_id) DO UPDATE SET name=excluded.name,configured=excluded.configured,face_bg=excluded.face_bg,
		mark_color=excluded.mark_color,accent_color=excluded.accent_color,personality=excluded.personality,
		face_svg=excluded.face_svg,updated_at=max(companions.updated_at+1,excluded.updated_at)`,
		id, c.Name, c.Configured, c.FaceBG, c.MarkColor, c.AccentColor, c.Personality, c.FaceSVG, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	return syncVirtualCharacter(ctx, tx, id, c.Name)
}

func saveCompanionProfile(ctx context.Context, tx *sql.Tx, c companionState, replace bool) error {
	slug := companionSlug(c.Name)
	if slug == "" {
		return nil
	}
	conflict := `ON CONFLICT(slug) DO NOTHING`
	if replace {
		conflict = `ON CONFLICT(slug) DO UPDATE SET name=excluded.name,face_bg=excluded.face_bg,mark_color=excluded.mark_color,
			accent_color=excluded.accent_color,personality=excluded.personality,voice=excluded.voice,
			face_svg=excluded.face_svg,updated_at=excluded.updated_at`
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO saved_companions (`+savedCompanionColumns+`) VALUES (?,?,?,?,?,?,?,?,?) `+conflict,
		slug, c.Name, c.FaceBG, c.MarkColor, c.AccentColor, c.Personality, c.Voice, c.FaceSVG, time.Now().UnixMilli())
	return err
}

type companionQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (b *bridge) readActiveCompanion(ctx context.Context, q companionQuery, who peer) (companionState, error) {
	id := strings.ToLower(who.DeviceID)
	c, err := readCompanion(q.QueryRowContext(ctx, `SELECT `+activeCompanionColumns+`,`+
		companionVoiceSQL+` FROM companions WHERE device_id=?`, id, id, id))
	if errors.Is(err, sql.ErrNoRows) {
		c = blankCompanion()
		if who.Character != "" && who.Character != c.Name && !strings.EqualFold(who.Character, id) && !webDeviceID(id) {
			c.Name = who.Character
			c.Configured = true
			c.FaceSVG = c.defaultFace()
		}
		err = q.QueryRowContext(ctx, "SELECT "+companionVoiceSQL, id, id).Scan(&c.Voice)
	}
	c.Voice = storedVoice(c.Voice)
	return c, err
}

func (b *bridge) companionFor(who peer) companionState {
	if b.db != nil {
		if c, err := b.readActiveCompanion(context.Background(), b.db, who); err == nil {
			return c
		}
	}
	return blankCompanion()
}

func updateCompanion(c companionState, args object, voices deviceVoices) (companionState, bool, error) {
	before := c
	save, err := companionBool(args, "save_profile", false)
	if err != nil {
		return c, false, err
	}
	editLook := false
	for key, val := range args {
		switch key {
		case "name":
			name := strings.TrimSpace(str(val))
			if !characterRE.MatchString(name) {
				return c, false, problem(400, "Invalid companion name (1–40 characters)")
			}
			c.Name, editLook = name, true
		case "face_bg", "mark_color", "accent_color":
			tint := color(val)
			if tint == "" {
				return c, false, problem(400, "Expected #RRGGBB for "+key)
			}
			switch key {
			case "face_bg":
				c.FaceBG = tint
			case "mark_color":
				c.MarkColor = tint
			case "accent_color":
				c.AccentColor = tint
			}
			editLook = true
		case "personality":
			vibe, ok := val.(string)
			if !ok || runeLen(vibe) > 160 || strings.ContainsAny(vibe, "[]") || strings.ContainsFunc(vibe, func(r rune) bool { return !unicode.IsPrint(r) }) {
				return c, false, problem(400, "Expected a single-line personality of at most 160 characters, without brackets")
			}
			c.Personality = strings.TrimSpace(vibe)
		case "voice":
			if !voices.allowed(str(val)) {
				return c, false, problem(400, "Unsupported voice")
			}
			c.Voice = str(val)
		case "face_svg", "face_edits", "preset":
			editLook = true
		case "configured", "save_profile", "device_id", "op":
		default:
			return c, false, problem(400, "Unknown companion field "+key)
		}
	}
	c.Configured, err = companionBool(args, "configured", c.Configured || editLook)
	if err != nil {
		return c, false, err
	}
	oldPalette := [...]string{before.FaceBG, before.MarkColor, before.AccentColor}
	newPalette := [...]string{c.FaceBG, c.MarkColor, c.AccentColor}
	edits, editParts := args["face_edits"]
	if raw, set := args["preset"]; set {
		for _, field := range []string{"face_svg", "face_bg", "mark_color", "accent_color"} {
			if _, exists := args[field]; exists {
				return c, false, fieldError("companion.preset", "cannot combine with %s", field)
			}
		}
		art, p, err := compileCompanion(raw)
		if err != nil {
			return c, false, err
		}
		c.FaceBG, c.MarkColor, c.AccentColor = p.bg, p.furShadow, p.blush
		if editParts {
			c.FaceSVG, err = editFaceParts(art, edits)
		} else {
			c.FaceSVG, err = sanitizeFaceSVG(art)
		}
		return c, save, err
	} else if raw, set := args["face_svg"]; set {
		if editParts {
			return c, false, fieldError("companion", "use face_svg or face_edits, not both")
		}
		c.FaceSVG, err = sanitizeFaceSVG(str(raw))
	} else if editParts || oldPalette != newPalette || c.Configured != before.Configured {
		if c.FaceSVG == "" || c.FaceSVG == before.defaultFace() {
			c.FaceSVG = c.defaultFace()
		} else if c.FaceSVG == defaultFaceArtwork(before.FaceBG, before.MarkColor, before.AccentColor, before.Configured, true) {
			c.FaceSVG = defaultFaceArtwork(c.FaceBG, c.MarkColor, c.AccentColor, c.Configured, true)
		} else if oldPalette != newPalette {
			c.FaceSVG, err = recolorFaceSVG(c.FaceSVG, oldPalette, newPalette)
		}
		if err == nil && editParts {
			c.FaceSVG, err = editFaceParts(c.FaceSVG, edits)
		}
	}
	return c, save, err
}

func companionBool(args object, key string, fallback bool) (bool, error) {
	if raw, exists := args[key]; exists {
		v, ok := raw.(bool)
		if !ok {
			return false, problem(400, "Expected boolean for "+key)
		}
		return v, nil
	}
	return fallback, nil
}

func (b *bridge) changeCompanion(ctx context.Context, who peer, theme, op string, args object) (companionState, bool, error) {
	return b.changeCompanionPrepared(ctx, who, theme, op, args, nil)
}

func (b *bridge) changeCompanionPrepared(ctx context.Context, who peer, theme, op string, args object, portrait *pendingPortrait) (companionState, bool, error) {
	id := strings.ToLower(who.DeviceID)
	unavailable := problem(502, "Cannot save companion")
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return companionState{}, false, unavailable
	}
	defer tx.Rollback()
	if virtualID.MatchString(id) {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM virtual_clients WHERE id=?)`, id).Scan(&exists); err != nil {
			return companionState{}, false, unavailable
		}
		if !exists {
			return companionState{}, false, problem(404, "Device not found")
		}
	}
	current, err := b.readActiveCompanion(ctx, tx, who)
	if err != nil {
		return companionState{}, false, unavailable
	}
	if !b.voices.allowed(current.Voice) {
		current.Voice = b.voices.defaultFor(theme)
	}
	next := current
	profilesChanged := false
	archive := func(c companionState, replace bool) error {
		if strings.HasPrefix(strings.ToLower(c.Name), "live-") || strings.HasPrefix(id, "lark-live-") || companionSlug(c.Name) == "" {
			return nil
		}
		profilesChanged = true
		return saveCompanionProfile(ctx, tx, c, replace)
	}
	switch op {
	case "get":
		return current, false, nil
	case "update":
		var save bool
		next, save, err = updateCompanion(current, args, b.voices)
		if err != nil {
			return companionState{}, false, err
		}
		if portrait != nil {
			if current.FaceSVG != portrait.base {
				return companionState{}, false, problem(409, "Companion changed while the portrait was being drawn")
			}
			next.Configured, err = companionBool(args, "configured", true)
			if err != nil {
				return companionState{}, false, err
			}
			next.FaceBG = portrait.bg
			next.FaceSVG = portrait.svg()
		}
		// An omitted flag retains legacy auto-save; an explicit false is a
		// temporary shift that must not overwrite a named favorite.
		if _, explicit := args["save_profile"]; save || !explicit && next.Configured {
			if err := archive(next, true); err != nil {
				return companionState{}, false, unavailable
			}
		}
	case "load_saved":
		slug := str(args["slug"])
		if !validCompanionSlug(slug) {
			return companionState{}, false, problem(400, "Invalid companion slug")
		}
		p, err := readSavedCompanion(tx.QueryRowContext(ctx, `SELECT `+savedCompanionColumns+` FROM saved_companions WHERE slug=?`, slug))
		if errors.Is(err, sql.ErrNoRows) {
			return companionState{}, false, problem(404, "Saved companion not found")
		}
		if err != nil {
			return companionState{}, false, unavailable
		}
		if current.Configured && companionSlug(current.Name) != slug {
			if err := archive(current, false); err != nil {
				return companionState{}, false, unavailable
			}
		}
		next = p.companionState
		if !b.voices.allowed(next.Voice) {
			next.Voice = current.Voice
		}
	case "reset_blank", "new_session":
		if current.Configured {
			if err := archive(current, false); err != nil {
				return companionState{}, false, unavailable
			}
		}
		next = blankCompanion()
		next.Voice = current.Voice
	}
	if err := writeCompanion(ctx, tx, id, next); err != nil {
		return companionState{}, false, unavailable
	}
	if err := writeCompanionVoice(ctx, tx, id, next.Voice); err != nil {
		return companionState{}, false, unavailable
	}
	if op == "new_session" {
		if err := resetCompanionSession(ctx, tx, who.DeviceID, args["archive_history"] == true); err != nil {
			return companionState{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return companionState{}, false, unavailable
	}
	return next, profilesChanged, nil
}

func validCompanionSlug(slug string) bool { return len(slug) <= 40 && companionSlugRE.MatchString(slug) }

func (b *bridge) savedCompanionState(ctx context.Context) (object, error) {
	out := object{}
	if b.db == nil {
		return out, nil
	}
	list, err := b.companionTool(ctx, object{"op": "list_saved"})
	if err != nil {
		return nil, err
	}
	for _, c := range list["companions"].([]savedCompanion) {
		out[c.Slug] = c
	}
	return out, nil
}

func (b *bridge) patchCompanion(ctx context.Context, id string, args object) (any, error) {
	c := withDevice(args, id)
	op, supplied := c["op"]
	if !supplied {
		op = "update"
	}
	for _, field := range []string{"face_edits", "preset", "portrait"} {
		if _, set := c[field]; set && op != "update" {
			return nil, fieldError("companion."+field, "requires companion update")
		}
	}
	switch op {
	case "load", "delete_saved":
		if str(c["slug"]) == "" {
			c["slug"] = companionSlug(str(c["name"]))
		}
		if op == "load" {
			op = "load_saved"
		}
	case "update", "reset_blank":
	default:
		return nil, fieldError("companion.op", "unknown operation")
	}
	c["op"] = op
	result, err := b.companionTool(ctx, c)
	if err != nil {
		return nil, err
	}
	if op == "delete_saved" {
		return object{"deleted_saved": c["slug"]}, nil
	}
	return result["companion"], nil
}

func (b *bridge) companionTool(ctx context.Context, args object) (object, error) {
	if b.db == nil {
		return nil, problem(503, "Companion storage unavailable")
	}
	op := str(args["op"])
	switch op {
	case "list_saved":
		rows, err := b.db.QueryContext(ctx, `SELECT `+savedCompanionColumns+` FROM saved_companions ORDER BY slug`)
		if err != nil {
			return nil, problem(502, "Cannot read saved companions")
		}
		defer rows.Close()
		list := []savedCompanion{}
		for rows.Next() {
			p, err := readSavedCompanion(rows)
			if err != nil {
				return nil, problem(502, "Cannot read saved companions")
			}
			list = append(list, p)
		}
		if rows.Err() != nil {
			return nil, problem(502, "Cannot read saved companions")
		}
		return object{"companions": list}, nil
	case "delete_saved":
		slug := str(args["slug"])
		if !validCompanionSlug(slug) {
			return nil, problem(400, "Invalid companion slug")
		}
		if _, err := b.db.ExecContext(ctx, `DELETE FROM saved_companions WHERE slug=?`, slug); err != nil {
			return nil, problem(502, "Cannot delete saved companion")
		}
		if b.ws != nil {
			b.ws.mu.Lock()
			b.ws.savedCompanionsChanged()
			b.ws.mu.Unlock()
		}
		return object{"ok": true}, nil
	case "get", "update", "load_saved", "reset_blank":
	default:
		return nil, problem(400, "Unknown companion operation")
	}
	id := str(args["device_id"])
	if !deviceIDRE.MatchString(id) {
		return nil, problem(400, "Invalid device ID")
	}
	who, err := b.lookupDevice(id)
	if err != nil {
		return nil, err
	}
	id = who.DeviceID
	var portrait *pendingPortrait
	if raw, ok := args["portrait"]; ok {
		if op != "update" {
			return nil, fieldError("companion.portrait", "requires companion update")
		}
		args = withDevice(args, id)
		delete(args, "portrait")
		portrait, err = b.preparePortrait(ctx, who, args, raw)
		if err != nil {
			return nil, err
		}
	}
	c, err := func() (companionState, error) {
		if op != "get" {
			b.companionMu.Lock()
			defer b.companionMu.Unlock()
		}
		c, profilesChanged, err := b.changeCompanionPrepared(ctx, who, b.voiceTheme(who), op, args, portrait)
		if b.ws != nil && err == nil && op != "get" {
			b.ws.mu.Lock()
			b.ws.companionChanged(id, c, profilesChanged)
			b.ws.mu.Unlock()
		}
		return c, err
	}()
	if err != nil {
		if portrait != nil {
			b.portraits.discard(portrait.id)
		}
		return nil, err
	}
	if op != "get" && c.Name != who.Character && b.oc != nil {
		b.syncCompanionTitle(ctx, who)
	}
	return object{"ok": true, "device_id": id, "companion": c}, nil
}

func (b *bridge) syncCompanionTitle(ctx context.Context, who peer) {
	b.companionTitles.Lock()
	defer b.companionTitles.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	sid, err := b.oc.get(who.DeviceID)
	if err != nil || sid == "" {
		return
	}
	title := larkTitle(who.DeviceID, b.companionFor(who).Name)
	if err := b.oc.jsonCall(ctx, http.MethodPatch, "/api/session/"+sid, object{"title": title}, nil); err != nil {
		log.Print("Lark could not update a companion session title")
	}
}

func (b *bridge) reassignCompanions(ctx context.Context, args object) (object, error) {
	fromID, toID := str(args["from_device_id"]), str(args["to_device_id"])
	mode := "swap"
	if raw, ok := args["mode"]; ok {
		mode = str(raw)
	}
	if !deviceIDRE.MatchString(fromID) || !deviceIDRE.MatchString(toID) || strings.EqualFold(fromID, toID) {
		return nil, problem(400, "Expected two distinct device IDs")
	}
	if mode != "move" && mode != "swap" {
		return nil, problem(400, "Expected mode move or swap")
	}
	if b.db == nil {
		return nil, problem(503, "Companion storage unavailable")
	}
	from, err := b.lookupDevice(fromID)
	if err != nil {
		return nil, err
	}
	to, err := b.lookupDevice(toID)
	if err != nil {
		return nil, err
	}
	fromID, toID = from.DeviceID, to.DeviceID
	apply := func() error {
		b.companionMu.Lock()
		defer b.companionMu.Unlock()
		nextFrom, nextTo, archived, err := b.transferCompanions(ctx, from, to, mode)
		if err != nil {
			return err
		}
		if b.ws != nil {
			b.ws.mu.Lock()
			defer b.ws.mu.Unlock()
			for _, id := range []string{fromID, toID} {
				if d := b.ws.devices[strings.ToLower(id)]; d != nil && b.ws.active(d) {
					b.ws.stopCompanionTurn(d)
				}
			}
			b.ws.companionChanged(fromID, nextFrom, false)
			b.ws.companionChanged(toID, nextTo, archived)
			b.ws.companionsReassigned(fromID, toID)
		}
		return nil
	}
	if b.oc != nil {
		run := b.oc.prepareMappingChange(fromID, toID)
		if b.ws != nil {
			b.ws.stopCompanionTurns(fromID, toID)
		}
		err = run(ctx, func() error {
			if err := apply(); err != nil {
				return err
			}
			for _, who := range []peer{from, to} {
				b.syncCompanionTitle(context.WithoutCancel(ctx), who)
			}
			return nil
		})
	} else {
		err = apply()
	}
	if err != nil {
		return nil, err
	}
	return object{"ok": true, "mode": mode, "from_device_id": fromID, "to_device_id": toID}, nil
}

func (b *bridge) transferCompanions(ctx context.Context, from, to peer, mode string) (companionState, companionState, bool, error) {
	fromTheme, toTheme := b.voiceTheme(from), b.voiceTheme(to)
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return companionState{}, companionState{}, false, problem(502, "Cannot reassign companions")
	}
	defer tx.Rollback()
	type assignment struct {
		companion     companionState
		sid, contract string
	}
	read := func(who peer, theme string) (assignment, error) {
		var a assignment
		key := strings.ToLower(who.DeviceID)
		if virtualID.MatchString(key) {
			var exists bool
			if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM virtual_clients WHERE id=?)", key).Scan(&exists); err != nil {
				return a, err
			}
			if !exists {
				return a, sql.ErrNoRows
			}
		}
		var err error
		a.companion, err = b.readActiveCompanion(ctx, tx, who)
		if err != nil {
			return a, err
		}
		if !b.voices.allowed(a.companion.Voice) {
			a.companion.Voice = b.voices.defaultFor(theme)
		}
		err = tx.QueryRowContext(ctx, "SELECT session_id,contract_hash FROM sessions WHERE device_id=?", who.DeviceID).Scan(&a.sid, &a.contract)
		if errors.Is(err, sql.ErrNoRows) {
			err = nil
		}
		return a, err
	}
	var nextFrom, nextTo assignment
	archived := false
	err = func() error {
		source, err := read(from, fromTheme)
		if err != nil {
			return err
		}
		target, err := read(to, toTheme)
		if err != nil {
			return err
		}
		nextFrom, nextTo = target, source
		if mode == "move" {
			if target.companion.Configured {
				if err := saveCompanionProfile(ctx, tx, target.companion, false); err != nil {
					return err
				}
				archived = true
			}
			nextFrom = assignment{companion: blankCompanion()}
			nextFrom.companion.Voice = b.voices.defaultFor("")
		}
		now := time.Now().UnixMilli()
		for id, a := range map[string]assignment{from.DeviceID: nextFrom, to.DeviceID: nextTo} {
			key := strings.ToLower(id)
			if err := writeCompanion(ctx, tx, key, a.companion); err != nil {
				return err
			}
			if err := writeCompanionVoice(ctx, tx, key, a.companion.Voice); err != nil {
				return err
			}
			if a.sid == "" {
				if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE device_id=?", id); err != nil {
					return err
				}
			} else if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (device_id,session_id,updated_at,contract_hash,context_after_ms)
				VALUES (?,?,?,?,?) ON CONFLICT(device_id) DO UPDATE SET session_id=excluded.session_id,
				updated_at=excluded.updated_at,contract_hash=excluded.contract_hash,context_after_ms=excluded.context_after_ms`,
				id, a.sid, now, a.contract, now); err != nil {
				return err
			}
		}
		return tx.Commit()
	}()
	if errors.Is(err, sql.ErrNoRows) {
		return companionState{}, companionState{}, false, problem(404, "Device not found")
	}
	if err != nil {
		return companionState{}, companionState{}, false, problem(502, "Cannot reassign companions")
	}
	return nextFrom.companion, nextTo.companion, archived, nil
}

func writeCompanionVoice(ctx context.Context, tx *sql.Tx, id, voice string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO voices VALUES (?,?) ON CONFLICT(device_id) DO UPDATE SET voice=excluded.voice`, id, voice)
	return err
}

func (b *bridge) companionAPI(w http.ResponseWriter, r *http.Request, path string) (any, error) {
	switch {
	case path == "/api/companions" && r.Method == http.MethodGet:
		return b.companionTool(r.Context(), object{"op": "list_saved"})
	case path == "/api/companions/reassign" && r.Method == http.MethodPost:
		args, err := jsonBody(r)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(r.Context(), turnTimeout)
		defer cancel()
		return b.reassignCompanions(ctx, args)
	case strings.HasPrefix(path, "/api/companions/") && r.Method == http.MethodDelete:
		if _, err := b.companionTool(r.Context(), object{"op": "delete_saved", "slug": strings.TrimPrefix(path, "/api/companions/")}); err != nil {
			return nil, err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil, nil
	case (r.Method == http.MethodPost || r.Method == http.MethodPut) && strings.HasPrefix(path, "/api/devices/") && strings.HasSuffix(path, "/companion"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/devices/"), "/companion")
		args, err := jsonBody(r)
		if err != nil {
			return nil, err
		}
		if _, load := args["slug"]; load {
			if len(args) != 1 {
				return nil, problem(400, "Supply either slug or companion fields")
			}
			args["op"] = "load_saved"
		} else {
			args["op"] = "update"
		}
		args["device_id"] = id
		return b.companionTool(r.Context(), args)
	}
	return nil, problem(404, "Not found")
}
