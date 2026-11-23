package main

import (
	"bufio"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
		return "?_query_only=1&_busy_timeout=5000&_cache_size=-64000"
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

func normalizeTitle(title string) string {
	title = strings.ToLower(strings.TrimSpace(title))
	title = strings.TrimPrefix(title, "the ")
	title = strings.Trim(title, " .,:;!?\"'()[]")
	return strings.Join(strings.Fields(title), " ")
}

func ftsQuery(query string) string {
	words := regexp.MustCompile(`[[:alnum:]]+`).FindAllString(strings.ToLower(query), -1)
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
			return nil, err
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

func searchWiki(ctx context.Context, db *sql.DB, query string, limit int, weight float64) ([]libraryHit, error) {
	run := func(match string) ([]libraryHit, error) {
		rows, err := db.QueryContext(ctx, `SELECT p.title, snippet(pages_fts, 1, '', '', '…', 28),
				bm25(pages_fts, 8.0, 1.0) - CASE WHEN lower(p.title) = lower(?) THEN 10.0 ELSE 0 END
			FROM pages_fts JOIN pages p ON p.id = pages_fts.rowid
			WHERE pages_fts MATCH ? ORDER BY 3 LIMIT ?`, query, match, limit*4+10)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []libraryHit{}
		for rows.Next() {
			var title, snip string
			var rank float64
			if err := rows.Scan(&title, &snip, &rank); err != nil {
				return nil, err
			}
			if normalizeTitle(title) == normalizeTitle(query) {
				rank -= 10
			}
			if words := len(strings.Fields(title)); words > 0 {
				rank -= 4 / float64(words)
			}
			out = append(out, libraryHit{Source: "wiki", Title: title, Ref: title,
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
				return nil, err
			}
		}
		return nil, problem(404, "Library text not found")
	}
	if _, ok := librarySources[source]; !ok {
		return nil, fieldError("source", "expected auto, simple, enwiki, or gutenberg")
	}
	return readLibraryText(ctx, b, b.libraryDir(), source, ref, args)
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

var pgStart = regexp.MustCompile(`(?m)^\*\*\* ?START OF (?:THE|THIS) PROJECT GUTENBERG[^\n]*\n`)
var pgEnd = regexp.MustCompile(`(?m)^\*\*\* ?END OF (?:THE|THIS) PROJECT GUTENBERG[^\n]*\n?`)

func gutenbergBody(text string) string {
	if m := pgStart.FindStringIndex(text); m != nil {
		text = text[m[1]:]
	}
	if m := pgEnd.FindStringIndex(text); m != nil {
		text = text[:m[0]]
	}
	return strings.TrimSpace(text)
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
				return nil, err
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
		return nil, err
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
				return nil, err
			}
		}
		return nil, problem(404, "Library text not found")
	}
	if _, ok := librarySources[source]; !ok {
		return nil, fieldError("source", "expected auto, simple, enwiki, or gutenberg")
	}
	return b.libraryImportSourceArgs(ctx, source, ref, args)
}

func (b *larkServer) libraryImportSource(ctx context.Context, source, ref string) (object, error) {
	return b.libraryImportSourceArgs(ctx, source, ref, object{})
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
	if openOn && companionID != "" && b.ws != nil {
		b.ws.mu.Lock()
		for _, d := range b.ws.terminalsFor(companionID) {
			b.ws.openStory(d, saved, false)
			opened = true
		}
		b.ws.mu.Unlock()
	}
	return object{"ok": true, "id": saved.ID, "title": saved.Title, "author": saved.Author,
		"passages": saved.Passages, "start": cs.Passages[cs.Start].Name, "source": source, "ref": ref,
		"opened": opened}, nil
}

func libraryTwee(title, text string) (string, error) {
	title = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(title, "\n", " "), "::", ":"))
	if title == "" {
		title = "Library Story"
	}
	passages := splitLibraryPassages(text, libraryPassage)
	if len(passages) == 0 {
		passages = []string{"Once upon a time."}
	}
	if len(passages) > libraryPassages {
		passages = passages[:libraryPassages]
	}
	var out strings.Builder
	fmt.Fprintf(&out, ":: StoryTitle\n%s\n\n", title)
	for i, passage := range passages {
		if i+1 == len(passages) {
			fmt.Fprintf(&out, ":: Chapter %d {\"end\":true}\n%s\n", i+1, passage)
			continue
		}
		fmt.Fprintf(&out, ":: Chapter %d\n%s\n\n[[Continue->Chapter %d]]\n\n", i+1, passage, i+2)
	}
	twee := out.String()
	if _, err := parseTwee(twee); err != nil {
		return "", fieldError("twee", "generated story is invalid: %s", err)
	}
	return twee, nil
}

