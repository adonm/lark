package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
)

type companionLook struct {
	Name        string `json:"name"`
	Configured  bool   `json:"configured"`
	Personality string `json:"personality"`
	Form        string `json:"form"`
}

type companionState struct {
	ID string `json:"id,omitempty"`
	companionLook
	Voice       string `json:"voice"`
	PortraitID  string `json:"portrait_id,omitempty"`
	UpdatedAt   int64  `json:"updated_at,omitempty"`
	DCAccountID uint32 `json:"dc_account_id,omitempty"`
	Listening   bool   `json:"listening"`

	IdentityVersion int64 `json:"identity_version,omitempty"`
	IdentitySynced  int64 `json:"identity_synced,omitempty"`
}

func blankCompanion() companionState {
	return companionState{companionLook: companionLook{Name: "New Companion"}}
}

var (
	companionSlugRE    = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	companionSlugSpace = regexp.MustCompile(`[^a-z0-9]+`)
)

func companionSlug(name string) string {
	return strings.Trim(companionSlugSpace.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

func validCompanionID(id string) bool {
	return len(id) >= 1 && len(id) <= 64 && companionSlugRE.MatchString(id)
}

const companionColumns = `id,name,configured,personality,voice,updated_at,dc_account_id,form,portrait_id,listening,identity_version,identity_synced`

func readCompanionRow(row rowScanner) (companionState, error) {
	var c companionState
	var configured, listening int
	err := row.Scan(&c.ID, &c.Name, &configured, &c.Personality, &c.Voice, &c.UpdatedAt, &c.DCAccountID, &c.Form, &c.PortraitID, &listening, &c.IdentityVersion, &c.IdentitySynced)
	if err != nil {
		return c, err
	}
	c.Configured = configured != 0
	c.Listening = listening != 0
	if c.Voice != "" && voiceByID(c.Voice).ID == "" {
		// A retired voice must not hide the companion. Resolution falls back to
		// the theme default; the raw id stays visible for doctor and repairs.
		log.Printf("Lark companion %s stores unknown voice %q; using the default", c.ID, c.Voice)
	}
	return c, nil
}

func writeCompanion(ctx context.Context, tx *sql.Tx, id string, c companionState) error {
	id = strings.ToLower(id)
	// A name or personality edit bumps the identity version and marks the
	// OpenCode entry dirty; the next turn re-sends it and records the version
	// it confirmed.
	_, err := tx.ExecContext(ctx, `INSERT INTO companions (`+companionColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name,configured=excluded.configured,
		personality=excluded.personality,voice=excluded.voice,
		updated_at=max(companions.updated_at+1,excluded.updated_at),
		dc_account_id=CASE WHEN excluded.dc_account_id>0 THEN excluded.dc_account_id ELSE companions.dc_account_id END,
		form=excluded.form,portrait_id=excluded.portrait_id,listening=excluded.listening,
		identity_version=companions.identity_version+CASE WHEN companions.name<>excluded.name OR companions.personality<>excluded.personality THEN 1 ELSE 0 END,
		identity_synced=CASE WHEN companions.name<>excluded.name OR companions.personality<>excluded.personality THEN 0 ELSE companions.identity_synced END`,
		id, c.Name, c.Configured, c.Personality, c.Voice, time.Now().UnixMilli(), c.DCAccountID, c.Form, c.PortraitID, c.Listening, c.IdentityVersion, c.IdentitySynced)
	return err
}

func (b *larkServer) uniqueCompanionSlug(ctx context.Context, q companionQuery, name string) (string, error) {
	base := companionSlug(name)
	if base == "" {
		base = "companion"
	}
	if len(base) > 56 {
		base = strings.TrimRight(base[:56], "-")
	}
	candidate := base
	for n := 2; ; n++ {
		var exists bool
		err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM companions WHERE id=?)`, candidate).Scan(&exists)
		if err != nil {
			return "", err
		}
		if !exists {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s-%d", base, n)
	}
}

type companionQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (b *larkServer) companionByID(id string) (companionState, bool) {
	if b.db == nil {
		return companionState{}, false
	}
	c, err := readCompanionRow(b.db.readDB().QueryRow(`SELECT `+companionColumns+` FROM companions WHERE id=?`, strings.ToLower(id)))
	if err != nil {
		return companionState{}, false
	}
	return c, true
}

func (b *larkServer) companionFor(id string) companionState {
	if c, ok := b.companionByID(id); ok {
		return c
	}
	c := blankCompanion()
	c.ID = strings.ToLower(id)
	return c
}

func (b *larkServer) allCompanions(ctx context.Context) ([]companionState, error) {
	if b.db == nil {
		return nil, nil
	}
	rows, err := b.db.readDB().QueryContext(ctx, `SELECT `+companionColumns+` FROM companions ORDER BY id`)
	if err != nil {
		return nil, problem(502, "Cannot read companions")
	}
	defer rows.Close()
	var list []companionState
	for rows.Next() {
		c, err := readCompanionRow(rows)
		if err != nil {
			return nil, problem(502, "Cannot read companions")
		}
		list = append(list, c)
	}
	if rows.Err() != nil {
		return nil, problem(502, "Cannot read companions")
	}
	return list, nil
}

func (b *larkServer) companionTargets(sender string) ([]companionTarget, error) {
	list, err := b.allCompanions(context.Background())
	if err != nil {
		return nil, err
	}
	out := make([]companionTarget, 0, len(list))
	for _, c := range list {
		if !strings.EqualFold(sender, c.ID) {
			out = append(out, companionTarget{ID: c.ID, Name: c.Name})
		}
	}
	return out, nil
}

func (b *larkServer) companionEntry(c companionState, full bool) object {
	voice := b.voices.resolve(c.Voice, companionTheme(c.Name))
	terminals, _ := b.terminalsForCompanion(c.ID)
	termEntries := make([]object, 0, len(terminals))
	online := false
	for _, t := range terminals {
		te := b.terminalEntry(t, full)
		if te["online"] == true {
			online = true
		}
		termEntries = append(termEntries, te)
	}
	entry := object{
		"id":          c.ID,
		"name":        c.Name,
		"configured":  c.Configured,
		"personality": c.Personality,
		"form":        c.Form,
		"voice":       voice,
		"listening":   c.Listening,
		"online":      online,
		"dormant":     len(termEntries) == 0,
		"terminals":   termEntries,
		"updated_at":  c.UpdatedAt,
	}
	if c.PortraitID != "" {
		entry["portrait_id"] = c.PortraitID
	}
	if c.Voice != "" && !b.voices.allowed(c.Voice) {
		entry["voice_invalid"] = c.Voice
	}
	if c.DCAccountID != 0 {
		entry["dc_account_id"] = c.DCAccountID
	}
	return entry
}

func companionTheme(name string) string {
	if strings.EqualFold(name, "mint") {
		return "mint"
	}
	return "cake"
}

func (b *larkServer) companionList(ctx context.Context, full bool) ([]object, error) {
	list, err := b.allCompanions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]object, 0, len(list))
	for _, c := range list {
		out = append(out, b.companionEntry(c, full))
	}
	return out, nil
}

