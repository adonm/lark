package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Local content library: one SQLite FTS5 database per source, plain text
// books on disk, and a small pinned downloader. No services, no vectors.

const (
	libraryMaxText    = 24 << 10 // Cap stored article text.
	libraryReadChunk  = 3200     // Characters returned per read section.
	libraryReadMax    = 4000     // Hard ceiling for one read.
	librarySnippet    = 240
	libraryImportText = 36 << 10
	libraryPassage    = 575
	libraryPassages   = 60
	libraryGutenberg  = "https://gutenberg.pglaf.org"
	libraryWikiDumps  = "https://dumps.wikimedia.org"
	libraryUserAgent  = "LarkLibrary/1.0 (local companion reader)"
)

type librarySource struct {
	name   string
	db     string
	label  string
	weight float64
}

var librarySources = map[string]librarySource{
	// Higher weights give shorter, kid-friendly text a ranking boost when one
	// query spans several sources. bm25 is negative, so a larger multiplier
	// sorts earlier.
	"simple":    {name: "simple", db: "simple.db", label: "Simple English Wikipedia", weight: 2},
	"enwiki":    {name: "enwiki", db: "enwiki.db", label: "English Wikipedia", weight: 1},
	"gutenberg": {name: "gutenberg", db: "gutenberg.db", label: "Project Gutenberg", weight: 1.2},
}

var librarySourceOrder = []string{"simple", "enwiki", "gutenberg"}

func defaultLibraryDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data := env("XDG_DATA_HOME", filepath.Join(home, ".local/share"))
	return filepath.Join(data, "lark", "library")
}

func (b *larkServer) libraryDir() string {
	if b != nil && b.library != "" {
		return b.library
	}
	return env("LARK_LIBRARY_DIR", defaultLibraryDir())
}

func libraryDBSN(reader bool) string {
	if reader {
		// bm25 scans the whole posting list for every hit; a larger page cache
		// keeps the enwiki index hot across agent calls.
		return "?_query_only=1&_busy_timeout=5000&_cache_size=-262144"
	}
	return "?_busy_timeout=5000&_cache_size=-64000"
}

func (b *larkServer) openLibrary(path string) (*sql.DB, error) {
	if b == nil {
		return openLibraryDB(path, true)
	}
	b.libraryMu.Lock()
	defer b.libraryMu.Unlock()
	if db := b.libraryDBs[path]; db != nil {
		return db, nil
	}
	db, err := openLibraryDB(path, true)
	if err != nil {
		return nil, err
	}
	if b.libraryDBs == nil {
		b.libraryDBs = map[string]*sql.DB{}
	}
	b.libraryDBs[path] = db
	return db, nil
}