var tweeUnsafe = strings.NewReplacer("[", "(", "]", ")", "<", "‹", ">", "›")

func splitLibraryPassages(text string, maxRunes int) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	paragraphs := strings.Split(text, "\n\n")
	var passages []string
	for _, paragraph := range paragraphs {
		paragraph = strings.Join(strings.Fields(paragraph), " ")
		if paragraph == "" {
			continue
		}
		paragraph = tweeUnsafe.Replace(paragraph)
		for len([]rune(paragraph)) > maxRunes {
			cut := maxRunes
			runes := []rune(paragraph)
			for i := cut; i > maxRunes/2; i-- {
				if runes[i] == ' ' || runes[i] == '.' || runes[i] == '!' || runes[i] == '?' {
					cut = i + 1
					break
				}
			}
			passages = append(passages, strings.TrimSpace(string(runes[:cut])))
			paragraph = strings.TrimSpace(string(runes[cut:]))
		}
		if paragraph != "" {
			passages = append(passages, strings.TrimSpace(paragraph))
		}
	}
	return passages
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
		meta := librarySources[name]
		path := filepath.Join(dir, meta.db)
		info, err := os.Stat(path)
		entry := object{"available": err == nil, "path": path}
		if err == nil {
			entry["db_bytes"] = info.Size()
			if db, err := openLibraryDB(path, true); err == nil {
				var total int
				table := "pages"
				if name == "gutenberg" {
					table = "books"
				}
				if db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&total) == nil {
					entry["items"] = total
				}
				var built string
				if db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='built_at'`).Scan(&built) == nil {
					entry["built_at"] = built
				}
				db.Close()
			}
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
		return errors.New("expected --download, --build, or --status")
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

// downloadFile resumes a partial destination and verifies it. The expected
// checksum is the hex digest of hashName ("sha1" or "sha256"); empty skips it.
func downloadFile(ctx context.Context, client *http.Client, url, dest, hashName, expect string, out io.Writer) error {
	lock, err := os.OpenFile(dest+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("%s: another download is already running", filepath.Base(dest))
	}
	part := dest + ".part"
	if size, ok := probeDownloadSize(ctx, client, url); ok && size >= 256<<20 {
		if err := segmentedDownload(ctx, client, url, dest, size, hashName, expect, out); err != nil {
			return err
		}
		return nil
	}
	var offset int64
	if info, err := os.Stat(part); err == nil {
		offset = info.Size()
	} else if info, err := os.Stat(dest); err == nil {
		if expect == "" || fileHash(dest, hashName) == expect {
			return nil
		}
		offset = info.Size()
		_ = os.Rename(dest, part)
	}
	for round := 0; round < 2; round++ {
		for attempt := 1; ; attempt++ {
			if err := downloadRange(ctx, client, url, part, offset, out); err != nil {
				if attempt >= 8 || ctx.Err() != nil {
					return err
				}
				libraryLog(out, "retry %s (%v)", url, err)
				time.Sleep(time.Duration(attempt) * 2 * time.Second)
				if info, err := os.Stat(part); err == nil {
					offset = info.Size()
				} else {
					offset = 0
				}
				continue
			}
			break
		}
		if expect == "" {
			return os.Rename(part, dest)
		}
		if got := fileHash(part, hashName); got == expect {
			return os.Rename(part, dest)
		} else if round == 0 {
			libraryLog(out, "%s: checksum mismatch, restarting (%s)", filepath.Base(dest), got)
			_ = os.Remove(part)
			offset = 0
		}
	}
	return fmt.Errorf("%s: checksum mismatch", filepath.Base(dest))
}

func probeDownloadSize(ctx context.Context, client *http.Client, url string) (int64, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return 0, false
	}
	req.Header.Set("User-Agent", libraryUserAgent)
	res, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || res.ContentLength <= 0 || res.Header.Get("Accept-Ranges") != "bytes" {
		return 0, false
	}
	return res.ContentLength, true
}

// segmentedDownload fetches a large file in parallel byte ranges. Each segment
// resumes from its own partial file, so interrupted runs make progress.
func segmentedDownload(ctx context.Context, client *http.Client, url, dest string, size int64, hashName, expect string, out io.Writer) error {
	const chunks, workers = 8, 3
	part := dest + ".part"
	segments := make([]string, chunks)
	chunk := (size + chunks - 1) / chunks
	var mu sync.Mutex
	var written int64
	started := time.Now()
	stop := make(chan struct{})
	done := make(chan struct{})
	for i := 0; i < chunks; i++ {
		segments[i] = fmt.Sprintf("%s.seg%d", dest, i)
		if info, err := os.Stat(segments[i]); err == nil {
			written += info.Size()
		}
	}
	base := written
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				mu.Lock()
				n := written
				mu.Unlock()
				elapsed := time.Since(started).Seconds()
				rate := float64(n-base) / max(0.001, elapsed) / 1e6
				libraryLog(out, "%s: %s/%s (%.1f%%, %.1f MB/s)", filepath.Base(part),
					humanBytes(n), humanBytes(size), 100*float64(n)/float64(size), rate)
			case <-stop:
				return
			}
		}
	}()
	ch := make(chan int, chunks)
	for i := 0; i < chunks; i++ {
		ch <- i
	}
	close(ch)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ch {
				start := int64(i) * chunk
				end := min(size-1, start+chunk-1)
				if start > end {
					continue
				}
				if err := downloadSegment(ctx, client, url, segments[i], start, end, &mu, &written); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	close(stop)
	<-done
	var firstErr error
	for err := range errs {
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return firstErr
	}
	outFile, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	for _, path := range segments {
		seg, err := os.Open(path)
		if err != nil {
			outFile.Close()
			return err
		}
		_, copyErr := io.Copy(outFile, seg)
		seg.Close()
		if copyErr != nil {
			outFile.Close()
			return copyErr
		}
	}
	if err := outFile.Sync(); err != nil {
		outFile.Close()
		return err
	}
	if err := outFile.Close(); err != nil {
		return err
	}
	if expect != "" && fileHash(part, hashName) != expect {
		for _, path := range segments {
			_ = os.Remove(path)
		}
		_ = os.Remove(part)
		return fmt.Errorf("%s: checksum mismatch", filepath.Base(dest))
	}
	for _, path := range segments {
		_ = os.Remove(path)
	}
	return os.Rename(part, dest)
}

func downloadSegment(ctx context.Context, client *http.Client, url, path string, start, end int64, mu *sync.Mutex, written *int64) error {
	expected := end - start + 1
	offset := start
	if info, err := os.Stat(path); err == nil {
		if info.Size() > expected {
			_ = os.Remove(path)
		} else {
			offset = start + info.Size()
		}
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if offset == start {
		flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	file, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	for offset <= end {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", libraryUserAgent)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, end))
		res, err := client.Do(req)
		if err != nil {
			return err
		}
		if res.StatusCode != http.StatusPartialContent {
			res.Body.Close()
			return fmt.Errorf("segment %s: HTTP %d", filepath.Base(path), res.StatusCode)
		}
		buf := make([]byte, 1<<20)
		for {
			n, readErr := res.Body.Read(buf)
			if n > 0 {
				if _, err := file.Write(buf[:n]); err != nil {
					res.Body.Close()
					return err
				}
				offset += int64(n)
				mu.Lock()
				*written += int64(n)
				mu.Unlock()
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				res.Body.Close()
				return readErr
			}
		}
		res.Body.Close()
	}
	return nil
}

func downloadRange(ctx context.Context, client *http.Client, url, part string, offset int64, out io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", libraryUserAgent)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if offset > 0 && res.StatusCode == http.StatusOK {
		// Server ignored the range; start over.
		offset = 0
	}
	if res.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		_ = os.Remove(part)
		return fmt.Errorf("%s: partial file was longer than the source; restarting", url)
	}
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("%s: HTTP %d", url, res.StatusCode)
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if offset == 0 {
		flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	file, err := os.OpenFile(part, flags, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	total := res.ContentLength
	if total > 0 {
		total += offset
	}
	start := time.Now()
	last := start
	var written int64
	buf := make([]byte, 1<<20)
	for {
		n, readErr := res.Body.Read(buf)
		if n > 0 {
			if _, err := file.Write(buf[:n]); err != nil {
				return err
			}
			offset += int64(n)
			written += int64(n)
			if time.Since(last) >= 10*time.Second {
				last = time.Now()
				rate := float64(written) / max(0.001, time.Since(start).Seconds())
				if total > 0 {
					libraryLog(out, "%s: %s/%s (%.1f%%, %.1f MB/s)", filepath.Base(part), humanBytes(offset), humanBytes(total), 100*float64(offset)/float64(total), rate/1e6)
				} else {
					libraryLog(out, "%s: %s (%.1f MB/s)", filepath.Base(part), humanBytes(offset), rate/1e6)
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	return file.Sync()
}

func fileHash(path, name string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	var h io.Writer
	var sum func() string
	switch name {
	case "sha256":
		s := sha256.New()
		h, sum = s, func() string { return hex.EncodeToString(s.Sum(nil)) }
	default:
		s := sha1.New()
		h, sum = s, func() string { return hex.EncodeToString(s.Sum(nil)) }
	}
	if _, err := io.Copy(h, file); err != nil {
		return ""
	}
	return sum()
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 5 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func wikiSlug(wiki string) string {
	if wiki == "simple" {
		return "simplewiki"
	}
	return wiki
}

func wikiDumpName(wiki string) string { return wikiSlug(wiki) + "-latest-pages-articles.xml.bz2" }

func downloadWikipedia(ctx context.Context, dir, wiki string, out io.Writer) error {
	name := wikiDumpName(wiki)
	dest := filepath.Join(dir, "raw", name)
	sums, err := fetchWikiSHA1(ctx, wikiSlug(wiki))
	if err != nil {
		return err
	}
	expect, ok := sums[name]
	if !ok {
		// Latest checksum lists use dated filenames; match the stable suffix.
		for key, sum := range sums {
			if strings.HasSuffix(key, "-pages-articles.xml.bz2") {
				expect, ok = sum, true
				break
			}
		}
	}
	if !ok {
		return fmt.Errorf("%s: checksum not published", name)
	}
	if fileHash(dest, "sha1") == expect {
		libraryLog(out, "%s already verified", name)
		return nil
	}
	libraryLog(out, "downloading %s", name)
	client := &http.Client{Timeout: 0, Transport: &http.Transport{ResponseHeaderTimeout: 60 * time.Second,
		MaxIdleConnsPerHost: 4, IdleConnTimeout: 90 * time.Second}}
	if err := downloadFile(ctx, client, libraryWikiDumps+"/"+wikiSlug(wiki)+"/latest/"+name, dest, "sha1", expect, out); err != nil {
		return err
	}
	libraryLog(out, "%s verified (%s)", name, humanBytes(fileSize(dest)))
	recordChecksum(dir, name, fileHash(dest, "sha256"))
	return nil
}

func fetchWikiSHA1(ctx context.Context, wiki string) (map[string]string, error) {
	url := libraryWikiDumps + "/" + wiki + "/latest/" + wiki + "-latest-sha1sums.txt"
	raw, err := fetchText(ctx, url, 16<<20)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			out[strings.TrimPrefix(fields[1], "*")] = fields[0]
		}
	}
	return out, nil
}

func fileSize(path string) int64 {
	if info, err := os.Stat(path); err == nil {
		return info.Size()
	}
	return 0
}

func fetchText(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", libraryUserAgent)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, res.StatusCode)
	}
	return io.ReadAll(io.LimitReader(res.Body, limit))
}

func recordChecksum(dir, name, sum string) {
	if sum == "" {
		return
	}
	path := filepath.Join(dir, "SHA256SUMS")
	lines := map[string]string{}
	if raw, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if fields := strings.Fields(line); len(fields) == 2 {
				lines[fields[1]] = fields[0]
			}
		}
	}
	lines[name] = sum
	writeChecksums(path, lines)
}

func writeChecksums(path string, lines map[string]string) {
	names := make([]string, 0, len(lines))
	for name := range lines {
		names = append(names, name)
	}
	slices.Sort(names)
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "%s  %s\n", lines[name], name)
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, []byte(b.String()), 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

type gutenbergRecord struct {
	ID     int64  `json:"id"`
	Title  string `json:"title"`
	Author string `json:"author"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func downloadGutenberg(ctx context.Context, dir string, limit, jobs int, out io.Writer) error {
	if jobs < 1 || jobs > 32 {
		jobs = 8
	}
	if limit < 1 {
		limit = 3000
	}
	catalog, err := loadGutenbergCatalog(ctx, dir, out)
	if err != nil {
		return err
	}
	ids, err := pinnedGutenbergIDs()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		ids = selectGutenbergIDs(catalog, limit)
	}
	if limit < len(ids) {
		ids = ids[:limit]
	}
	manifestPath := filepath.Join(dir, "raw", "gutenberg.jsonl")
	records := map[int64]gutenbergRecord{}
	if raw, err := os.ReadFile(manifestPath); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			var rec gutenbergRecord
			if json.Unmarshal([]byte(line), &rec) == nil && rec.ID != 0 {
				records[rec.ID] = rec
			}
		}
	}
	libraryLog(out, "gutenberg: %d selected titles, %d already recorded", len(ids), len(records))
	var mu sync.Mutex
	jobsCh := make(chan int64)
	var wg sync.WaitGroup
	for range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := &http.Client{Timeout: 0, Transport: &http.Transport{MaxIdleConnsPerHost: 2,
				ResponseHeaderTimeout: 30 * time.Second}}
			for id := range jobsCh {
				rec, ok := records[id]
				path := filepath.Join(dir, "books", fmt.Sprintf("%05d.txt", id))
				if ok && rec.SHA256 != "" && fileHash(path, "sha256") == rec.SHA256 {
					continue
				}
				meta, exists := catalog[id]
				if !exists {
					continue
				}
				url := fmt.Sprintf("%s/cache/epub/%d/pg%d.txt", libraryGutenberg, id, id)
				dest := path + ".part"
				if err := downloadSmall(ctx, client, url, dest); err != nil {
					libraryLog(out, "gutenberg %d: %v", id, err)
					_ = os.Remove(dest)
					continue
				}
				if err := os.Rename(dest, path); err != nil {
					continue
				}
				sum := fileHash(path, "sha256")
				rec = gutenbergRecord{ID: id, Title: meta.title, Author: meta.author, Path: path, SHA256: sum}
				mu.Lock()
				records[id] = rec
				appendLine(manifestPath, rec)
				mu.Unlock()
				libraryLog(out, "gutenberg %d: %s", id, meta.title)
			}
		}()
	}
	for _, id := range ids {
		jobsCh <- id
	}
	close(jobsCh)
	wg.Wait()
	sums := map[string]string{}
	for _, rec := range records {
		if rec.SHA256 != "" {
			sums[filepath.Base(rec.Path)] = rec.SHA256
		}
	}
	writeChecksums(filepath.Join(dir, "books", "SHA256SUMS"), sums)
	libraryLog(out, "gutenberg: %d files recorded", len(records))
	return nil
}