func updateCompanion(c companionState, args object, voices companionVoices) (companionState, error) {
	editLook := false
	for key, val := range args {
		switch key {
		case "name":
			name := strings.TrimSpace(str(val))
			if !characterRE.MatchString(name) {
				return c, problem(400, "Invalid companion name (1–40 characters)")
			}
			c.Name, editLook = name, true
		case "personality":
			vibe, ok := val.(string)
			if !ok || runeLen(vibe) > 160 || strings.ContainsAny(vibe, "[]") || strings.ContainsFunc(vibe, func(r rune) bool { return !unicode.IsPrint(r) }) {
				return c, problem(400, "Expected a single-line personality of at most 160 characters, without brackets")
			}
			c.Personality = strings.TrimSpace(vibe)
		case "form":
			form, ok := val.(string)
			if !ok || runeLen(form) > 160 || strings.ContainsAny(form, "[]") || strings.ContainsFunc(form, func(r rune) bool { return !unicode.IsPrint(r) }) {
				return c, problem(400, "Expected a single-line form of at most 160 characters, without brackets")
			}
			c.Form = strings.TrimSpace(form)
		case "voice":
			if !voices.allowed(str(val)) {
				return c, problem(400, "Unsupported voice")
			}
			c.Voice = str(val)
		case "listening":
			on, ok := val.(bool)
			if !ok {
				return c, problem(400, "Expected listening to be true or false")
			}
			c.Listening = on
		case "configured", "companion_id", "op", "id":
		default:
			return c, problem(400, "Unknown companion field "+key)
		}
	}
	var err error
	c.Configured, err = companionBool(args, "configured", c.Configured || editLook)
	return c, err
}

