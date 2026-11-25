//go:build sqlite_fts5

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lark/internal/library"
	"lark/internal/stories"
)

func testWikiDB(t testing.TB, path string, pages map[string]string) {
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

type gutenbergFixture struct {
	ID     int64
	Title  string
	Author string
	Path   string
	SHA256 string
}

func testGutenbergDB(t testing.TB, dir string, books []gutenbergFixture) {
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
	testGutenbergDB(t, dir, []gutenbergFixture{{ID: 11, Title: "Alice's Adventures in Wonderland",
		Author: "Lewis Carroll", Path: bookPath}})

	db, err := openLarkDB(context.Background(), filepath.Join(dir, "lark.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	b := &larkServer{lib: library.New(dir), db: db}
	ctx := context.Background()

	out, err := b.mcpLibrary(ctx, object{"op": "search", "query": "why is the sky blue"})
	if err != nil {
		t.Fatal(err)
	}
	hits, _ := out["results"].([]library.Hit)
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
	if chars := int(read["chars"].(int)); chars > library.ReadMax || chars == 0 {
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
	if _, err := stories.Parse(saved.Twee); err != nil {
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
	// ~4 bytes/token. Both strings are part of the model's system prompt, so
	// the budget covers agent prompt + MCP instructions.
	total := len(prompt) + len(mcpInstructions)
	if total > 4250 {
		t.Fatalf("static prompt grew to %d bytes (agent %d + MCP instructions %d); budget 4250",
			total, len(prompt), len(mcpInstructions))
	}
	t.Logf("static prompt %d bytes (agent %d + MCP instructions %d)", total, len(prompt), len(mcpInstructions))
}

func TestTurnStateBudget(t *testing.T) {
	t.Parallel()
	// turnStateLine lists every attached online terminal, pending timer, and
	// story with no server-side entry cap. Pin a realistic envelope so growth
	// is deliberate: 3 terminals, 5 timers, 5 stories, 60-byte labels/titles.
	terminals := strings.Join([]string{
		"vocat-33d8(esp-vocat,online,battery:100,vol:100)",
		"box3-b324(esp32-s3-box3,online,battery:100,vol:100)",
		"vocat-2cac(esp-vocat,online,battery:100,sleeping,vol:100)",
	}, ",")
	timerParts := make([]string, 0, 5)
	storyParts := make([]string, 0, 5)
	for i := range 5 {
		label := strings.Repeat("t", 60)
		timerParts = append(timerParts, fmt.Sprintf("%024x(%q,3600s)", i, label))
		storyParts = append(storyParts, fmt.Sprintf("story-%d(%q)", i, strings.Repeat("s", 60)))
	}
	line := turnStateLine(time.Now(),
		companionState{companionLook: companionLook{Form: strings.Repeat("f", 60)}, Listening: true},
		terminals, strings.Join(timerParts, ","), strings.Join(storyParts, ","))
	const stateBudget = 1280
	if len(line) > stateBudget {
		t.Fatalf("state line grew to %d bytes (budget %d): %s", len(line), stateBudget, line)
	}
	t.Logf("worst-case state line %d bytes", len(line))
}
