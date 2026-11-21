package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testDB(t *testing.T, path string) *bridgeDB {
	t.Helper()
	d, err := openBridgeDB(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestDailyBackupIncludesWALAndPrunes(t *testing.T) {
	root := t.TempDir()
	d := testDB(t, filepath.Join(root, "lark.db"))
	dir := filepath.Join(root, "it's backed up")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 21, 15, 0, 0, 0, time.UTC)
	for i := range 9 {
		name := "lark-" + now.AddDate(0, 0, -1-i).Format(time.DateOnly) + ".db"
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	unrelated := filepath.Join(dir, "lark-keep.db")
	if err := os.WriteFile(unrelated, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO apps (name,title,kind,svg,updated_at) VALUES ('sun','Sun','svg','<svg/>',100)`); err != nil {
		t.Fatal(err)
	}
	if err := d.backup(t.Context(), dir, now); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "lark-2026-11-21.db")
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || d.lastBackup.Load() != info.ModTime().Unix() {
		t.Fatalf("backup mode or timestamp: %v %d", info.Mode(), d.lastBackup.Load())
	}
	if _, err := d.Exec(`UPDATE apps SET title='Moon'`); err != nil {
		t.Fatal(err)
	}
	if err := d.backup(t.Context(), dir, now); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 8 {
		t.Fatalf("expected 7 daily files and unrelated file: %v %v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "lark-2026-11-14.db")); !os.IsNotExist(err) {
		t.Fatalf("expected eighth daily backup pruned, stat: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatal(err)
	}
	backup, err := sql.Open("lark-sqlite3", target)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var title string
	if err := backup.QueryRow(`SELECT title FROM apps WHERE name='sun'`).Scan(&title); err != nil || title != "Sun" {
		t.Fatalf("backup did not preserve WAL data: %q %v", title, err)
	}
}

func writeMap(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func TestSQLiteWALMmapAndConcurrentPool(t *testing.T) {
	d := testDB(t, filepath.Join(t.TempDir(), "lark.db"))

	var journal, tempStore string
	var syncMode, cacheSize, fk int
	var mmapSize int64
	if err := d.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("journal_mode = %q, err = %v", journal, err)
	}
	if err := d.QueryRow(`PRAGMA synchronous`).Scan(&syncMode); err != nil || syncMode != 1 {
		t.Fatalf("synchronous = %d, err = %v", syncMode, err)
	}
	if err := d.QueryRow(`PRAGMA cache_size`).Scan(&cacheSize); err != nil || cacheSize != -32000 {
		t.Fatalf("cache_size = %d, err = %v", cacheSize, err)
	}
	if err := d.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys = %d, err = %v", fk, err)
	}
	if err := d.QueryRow(`PRAGMA mmap_size`).Scan(&mmapSize); err != nil || mmapSize <= 0 {
		t.Fatalf("mmap_size = %d, err = %v", mmapSize, err)
	}
	if err := d.QueryRow(`PRAGMA temp_store`).Scan(&tempStore); err != nil || tempStore != "2" {
		t.Fatalf("temp_store = %q, err = %v", tempStore, err)
	}

	var queryOnly int
	var readerMmap int64
	if err := d.readDB().QueryRow(`PRAGMA query_only`).Scan(&queryOnly); err != nil || queryOnly != 1 {
		t.Fatalf("reader query_only = %d, err = %v", queryOnly, err)
	}
	if err := d.readDB().QueryRow(`PRAGMA mmap_size`).Scan(&readerMmap); err != nil || readerMmap <= 0 {
		t.Fatalf("reader mmap_size = %d, err = %v", readerMmap, err)
	}
	if _, err := d.readDB().Exec(`INSERT INTO meta VALUES ('ro','fail')`); err == nil {
		t.Fatal("expected read-only pool to reject writes")
	}

	if err := d.setMeta(t.Context(), "counter", "0"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range 25 {
				if id == 0 {
					if err := d.setMeta(t.Context(), "counter", fmt.Sprintf("%d", i+1)); err != nil {
						t.Errorf("writer failed: %v", err)
						return
					}
				} else {
					if _, err := d.getMeta(t.Context(), "counter"); err != nil {
						t.Errorf("reader failed: %v", err)
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()
}