func companionInt(raw any) (int, bool) {
	switch v := raw.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		if float64(int(v)) != v {
			return 0, false
		}
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, false
		}
		return int(n), true
	default:
		return 0, false
	}
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

func (b *larkServer) changeCompanion(ctx context.Context, id, op string, args object) (companionState, error) {
	return b.changeCompanionPrepared(ctx, id, op, args, nil)
}

func (b *larkServer) changeCompanionPrepared(ctx context.Context, id, op string, args object, portrait *pendingPortrait) (companionState, error) {
	id = strings.ToLower(id)
	unavailable := problem(502, "Cannot save companion")
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return companionState{}, unavailable
	}
	defer tx.Rollback()
	current, err := readCompanionRow(tx.QueryRowContext(ctx, `SELECT `+companionColumns+` FROM companions WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return companionState{}, problem(404, "Companion not found")
	}
	if err != nil {
		return companionState{}, unavailable
	}
	if !b.voices.allowed(current.Voice) {
		current.Voice = b.voices.defaultFor(companionTheme(current.Name))
	}
	next := current
	switch op {
	case "get":
		return current, nil
	case "update":
		next, err = updateCompanion(current, args, b.voices)
		if err != nil {
			return companionState{}, err
		}
		if portrait != nil {
			if current.PortraitID != portrait.base {
				return companionState{}, problem(409, "Companion changed while the portrait was being drawn")
			}
			next.Configured, err = companionBool(args, "configured", true)
			if err != nil {
				return companionState{}, err
			}
			next.PortraitID = portrait.id
			if portrait.form != "" {
				next.Form = portrait.form
			}
		}
	case "reset_blank", "new_session":
		next = blankCompanion()
		next.ID = id
		next.Voice = current.Voice
	}
	if err := writeCompanion(ctx, tx, id, next); err != nil {
		return companionState{}, unavailable
	}
	if op == "new_session" {
		if err := resetCompanionSession(ctx, tx, id, args["archive_history"] == true); err != nil {
			return companionState{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return companionState{}, unavailable
	}
	return next, nil
}

func (b *larkServer) patchCompanion(ctx context.Context, id string, args object) (any, error) {
	var edit companionPatch
	if err := decodeInput(args, &edit); err != nil {
		return nil, err
	}
	return b.patchCompanionEdit(ctx, id, edit)
}

// patchCompanionEdit applies one decoded companion patch.
func (b *larkServer) patchCompanionEdit(ctx context.Context, id string, edit companionPatch) (any, error) {
	c := edit.object()
	if _, supplied := c["op"]; !supplied {
		c["op"] = "update"
	}
	if _, set := c["portrait"]; set && c["op"] != "update" {
		return nil, fieldError("companion.portrait", "requires companion update")
	}
	switch c["op"] {
	case "update", "reset_blank":
	default:
		return nil, fieldError("companion.op", "unknown operation")
	}
	c["companion_id"] = id
	result, err := b.companionTool(ctx, c)
	if err != nil {
		return nil, err
	}
	return result["companion"], nil
}

func (b *larkServer) companionTool(ctx context.Context, args object) (object, error) {
	if b.db == nil {
		return nil, problem(503, "Companion storage unavailable")
	}
	op := str(args["op"])
	switch op {
	case "get", "update", "reset_blank":
	default:
		return nil, problem(400, "Unknown companion operation")
	}
	id := strings.ToLower(str(args["companion_id"]))
	if !validCompanionID(id) {
		return nil, problem(400, "Invalid companion ID")
	}
	before, ok := b.companionByID(id)
	if !ok {
		return nil, problem(404, "Companion not found")
	}
	var portrait *pendingPortrait
	if raw, ok := args["portrait"]; ok {
		if op != "update" {
			return nil, fieldError("companion.portrait", "requires companion update")
		}
		args = withCompanion(args, id)
		delete(args, "portrait")
		var err error
		portrait, err = b.preparePortrait(ctx, id, args, raw)
		if err != nil {
			return nil, err
		}
	}
	c, err := func() (companionState, error) {
		if op != "get" {
			b.companionMu.Lock()
			defer b.companionMu.Unlock()
		}
		c, err := b.changeCompanionPrepared(ctx, id, op, args, portrait)
		if b.hub != nil && err == nil && op != "get" {
			b.hub.mu.Lock()
			b.hub.companionChanged(id, c)
			b.hub.mu.Unlock()
		}
		return c, err
	}()
	if err != nil {
		if portrait != nil {
			b.portraits.discard(portrait.id)
		}
		return nil, err
	}
	if op != "get" && b.oc != nil && (c.Name != before.Name || c.Personality != before.Personality) {
		if c.Name != before.Name {
			b.syncCompanionTitle(ctx, id)
		}
		b.syncCompanionEntry(ctx, c)
	}
	if op != "get" && b.dc != nil && b.dc.ready.Load() && c.DCAccountID != 0 {
		go func() {
			if err := b.dc.syncProfile(c, c.DCAccountID); err != nil {
				log.Printf("Delta Chat profile for %s: %v", c.ID, err)
			}
		}()
	}
	if op != "get" && before.PortraitID != "" && before.PortraitID != c.PortraitID {
		b.hub.invalidateAssets(before.PortraitID)
		b.portraits.discard(before.PortraitID)
	}
	if op != "get" && c.Form != "" && c.Form != before.Form && c.PortraitID != "" && portrait == nil && b.portraits != nil && b.portraits.generate != nil {
		if b.hub != nil {
			b.hub.invalidateAssets(c.PortraitID)
		}
		b.schedulePortraitBackground(c.PortraitID, c.Form, c.Personality)
	}
	return object{"ok": true, "companion_id": id, "companion": c}, nil
}

func (b *larkServer) schedulePortraitBackground(id, form, vibe string) {
	if b == nil || b.portraits == nil {
		return
	}
	b.portraitBGMu.Lock()
	if b.portraitBG == nil {
		b.portraitBG = map[string]*portraitBGQueue{}
	}
	q := b.portraitBG[id]
	if q == nil {
		q = &portraitBGQueue{}
		b.portraitBG[id] = q
	}
	q.form, q.vibe = form, vibe
	if q.running {
		q.dirty = true
		b.portraitBGMu.Unlock()
		return
	}
	q.running = true
	b.portraitBGMu.Unlock()
	go b.runPortraitBackground(id, q)
}

// runPortraitBackground keeps one generation in flight per portrait. A newer
// form edit marks the queue dirty; when the current image lands, the newest
// values are generated again so an older request cannot win.
func (b *larkServer) runPortraitBackground(id string, q *portraitBGQueue) {
	for {
		b.portraitBGMu.Lock()
		form, vibe := q.form, q.vibe
		q.dirty = false
		b.portraitBGMu.Unlock()
		ctx := b.root
		if ctx == nil {
			ctx = context.Background()
		}
		if err := b.portraits.generateBackground(ctx, id, form, vibe); err != nil {
			log.Printf("Lark could not regenerate the background for %s: %v", id, err)
		}
		if b.hub != nil {
			b.hub.invalidateAssets(id)
		}
		b.portraitBGMu.Lock()
		if !q.dirty {
			delete(b.portraitBG, id)
			q.running = false
			b.portraitBGMu.Unlock()
			return
		}
		b.portraitBGMu.Unlock()
	}
}

func (b *larkServer) syncCompanionTitle(ctx context.Context, companionID string) {
	b.companionTitles.Lock()
	defer b.companionTitles.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	sid, err := b.oc.get(companionID)
	if err != nil || sid == "" {
		return
	}
	title := larkTitle(companionID, b.companionFor(companionID).Name)
	if err := b.oc.jsonCall(ctx, http.MethodPatch, "/api/session/"+sid, object{"title": title}, nil); err != nil {
		log.Print("Lark could not update a companion session title")
	}
}

func (b *larkServer) syncCompanionEntry(ctx context.Context, c companionState) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	b.oc.reconcileCompanionIdentity(ctx, c.ID)
}

func (b *larkServer) createCompanion(ctx context.Context, obj object) (companionState, error) {
	if b.db == nil {
		return companionState{}, problem(503, "Companion storage unavailable")
	}
	c := blankCompanion()
	if rawName, ok := obj["name"]; ok {
		name := strings.TrimSpace(str(rawName))
		if name != "" {
			if !characterRE.MatchString(name) {
				return companionState{}, problem(400, "Invalid companion name (1–40 characters)")
			}
			c.Name = name
			c.Configured = true
		}
	}
	patch := object{}
	for _, k := range []string{"personality", "form", "voice", "configured"} {
		if v, exists := obj[k]; exists {
			patch[k] = v
		}
	}
	if len(patch) > 0 {
		var err error
		c, err = updateCompanion(c, patch, b.voices)
		if err != nil {
			return companionState{}, err
		}
	}
	if c.Configured && strings.TrimSpace(c.Form) == "" {
		return companionState{}, fieldError("form", "companion form is required (for example, \"an old chameleon\")")
	}
	if !b.voices.allowed(c.Voice) {
		c.Voice = b.voices.defaultFor(companionTheme(c.Name))
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return companionState{}, problem(502, "Cannot create companion")
	}
	defer tx.Rollback()
	id := strings.ToLower(strings.TrimSpace(str(obj["id"])))
	if id != "" {
		if !validCompanionID(id) {
			return companionState{}, problem(400, "Invalid companion ID")
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM companions WHERE id=?)`, id).Scan(&exists); err != nil {
			return companionState{}, problem(502, "Cannot create companion")
		}
		if exists {
			return companionState{}, problem(409, "Companion ID already exists")
		}
	} else {
		id, err = b.uniqueCompanionSlug(ctx, tx, c.Name)
		if err != nil {
			return companionState{}, problem(502, "Cannot create companion")
		}
	}
	c.ID = id
	if err := writeCompanion(ctx, tx, id, c); err != nil {
		return companionState{}, problem(502, "Cannot create companion")
	}
	if err := tx.Commit(); err != nil {
		return companionState{}, problem(502, "Cannot create companion")
	}
	if b.hub != nil {
		b.hub.mu.Lock()
		b.hub.broadcastPeers()
		b.hub.mu.Unlock()
	}
	if b.dc != nil && b.dc.ready.Load() {
		if accID, err := b.dc.ensureCompanion(ctx, c); err == nil && accID != 0 {
			c.DCAccountID = accID
			go b.dc.autoPairAll(b.root)
		} else if err != nil {
			log.Printf("Delta Chat provision for %s: %v", c.ID, err)
		}
	}
	return c, nil
}

