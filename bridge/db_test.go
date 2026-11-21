package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "turso.tech/database/tursogo"
)

func testDB(t *testing.T, path string) *bridgeDB {
	t.Helper()
	d, err := openBridgeDB(t.Context(), path, "", "")
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
	if _, err := d.Exec(`INSERT INTO photos VALUES ('photo-12345678','cake','image/png',x'fedc','image',100)`); err != nil {
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
	if _, err := d.Exec(`UPDATE photos SET caption='new'`); err != nil {
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
	backup, err := sql.Open("turso", target)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var caption string
	var data []byte
	if err := backup.QueryRow(`SELECT data,caption FROM photos`).Scan(&data, &caption); err != nil || !bytes.Equal(data, []byte{0xfe, 0xdc}) || caption != "image" {
		t.Fatalf("backup did not preserve WAL data: %x %q %v", data, caption, err)
	}
}

func writeMap(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func TestTursoDriver(t *testing.T) {
	db, err := sql.Open("turso", filepath.Join(t.TempDir(), "lark.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE probe (id INTEGER PRIMARY KEY, value TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO probe VALUES (1, 'ok')"); err != nil {
		t.Fatal(err)
	}
	var value string
	if err := db.QueryRow("SELECT value FROM probe").Scan(&value); err != nil || value != "ok" {
		t.Fatalf("value = %q, err = %v", value, err)
	}
}

func TestCloudOptionWorksOffline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lark.db")
	local := testDB(t, path)
	if err := voiceStore(local).save("offline", heartVoice, false); err != nil {
		t.Fatal(err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer unavailable.Close()
	db, err := openBridgeDB(t.Context(), path, unavailable.URL, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.cloud == nil {
		t.Fatal("cloud sync not configured")
	}
	if err := db.syncCloud(t.Context()); err == nil {
		t.Fatal("false sync success against unavailable server")
	}
	if got, _, err := voiceStore(db).get("offline"); err != nil || got != heartVoice {
		t.Fatal("cloud configuration lost local data", err)
	}
	if err := voiceStore(db).save("offline", fableVoice, false); err != nil {
		t.Fatal("cloud error blocked local write", err)
	}
}
