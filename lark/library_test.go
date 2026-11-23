//go:build sqlite_fts5

package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func testWikiDB(t *testing.T, path string, pages map[string]string) {
	t.Helper()
	db, err := sql.Open("lark-sqlite3", path+"?_journal_mode=OFF&_synchronous=OFF")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE pages (id INTEGER PRIMARY KEY, title TEXT NOT NULL, text TEXT NOT NULL)`,
		`CREATE INDEX pages_title ON pages(title COLLATE NOCASE)`,
		`CREATE VIRTUAL TABLE pages_fts USING fts5(title, text, content='pages', content_rowid='id', tokenize='porter unicode61')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	titles := make([]string, 0, len(pages))
	for title := range pages {
		titles = append(titles, title)
	}
	slices.Sort(titles)
	id := 0
	for _, title := range titles {
		id++
		if _, err := db.Exec(`INSERT INTO pages(id,title,text) VALUES(?,?,?)`, id, title, pages[title]); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO pages_fts(rowid,title,text) VALUES(?,?,?)`, id, title, pages[title]); err != nil {
			t.Fatal(err)
		}
	}
}

func testGutenbergDB(t *testing.T, dir string, books []gutenbergRecord) {
	t.Helper()
	path := filepath.Join(dir, "gutenberg.db")
	db, err := sql.Open("lark-sqlite3", path+"?_journal_mode=OFF&_synchronous=OFF")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE books (id INTEGER PRIMARY KEY, title TEXT NOT NULL, author TEXT NOT NULL, path TEXT NOT NULL, sha256 TEXT NOT NULL)`,
		`CREATE VIRTUAL TABLE books_fts USING fts5(title, author, content='books', content_rowid='id', tokenize='porter unicode61')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	for _, book := range books {
		if _, err := db.Exec(`INSERT INTO books(id,title,author,path,sha256) VALUES(?,?,?,?,?)`,
			book.ID, book.Title, book.Author, book.Path, book.SHA256); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO books_fts(rowid,title,author) VALUES(?,?,?)`,
			book.ID, book.Title, book.Author); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLibrarySearchReadImport(t *testing.T) {
	serial(t)
	dir := t.TempDir()
	long := strings.Repeat("The sky looks blue because air scatters short blue light. ", 400)
	testWikiDB(t, filepath.Join(dir, "simple.db"), map[string]string{
		"Sky":               "The sky is blue. " + long,
		"Sun":               "The Sun is a star.",
		"Three Little Pigs": "Three little pigs built houses of straw, wood, and brick.",
	})
	testWikiDB(t, filepath.Join(dir, "enwiki.db"), map[string]string{
		"Rayleigh scattering": "Rayleigh scattering makes the sky look blue.",
	})
	bookPath := filepath.Join(dir, "books", "0011.txt")
	if err := os.MkdirAll(filepath.Dir(bookPath), 0o700); err != nil {
		t.Fatal(err)
	}
	book := "*** START OF THE PROJECT GUTENBERG EBOOK ALICE ***\nAlice was beginning to get very tired.\n*** END OF THE PROJECT GUTENBERG EBOOK ALICE ***\n"
	if err := os.WriteFile(bookPath, []byte(book), 0o600); err != nil {
		t.Fatal(err)
	}
	testGutenbergDB(t, dir, []gutenbergRecord{{ID: 11, Title: "Alice's Adventures in Wonderland",
		Author: "Lewis Carroll", Path: bookPath}})

	db, err := openLarkDB(context.Background(), filepath.Join(dir, "lark.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	b := &larkServer{library: dir, db: db}
	ctx := context.Background()

	out, err := b.mcpLibrary(ctx, object{"op": "search", "query": "why is the sky blue"})
	if err != nil {
		t.Fatal(err)
	}
	hits, _ := out["results"].([]libraryHit)
	if len(hits) == 0 || hits[0].Source != "simple" || hits[0].Title != "Sky" {
		t.Fatalf("unexpected search results: %#v", out["results"])
	}

	// Warm and measure ranked search latency on this small fixture.
	var elapsed []time.Duration
	for range 20 {
		start := time.Now()
		if _, err := b.mcpLibrary(ctx, object{"op": "search", "query": "sky blue light"}); err != nil {
			t.Fatal(err)
		}
		elapsed = append(elapsed, time.Since(start))
	}
	slices.Sort(elapsed)
	median := elapsed[len(elapsed)/2]
	if median > 50*time.Millisecond {
		t.Fatalf("median search %v exceeds 50ms", median)
	}
	t.Logf("median search %v", median)

	read, err := b.mcpLibrary(ctx, object{"op": "read", "source": "simple", "title": "Sky"})
	if err != nil {
		t.Fatal(err)
	}
	if chars := int(read["chars"].(int)); chars > libraryReadMax || chars == 0 {
		t.Fatalf("read returned %d chars", chars)
	}
	next, err := b.mcpLibrary(ctx, object{"op": "read", "source": "simple", "title": "Sky", "section": json.Number("1")})
	if err != nil {
		t.Fatal(err)
	}
	if section, _ := next["section"].(int); section != 1 {
		t.Fatalf("expected section 1, got %#v", next)
	}
	if next["text"] == read["text"] {
		t.Fatal("second section repeated the first")
	}

	imported, err := b.mcpLibrary(ctx, object{"op": "import", "source": "simple", "title": "Sky"})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := b.loadStory(ctx, str(imported["id"]))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(saved.Author, "Wikipedia") {
		t.Fatalf("missing attribution: %q", saved.Author)
	}
	if _, err := parseTwee(saved.Twee); err != nil {
		t.Fatalf("imported Twee is invalid: %v", err)
	}

	imported, err = b.mcpLibrary(ctx, object{"op": "import", "source": "gutenberg", "title": "11"})
	if err != nil {
		t.Fatal(err)
	}
	saved, err = b.loadStory(ctx, str(imported["id"]))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(saved.Author, "Project Gutenberg") {
		t.Fatalf("missing Gutenberg attribution: %q", saved.Author)
	}

	list, err := b.mcpLibrary(ctx, object{"op": "list", "source": "gutenberg"})
	if err != nil {
		t.Fatal(err)
	}
	nested, _ := list["items"].(object)
	if items, _ := nested["items"].([]object); len(items) != 1 {
		t.Fatalf("expected one Gutenberg title, got %#v", list)
	}
}

func TestLibraryTweePassageBounds(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("A sentence with several words to fill the passage. ", 2000)
	twee, err := libraryTwee("A Long Book", text)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := parseTwee(twee)
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled.Passages) > libraryPassages {
		t.Fatalf("too many passages: %d", len(compiled.Passages))
	}
	for _, passage := range compiled.Passages {
		if !passage.End {
			if len(passage.Ops) == 0 {
				t.Fatalf("empty passage %q", passage.Name)
			}
		}
	}
	if strings.Contains(twee, "[[[") || strings.Contains(twee, "]]]") {
		t.Fatalf("unsafe bracket sequence survived:\n%s", twee)
	}
}

func TestDownloadResumeAndChecksum(t *testing.T) {
	t.Parallel()
	payload := bytes.Repeat([]byte("offline library payload\n"), 4096)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "file", time.Time{}, bytes.NewReader(payload))
	}))
	defer server.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "file.bin")
	part := dest + ".part"
	if err := os.WriteFile(part, payload[:1000], 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum(payload)
	expect := hex.EncodeToString(sum[:])
	if err := downloadFile(t.Context(), server.Client(), server.URL, dest, "sha1", expect, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("resumed file mismatch: %d bytes", len(got))
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatalf("partial file survived: %v", err)
	}

	if err := downloadFile(t.Context(), server.Client(), server.URL, filepath.Join(dir, "bad.bin"), "sha1",
		strings.Repeat("0", 40), ioDiscard{}); err == nil {
		t.Fatal("expected checksum mismatch")
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

func TestLibraryPromptBudget(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "agents", "lark.md"))
	if err != nil {
		t.Fatal(err)
	}
	prompt := string(raw)
	if !strings.Contains(prompt, "library") {
		t.Fatal("agent prompt does not mention the library")
	}
	// ~4 bytes/token; keep the static prompt under ~1000 tokens (~4000 bytes).
	if len(prompt) > 4000 {
		t.Fatalf("agent prompt grew to %d bytes (budget 4000)", len(prompt))
	}
}
