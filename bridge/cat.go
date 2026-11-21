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

type catLook struct {
	Name        string `json:"name"`
	Configured  bool   `json:"configured"`
	FaceBG      string `json:"face_bg"`
	MarkColor   string `json:"mark_color"`
	AccentColor string `json:"accent_color"`
	Personality string `json:"personality"`
}

type catState struct {
	catLook
	Voice   string `json:"voice"`
	FaceSVG string `json:"face_svg"`
}

type savedCat struct {
	catState
	Slug      string `json:"slug"`
	UpdatedAt int64  `json:"updated_at"`
}

func blankCat() catState {
	p := storybookPalettes["peach"]
	c := catState{catLook: catLook{Name: "New Cat", FaceBG: p.bg, MarkColor: p.furShadow, AccentColor: p.blush}}
	c.FaceSVG = c.defaultFace()
	return c
}

func (c catState) defaultFace() string {
	return defaultFaceSVG(c.FaceBG, c.MarkColor, c.AccentColor, c.Configured)
}

var (
	catSlugRE    = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	catSlugSpace = regexp.MustCompile(`[^a-z0-9]+`)
)

func catSlug(name string) string {
	return strings.Trim(catSlugSpace.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

const activeCatColumns = `name,configured,face_bg,mark_color,accent_color,personality,face_svg`
const savedCatColumns = `slug,name,face_bg,mark_color,accent_color,personality,voice,face_svg,updated_at`
const catVoiceSQL = `COALESCE((SELECT voice FROM voices WHERE device_id=?),(SELECT voice FROM virtual_clients WHERE id=?),'')`

func readCat(row rowScanner) (catState, error) {
	var c catState
	var configured int
	err := row.Scan(&c.Name, &configured, &c.FaceBG, &c.MarkColor, &c.AccentColor, &c.Personality, &c.FaceSVG, &c.Voice)
	c.Configured = configured != 0
	if err == nil && c.FaceSVG == "" {
		c.FaceSVG = c.defaultFace()
	}
	return c, err
}

func readSavedCat(row rowScanner) (savedCat, error) {
	var p savedCat
	p.Configured = true
	err := row.Scan(&p.Slug, &p.Name, &p.FaceBG, &p.MarkColor, &p.AccentColor, &p.Personality, &p.Voice, &p.FaceSVG, &p.UpdatedAt)
	p.Voice = storedVoice(p.Voice)
	if err == nil && p.FaceSVG == "" {
		p.FaceSVG = p.defaultFace()
	}
	return p, err
}

func writeCat(ctx context.Context, tx *sql.Tx, id string, c catState) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO cats (device_id,`+activeCatColumns+`,updated_at) VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(device_id) DO UPDATE SET name=excluded.name,configured=excluded.configured,face_bg=excluded.face_bg,
		mark_color=excluded.mark_color,accent_color=excluded.accent_color,personality=excluded.personality,
		face_svg=excluded.face_svg,updated_at=max(cats.updated_at+1,excluded.updated_at)`,
		id, c.Name, c.Configured, c.FaceBG, c.MarkColor, c.AccentColor, c.Personality, c.FaceSVG, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	return syncVirtualCharacter(ctx, tx, id, c.Name)
}

func saveCatProfile(ctx context.Context, tx *sql.Tx, c catState, replace bool) error {
	slug := catSlug(c.Name)
	if slug == "" {
		return nil
	}
	conflict := `ON CONFLICT(slug) DO NOTHING`
	if replace {
		conflict = `ON CONFLICT(slug) DO UPDATE SET name=excluded.name,face_bg=excluded.face_bg,mark_color=excluded.mark_color,
			accent_color=excluded.accent_color,personality=excluded.personality,voice=excluded.voice,
			face_svg=excluded.face_svg,updated_at=excluded.updated_at`
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO saved_cats (`+savedCatColumns+`) VALUES (?,?,?,?,?,?,?,?,?) `+conflict,
		slug, c.Name, c.FaceBG, c.MarkColor, c.AccentColor, c.Personality, c.Voice, c.FaceSVG, time.Now().UnixMilli())
	return err
}

type catQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (b *bridge) readActiveCat(ctx context.Context, q catQuery, who peer) (catState, error) {
	id := strings.ToLower(who.DeviceID)
	c, err := readCat(q.QueryRowContext(ctx, `SELECT `+activeCatColumns+`,`+
		catVoiceSQL+` FROM cats WHERE device_id=?`, id, id, id))
	if errors.Is(err, sql.ErrNoRows) {
		c = blankCat()
		if who.Character != "" && who.Character != c.Name && !strings.EqualFold(who.Character, id) && !webDeviceID(id) {
			c.Name = who.Character
			c.Configured = true
			c.FaceSVG = c.defaultFace()
		}
		err = q.QueryRowContext(ctx, "SELECT "+catVoiceSQL, id, id).Scan(&c.Voice)
	}
	c.Voice = storedVoice(c.Voice)
	return c, err
}