func downloadSmall(ctx context.Context, client *http.Client, url, dest string) error {
	var last error
	for attempt := 1; attempt <= 6; attempt++ {
		var offset int64
		if info, err := os.Stat(dest); err == nil {
			offset = info.Size()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", libraryUserAgent)
		if offset > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		}
		res, err := client.Do(req)
		if err != nil {
			last = err
		} else if res.StatusCode == http.StatusNotFound {
			res.Body.Close()
			return fmt.Errorf("not available")
		} else if res.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			res.Body.Close()
			_ = os.Remove(dest)
			last = fmt.Errorf("range rejected")
		} else if res.StatusCode == http.StatusOK || res.StatusCode == http.StatusPartialContent {
			flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
			if res.StatusCode == http.StatusOK {
				offset = 0
				flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
			}
			file, err := os.OpenFile(dest, flags, 0o600)
			if err != nil {
				res.Body.Close()
				return err
			}
			_, copyErr := io.Copy(file, res.Body)
			closeErr := file.Close()
			res.Body.Close()
			if copyErr == nil && closeErr == nil {
				return nil
			}
			last = errors.Join(copyErr, closeErr)
		} else {
			res.Body.Close()
			last = fmt.Errorf("HTTP %d", res.StatusCode)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * time.Second):
		}
	}
	return last
}

