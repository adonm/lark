//go:build sqlite_fts5

package library

import (
	"database/sql"
	"slices"
	"testing"

	sqlite3 "github.com/mattn/go-sqlite3"
)

func init() {
	sql.Register("lark-sqlite3", &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			_, err := conn.Exec("PRAGMA temp_store=MEMORY; PRAGMA mmap_size=268435456;", nil)
			return err
		},
	})
}

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