func withCompanion(args object, id string) object {
	out := maps.Clone(args)
	out["companion_id"] = id
	return out
}

func (b *larkServer) deleteCompanion(ctx context.Context, id string) error {
	id = strings.ToLower(id)
	if !validCompanionID(id) {
		return problem(400, "Invalid companion ID")
	}
	c, ok := b.companionByID(id)
	if !ok {
		return problem(404, "Companion not found")
	}
	terms, err := b.terminalsForCompanion(id)
	if err != nil {
		return err
	}
	if len(terms) > 0 {
		return problem(409, "Detach or remove terminals before deleting companion")
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return problem(502, "Cannot delete companion")
	}
	defer tx.Rollback()
	for _, query := range []string{
		`DELETE FROM sessions WHERE companion_id=?`,
		`DELETE FROM messages WHERE companion_id=?`,
		`DELETE FROM timers WHERE companion_id=?`,
		`DELETE FROM story_progress WHERE companion_id=?`,
		`DELETE FROM memory WHERE scope=?`,
		`DELETE FROM meta WHERE key LIKE 'dc_welcome:' || ? || ':%'`,
		`DELETE FROM meta WHERE key LIKE 'dc_story_msgs:' || ? || ':%'`,
		`DELETE FROM companions WHERE id=?`,
	} {
		arg := id
		if strings.Contains(query, "FROM memory") {
			arg = "companion:" + id
		}
		if _, err := tx.ExecContext(ctx, query, arg); err != nil {
			return problem(502, "Cannot delete companion")
		}
	}
	if err := tx.Commit(); err != nil {
		return problem(502, "Cannot delete companion")
	}
	if b.portraits != nil && c.PortraitID != "" {
		if b.hub != nil {
			b.hub.invalidateAssets(c.PortraitID)
		}
		b.portraits.discard(c.PortraitID)
	}
	if err := deleteCompanionCaptures(debugCaptureDir(), id); err != nil {
		log.Printf("Lark could not delete captures for %s: %v", id, err)
	}
	if b.remote != nil {
		if err := b.remote.stopCompanion(ctx, id); err != nil {
			log.Printf("Lark could not stop remote controls for %s: %v", id, err)
		}
	}
	if b.dc != nil {
		b.dc.forget(id)
		if accID := c.DCAccountID; accID != 0 {
			b.dc.dropAccount(ctx, accID)
		}
	}
	if b.hub != nil {
		b.hub.mu.Lock()
		b.hub.broadcastPeers()
		b.hub.mu.Unlock()
	}
	return nil
}