func appendLine(path string, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(raw, '\n'))
}

type gutenbergMeta struct{ title, author string }

func loadGutenbergCatalog(ctx context.Context, dir string, out io.Writer) (map[int64]gutenbergMeta, error) {
	path := filepath.Join(dir, "raw", "pg_catalog.csv.gz")
	if _, err := os.Stat(path); err != nil {
		libraryLog(out, "fetching Gutenberg catalog")
		if err := downloadSmall(ctx, &http.Client{Timeout: 300 * time.Second},
			libraryGutenberg+"/cache/epub/feeds/pg_catalog.csv.gz", path+".part"); err != nil {
			return nil, err
		}
		if err := os.Rename(path+".part", path); err != nil {
			return nil, err
		}
		recordChecksum(dir, "pg_catalog.csv.gz", fileHash(path, "sha256"))
	}
	catalog := map[int64]gutenbergMeta{}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	rows, err := csv.NewReader(gz).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return catalog, nil
	}
	head := map[string]int{}
	for i, name := range rows[0] {
		head[name] = i
	}
	for _, row := range rows[1:] {
		if len(row) <= head["Text#"] {
			continue
		}
		id, err := strconv.ParseInt(row[head["Text#"]], 10, 64)
		if err != nil || (head["Type"] < len(row) && row[head["Type"]] != "Text") {
			continue
		}
		if head["Language"] < len(row) && !strings.Contains(row[head["Language"]], "en") {
			continue
		}
		catalog[id] = gutenbergMeta{title: strings.TrimSpace(row[head["Title"]]), author: strings.TrimSpace(row[head["Authors"]])}
	}
	return catalog, nil
}

