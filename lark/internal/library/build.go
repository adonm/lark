package library

import (
	"bufio"
	"compress/bzip2"
	"context"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// buildWikipedia streams the dump through bzip2 and a small wiki-markup
// cleaner, writing both the content table and its FTS5 shadow table.
func buildWikipedia(ctx context.Context, dir, wiki string, force bool, out io.Writer) error {
	dump := filepath.Join(dir, "raw", wikiDumpName(wiki))
	if _, err := os.Stat(dump); err != nil {
		return fmt.Errorf("%s: dump missing (%w)", wiki, err)
	}
	dbPath := filepath.Join(dir, sources[wiki].db)
	if _, err := os.Stat(dbPath); err == nil && !force {
		libraryLog(out, "%s: index exists", wiki)
		return nil
	}
	release, err := libraryBuildLock(dbPath)
	if err != nil {
		return err
	}
	if release == nil {
		libraryLog(out, "%s: another index build is already running", wiki)
		return nil
	}
	defer release()
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
	if reclaimed := reclaimWikiDump(dump); reclaimed > 0 {
		libraryLog(out, "%s: removed the downloaded dump after indexing (%s reclaimed)", wiki, humanBytes(reclaimed))
	}
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
		if len(text) > maxText {
			text = strings.ToValidUTF8(text[:maxText], "")
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

func buildGutenberg(ctx context.Context, dir string, force bool, out io.Writer) error {
	dbPath := filepath.Join(dir, sources["gutenberg"].db)
	if _, err := os.Stat(dbPath); err == nil && !force {
		libraryLog(out, "gutenberg: index exists")
		return nil
	}
	release, err := libraryBuildLock(dbPath)
	if err != nil {
		return err
	}
	if release == nil {
		libraryLog(out, "gutenberg: another index build is already running")
		return nil
	}
	defer release()
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