func openLibraryDB(path string, reader bool) (*sql.DB, error) {
	if reader {
		if _, err := os.Stat(path); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("lark-sqlite3", path+libraryDBSN(reader))
	if err != nil {
		return nil, err
	}
	if !reader {
		db.SetMaxOpenConns(1)
	} else {
		// Each connection owns its page cache; bound the reader pool so three
		// packs cannot multiply the 256 MiB cache without limit.
		db.SetMaxOpenConns(2)
		db.SetMaxIdleConns(2)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

type libraryHit struct {
	Source  string  `json:"source"`
	Title   string  `json:"title"`
	Ref     string  `json:"ref"`
	Snippet string  `json:"snippet,omitempty"`
	Rank    float64 `json:"rank"`
}

var ftsWordRE = regexp.MustCompile(`[[:alnum:]]+`)

func ftsQuery(query string) string {
	words := ftsWordRE.FindAllString(strings.ToLower(query), -1)
	stop := map[string]bool{"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
		"be": true, "by": true, "for": true, "from": true, "how": true, "in": true, "is": true,
		"it": true, "of": true, "on": true, "or": true, "that": true, "the": true, "this": true,
		"to": true, "was": true, "what": true, "when": true, "where": true, "which": true,
		"who": true, "why": true, "with": true}
	kept := words[:0]
	for _, word := range words {
		if !stop[word] {
			kept = append(kept, word)
		}
	}
	if len(kept) == 0 {
		kept = words
	}
	quoted := make([]string, len(kept))
	for i, word := range kept {
		quoted[i] = `"` + strings.ReplaceAll(word, `"`, "") + `"`
	}
	return strings.Join(quoted, " AND ")
}

func ftsQueryOr(query string) string {
	terms := strings.Split(ftsQuery(query), " AND ")
	return strings.Join(terms, " OR ")
}

func (b *larkServer) librarySearch(ctx context.Context, args object) (object, error) {
	query := strings.TrimSpace(str(args["query"]))
	if query == "" {
		return nil, fieldError("query", "required")
	}
	source := strings.TrimSpace(str(args["source"]))
	if source == "" {
		source = "auto"
	}
	limit := 5
	if n, err := mcpUint(args, "limit"); err != nil {
		return nil, err
	} else if n != nil {
		limit = int(*n)
	}
	if limit < 1 || limit > 20 {
		return nil, fieldError("limit", "expected 1–20")
	}
	dir := b.libraryDir()
	names := librarySourceOrder
	if source != "auto" {
		if _, ok := librarySources[source]; !ok {
			return nil, fieldError("source", "expected auto, simple, enwiki, or gutenberg")
		}
		names = []string{source}
	}
	var hits []libraryHit
	available := []string{}
	for _, name := range names {
		list, ok, err := searchLibrarySource(ctx, b, dir, name, query, limit)
		if err != nil {
			return nil, libraryProblem(err, "Cannot search the library")
		}
		if ok {
			available = append(available, name)
		}
		hits = append(hits, list...)
	}
	slices.SortStableFunc(hits, func(a, b libraryHit) int {
		if a.Rank < b.Rank {
			return -1
		}
		if a.Rank > b.Rank {
			return 1
		}
		return 0
	})
	if source == "auto" && len(hits) > len(librarySourceOrder) && limit >= len(librarySourceOrder) {
		// Keep one strong hit per source so a book request can still see a
		// Gutenberg title when a short Simple Wikipedia article ranks higher.
		best := map[string]libraryHit{}
		for _, hit := range hits {
			if _, ok := best[hit.Source]; !ok {
				best[hit.Source] = hit
			}
		}
		seen := map[string]bool{}
		merged := make([]libraryHit, 0, limit)
		for _, name := range librarySourceOrder {
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
	return object{"ok": true, "results": hits, "available": available}, nil
}

func searchLibrarySource(ctx context.Context, b *larkServer, dir, name, query string, limit int) ([]libraryHit, bool, error) {
	meta := librarySources[name]
	path := filepath.Join(dir, meta.db)
	if _, err := os.Stat(path); err != nil {
		return nil, false, nil
	}
	db, err := b.openLibrary(path)
	if err != nil {
		return nil, true, err
	}
	var hits []libraryHit
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

func searchWiki(ctx context.Context, db *sql.DB, query string, limit int, weight float64) ([]libraryHit, error) {
	run := func(match string) ([]libraryHit, error) {
		return searchWikiMatch(ctx, db, query, match, limit, weight)
	}
	hits, err := run(ftsQuery(query))
	if err == nil && len(hits) == 0 {
		hits, err = run(ftsQueryOr(query))
	}
	return hits, err
}

func searchWikiMatch(ctx context.Context, db *sql.DB, query, match string, limit int, weight float64) ([]libraryHit, error) {
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
	out := make([]libraryHit, 0, len(candidates))
	for _, c := range candidates {
		var text string
		if err := db.QueryRowContext(ctx, `SELECT text FROM pages WHERE id = ?`, c.id).Scan(&text); err != nil {
			return nil, err
		}
		out = append(out, libraryHit{Source: "wiki", Title: c.title, Ref: c.title,
			Snippet: libraryPreview(text, words), Rank: c.rank * weight})
	}
	return out, nil
}

// libraryPreview returns a short window around the first query term. FTS5's
// snippet() reloads the full article for every ranked candidate; one window
// from the final rows is enough for the agent's preview.
func libraryPreview(text string, words []string) string {
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
	end := min(len(text), start+librarySnippet)
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

func searchGutenberg(ctx context.Context, db *sql.DB, query string, limit int, weight float64) ([]libraryHit, error) {
	run := func(match string) ([]libraryHit, error) {
		rows, err := db.QueryContext(ctx, `SELECT b.id, b.title, b.author, snippet(books_fts, 0, '', '', '…', 16), bm25(books_fts, 8.0, 2.0)
			FROM books_fts JOIN books b ON b.id = books_fts.rowid
			WHERE books_fts MATCH ? ORDER BY bm25(books_fts, 8.0, 2.0) LIMIT ?`, match, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []libraryHit{}
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
			out = append(out, libraryHit{Source: "gutenberg", Title: title, Ref: strconv.FormatInt(id, 10),
				Snippet: strings.Join(strings.Fields(snip), " "), Rank: rank * weight})
		}
		slices.SortStableFunc(out, func(a, b libraryHit) int {
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

func (b *larkServer) libraryRead(ctx context.Context, args object) (object, error) {
	source := strings.TrimSpace(str(args["source"]))
	ref := strings.TrimSpace(str(args["title"]))
	if ref == "" {
		ref = strings.TrimSpace(str(args["ref"]))
	}
	if ref == "" {
		return nil, fieldError("title", "required")
	}
	if source == "" {
		source = "auto"
	}
	if source == "auto" {
		for _, name := range librarySourceOrder {
			if _, err := os.Stat(filepath.Join(b.libraryDir(), librarySources[name].db)); err != nil {
				continue
			}
			out, err := readLibraryText(ctx, b, b.libraryDir(), name, ref, args)
			if err == nil {
				return out, nil
			}
			if !errors.Is(err, errLibraryNotFound) {
				return nil, libraryProblem(err, "Cannot read library text")
			}
		}
		return nil, problem(404, "Library text not found")
	}
	if _, ok := librarySources[source]; !ok {
		return nil, fieldError("source", "expected auto, simple, enwiki, or gutenberg")
	}
	out, err := readLibraryText(ctx, b, b.libraryDir(), source, ref, args)
	if err != nil {
		return nil, libraryProblem(err, "Cannot read library text")
	}
	return out, nil
}

var errLibraryNotFound = errors.New("library text not found")

func readLibraryText(ctx context.Context, b *larkServer, dir, source, ref string, args object) (object, error) {
	section := 0
	if n, err := mcpUint(args, "section"); err != nil {
		return nil, err
	} else if n != nil {
		section = int(*n)
	}
	if section < 0 || section > 100000 {
		return nil, fieldError("section", "expected a non-negative integer")
	}
	var title, text string
	var err error
	if source == "gutenberg" {
		title, _, text, err = gutenbergText(ctx, b, dir, ref)
	} else {
		title, text, err = wikiText(ctx, b, dir, source, ref)
	}
	if err != nil {
		return nil, err
	}
	return boundedLibraryText(source, title, ref, text, section), nil
}

func boundedLibraryText(source, title, ref, text string, section int) object {
	runes := []rune(text)
	total := len(runes)
	sections := (total + libraryReadChunk - 1) / libraryReadChunk
	if sections == 0 {
		sections = 1
	}
	if section >= sections {
		return object{"source": source, "title": title, "ref": ref, "section": section,
			"sections": sections, "text": "", "chars": 0, "total_chars": total}
	}
	start := section * libraryReadChunk
	end := min(total, start+libraryReadChunk)
	if end-start > libraryReadMax {
		end = start + libraryReadMax
	}
	out := object{"source": source, "title": title, "ref": ref, "section": section,
		"sections": sections, "text": string(runes[start:end]), "chars": end - start, "total_chars": total}
	if section+1 < sections {
		out["next_section"] = section + 1
	}
	return out
}

func wikiText(ctx context.Context, b *larkServer, dir, source, ref string) (string, string, error) {
	path := filepath.Join(dir, librarySources[source].db)
	if _, err := os.Stat(path); err != nil {
		return "", "", errLibraryNotFound
	}
	db, err := b.openLibrary(path)
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
			return "", "", errLibraryNotFound
		}
		ref = hits[0].Ref
		err = db.QueryRowContext(ctx, `SELECT title, text FROM pages WHERE title = ? COLLATE NOCASE LIMIT 1`, ref).Scan(&title, &text)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", errLibraryNotFound
	}
	return title, text, err
}

func gutenbergText(ctx context.Context, b *larkServer, dir, ref string) (string, string, string, error) {
	path := filepath.Join(dir, librarySources["gutenberg"].db)
	if _, err := os.Stat(path); err != nil {
		return "", "", "", errLibraryNotFound
	}
	db, err := b.openLibrary(path)
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
			return "", "", "", errLibraryNotFound
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

func (b *larkServer) libraryList(ctx context.Context, args object) (object, error) {
	source := strings.TrimSpace(str(args["source"]))
	if source == "" {
		source = "auto"
	}
	limit := 50
	if n, err := mcpUint(args, "limit"); err != nil {
		return nil, err
	} else if n != nil {
		limit = int(*n)
	}
	if limit < 1 || limit > 200 {
		return nil, fieldError("limit", "expected 1–200")
	}
	if source == "auto" {
		out := object{"ok": true, "sources": map[string]any{}}
		for _, name := range librarySourceOrder {
			list, err := libraryListSource(ctx, b, b.libraryDir(), name, limit)
			if err != nil {
				return nil, libraryProblem(err, "Cannot list the library")
			}
			out["sources"].(map[string]any)[name] = list
		}
		return out, nil
	}
	if _, ok := librarySources[source]; !ok {
		return nil, fieldError("source", "expected auto, simple, enwiki, or gutenberg")
	}
	list, err := libraryListSource(ctx, b, b.libraryDir(), source, limit)
	if err != nil {
		return nil, libraryProblem(err, "Cannot list the library")
	}
	return object{"ok": true, "source": source, "items": list}, nil
}

func libraryListSource(ctx context.Context, b *larkServer, dir, source string, limit int) (object, error) {
	path := filepath.Join(dir, librarySources[source].db)
	if _, err := os.Stat(path); err != nil {
		return object{"available": false, "items": []any{}, "total": 0}, nil
	}
	db, err := b.openLibrary(path)
	if err != nil {
		return nil, err
	}
	var total int
	items := []object{}
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
			items = append(items, object{"ref": strconv.FormatInt(id, 10), "title": title, "author": author})
		}
		return object{"available": true, "items": items, "total": total}, rows.Err()
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
		items = append(items, object{"ref": title, "title": title})
	}
	return object{"available": true, "items": items, "total": total}, rows.Err()
}

func (b *larkServer) libraryImport(ctx context.Context, args object) (object, error) {
	source := strings.TrimSpace(str(args["source"]))
	ref := strings.TrimSpace(str(args["title"]))
	if ref == "" {
		ref = strings.TrimSpace(str(args["ref"]))
	}
	if ref == "" {
		return nil, fieldError("title", "required")
	}
	if source == "" || source == "auto" {
		for _, name := range librarySourceOrder {
			if _, err := os.Stat(filepath.Join(b.libraryDir(), librarySources[name].db)); err != nil {
				continue
			}
			res, err := b.libraryImportSourceArgs(ctx, name, ref, args)
			if err == nil {
				return res, nil
			}
			if !errors.Is(err, errLibraryNotFound) {
				return nil, libraryProblem(err, "Cannot import library text")
			}
		}
		return nil, problem(404, "Library text not found")
	}
	if _, ok := librarySources[source]; !ok {
		return nil, fieldError("source", "expected auto, simple, enwiki, or gutenberg")
	}
	out, err := b.libraryImportSourceArgs(ctx, source, ref, args)
	if err != nil && !errors.Is(err, errLibraryNotFound) {
		return nil, libraryProblem(err, "Cannot import library text")
	}
	return out, err
}

// libraryProblem logs the underlying SQLite or filesystem detail and returns
// the server's stable problem shape to the agent.
func libraryProblem(err error, message string) error {
	if err == nil || errors.Is(err, errLibraryNotFound) {
		return err
	}
	var api *apiError
	if errors.As(err, &api) {
		return err
	}
	log.Printf("library: %s: %v", message, err)
	return problem(502, message)
}

func (b *larkServer) libraryImportSourceArgs(ctx context.Context, source, ref string, args object) (object, error) {
	var title, text, author string
	var err error
	if source == "gutenberg" {
		title, author, text, err = gutenbergText(ctx, b, b.libraryDir(), ref)
	} else {
		title, text, err = wikiText(ctx, b, b.libraryDir(), source, ref)
		if err == nil {
			author = "Wikipedia (CC BY-SA 4.0)"
			if source == "simple" {
				author = "Simple English Wikipedia (CC BY-SA 4.0)"
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if len(text) > libraryImportText {
		text = text[:libraryImportText]
	}
	twee, err := libraryTwee(title, text)
	if err != nil {
		return nil, err
	}
	slug := b.nextAvailableStorySlug(ctx, title)
	saved, cs, err := b.saveStory(ctx, slug, twee, author, true)
	if err != nil {
		return nil, err
	}
	openOn := true
	if raw, ok := args["open"]; ok {
		openOn, _ = raw.(bool)
	}
	companionID := strings.ToLower(strings.TrimSpace(str(args["companion_id"])))
	opened := false
	if openOn && companionID != "" && b.hub != nil {
		b.hub.mu.Lock()
		for _, d := range b.hub.terminalsFor(companionID) {
			b.hub.openStory(d, saved, false)
			opened = true
		}
		b.hub.mu.Unlock()
	}
	return object{"ok": true, "id": saved.ID, "title": saved.Title, "author": saved.Author,
		"passages": saved.Passages, "start": cs.Passages[cs.Start].Name, "source": source, "ref": ref,
		"opened": opened}, nil
}

// mcpLibrary is the typed MCP entry point.
func (b *larkServer) mcpLibrary(ctx context.Context, args object) (object, error) {
	op := strings.TrimSpace(str(args["op"]))
	start := time.Now()
	var out object
	var err error
	switch op {
	case "search":
		out, err = b.librarySearch(ctx, args)
	case "read":
		out, err = b.libraryRead(ctx, args)
	case "list":
		out, err = b.libraryList(ctx, args)
	case "import":
		out, err = b.libraryImport(ctx, args)
	default:
		return nil, fieldError("op", "expected search, read, list, or import")
	}
	opened, _ := out["opened"].(bool)
	b.libraryStats.record(op, str(args["query"]), str(args["title"]), time.Since(start), err == nil, opened)
	return out, err
}

type libraryCall struct {
	Op    string  `json:"op"`
	Query string  `json:"query,omitempty"`
	MS    float64 `json:"ms"`
	OK    bool    `json:"ok"`
	At    string  `json:"at"`
}

type libraryStats struct {
	mu       sync.Mutex
	searches int64
	reads    int64
	imports  int64
	opens    int64
	lists    int64
	recent   []libraryCall
}

func (s *libraryStats) record(op, query, title string, elapsed time.Duration, ok, opened bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch op {
	case "search":
		s.searches++
		if query == "" {
			query = title
		}
	case "read":
		s.reads++
		if query == "" {
			query = title
		}
	case "import":
		s.imports++
		if opened {
			s.opens++
		}
		if query == "" {
			query = title
		}
	case "list":
		s.lists++
	}
	s.recent = append(s.recent, libraryCall{Op: op, Query: shortText(query, 96, false),
		MS: milliseconds(elapsed), OK: ok, At: time.Now().UTC().Format(time.RFC3339)})
	if len(s.recent) > 20 {
		s.recent = s.recent[len(s.recent)-20:]
	}
}

func (b *larkServer) libraryReport() object {
	b.libraryStats.mu.Lock()
	out := object{"searches": b.libraryStats.searches, "reads": b.libraryStats.reads,
		"imports": b.libraryStats.imports, "opens": b.libraryStats.opens, "lists": b.libraryStats.lists,
		"recent": append([]libraryCall(nil), b.libraryStats.recent...)}
	b.libraryStats.mu.Unlock()
	return out
}

func libraryStatus(ctx context.Context, dir string) object {
	out := object{"dir": dir, "sources": object{}}
	for _, name := range librarySourceOrder {
		pack, _ := libraryPackByName(name)
		path := filepath.Join(dir, librarySources[name].db)
		entry := object{"path": path}
		available, reason := libraryPackHealth(dir, pack)
		entry["available"] = available
		if reason != "" {
			entry["error"] = reason
		}
		if info, err := os.Stat(path); err == nil {
			entry["db_bytes"] = info.Size()
		} else if !errors.Is(err, os.ErrNotExist) {
			entry["error"] = err.Error()
		}
		if available {
			db, err := openLibraryDB(path, true)
			if err != nil {
				entry["available"], entry["error"] = false, err.Error()
			} else {
				table := "pages"
				if name == "gutenberg" {
					table = "books"
				}
				var total int
				if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&total); err != nil {
					entry["available"], entry["error"] = false, err.Error()
				} else {
					entry["items"] = total
				}
				var built string
				if db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='built_at'`).Scan(&built) == nil {
					entry["built_at"] = built
				}
				db.Close()
			}
		}
		if lastErr := readPackError(dir, name); lastErr != "" {
			entry["last_error"] = lastErr
		}
		if name == "gutenberg" {
			if books, err := os.ReadDir(filepath.Join(dir, "books")); err == nil {
				var bytes int64
				for _, book := range books {
					if info, err := book.Info(); err == nil {
						bytes += info.Size()
					}
				}
				entry["book_files"], entry["book_bytes"] = len(books), bytes
			}
		}
		out["sources"].(map[string]any)[name] = entry
	}
	return out
}

// ---- Downloader and index builder ----

func runLibrary(ctx context.Context, args []string, out, errOut io.Writer) error {
	flags := flag.NewFlagSet("library", flag.ContinueOnError)
	flags.SetOutput(errOut)
	download := flags.Bool("download", false, "download raw dumps and books")
	build := flags.Bool("build", false, "build SQLite indexes from raw files")
	force := flags.Bool("force", false, "rebuild indexes even when present")
	status := flags.Bool("status", false, "print library paths, sizes, and counts")
	plan := flags.Bool("plan", false, "print the disk-aware provisioning plan and exit")
	search := flags.String("search", "", "search the installed indexes and print JSON")
	read := flags.String("read", "", "read a bounded section and print JSON")
	source := flags.String("source", "all", "all, enwiki, simple, or gutenberg")
	limit := flags.Int("limit", 3000, "maximum Gutenberg titles")
	jobs := flags.Int("jobs", 8, "parallel Gutenberg downloads")
	if err := flags.Parse(args); err != nil {
		return err
	}
	dir := env("LARK_LIBRARY_DIR", defaultLibraryDir())
	if dir == "" {
		return errors.New("cannot resolve the library directory")
	}
	if err := os.MkdirAll(filepath.Join(dir, "raw"), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "books"), 0o700); err != nil {
		return err
	}
	if *status {
		raw, _ := json.MarshalIndent(libraryStatus(ctx, dir), "", "  ")
		fmt.Fprintln(out, string(raw))
		return nil
	}
	if *plan {
		p, err := planLibrary(dir, nil)
		if err != nil {
			return err
		}
		raw, _ := json.MarshalIndent(p, "", "  ")
		fmt.Fprintln(out, string(raw))
		return nil
	}
	if *search != "" || *read != "" {
		b := &larkServer{library: dir}
		args := object{"source": *source}
		op := "search"
		if *read != "" {
			op, args["title"] = "read", *read
		} else {
			args["query"] = *search
		}
		if *source == "all" {
			args["source"] = "auto"
		}
		result, err := b.mcpLibrary(ctx, object{"op": op, "query": args["query"], "title": args["title"], "source": args["source"]})
		if err != nil {
			return err
		}
		raw, _ := json.MarshalIndent(result, "", "  ")
		fmt.Fprintln(out, string(raw))
		return nil
	}
	if !*download && !*build {
		return errors.New("expected --plan, --download, --build, or --status")
	}
	if *download {
		if *source == "all" || *source == "enwiki" {
			if err := downloadWikipedia(ctx, dir, "enwiki", out); err != nil {
				return err
			}
		}
		if *source == "all" || *source == "simple" {
			if err := downloadWikipedia(ctx, dir, "simple", out); err != nil {
				return err
			}
		}
		if *source == "all" || *source == "gutenberg" {
			if err := downloadGutenberg(ctx, dir, *limit, *jobs, out); err != nil {
				return err
			}
		}
	}
	if *build {
		if *source == "all" || *source == "enwiki" {
			if err := buildWikipedia(ctx, dir, "enwiki", *force, out); err != nil {
				return err
			}
		}
		if *source == "all" || *source == "simple" {
			if err := buildWikipedia(ctx, dir, "simple", *force, out); err != nil {
				return err
			}
		}
		if *source == "all" || *source == "gutenberg" {
			if err := buildGutenberg(ctx, dir, *force, out); err != nil {
				return err
			}
		}
	}
	return nil
}

func libraryLog(out io.Writer, format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	fmt.Fprintln(out, line)
	log.Print("library: " + line)
}
