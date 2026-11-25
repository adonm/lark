// Package library owns the local content library: one SQLite FTS5 database per
// source, plain text books on disk, the disk-aware planner, and the download
// and index builders. It has no dependency on the server, hub, or HTTP layer.
package library

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const (
	maxText       = 24 << 10 // Cap stored article text.
	readChunk     = 3200     // Characters returned per read section.
	ReadMax       = 4000     // Hard ceiling for one read.
	snippetWidth  = 240
	importTextMax = 36 << 10
	passageRunes  = 575
	maxPassages   = 60
	gutenbergBase = "https://gutenberg.pglaf.org"
	wikiDumpsBase = "https://dumps.wikimedia.org"
	userAgent     = "LarkLibrary/1.0 (local companion reader)"
)

var (
	// ErrNotFound reports that no installed source has the requested text.
	ErrNotFound = errors.New("library text not found")
)

type source struct {
	name   string
	db     string
	label  string
	weight float64
}

var sources = map[string]source{
	// Higher weights give shorter, kid-friendly text a ranking boost when one
	// query spans several sources. bm25 is negative, so a larger multiplier
	// sorts earlier.
	"simple":    {name: "simple", db: "simple.db", label: "Simple English Wikipedia", weight: 2},
	"enwiki":    {name: "enwiki", db: "enwiki.db", label: "English Wikipedia", weight: 1},
	"gutenberg": {name: "gutenberg", db: "gutenberg.db", label: "Project Gutenberg", weight: 1.2},
}

var sourceOrder = []string{"simple", "enwiki", "gutenberg"}

// Library is one library directory and its lazily opened read-only databases.
type Library struct {
	dir string
	mu  sync.Mutex
	dbs map[string]*sql.DB
}

// New returns a library rooted at dir.
func New(dir string) *Library { return &Library{dir: dir} }

// Dir is the directory holding the indexes, downloads, and books.
func (l *Library) Dir() string {
	if l == nil || l.dir == "" {
		return DefaultDir()
	}
	return l.dir
}

// DefaultDir is $XDG_DATA_HOME/lark/library.
func DefaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data := env("XDG_DATA_HOME", filepath.Join(home, ".local/share"))
	return filepath.Join(data, "lark", "library")
}

// SourceNames lists the known sources in search order.
func SourceNames() []string { return append([]string(nil), sourceOrder...) }

// IsSource reports whether name is a known source.
func IsSource(name string) bool {
	_, ok := sources[name]
	return ok
}

func repoRoot() (string, error) {
	project, err := filepath.Abs(env("LARK_REPO_DIR", "."))
	if err != nil {
		return "", err
	}
	if os.Getenv("LARK_REPO_DIR") == "" && filepath.Base(project) == "lark" {
		if _, err := os.Stat(filepath.Join(project, "firmware")); err != nil {
			project = filepath.Dir(project)
		}
	}
	return project, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func (l *Library) path(name string) string {
	return filepath.Join(l.Dir(), sources[name].db)
}

func (l *Library) installed(name string) bool {
	_, err := os.Stat(l.path(name))
	return err == nil
}

func dbsn(reader bool) string {
	if reader {
		// bm25 scans the whole posting list for every hit; a larger page cache
		// keeps the enwiki index hot across agent calls.
		return "?_query_only=1&_busy_timeout=5000&_cache_size=-262144"
	}
	return "?_busy_timeout=5000&_cache_size=-64000"
}

func (l *Library) open(path string) (*sql.DB, error) {
	if l == nil {
		return openDB(path, true)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if db := l.dbs[path]; db != nil {
		return db, nil
	}
	db, err := openDB(path, true)
	if err != nil {
		return nil, err
	}
	if l.dbs == nil {
		l.dbs = map[string]*sql.DB{}
	}
	l.dbs[path] = db
	return db, nil
}

func openDB(path string, reader bool) (*sql.DB, error) {
	if reader {
		if _, err := os.Stat(path); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("lark-sqlite3", path+dbsn(reader))
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

// Hit is one ranked search result.
type Hit struct {
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