func pinnedGutenbergIDs() ([]int64, error) {
	path := env("LARK_GUTENBERG_IDS", filepath.Join("scripts", "gutenberg-ids.txt"))
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		if line == "" {
			continue
		}
		id, err := strconv.ParseInt(line, 10, 64)
		if err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

var gutenbergKeywords = []string{"children", "juvenile", "fairy", "fable", "myth", "poetry", "poem",
	"nursery", "tale", "legend", "folklore", "classic", "adventure", "literature", "fiction", "story"}

func selectGutenbergIDs(catalog map[int64]gutenbergMeta, limit int) []int64 {
	type scored struct {
		id    int64
		score int
	}
	list := make([]scored, 0, len(catalog))
	for id, meta := range catalog {
		text := strings.ToLower(meta.title + " " + meta.author)
		score := 0
		for _, word := range gutenbergKeywords {
			if strings.Contains(text, word) {
				score++
			}
		}
		list = append(list, scored{id, score})
	}
	slices.SortFunc(list, func(a, b scored) int {
		if a.score != b.score {
			return b.score - a.score
		}
		return int(a.id - b.id)
	})
	out := make([]int64, 0, min(limit, len(list)))
	for _, item := range list {
		if item.score == 0 {
			break
		}
		out = append(out, item.id)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// buildWikipedia streams the dump through bzip2 and a small wiki-markup
// cleaner, writing both the content table and its FTS5 shadow table.
func buildWikipedia(ctx context.Context, dir, wiki string, force bool, out io.Writer) error {
	dump := filepath.Join(dir, "raw", wikiDumpName(wiki))
	if _, err := os.Stat(dump); err != nil {
		return fmt.Errorf("%s: dump missing (%w)", wiki, err)
	}
	dbPath := filepath.Join(dir, librarySources[wiki].db)
	if _, err := os.Stat(dbPath); err == nil && !force {
		libraryLog(out, "%s: index exists", wiki)
		return nil
	}
	lock, err := os.OpenFile(dbPath+".build.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		libraryLog(out, "%s: another index build is already running", wiki)
		return nil
	}
	tmp := dbPath + ".building"
	_ = os.Remove(tmp)
	db, err := sql.Open("lark-sqlite3", tmp+"?_journal_mode=OFF&_synchronous=OFF&_busy_timeout=5000&_cache_size=-1000000")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	defer func() {
		db.Close()
		if ctx.Err() == nil {
			_ = os.Remove(tmp)
		}
	}()
	for _, stmt := range []string{
		`PRAGMA temp_store=MEMORY`,
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE pages (id INTEGER PRIMARY KEY, title TEXT NOT NULL, text TEXT NOT NULL)`,
		`CREATE INDEX pages_title_nocase ON pages(title COLLATE NOCASE)`,
		`CREATE VIRTUAL TABLE pages_fts USING fts5(title, text, content='pages', content_rowid='id', tokenize='porter unicode61')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	start := time.Now()
	count, err := ingestWikiDump(ctx, dump, db, out)
	if err != nil {
		return err
	}
	libraryLog(out, "%s: optimizing FTS index", wiki)
	if _, err := db.ExecContext(ctx, `INSERT INTO pages_fts(pages_fts) VALUES('optimize')`); err != nil {
		return err
	}
	built := time.Now().UTC().Format(time.RFC3339)
	for key, value := range map[string]string{"source": wiki, "pages": strconv.Itoa(count), "built_at": built,
		"dump": filepath.Base(dump), "dump_sha1": fileHash(dump, "sha1"), "build_seconds": strconv.Itoa(int(time.Since(start).Seconds()))} {
		if _, err := db.Exec(`INSERT INTO meta(key,value) VALUES(?,?)`, key, value); err != nil {
			return err
		}
	}
	if err := db.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dbPath); err != nil {
		return err
	}
	libraryLog(out, "%s: %d pages indexed in %s (%s)", wiki, count, time.Since(start).Round(time.Second), humanBytes(fileSize(dbPath)))
	return nil
}

func skipWikiTitle(title string) bool {
	lower := strings.ToLower(strings.TrimSpace(title))
	for _, prefix := range []string{"category:", "template:", "wikipedia:", "file:", "portal:",
		"help:", "talk:", "user:", "mediawiki:", "draft:", "module:", "book:", "timedtext:"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

type wikiDumpPage struct {
	Title    string    `xml:"title"`
	Redirect *struct{} `xml:"redirect"`
	Revision struct {
		Text string `xml:"text"`
	} `xml:"revision"`
}

func ingestWikiDump(ctx context.Context, dump string, db *sql.DB, out io.Writer) (int, error) {
	var reader io.Reader
	var cmd *exec.Cmd
	if _, err := exec.LookPath("bzip2"); err == nil {
		cmd = exec.CommandContext(ctx, "bzip2", "-dc", dump)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return 0, err
		}
		if err := cmd.Start(); err != nil {
			return 0, err
		}
		defer func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}()
		reader = stdout
	} else {
		file, err := os.Open(dump)
		if err != nil {
			return 0, err
		}
		defer file.Close()
		reader = bzip2.NewReader(file)
	}
	dec := xml.NewDecoder(bufio.NewReaderSize(reader, 1<<20))
	dec.Strict = false

	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	insertPage, err := tx.Prepare(`INSERT INTO pages(id,title,text) VALUES(?,?,?)`)
	if err != nil {
		return 0, err
	}
	insertFTS, err := tx.Prepare(`INSERT INTO pages_fts(rowid,title,text) VALUES(?,?,?)`)
	if err != nil {
		return 0, err
	}
	count, id := 0, int64(0)
	last := time.Now()
	batch := 0
	for {
		if ctx.Err() != nil {
			return count, ctx.Err()
		}
		token, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return count, fmt.Errorf("xml: %w", err)
		}
		se, ok := token.(xml.StartElement)
		if !ok || se.Name.Local != "page" {
			continue
		}
		var page wikiDumpPage
		if err := dec.DecodeElement(&page, &se); err != nil {
			return count, err
		}
		if page.Redirect != nil || skipWikiTitle(page.Title) {
			continue
		}
		text := wikiPlainText(page.Revision.Text)
		if len(text) < 24 {
			continue
		}
		if len(text) > libraryMaxText {
			text = strings.ToValidUTF8(text[:libraryMaxText], "")
		}
		id++
		if _, err := insertPage.Exec(id, page.Title, text); err != nil {
			return count, err
		}
		if _, err := insertFTS.Exec(id, page.Title, text); err != nil {
			return count, err
		}
		count++
		batch++
		if batch >= 2000 {
			if err := tx.Commit(); err != nil {
				return count, err
			}
			if tx, err = db.Begin(); err != nil {
				return count, err
			}
			if insertPage, err = tx.Prepare(`INSERT INTO pages(id,title,text) VALUES(?,?,?)`); err != nil {
				return count, err
			}
			if insertFTS, err = tx.Prepare(`INSERT INTO pages_fts(rowid,title,text) VALUES(?,?,?)`); err != nil {
				return count, err
			}
			batch = 0
		}
		if time.Since(last) >= 15*time.Second {
			last = time.Now()
			libraryLog(out, "indexed %d pages", count)
		}
	}
	if err := tx.Commit(); err != nil {
		return count, err
	}
	return count, nil
}

var wikiComment = regexp.MustCompile(`(?s)<!--.*?-->`)
var wikiRef = regexp.MustCompile(`(?is)<ref[^>]*?/>|<ref[^>]*?>.*?</ref>`)
var wikiTag = regexp.MustCompile(`(?s)<[^>]*>`)
var wikiExternal = regexp.MustCompile(`\[(?:https?|ftp)://[^\s\]]+(?:\s+([^\]]*))?\]`)
var wikiHeading = regexp.MustCompile(`(?m)^={2,6}\s*(.*?)\s*={2,6}\s*$`)
var wikiMagic = regexp.MustCompile(`__[A-Z]+__`)

func wikiTailHeading(line string) bool {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "references", "external links", "see also", "notes", "further reading",
		"bibliography", "sources", "citations", "footnotes", "gallery", "media":
		return true
	}
	return false
}

// wikiPlainText is deliberately simple: drop templates, tables, files, and
// references, keep link labels and prose. Good enough for retrieval and read.
func wikiPlainText(source string) string {
	s := strings.ReplaceAll(source, "\r\n", "\n")
	s = wikiComment.ReplaceAllString(s, "")
	s = wikiRef.ReplaceAllString(s, "")
	lines := strings.Split(s, "\n")
	var out strings.Builder
	templateDepth, tableDepth := 0, 0
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "{|") {
			tableDepth++
			continue
		}
		if tableDepth > 0 {
			if strings.HasPrefix(strings.TrimSpace(line), "|}") {
				tableDepth--
			}
			continue
		}
		var kept strings.Builder
		for i := 0; i < len(line); i++ {
			if i+1 < len(line) && line[i] == '{' && line[i+1] == '{' {
				templateDepth++
				i++
				continue
			}
			if i+1 < len(line) && line[i] == '}' && line[i+1] == '}' && templateDepth > 0 {
				templateDepth--
				i++
				continue
			}
			if templateDepth > 0 {
				continue
			}
			kept.WriteByte(line[i])
		}
		line = kept.String()
		if templateDepth > 0 {
			continue
		}
		line = wikiHeading.ReplaceAllString(line, "$1")
		line = strings.TrimLeft(line, "*#:; ")
		if wikiTailHeading(line) {
			break
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	s = out.String()
	for {
		start := strings.Index(s, "[[")
		if start < 0 {
			break
		}
		end := strings.Index(s[start+2:], "]]")
		if end < 0 {
			break
		}
		body := s[start+2 : start+2+end]
		keep := body
		if pipe := strings.LastIndexByte(body, '|'); pipe >= 0 {
			keep = body[pipe+1:]
		}
		lower := strings.ToLower(strings.TrimSpace(body))
		if strings.HasPrefix(lower, "file:") || strings.HasPrefix(lower, "image:") ||
			strings.HasPrefix(lower, "category:") || strings.HasPrefix(lower, "template:") {
			keep = ""
		}
		s = s[:start] + keep + s[start+2+end+2:]
	}
	s = strings.ReplaceAll(s, "[[", "")
	s = strings.ReplaceAll(s, "]]", "")
	s = wikiExternal.ReplaceAllString(s, "$1")
	s = wikiTag.ReplaceAllString(s, "")
	s = wikiMagic.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "'''", "")
	s = strings.ReplaceAll(s, "''", "")
	s = html.UnescapeString(s)
	s = strings.Join(strings.Fields(s), " ")
	// Reflow into paragraphs at sentence boundaries so read/import can chunk.
	var b strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		b.WriteRune(r)
		if (r == '.' || r == '!' || r == '?') && i+2 < len(runes) && runes[i+1] == ' ' && runes[i+2] >= 'A' && runes[i+2] <= 'Z' && b.Len() > 240 {
			b.WriteString("\n\n")
		}
	}
	return strings.TrimSpace(b.String())
}

