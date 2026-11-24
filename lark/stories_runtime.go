package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	maxStoryPassages    = 64
	maxStoryPassageText = 600
	maxStoryChoices     = 3
	maxStoryFlags       = 64
)

type storyVal struct {
	IsNum bool
	Bool  bool
	Num   float64
}

func (v storyVal) truthy() bool {
	if v.IsNum {
		return v.Num != 0
	}
	return v.Bool
}

func (v storyVal) MarshalJSON() ([]byte, error) {
	if v.IsNum {
		return json.Marshal(v.Num)
	}
	return json.Marshal(v.Bool)
}

func (v *storyVal) UnmarshalJSON(data []byte) error {
	var b bool
	if json.Unmarshal(data, &b) == nil {
		*v = storyVal{Bool: b}
		return nil
	}
	var n float64
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	*v = storyVal{IsNum: true, Num: n}
	return nil
}

const (
	opText uint8 = iota + 1
	opChoice
	opSet
	opIfNot
	opJump
)

type storyOp struct {
	kind   uint8
	flag   uint16
	val    storyVal
	text   string
	target int // passage index for opChoice, op index for opIfNot/opJump
}

type storyPassage struct {
	Name string
	BG   string
	End  bool
	Ops  []storyOp
}

type storyChoice struct {
	Label  string
	Target int
}

type compiledStory struct {
	Title    string
	Start    int
	Passages []storyPassage
	ByName   map[string]int
	Flags    []string
	FlagIdx  map[string]uint16
}

type savedStory struct {
	ID        string
	Title     string
	Twee      string
	Author    string
	UpdatedAt int64
	Passages  int
}

type storyProgress struct {
	CompanionID string
	StoryID     string
	Passage     string
	Page        int
	Flags       map[string]storyVal
	UpdatedAt   int64
}

func (cs *compiledStory) evalPassage(pIdx int, flags []storyVal, applySets bool) (string, []storyChoice) {
	if pIdx < 0 || pIdx >= len(cs.Passages) {
		return "", nil
	}
	ops := cs.Passages[pIdx].Ops
	var text strings.Builder
	var choices []storyChoice
	for pc := 0; pc < len(ops); {
		op := ops[pc]
		switch op.kind {
		case opText:
			text.WriteString(op.text)
			pc++
		case opChoice:
			if len(choices) < maxStoryChoices {
				choices = append(choices, storyChoice{Label: op.text, Target: op.target})
			}
			pc++
		case opSet:
			if applySets && int(op.flag) < len(flags) {
				flags[op.flag] = op.val
			}
			pc++
		case opIfNot:
			if int(op.flag) >= len(flags) || !flags[op.flag].truthy() {
				pc = op.target
			} else {
				pc++
			}
		case opJump:
			pc = op.target
		default:
			pc++
		}
	}
	return strings.TrimSpace(text.String()), choices
}

func (cs *compiledStory) flagsSlice(m map[string]storyVal) []storyVal {
	arr := make([]storyVal, len(cs.Flags))
	for name, idx := range cs.FlagIdx {
		if v, ok := m[name]; ok {
			arr[idx] = v
		}
	}
	return arr
}

func (cs *compiledStory) flagsMap(arr []storyVal) map[string]storyVal {
	out := map[string]storyVal{}
	for i, name := range cs.Flags {
		if i < len(arr) && arr[i].truthy() || (i < len(arr) && arr[i].IsNum) {
			out[name] = arr[i]
		}
	}
	return out
}

// matchStoryChoice matches spoken text against choice labels using normalized
// token overlap and ordinal phrases without calling an LLM.
func matchStoryChoice(spoken string, choices []storyChoice) (int, bool) {
	if len(choices) == 0 {
		return 0, false
	}
	tokens := normalizeSpeechTokens(spoken)
	if len(tokens) == 0 {
		return 0, false
	}

	// Score token overlap against each choice.
	bestIdx, bestScore, secondScore := -1, 0.0, 0.0
	spokenJoined := strings.Join(tokens, " ")
	for i, ch := range choices {
		cTokens := filterStopTokens(normalizeSpeechTokens(ch.Label))
		if len(cTokens) == 0 {
			cTokens = normalizeSpeechTokens(ch.Label)
		}
		if len(cTokens) == 0 {
			continue
		}
		hits := 0
		for _, ct := range cTokens {
			if slices.Contains(tokens, ct) {
				hits++
			}
		}
		if hits == 0 {
			continue
		}
		score := float64(hits) / float64(len(cTokens))
		if strings.Contains(spokenJoined, strings.Join(cTokens, " ")) {
			score += 0.5
		}
		if score > bestScore {
			secondScore = bestScore
			bestScore = score
			bestIdx = i
		} else if score > secondScore {
			secondScore = score
		}
	}
	if bestIdx >= 0 && bestScore >= 0.5 && bestScore > secondScore {
		return bestIdx, true
	}

	// Check ordinals: "first", "second", "third", "last", "one", "two", "three".
	ord := -1
	for _, t := range tokens {
		switch t {
		case "first", "1st", "one", "1":
			ord = 0
		case "second", "2nd", "two", "2":
			ord = 1
		case "third", "3rd", "three", "3":
			ord = 2
		case "last":
			ord = len(choices) - 1
		}
		if ord >= 0 {
			break
		}
	}
	if ord >= 0 && ord < len(choices) {
		return ord, true
	}
	return 0, false
}