func (b *larkServer) companionAPI(w http.ResponseWriter, r *http.Request, path string) (any, error) {
	switch {
	case path == "/api/companions" && r.Method == http.MethodGet:
		list, err := b.companionList(r.Context(), false)
		if err != nil {
			return nil, err
		}
		return object{"companions": list}, nil
	case path == "/api/companions" && r.Method == http.MethodPost:
		obj, err := jsonBody(r)
		if err != nil {
			return nil, err
		}
		c, err := b.createCompanion(r.Context(), obj)
		if err != nil {
			return nil, err
		}
		sendJSON(w, 201, b.companionEntry(c, true))
		return nil, nil
	}
	rest, ok := strings.CutPrefix(path, "/api/companions/")
	if !ok || rest == "" {
		return nil, problem(404, "Not found")
	}
	if id, ok := strings.CutSuffix(rest, "/reset"); ok && r.Method == http.MethodPost {
		if !validCompanionID(strings.ToLower(id)) {
			return nil, problem(400, "Invalid companion ID")
		}
		return b.resetCompanion(r, strings.ToLower(id))
	}
	if id, ok := strings.CutSuffix(rest, "/voice"); ok && r.Method == http.MethodPost {
		if !validCompanionID(strings.ToLower(id)) {
			return nil, problem(400, "Invalid companion ID")
		}
		return b.voiceAPI(r, strings.ToLower(id))
	}
	if id, ok := strings.CutSuffix(rest, "/dc"); ok && r.Method == http.MethodGet {
		id = strings.ToLower(id)
		if !validCompanionID(id) {
			return nil, problem(400, "Invalid companion ID")
		}
		c, ok := b.companionByID(id)
		if !ok {
			return nil, problem(404, "Companion not found")
		}
		if b.dc == nil || c.DCAccountID == 0 {
			return object{"dc_account_id": c.DCAccountID, "verified_peers": []string{}}, nil
		}
		rpc := b.dc.getRPC()
		var peers []string
		if rpc != nil {
			if contacts, err := rpc.GetContacts(c.DCAccountID, 0, nil); err == nil {
				for _, ct := range contacts {
					if ct.IsVerified && !ct.IsBlocked {
						if pid, isPeer := b.dc.companionByAddr(ct.Address); isPeer {
							peers = append(peers, pid)
						}
					}
				}
			}
		}
		return object{"dc_account_id": c.DCAccountID, "verified_peers": peers}, nil
	}
	if strings.Contains(rest, "/") {
		return nil, problem(404, "Not found")
	}
	id := strings.ToLower(rest)
	if !validCompanionID(id) {
		return nil, problem(400, "Invalid companion ID")
	}
	switch r.Method {
	case http.MethodGet:
		c, ok := b.companionByID(id)
		if !ok {
			return nil, problem(404, "Companion not found")
		}
		return b.companionEntry(c, true), nil
	case http.MethodPatch, http.MethodPut:
		var edit companionPatch
		if err := decodeRequest(r, &edit); err != nil {
			return nil, err
		}
		if edit.Op == nil {
			op := "update"
			edit.Op = &op
		}
		return b.patchCompanionEdit(r.Context(), id, edit)
	case http.MethodDelete:
		if err := b.deleteCompanion(r.Context(), id); err != nil {
			return nil, err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil, nil
	}
	return nil, problem(404, "Not found")
}
