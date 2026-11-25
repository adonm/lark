package library

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

func (l *Library) searchSource(ctx context.Context, name, query string, limit int) ([]Hit, bool, error) {
	meta := sources[name]
	path := l.path(name)
	if _, err := os.Stat(path); err != nil {
		return nil, false, nil
	}
	db, err := l.open(path)
	if err != nil {
		return nil, true, err
	}
	var hits []Hit
	if name == "gutenberg" {
		hits, err = searchGutenberg(ctx, db, query, limit, meta.weight)
	} else {
		hits, err = searchWiki(ctx, db, query, limit, meta.weight)
	}
	for i := range hits {
		hits[i].Source = name
	}
	return hits, true, err
}

// wikiCandidate is an FTS row before display snippets are loaded.
type wikiCandidate struct {
	id    int64
	title string
	rank  float64
}

func searchWiki(ctx context.Context, db *sql.DB, query string, limit int, weight float64) ([]Hit, error) {
	run := func(match string) ([]Hit, error) {
		return searchWikiMatch(ctx, db, query, match, limit, weight)
	}
	hits, err := run(ftsQuery(query))
	if err == nil && len(hits) == 0 {
		hits, err = run(ftsQueryOr(query))
	}
	return hits, err
}

func searchWikiMatch(ctx context.Context, db *sql.DB, query, match string, limit int, weight float64) ([]Hit, error) {
	terms := strings.Split(strings.ReplaceAll(match, " OR ", " AND "), " AND ")
	words := make([]string, len(terms))
	for i, term := range terms {
		words[i] = strings.ToLower(strings.Trim(term, `"`))
	}
	seen := map[int64]int{}
	var candidates []wikiCandidate
	add := func(c wikiCandidate) {
		if i, ok := seen[c.id]; ok {
			// A later pass can only improve a candidate's rank.
			if c.rank < candidates[i].rank {
				candidates[i].rank = c.rank
			}
			return
		}
		seen[c.id] = len(candidates)
		candidates = append(candidates, c)
	}
	rows, err := db.QueryContext(ctx, `SELECT p.id, p.title, rank FROM pages_fts
		JOIN pages p ON p.id = pages_fts.rowid
		WHERE pages_fts MATCH ? ORDER BY rank LIMIT ?`, match, limit*4+10)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c wikiCandidate
		if err := rows.Scan(&c.id, &c.title, &c.rank); err != nil {
			rows.Close()
			return nil, err
		}
		add(c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// FTS5 ranking lets a long list article outrank the exact title. Look it
	// up through the case-insensitive title index instead of widening the scan.
	for _, title := range []string{query, strings.TrimSpace(strings.TrimPrefix(strings.ToLower(query), "the "))} {
		rows, err := db.QueryContext(ctx, `SELECT id, title FROM pages WHERE title = ? COLLATE NOCASE LIMIT 2`, title)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var c wikiCandidate
			if err := rows.Scan(&c.id, &c.title); err != nil {
				rows.Close()
				return nil, err
			}
			c.rank = -20
			add(c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	// Prefer titles made of the query terms over body matches.
	var titleMatch []string
	for _, term := range terms {
		titleMatch = append(titleMatch, "title:"+term)
	}
	rows, err = db.QueryContext(ctx, `SELECT p.id, p.title, rank FROM pages_fts
		JOIN pages p ON p.id = pages_fts.rowid
		WHERE pages_fts MATCH ? ORDER BY rank LIMIT 8`, strings.Join(titleMatch, " AND "))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c wikiCandidate
		if err := rows.Scan(&c.id, &c.title, &c.rank); err != nil {
			rows.Close()
			return nil, err
		}
		c.rank -= 6
		add(c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortStableFunc(candidates, func(a, b wikiCandidate) int {
		switch {
		case a.rank < b.rank:
			return -1
		case a.rank > b.rank:
			return 1
		}
		return 0
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	out := make([]Hit, 0, len(candidates))
	for _, c := range candidates {
		var text string
		if err := db.QueryRowContext(ctx, `SELECT text FROM pages WHERE id = ?`, c.id).Scan(&text); err != nil {
			return nil, err
		}
		out = append(out, Hit{Source: "wiki", Title: c.title, Ref: c.title,
			Snippet: previewText(text, words), Rank: c.rank * weight})
	}
	return out, nil
}

// previewText returns a short window around the first query term. FTS5's
// snippet() reloads the full article for every ranked candidate; one window
// from the final rows is enough for the agent's preview.
func previewText(text string, words []string) string {
	lower := strings.ToLower(text)
	best := -1
	for _, word := range words {
		if i := strings.Index(lower, word); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	if best < 0 {
		best = 0
	}
	start := max(0, best-80)
	end := min(len(text), start+snippetWidth)
	for start > 0 && text[start-1] != ' ' {
		start--
	}
	for end < len(text) && text[end] != ' ' {
		end++
	}
	out := strings.Join(strings.Fields(text[start:end]), " ")
	if end < len(text) {
		out += "…"
	}
	return out
}

func searchGutenberg(ctx context.Context, db *sql.DB, query string, limit int, weight float64) ([]Hit, error) {
	run := func(match string) ([]Hit, error) {
		rows, err := db.QueryContext(ctx, `SELECT b.id, b.title, b.author, snippet(books_fts, 0, '', '', '…', 16), bm25(books_fts, 8.0, 2.0)
			FROM books_fts JOIN books b ON b.id = books_fts.rowid
			WHERE books_fts MATCH ? ORDER BY bm25(books_fts, 8.0, 2.0) LIMIT ?`, match, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []Hit{}
		for rows.Next() {
			var id int64
			var title, author, snip string
			var rank float64
			if err := rows.Scan(&id, &title, &author, &snip, &rank); err != nil {
				return nil, err
			}
			if author != "" {
				snip = strings.TrimSpace(snip + " — " + author)
			}
			out = append(out, Hit{Source: "gutenberg", Title: title, Ref: strconv.FormatInt(id, 10),
				Snippet: strings.Join(strings.Fields(snip), " "), Rank: rank * weight})
		}
		slices.SortStableFunc(out, func(a, b Hit) int {
			if a.Rank < b.Rank {
				return -1
			}
			if a.Rank > b.Rank {
				return 1
			}
			return 0
		})
		return out, rows.Err()
	}
	hits, err := run(ftsQuery(query))
	if err == nil && len(hits) == 0 {
		hits, err = run(ftsQueryOr(query))
	}
	return hits, err
}

func (l *Library) readText(ctx context.Context, source, ref string, section int) (map[string]any, error) {
	var title, text string
	var err error
	if source == "gutenberg" {
		title, _, text, err = l.gutenbergText(ctx, ref)
	} else {
		title, text, err = l.wikiText(ctx, source, ref)
	}
	if err != nil {
		return nil, err
	}
	return boundedText(source, title, ref, text, section), nil
}

func boundedText(source, title, ref, text string, section int) map[string]any {
	runes := []rune(text)
	total := len(runes)
	sections := (total + readChunk - 1) / readChunk
	if sections == 0 {
		sections = 1
	}
	if section >= sections {
		return map[string]any{"source": source, "title": title, "ref": ref, "section": section,
			"sections": sections, "text": "", "chars": 0, "total_chars": total}
	}
	start := section * readChunk
	end := min(total, start+readChunk)
	if end-start > ReadMax {
		end = start + ReadMax
	}
	out := map[string]any{"source": source, "title": title, "ref": ref, "section": section,
		"sections": sections, "text": string(runes[start:end]), "chars": end - start, "total_chars": total}
	if section+1 < sections {
		out["next_section"] = section + 1
	}
	return out
}

func (l *Library) wikiText(ctx context.Context, source, ref string) (string, string, error) {
	path := l.path(source)
	if _, err := os.Stat(path); err != nil {
		return "", "", ErrNotFound
	}
	db, err := l.open(path)
	if err != nil {
		return "", "", err
	}
	var title, text string
	err = db.QueryRowContext(ctx, `SELECT title, text FROM pages WHERE title = ? COLLATE NOCASE LIMIT 1`, ref).Scan(&title, &text)
	if errors.Is(err, sql.ErrNoRows) {
		hits, searchErr := searchWiki(ctx, db, ref, 1, 1)
		if searchErr != nil {
			return "", "", searchErr
		}
		if len(hits) == 0 {
			return "", "", ErrNotFound
		}
		ref = hits[0].Ref
		err = db.QueryRowContext(ctx, `SELECT title, text FROM pages WHERE title = ? COLLATE NOCASE LIMIT 1`, ref).Scan(&title, &text)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return title, text, err
}

func (l *Library) gutenbergText(ctx context.Context, ref string) (string, string, string, error) {
	path := l.path("gutenberg")
	if _, err := os.Stat(path); err != nil {
		return "", "", "", ErrNotFound
	}
	db, err := l.open(path)
	if err != nil {
		return "", "", "", err
	}
	var id int64
	var title, author, bookPath string
	query := `SELECT id, title, author, path FROM books WHERE id = ?`
	err = db.QueryRowContext(ctx, query, ref).Scan(&id, &title, &author, &bookPath)
	if errors.Is(err, sql.ErrNoRows) {
		rows, searchErr := db.QueryContext(ctx, `SELECT b.id, b.title, b.author, b.path FROM books_fts JOIN books b ON b.id = books_fts.rowid
			WHERE books_fts MATCH ? ORDER BY bm25(books_fts) LIMIT 1`, ftsQuery(ref))
		if searchErr != nil {
			return "", "", "", searchErr
		}
		defer rows.Close()
		if !rows.Next() {
			return "", "", "", ErrNotFound
		}
		if err := rows.Scan(&id, &title, &author, &bookPath); err != nil {
			return "", "", "", err
		}
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", "", "", err
	}
	if info, statErr := os.Stat(bookPath); statErr != nil || info.Size() > 16<<20 {
		return "", "", "", errors.New("book text is unavailable or too large")
	}
	raw, err := os.ReadFile(bookPath)
	if err != nil {
		return "", "", "", err
	}
	attribution := fmt.Sprintf("Project Gutenberg #%d", id)
	if author != "" {
		attribution += " — " + author
	}
	return title, attribution, gutenbergBody(string(raw)), nil
}

func (l *Library) listSource(ctx context.Context, source string, limit int) (map[string]any, error) {
	path := l.path(source)
	if _, err := os.Stat(path); err != nil {
		return map[string]any{"available": false, "items": []any{}, "total": 0}, nil
	}
	db, err := l.open(path)
	if err != nil {
		return nil, err
	}
	var total int
	items := []map[string]any{}
	if source == "gutenberg" {
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM books`).Scan(&total); err != nil {
			return nil, err
		}
		rows, err := db.QueryContext(ctx, `SELECT id, title, author FROM books ORDER BY title LIMIT ?`, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var title, author string
			if err := rows.Scan(&id, &title, &author); err != nil {
				return nil, err
			}
			items = append(items, map[string]any{"ref": strconv.FormatInt(id, 10), "title": title, "author": author})
		}
		return map[string]any{"available": true, "items": items, "total": total}, rows.Err()
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pages`).Scan(&total); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT title FROM pages ORDER BY title LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var title string
		if err := rows.Scan(&title); err != nil {
			return nil, err
		}
		items = append(items, map[string]any{"ref": title, "title": title})
	}
	return map[string]any{"available": true, "items": items, "total": total}, rows.Err()
}

// Search ranks query across source ("auto" or one of SourceNames).
func (l *Library) Search(ctx context.Context, query, source string, limit int) ([]Hit, []string, error) {
	names := sourceOrder
	if source != "auto" {
		names = []string{source}
	}
	var hits []Hit
	available := []string{}
	for _, name := range names {
		list, ok, err := l.searchSource(ctx, name, query, limit)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			available = append(available, name)
		}
		hits = append(hits, list...)
	}
	slices.SortStableFunc(hits, func(a, b Hit) int {
		if a.Rank < b.Rank {
			return -1
		}
		if a.Rank > b.Rank {
			return 1
		}
		return 0
	})
	if source == "auto" && len(hits) > len(sourceOrder) && limit >= len(sourceOrder) {
		// Keep one strong hit per source so a book request can still see a
		// Gutenberg title when a short Simple Wikipedia article ranks higher.
		best := map[string]Hit{}
		for _, hit := range hits {
			if _, ok := best[hit.Source]; !ok {
				best[hit.Source] = hit
			}
		}
		seen := map[string]bool{}
		merged := make([]Hit, 0, limit)
		for _, name := range sourceOrder {
			if hit, ok := best[name]; ok {
				merged = append(merged, hit)
				seen[hit.Source+"\x00"+hit.Ref] = true
			}
		}
		for _, hit := range hits {
			if len(merged) >= limit {
				break
			}
			if !seen[hit.Source+"\x00"+hit.Ref] {
				merged = append(merged, hit)
				seen[hit.Source+"\x00"+hit.Ref] = true
			}
		}
		hits = merged
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, available, nil
}

// Read returns one bounded section of the named text.
func (l *Library) Read(ctx context.Context, source, ref string, section int) (map[string]any, error) {
	if source == "" {
		source = "auto"
	}
	if source == "auto" {
		for _, name := range sourceOrder {
			if !l.installed(name) {
				continue
			}
			out, err := l.readText(ctx, name, ref, section)
			if err == nil {
				return out, nil
			}
			if !errors.Is(err, ErrNotFound) {
				return nil, err
			}
		}
		return nil, ErrNotFound
	}
	return l.readText(ctx, source, ref, section)
}

// List returns the titles of one source, or of every source for "auto".
func (l *Library) List(ctx context.Context, source string, limit int) (map[string]any, error) {
	if source == "" {
		source = "auto"
	}
	if source == "auto" {
		out := map[string]any{"ok": true, "sources": map[string]any{}}
		for _, name := range sourceOrder {
			list, err := l.listSource(ctx, name, limit)
			if err != nil {
				return nil, err
			}
			out["sources"].(map[string]any)[name] = list
		}
		return out, nil
	}
	list, err := l.listSource(ctx, source, limit)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "source": source, "items": list}, nil
}

// SourceText loads the full text used by a story import. source may be "auto",
// in which case the first installed source with a match wins.
func (l *Library) SourceText(ctx context.Context, source, ref string) (title, author, text, used string, err error) {
	if source == "" {
		source = "auto"
	}
	if source == "auto" {
		for _, name := range sourceOrder {
			if !l.installed(name) {
				continue
			}
			title, author, text, err = l.sourceText(ctx, name, ref)
			if err == nil {
				return truncateImport(title), author, truncateText(text), name, nil
			}
			if !errors.Is(err, ErrNotFound) {
				return "", "", "", "", err
			}
		}
		return "", "", "", "", ErrNotFound
	}
	title, author, text, err = l.sourceText(ctx, source, ref)
	if err != nil {
		return "", "", "", "", err
	}
	return truncateImport(title), author, truncateText(text), source, nil
}

func (l *Library) sourceText(ctx context.Context, source, ref string) (title, author, text string, err error) {
	if source == "gutenberg" {
		return l.gutenbergText(ctx, ref)
	}
	title, text, err = l.wikiText(ctx, source, ref)
	if err != nil {
		return "", "", "", err
	}
	author = "Wikipedia (CC BY-SA 4.0)"
	if source == "simple" {
		author = "Simple English Wikipedia (CC BY-SA 4.0)"
	}
	return title, author, text, nil
}

func truncateImport(s string) string { return s }

func truncateText(text string) string {
	if len(text) > importTextMax {
		return text[:importTextMax]
	}
	return text
}