func normalizeSpeechTokens(s string) []string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	return strings.Fields(b.String())
}

func filterStopTokens(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		switch t {
		case "the", "a", "an", "to", "and", "or", "of", "in", "on", "at", "for", "with":
			continue
		}
		out = append(out, t)
	}
	return out
}

func (b *larkServer) saveStory(ctx context.Context, id, twee, author string, createOnly bool) (savedStory, *compiledStory, error) {
	if b.db == nil {
		return savedStory{}, nil, problem(503, "Stories unavailable")
	}
	id = strings.ToLower(strings.TrimSpace(id))
	if !slugRE.MatchString(id) {
		return savedStory{}, nil, fieldError("id", "invalid story slug")
	}
	cs, err := parseTwee(twee)
	if err != nil {
		return savedStory{}, nil, fieldError("twee", "%s", err.Error())
	}
	now := time.Now().UnixMilli()
	var saved savedStory
	if createOnly {
		err = b.db.QueryRowContext(ctx, `INSERT INTO stories (id,title,twee,author,updated_at,passages)
			SELECT ?,?,?,?,?,? WHERE NOT EXISTS(SELECT 1 FROM stories WHERE id=?) AND (SELECT count(*) FROM stories) < 128
			RETURNING id,title,twee,author,updated_at,passages`,
			id, cs.Title, twee, author, now, len(cs.Passages), id).
			Scan(&saved.ID, &saved.Title, &saved.Twee, &saved.Author, &saved.UpdatedAt, &saved.Passages)
		if errors.Is(err, sql.ErrNoRows) {
			var exists bool
			_ = b.db.readDB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM stories WHERE id=?)`, id).Scan(&exists)
			if exists {
				return savedStory{}, nil, problem(409, "Story already exists")
			}
			return savedStory{}, nil, problem(409, "Story library is full")
		}
	} else {
		err = b.db.QueryRowContext(ctx, `UPDATE stories SET title=?,twee=?,author=?,updated_at=max(updated_at+1,?),passages=? WHERE id=?
			RETURNING id,title,twee,author,updated_at,passages`,
			cs.Title, twee, author, now, len(cs.Passages), id).
			Scan(&saved.ID, &saved.Title, &saved.Twee, &saved.Author, &saved.UpdatedAt, &saved.Passages)
		if errors.Is(err, sql.ErrNoRows) {
			return savedStory{}, nil, problem(404, "Story not found")
		}
	}
	if err != nil {
		return savedStory{}, nil, problem(502, "Cannot save story")
	}
	b.invalidateStoryCache(saved.ID)
	return saved, cs, nil
}

func (b *larkServer) loadStory(ctx context.Context, id string) (savedStory, error) {
	var s savedStory
	if b.db == nil {
		return s, problem(503, "Stories unavailable")
	}
	id = strings.ToLower(strings.TrimSpace(id))
	if !slugRE.MatchString(id) {
		return s, problem(400, "Invalid story slug")
	}
	err := b.db.readDB().QueryRowContext(ctx, `SELECT id,title,twee,author,updated_at,passages FROM stories WHERE id=?`, id).
		Scan(&s.ID, &s.Title, &s.Twee, &s.Author, &s.UpdatedAt, &s.Passages)
	if errors.Is(err, sql.ErrNoRows) {
		return s, problem(404, "Story not found")
	}
	return s, err
}

func (b *larkServer) listStories(ctx context.Context) ([]savedStory, error) {
	if b.db == nil {
		return nil, nil
	}
	rows, err := b.db.readDB().QueryContext(ctx, `SELECT id,title,twee,author,updated_at,passages FROM stories ORDER BY title, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []savedStory
	for rows.Next() {
		var s savedStory
		if err := rows.Scan(&s.ID, &s.Title, &s.Twee, &s.Author, &s.UpdatedAt, &s.Passages); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// listStorySummaries lists stories without their Twee source. Prompt building,
// remote snapshots, and device menus only need metadata, and loading every
// story body on each 750 ms poll dominated remote snapshot time.
func (b *larkServer) listStorySummaries(ctx context.Context) ([]savedStory, error) {
	if b.db == nil {
		return nil, nil
	}
	rows, err := b.db.readDB().QueryContext(ctx, `SELECT id,title,author,updated_at,passages FROM stories ORDER BY title, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []savedStory
	for rows.Next() {
		var s savedStory
		if err := rows.Scan(&s.ID, &s.Title, &s.Author, &s.UpdatedAt, &s.Passages); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (b *larkServer) storySummary(ctx context.Context, id string) (savedStory, bool, error) {
	var s savedStory
	if b.db == nil {
		return s, false, nil
	}
	err := b.db.readDB().QueryRowContext(ctx, `SELECT id,title,author,updated_at,passages FROM stories WHERE id=?`, id).
		Scan(&s.ID, &s.Title, &s.Author, &s.UpdatedAt, &s.Passages)
	if errors.Is(err, sql.ErrNoRows) {
		return s, false, nil
	}
	if err != nil {
		return s, false, err
	}
	return s, true, nil
}

type storyCompileEntry struct {
	rev int64
	cs  *compiledStory
}

type storyCompileCache struct {
	mu      sync.Mutex
	entries map[string]storyCompileEntry
}

// compiledStory caches parses by story revision; the remote page polls every
// 750 ms and must not re-parse the same Twee each time.
func (b *larkServer) compiledStory(st savedStory) (*compiledStory, error) {
	b.storyCache.mu.Lock()
	if entry, ok := b.storyCache.entries[st.ID]; ok && entry.rev == st.UpdatedAt {
		b.storyCache.mu.Unlock()
		return entry.cs, nil
	}
	b.storyCache.mu.Unlock()
	cs, err := parseTwee(st.Twee)
	if err != nil {
		return nil, err
	}
	b.storyCache.mu.Lock()
	if b.storyCache.entries == nil {
		b.storyCache.entries = map[string]storyCompileEntry{}
	}
	b.storyCache.entries[st.ID] = storyCompileEntry{rev: st.UpdatedAt, cs: cs}
	b.storyCache.mu.Unlock()
	return cs, nil
}

// compiledStoryByID loads the body only when the cached revision is stale.
func (b *larkServer) compiledStoryByID(ctx context.Context, id string) (*compiledStory, error) {
	st, ok, err := b.storySummary(ctx, id)
	if err != nil || !ok {
		return nil, err
	}
	b.storyCache.mu.Lock()
	entry, cached := b.storyCache.entries[st.ID]
	b.storyCache.mu.Unlock()
	if cached && entry.rev == st.UpdatedAt {
		return entry.cs, nil
	}
	full, err := b.loadStory(ctx, st.ID)
	if err != nil {
		return nil, err
	}
	return b.compiledStory(full)
}

func (b *larkServer) invalidateStoryCache(storyID string) {
	b.storyCache.mu.Lock()
	delete(b.storyCache.entries, storyID)
	b.storyCache.mu.Unlock()
}

func (b *larkServer) deleteStory(ctx context.Context, id string) (bool, error) {
	if b.db == nil {
		return false, problem(503, "Stories unavailable")
	}
	id = strings.ToLower(strings.TrimSpace(id))
	if !slugRE.MatchString(id) {
		return false, problem(400, "Invalid story slug")
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return false, problem(502, "Cannot delete story")
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM stories WHERE id=?`, id)
	if err != nil {
		return false, problem(502, "Cannot delete story")
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM story_progress WHERE story_id=?`, id); err != nil {
		return false, problem(502, "Cannot delete story")
	}
	if err := tx.Commit(); err != nil {
		return false, problem(502, "Cannot delete story")
	}
	b.invalidateStoryCache(id)
	if b.portraits != nil && b.portraits.dir != "" {
		if err := os.RemoveAll(filepath.Join(filepath.Dir(b.portraits.dir), "story-bg", id)); err != nil {
			log.Printf("Lark could not delete story background for %s: %v", id, err)
		}
	}
	return n > 0, nil
}

func (b *larkServer) getStoryProgress(ctx context.Context, companionID, storyID string) (storyProgress, bool, error) {
	var p storyProgress
	if b.db == nil {
		return p, false, nil
	}
	var flagsRaw string
	err := b.db.readDB().QueryRowContext(ctx,
		`SELECT companion_id,story_id,passage,page,flags_json,updated_at FROM story_progress WHERE companion_id=? AND story_id=?`,
		companionID, storyID).Scan(&p.CompanionID, &p.StoryID, &p.Passage, &p.Page, &flagsRaw, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	p.Flags = map[string]storyVal{}
	_ = json.Unmarshal([]byte(flagsRaw), &p.Flags)
	return p, true, nil
}

func (b *larkServer) setStoryProgress(ctx context.Context, p storyProgress) error {
	if b.db == nil {
		return nil
	}
	if p.Flags == nil {
		p.Flags = map[string]storyVal{}
	}
	flagsRaw, _ := json.Marshal(p.Flags)
	now := time.Now().UnixMilli()
	_, err := b.db.ExecContext(ctx, `INSERT INTO story_progress (companion_id,story_id,passage,page,flags_json,updated_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(companion_id,story_id) DO UPDATE SET passage=excluded.passage,page=excluded.page,flags_json=excluded.flags_json,updated_at=excluded.updated_at`,
		p.CompanionID, p.StoryID, p.Passage, p.Page, string(flagsRaw), now)
	return err
}

func (b *larkServer) listStoryProgress(ctx context.Context, companionID string) ([]storyProgress, error) {
	if b.db == nil {
		return nil, nil
	}
	rows, err := b.db.readDB().QueryContext(ctx,
		`SELECT companion_id,story_id,passage,page,flags_json,updated_at FROM story_progress WHERE companion_id=? ORDER BY updated_at DESC`,
		companionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storyProgress
	for rows.Next() {
		var p storyProgress
		var flagsRaw string
		if err := rows.Scan(&p.CompanionID, &p.StoryID, &p.Passage, &p.Page, &flagsRaw, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.Flags = map[string]storyVal{}
		_ = json.Unmarshal([]byte(flagsRaw), &p.Flags)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (b *larkServer) mcpStory(ctx context.Context, args object) (object, error) {
	op := strings.TrimSpace(str(args["op"]))
	id := strings.ToLower(strings.TrimSpace(str(args["id"])))
	switch op {
	case "create", "update":
		twee := str(args["twee"])
		if strings.TrimSpace(twee) == "" {
			return nil, fieldError("twee", "twee source is required")
		}
		author := strings.TrimSpace(str(args["author"]))
		saved, cs, err := b.saveStory(ctx, id, twee, author, op == "create")
		if err != nil {
			return nil, err
		}
		return object{"ok": true, "id": saved.ID, "title": saved.Title, "passages": saved.Passages, "start": cs.Passages[cs.Start].Name, "updated_at": saved.UpdatedAt}, nil
	case "get":
		saved, err := b.loadStory(ctx, id)
		if err != nil {
			return nil, err
		}
		return object{"id": saved.ID, "title": saved.Title, "twee": saved.Twee, "author": saved.Author, "passages": saved.Passages, "updated_at": saved.UpdatedAt}, nil
	case "delete":
		deleted, err := b.deleteStory(ctx, id)
		if err != nil {
			return nil, err
		}
		return object{"ok": true, "deleted": deleted}, nil
	default:
		return nil, fieldError("op", "expected create, update, delete, or get")
	}
}

func (b *larkServer) storiesState(ctx context.Context) (object, error) {
	list, err := b.listStorySummaries(ctx)
	if err != nil {
		return nil, err
	}
	out := make(object, len(list))
	for _, s := range list {
		out[s.ID] = object{"title": s.Title, "passages": s.Passages, "updated_at": s.UpdatedAt}
	}
	return out, nil
}

func slugFromTitle(title string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "story"
	}
	return s
}

func (b *larkServer) nextAvailableStorySlug(ctx context.Context, title string) string {
	base := slugFromTitle(title)
	candidate := base
	for i := 2; i <= 99; i++ {
		var exists bool
		if err := b.db.readDB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM stories WHERE id=?)`, candidate).Scan(&exists); err != nil || !exists {
			return candidate
		}
		suffix := fmt.Sprintf("-%d", i)
		trimLen := min(len(base), 48-len(suffix))
		candidate = base[:trimLen] + suffix
	}
	return fmt.Sprintf("%s-%d", base[:min(len(base), 36)], time.Now().Unix()%100000)
}

var _ = maps.Clone[map[string]storyVal]

type storySession struct {
	id         string
	title      string
	compiled   *compiledStory
	passageIdx int
	flags      []storyVal
	text       string
	choices    []storyChoice
	end        bool
	bg         string
	bgImg      *image.RGBA
}

func (s *storySession) currentPassageName() string {
	if s == nil || s.compiled == nil || s.passageIdx < 0 || s.passageIdx >= len(s.compiled.Passages) {
		return ""
	}
	return s.compiled.Passages[s.passageIdx].Name
}

func (s *storySession) enterPassage(idx int, applySets bool) {
	if s == nil || s.compiled == nil || idx < 0 || idx >= len(s.compiled.Passages) {
		return
	}
	s.passageIdx = idx
	p := s.compiled.Passages[idx]
	s.end = p.End
	s.bg = p.BG
	s.bgImg = nil
	s.text, s.choices = s.compiled.evalPassage(idx, s.flags, applySets)
}

func storyBGPrompt(desc string) string {
	return `Create a cozy indie-game pixel art background scene (Stardew Valley or GBA style) depicting: ` + strings.TrimSpace(desc) +
		`. Use crisp pixel clusters, soft atmospheric lighting, and a harmonious limited palette. No characters, no creatures, no text, no UI, no borders, no frames.`
}

func (b *larkServer) storyBGPath(storyID, passageName, bgDesc string) string {
	if b == nil || b.portraits == nil || b.portraits.dir == "" || strings.TrimSpace(bgDesc) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(passageName + "\x00" + strings.TrimSpace(bgDesc)))
	key := hex.EncodeToString(sum[:12])
	return filepath.Join(filepath.Dir(b.portraits.dir), "story-bg", storyID, key+".png")
}

func (b *larkServer) loadStoryBG(storyID, passageName, bgDesc string) *image.RGBA {
	p := b.storyBGPath(storyID, passageName, bgDesc)
	if p == "" {
		return nil
	}
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil
	}
	rgba, ok := img.(*image.RGBA)
	if !ok {
		bnds := img.Bounds()
		rgba = image.NewRGBA(image.Rect(0, 0, bnds.Dx(), bnds.Dy()))
		for y := range bnds.Dy() {
			for x := range bnds.Dx() {
				rgba.Set(x, y, img.At(bnds.Min.X+x, bnds.Min.Y+y))
			}
		}
	}
	return rgba
}

func (b *larkServer) ensureStoryBGAsync(d *terminalSession, storyID, passageName, bgDesc string) {
	if b == nil || b.portraits == nil || b.portraits.generate == nil || strings.TrimSpace(bgDesc) == "" {
		return
	}
	dst := b.storyBGPath(storyID, passageName, bgDesc)
	if dst == "" {
		return
	}
	if _, err := os.Stat(dst); err == nil {
		return
	}
	b.storyBGMu.Lock()
	if b.storyBGInflight == nil {
		b.storyBGInflight = map[string]bool{}
	}
	if b.storyBGInflight[dst] {
		b.storyBGMu.Unlock()
		return
	}
	b.storyBGInflight[dst] = true
	b.storyBGMu.Unlock()

	go func() {
		defer func() {
			b.storyBGMu.Lock()
			delete(b.storyBGInflight, dst)
			b.storyBGMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(b.root, portraitTimeout)
		defer cancel()
		raw, err := b.portraits.generate(ctx, storyBGPrompt(bgDesc), nil)
		if err != nil {
			return
		}
		img, _, err := image.Decode(bytes.NewReader(raw))
		if err != nil {
			return
		}
		norm, err := normalizeBackground(img, pixelSpecBox3.logicalW, pixelSpecBox3.logicalH)
		if err != nil {
			return
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return
		}
		if err := writePortraitPNG(filepath.Dir(dst), filepath.Base(dst), norm); err != nil {
			return
		}
		if b.hub == nil || d == nil {
			return
		}
		b.hub.mu.Lock()
		if b.hub.active(d) && d.drawer == drawerStoryReader && d.story != nil &&
			d.story.id == storyID && d.story.currentPassageName() == passageName {
			d.story.bgImg = norm
			b.hub.render(d, time.Now(), false)
		}
		b.hub.mu.Unlock()
	}()
}
