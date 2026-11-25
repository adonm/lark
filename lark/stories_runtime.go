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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"lark/internal/stories"
)

func (b *larkServer) saveStory(ctx context.Context, id, twee, author string, createOnly bool) (stories.Saved, *stories.Story, error) {
	if b.db == nil {
		return stories.Saved{}, nil, problem(503, "Stories unavailable")
	}
	id = strings.ToLower(strings.TrimSpace(id))
	if !slugRE.MatchString(id) {
		return stories.Saved{}, nil, fieldError("id", "invalid story slug")
	}
	cs, err := stories.Parse(twee)
	if err != nil {
		return stories.Saved{}, nil, fieldError("twee", "%s", err.Error())
	}
	now := time.Now().UnixMilli()
	var saved stories.Saved
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
				return stories.Saved{}, nil, problem(409, "Story already exists")
			}
			return stories.Saved{}, nil, problem(409, "Story library is full")
		}
	} else {
		err = b.db.QueryRowContext(ctx, `UPDATE stories SET title=?,twee=?,author=?,updated_at=max(updated_at+1,?),passages=? WHERE id=?
			RETURNING id,title,twee,author,updated_at,passages`,
			cs.Title, twee, author, now, len(cs.Passages), id).
			Scan(&saved.ID, &saved.Title, &saved.Twee, &saved.Author, &saved.UpdatedAt, &saved.Passages)
		if errors.Is(err, sql.ErrNoRows) {
			return stories.Saved{}, nil, problem(404, "Story not found")
		}
	}
	if err != nil {
		return stories.Saved{}, nil, problem(502, "Cannot save story")
	}
	b.invalidateStoryCache(saved.ID)
	return saved, cs, nil
}

func (b *larkServer) loadStory(ctx context.Context, id string) (stories.Saved, error) {
	var s stories.Saved
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

func (b *larkServer) listStories(ctx context.Context) ([]stories.Saved, error) {
	if b.db == nil {
		return nil, nil
	}
	rows, err := b.db.readDB().QueryContext(ctx, `SELECT id,title,twee,author,updated_at,passages FROM stories ORDER BY title, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []stories.Saved
	for rows.Next() {
		var s stories.Saved
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
func (b *larkServer) listStorySummaries(ctx context.Context) ([]stories.Saved, error) {
	if b.db == nil {
		return nil, nil
	}
	rows, err := b.db.readDB().QueryContext(ctx, `SELECT id,title,author,updated_at,passages FROM stories ORDER BY title, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []stories.Saved
	for rows.Next() {
		var s stories.Saved
		if err := rows.Scan(&s.ID, &s.Title, &s.Author, &s.UpdatedAt, &s.Passages); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (b *larkServer) storySummary(ctx context.Context, id string) (stories.Saved, bool, error) {
	var s stories.Saved
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
	cs  *stories.Story
}

type storyCompileCache struct {
	mu      sync.Mutex
	entries map[string]storyCompileEntry
}

// compiledStory caches parses by story revision; the remote page polls every
// 750 ms and must not re-parse the same Twee each time.
func (b *larkServer) compiledStory(st stories.Saved) (*stories.Story, error) {
	b.storyCache.mu.Lock()
	if entry, ok := b.storyCache.entries[st.ID]; ok && entry.rev == st.UpdatedAt {
		b.storyCache.mu.Unlock()
		return entry.cs, nil
	}
	b.storyCache.mu.Unlock()
	cs, err := stories.Parse(st.Twee)
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
func (b *larkServer) compiledStoryByID(ctx context.Context, id string) (*stories.Story, error) {
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

func (b *larkServer) getStoryProgress(ctx context.Context, companionID, storyID string) (stories.Progress, bool, error) {
	var p stories.Progress
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
	p.Flags = map[string]stories.Value{}
	_ = json.Unmarshal([]byte(flagsRaw), &p.Flags)
	return p, true, nil
}

func (b *larkServer) setStoryProgress(ctx context.Context, p stories.Progress) error {
	if b.db == nil {
		return nil
	}
	if p.Flags == nil {
		p.Flags = map[string]stories.Value{}
	}
	flagsRaw, _ := json.Marshal(p.Flags)
	now := time.Now().UnixMilli()
	_, err := b.db.ExecContext(ctx, `INSERT INTO story_progress (companion_id,story_id,passage,page,flags_json,updated_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(companion_id,story_id) DO UPDATE SET passage=excluded.passage,page=excluded.page,flags_json=excluded.flags_json,updated_at=excluded.updated_at`,
		p.CompanionID, p.StoryID, p.Passage, p.Page, string(flagsRaw), now)
	return err
}

func (b *larkServer) listStoryProgress(ctx context.Context, companionID string) ([]stories.Progress, error) {
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
	var out []stories.Progress
	for rows.Next() {
		var p stories.Progress
		var flagsRaw string
		if err := rows.Scan(&p.CompanionID, &p.StoryID, &p.Passage, &p.Page, &flagsRaw, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.Flags = map[string]stories.Value{}
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

func (b *larkServer) nextAvailableStorySlug(ctx context.Context, title string) string {
	base := stories.SlugFromTitle(title)
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

type storySession struct {
	id         string
	title      string
	compiled   *stories.Story
	passageIdx int
	flags      []stories.Value
	text       string
	choices    []stories.Choice
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
	s.text, s.choices = s.compiled.EvalPassage(idx, s.flags, applySets)
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
		raw, err := b.portraits.generate(ctx, stories.BGPrompt(bgDesc), nil)
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
		b.hub.postStoryBG(d, storyID, passageName, norm)
	}()
}