func (b *bridge) catFor(who peer) catState {
	if b.db != nil {
		if c, err := b.readActiveCat(context.Background(), b.db, who); err == nil {
			return c
		}
	}
	return blankCat()
}

func updateCat(c catState, args object, voices deviceVoices) (catState, bool, error) {
	before := c
	save, err := catBool(args, "save_profile", false)
	if err != nil {
		return c, false, err
	}
	editLook := false
	for key, val := range args {
		switch key {
		case "name":
			name := strings.TrimSpace(str(val))
			if !characterRE.MatchString(name) {
				return c, false, problem(400, "Invalid cat name (1–40 characters)")
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
		case "face_svg", "face_edits", "companion":
			editLook = true
		case "configured", "save_profile", "device_id", "op":
		default:
			return c, false, problem(400, "Unknown cat field "+key)
		}
	}
	c.Configured, err = catBool(args, "configured", c.Configured || editLook)
	if err != nil {
		return c, false, err
	}
	oldPalette := [...]string{before.FaceBG, before.MarkColor, before.AccentColor}
	newPalette := [...]string{c.FaceBG, c.MarkColor, c.AccentColor}
	edits, editParts := args["face_edits"]
	if raw, set := args["companion"]; set {
		for _, field := range []string{"face_svg", "face_bg", "mark_color", "accent_color"} {
			if _, exists := args[field]; exists {
				return c, false, fieldError("cat.companion", "cannot combine with %s", field)
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
			return c, false, fieldError("cat", "use face_svg or face_edits, not both")
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

func catBool(args object, key string, fallback bool) (bool, error) {
	if raw, exists := args[key]; exists {
		v, ok := raw.(bool)
		if !ok {
			return false, problem(400, "Expected boolean for "+key)
		}
		return v, nil
	}
	return fallback, nil
}

func (b *bridge) changeCat(ctx context.Context, who peer, theme, op string, args object) (catState, bool, error) {
	return b.changeCatPrepared(ctx, who, theme, op, args, nil)
}

func (b *bridge) changeCatPrepared(ctx context.Context, who peer, theme, op string, args object, portrait *pendingPortrait) (catState, bool, error) {
	id := strings.ToLower(who.DeviceID)
	unavailable := problem(502, "Cannot save cat")
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return catState{}, false, unavailable
	}
	defer tx.Rollback()
	if virtualID.MatchString(id) {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM virtual_clients WHERE id=?)`, id).Scan(&exists); err != nil {
			return catState{}, false, unavailable
		}
		if !exists {
			return catState{}, false, problem(404, "Device not found")
		}
	}
	current, err := b.readActiveCat(ctx, tx, who)
	if err != nil {
		return catState{}, false, unavailable
	}
	if !b.voices.allowed(current.Voice) {
		current.Voice = b.voices.defaultFor(theme)
	}
	next := current
	profilesChanged := false
	archive := func(c catState, replace bool) error {
		if strings.HasPrefix(strings.ToLower(c.Name), "live-") || strings.HasPrefix(id, "lark-live-") || catSlug(c.Name) == "" {
			return nil
		}
		profilesChanged = true
		return saveCatProfile(ctx, tx, c, replace)
	}
	switch op {
	case "get":
		return current, false, nil
	case "update":
		var save bool
		next, save, err = updateCat(current, args, b.voices)
		if err != nil {
			return catState{}, false, err
		}
		if portrait != nil {
			if current.FaceSVG != portrait.base {
				return catState{}, false, problem(409, "Companion changed while the portrait was being drawn")
			}
			next.Configured, err = catBool(args, "configured", true)
			if err != nil {
				return catState{}, false, err
			}
			next.FaceBG = portrait.bg
			next.FaceSVG = portrait.svg()
		}
		// An omitted flag retains legacy auto-save; an explicit false is a
		// temporary shift that must not overwrite a named favorite.
		if _, explicit := args["save_profile"]; save || !explicit && next.Configured {
			if err := archive(next, true); err != nil {
				return catState{}, false, unavailable
			}
		}
	case "load_saved":
		slug := str(args["slug"])
		if !validCatSlug(slug) {
			return catState{}, false, problem(400, "Invalid cat slug")
		}
		p, err := readSavedCat(tx.QueryRowContext(ctx, `SELECT `+savedCatColumns+` FROM saved_cats WHERE slug=?`, slug))
		if errors.Is(err, sql.ErrNoRows) {
			return catState{}, false, problem(404, "Saved cat not found")
		}
		if err != nil {
			return catState{}, false, unavailable
		}
		if current.Configured && catSlug(current.Name) != slug {
			if err := archive(current, false); err != nil {
				return catState{}, false, unavailable
			}
		}
		next = p.catState
		if !b.voices.allowed(next.Voice) {
			next.Voice = current.Voice
		}
	case "reset_blank", "new_session":
		if current.Configured {
			if err := archive(current, false); err != nil {
				return catState{}, false, unavailable
			}
		}
		next = blankCat()
		next.Voice = current.Voice
	}
	if err := writeCat(ctx, tx, id, next); err != nil {
		return catState{}, false, unavailable
	}
	if err := writeCatVoice(ctx, tx, id, next.Voice); err != nil {
		return catState{}, false, unavailable
	}
	if op == "new_session" {
		if err := resetCatSession(ctx, tx, who.DeviceID, args["archive_history"] == true); err != nil {
			return catState{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return catState{}, false, unavailable
	}
	return next, profilesChanged, nil
}

func validCatSlug(slug string) bool { return len(slug) <= 40 && catSlugRE.MatchString(slug) }

func (b *bridge) savedCatState(ctx context.Context) (object, error) {
	out := object{}
	if b.db == nil {
		return out, nil
	}
	list, err := b.catTool(ctx, object{"op": "list_saved"})
	if err != nil {
		return nil, err
	}
	for _, cat := range list["cats"].([]savedCat) {
		out[cat.Slug] = cat
	}
	return out, nil
}

func (b *bridge) patchCat(ctx context.Context, id string, args object) (any, error) {
	c := withDevice(args, id)
	op, supplied := c["op"]
	if !supplied {
		op = "update"
	}
	for _, field := range []string{"face_edits", "companion", "portrait"} {
		if _, set := c[field]; set && op != "update" {
			return nil, fieldError("cat."+field, "requires cat update")
		}
	}
	switch op {
	case "load", "delete_saved":
		if str(c["slug"]) == "" {
			c["slug"] = catSlug(str(c["name"]))
		}
		if op == "load" {
			op = "load_saved"
		}
	case "update", "reset_blank":
	default:
		return nil, fieldError("cat.op", "unknown operation")
	}
	c["op"] = op
	result, err := b.catTool(ctx, c)
	if err != nil {
		return nil, err
	}
	if op == "delete_saved" {
		return object{"deleted_saved": c["slug"]}, nil
	}
	return result["cat"], nil
}

func (b *bridge) catTool(ctx context.Context, args object) (object, error) {
	if b.db == nil {
		return nil, problem(503, "Cat storage unavailable")
	}
	op := str(args["op"])
	switch op {
	case "list_saved":
		rows, err := b.db.QueryContext(ctx, `SELECT `+savedCatColumns+` FROM saved_cats ORDER BY slug`)
		if err != nil {
			return nil, problem(502, "Cannot read saved cats")
		}
		defer rows.Close()
		list := []savedCat{}
		for rows.Next() {
			p, err := readSavedCat(rows)
			if err != nil {
				return nil, problem(502, "Cannot read saved cats")
			}
			list = append(list, p)
		}
		if rows.Err() != nil {
			return nil, problem(502, "Cannot read saved cats")
		}
		return object{"cats": list}, nil
	case "delete_saved":
		slug := str(args["slug"])
		if !validCatSlug(slug) {
			return nil, problem(400, "Invalid cat slug")
		}
		if _, err := b.db.ExecContext(ctx, `DELETE FROM saved_cats WHERE slug=?`, slug); err != nil {
			return nil, problem(502, "Cannot delete saved cat")
		}
		if b.ws != nil {
			b.ws.mu.Lock()
			b.ws.savedCatsChanged()
			b.ws.mu.Unlock()
		}
		return object{"ok": true}, nil
	case "get", "update", "load_saved", "reset_blank":
	default:
		return nil, problem(400, "Unknown cat operation")
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
			return nil, fieldError("cat.portrait", "requires cat update")
		}
		args = withDevice(args, id)
		delete(args, "portrait")
		portrait, err = b.preparePortrait(ctx, who, args, raw)
		if err != nil {
			return nil, err
		}
	}
	c, err := func() (catState, error) {
		if op != "get" {
			b.catMu.Lock()
			defer b.catMu.Unlock()
		}
		c, profilesChanged, err := b.changeCatPrepared(ctx, who, b.voiceTheme(who), op, args, portrait)
		if b.ws != nil && err == nil && op != "get" {
			b.ws.mu.Lock()
			b.ws.catChanged(id, c, profilesChanged)
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
		b.syncCatTitle(ctx, who)
	}
	return object{"ok": true, "device_id": id, "cat": c}, nil
}

func (b *bridge) syncCatTitle(ctx context.Context, who peer) {
	b.catTitles.Lock()
	defer b.catTitles.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	sid, err := b.oc.get(who.DeviceID)
	if err != nil || sid == "" {
		return
	}
	title := larkTitle(who.DeviceID, b.catFor(who).Name)
	if err := b.oc.jsonCall(ctx, http.MethodPatch, "/api/session/"+sid, object{"title": title}, nil); err != nil {
		log.Print("Lark could not update a cat session title")
	}
}

func (b *bridge) reassignCats(ctx context.Context, args object) (object, error) {
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
		return nil, problem(503, "Cat storage unavailable")
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
		b.catMu.Lock()
		defer b.catMu.Unlock()
		nextFrom, nextTo, archived, err := b.transferCats(ctx, from, to, mode)
		if err != nil {
			return err
		}
		if b.ws != nil {
			b.ws.mu.Lock()
			defer b.ws.mu.Unlock()
			for _, id := range []string{fromID, toID} {
				if d := b.ws.devices[strings.ToLower(id)]; d != nil && b.ws.active(d) {
					b.ws.stopCatTurn(d)
				}
			}
			b.ws.catChanged(fromID, nextFrom, false)
			b.ws.catChanged(toID, nextTo, archived)
			b.ws.catsReassigned(fromID, toID)
		}
		return nil
	}
	if b.oc != nil {
		run := b.oc.prepareMappingChange(fromID, toID)
		if b.ws != nil {
			b.ws.stopCatTurns(fromID, toID)
		}
		err = run(ctx, func() error {
			if err := apply(); err != nil {
				return err
			}
			for _, who := range []peer{from, to} {
				b.syncCatTitle(context.WithoutCancel(ctx), who)
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

func (b *bridge) transferCats(ctx context.Context, from, to peer, mode string) (catState, catState, bool, error) {
	fromTheme, toTheme := b.voiceTheme(from), b.voiceTheme(to)
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return catState{}, catState{}, false, problem(502, "Cannot reassign cats")
	}
	defer tx.Rollback()
	type assignment struct {
		cat           catState
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
		a.cat, err = b.readActiveCat(ctx, tx, who)
		if err != nil {
			return a, err
		}
		if !b.voices.allowed(a.cat.Voice) {
			a.cat.Voice = b.voices.defaultFor(theme)
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
			if target.cat.Configured {
				if err := saveCatProfile(ctx, tx, target.cat, false); err != nil {
					return err
				}
				archived = true
			}
			nextFrom = assignment{cat: blankCat()}
			nextFrom.cat.Voice = b.voices.defaultFor("")
		}
		now := time.Now().UnixMilli()
		for id, a := range map[string]assignment{from.DeviceID: nextFrom, to.DeviceID: nextTo} {
			key := strings.ToLower(id)
			if err := writeCat(ctx, tx, key, a.cat); err != nil {
				return err
			}
			if err := writeCatVoice(ctx, tx, key, a.cat.Voice); err != nil {
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
		return catState{}, catState{}, false, problem(404, "Device not found")
	}
	if err != nil {
		return catState{}, catState{}, false, problem(502, "Cannot reassign cats")
	}
	return nextFrom.cat, nextTo.cat, archived, nil
}

func writeCatVoice(ctx context.Context, tx *sql.Tx, id, voice string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO voices VALUES (?,?) ON CONFLICT(device_id) DO UPDATE SET voice=excluded.voice`, id, voice)
	return err
}

func (b *bridge) catAPI(w http.ResponseWriter, r *http.Request, path string) (any, error) {
	switch {
	case path == "/api/cats" && r.Method == http.MethodGet:
		return b.catTool(r.Context(), object{"op": "list_saved"})
	case path == "/api/cats/reassign" && r.Method == http.MethodPost:
		args, err := jsonBody(r)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(r.Context(), turnTimeout)
		defer cancel()
		return b.reassignCats(ctx, args)
	case strings.HasPrefix(path, "/api/cats/") && r.Method == http.MethodDelete:
		if _, err := b.catTool(r.Context(), object{"op": "delete_saved", "slug": strings.TrimPrefix(path, "/api/cats/")}); err != nil {
			return nil, err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil, nil
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/devices/") && strings.HasSuffix(path, "/cat"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/devices/"), "/cat")
		args, err := jsonBody(r)
		if err != nil {
			return nil, err
		}
		if _, load := args["slug"]; load {
			if len(args) != 1 {
				return nil, problem(400, "Supply either slug or cat fields")
			}
			args["op"] = "load_saved"
		} else {
			args["op"] = "update"
		}
		args["device_id"] = id
		return b.catTool(r.Context(), args)
	}
	return nil, problem(404, "Not found")
}