func buildGutenberg(ctx context.Context, dir string, force bool, out io.Writer) error {
	dbPath := filepath.Join(dir, librarySources["gutenberg"].db)
	if _, err := os.Stat(dbPath); err == nil && !force {
		libraryLog(out, "gutenberg: index exists")
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, "raw", "gutenberg.jsonl"))
	if err != nil {
		return err
	}
	tmp := dbPath + ".building"
	_ = os.Remove(tmp)
	db, err := sql.Open("lark-sqlite3", tmp+"?_journal_mode=OFF&_synchronous=OFF&_busy_timeout=5000&_cache_size=-64000")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	defer func() {
		db.Close()
		_ = os.Remove(tmp)
	}()
	for _, stmt := range []string{
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE books (id INTEGER PRIMARY KEY, title TEXT NOT NULL, author TEXT NOT NULL, path TEXT NOT NULL, sha256 TEXT NOT NULL)`,
		`CREATE VIRTUAL TABLE books_fts USING fts5(title, author, content='books', content_rowid='id', tokenize='porter unicode61')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	insert, err := tx.Prepare(`INSERT OR REPLACE INTO books(id,title,author,path,sha256) VALUES(?,?,?,?,?)`)
	if err != nil {
		return err
	}
	fts, err := tx.Prepare(`INSERT OR REPLACE INTO books_fts(rowid,title,author) VALUES(?,?,?)`)
	if err != nil {
		return err
	}
	count := 0
	for _, line := range strings.Split(string(raw), "\n") {
		var rec gutenbergRecord
		if json.Unmarshal([]byte(line), &rec) != nil || rec.ID == 0 || fileSize(rec.Path) == 0 {
			continue
		}
		if _, err := insert.Exec(rec.ID, rec.Title, rec.Author, rec.Path, rec.SHA256); err != nil {
			return err
		}
		if _, err := fts.Exec(rec.ID, rec.Title, rec.Author); err != nil {
			return err
		}
		count++
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if _, err := db.Exec(`INSERT INTO books_fts(books_fts) VALUES('optimize')`); err != nil {
		return err
	}
	for key, value := range map[string]string{"source": "gutenberg", "books": strconv.Itoa(count),
		"built_at": time.Now().UTC().Format(time.RFC3339)} {
		if _, err := db.Exec(`INSERT INTO meta(key,value) VALUES(?,?)`, key, value); err != nil {
			return err
		}
	}
	if err := db.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dbPath); err != nil {
		return err
	}
	libraryLog(out, "gutenberg: %d books indexed (%s)", count, humanBytes(fileSize(dbPath)))
	return nil
}
