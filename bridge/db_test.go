package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "turso.tech/database/tursogo"
)

func testDB(t *testing.T, path, legacy string) *bridgeDB {
	t.Helper()
	d, err := openBridgeDB(t.Context(), path, legacy, "", "", heartVoice)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestDailyBackupIncludesWALAndPrunes(t *testing.T) {
	root := t.TempDir()
	d := testDB(t, filepath.Join(root, "echoear.db"), "")
	dir := filepath.Join(root, "it's backed up")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 21, 15, 0, 0, 0, time.UTC)
	for i := range 9 {
		name := "echoear-" + now.AddDate(0, 0, -1-i).Format(time.DateOnly) + ".db"
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	unrelated := filepath.Join(dir, "echoear-keep.db")
	if err := os.WriteFile(unrelated, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO photos VALUES ('photo-12345678','cake','image/png',x'fedc','image',100)`); err != nil {
		t.Fatal(err)
	}
	if err := d.backup(t.Context(), dir, now); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "echoear-2026-11-21.db")
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
	if _, err := os.Stat(filepath.Join(dir, "echoear-2026-11-14.db")); !os.IsNotExist(err) {
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
	db, err := sql.Open("turso", filepath.Join(t.TempDir(), "echoear.db"))
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

func TestTursoMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "echoear.db")
	at := time.Date(2026, 10, 1, 12, 5, 0, 123000000, time.UTC)
	cake := peer{"echoear-0001", "Cake", "127.0.0.21", 80, at.Unix()}
	mint := peer{"echoear-0002", "Mint", "127.0.0.22", 80, at.Unix()}
	nonLANCake := peer{"echoear-0003", "Cake", "192.0.2.21", 80, at.Unix()}
	nonLANMint := peer{"echoear-0004", "Mint", "192.0.2.22", 80, at.Unix()}
	greg := virtualClient{ID: "virt-12345678", Character: "Greg", Theme: "plum", CreatedAt: at}
	tunnel := tunnelRecord{greg.ID, "greg", "https://greg.example.test", 1234, strings.Repeat("a", 32), "echoear-" + strings.Repeat("b", 24), ""}
	chat := chatEntry{ID: "legacy-1", Role: "system", Sender: "Greg", Text: "First", Timestamp: at, Source: "mcp", SceneTitle: "Quiz", ToolSummary: "Card"}
	sessions := map[string]string{cake.DeviceID: "ses_cake", mint.DeviceID: "ses_mint", greg.ID: "ses_greg"}
	voices := map[string]string{cake.DeviceID: heartVoice, mint.DeviceID: fableVoice}
	all := map[string]any{
		"devices": object{
			cake.DeviceID: cake, mint.DeviceID: mint,
			nonLANCake.DeviceID: nonLANCake, nonLANMint.DeviceID: nonLANMint,
			"bad": []int{1},
		},
		"virtual_clients": map[string]virtualClient{greg.ID: greg},
		"tunnels":         map[string]tunnelRecord{greg.ID: tunnel},
		"sessions":        sessions,
		"voices":          voices,
		"chat_events":     object{greg.ID: []any{chat, "bad", chatEntry{ID: "legacy-2", Timestamp: at}}},
	}
	backups := make(map[string][]byte)
	for name, rows := range all {
		backup := filepath.Join(dir, name+".json")
		if err := writeMap(backup, rows); err != nil {
			t.Fatal(err)
		}
		backups[backup], _ = os.ReadFile(backup)
	}
	db := testDB(t, path, dir)
	assertRows := func(name string, n int) {
		t.Helper()
		var count int
		if err := db.QueryRow("SELECT count(*) FROM " + name).Scan(&count); err != nil || count != n {
			t.Fatalf("%s rows = %d, want %d: %v", name, count, n, err)
		}
	}
	for name, n := range map[string]int{"cats": 3, "saved_cats": 3, "devices": 2, "virtual_clients": 1, "tunnels": 1, "sessions": 3,
		"voices": 2, "chat_events": 2, "messages": 1, "memory": 0, "apps": 0, "timers": 0} {
		assertRows(name, n)
	}
	for _, want := range []peer{cake, mint} {
		if got, _, err := deviceStore(db).get(want.DeviceID); err != nil || got != want {
			t.Fatalf("device %s did not migrate: %v", want.DeviceID, err)
		}
	}
	if got, _, err := virtualStore(db).get(greg.ID); err != nil || got != greg {
		t.Fatal("virtual client fields did not migrate", err)
	}
	if got, _, err := tunnelStore(db).get(greg.ID); err != nil || got.CreatedAt == "" || got.Token != tunnel.Token || got.URL != tunnel.URL {
		t.Fatal("tunnel fields did not migrate", err)
	}
	if got, err := sessionStore(db).all(); err != nil || !reflect.DeepEqual(got, sessions) {
		t.Fatal("sessions did not migrate", err)
	}
	if got, err := voiceStore(db).all(); err != nil || !reflect.DeepEqual(got, voices) {
		t.Fatal("voices did not migrate", err)
	}
	if got, err := (&chatHistory{db}).list(t.Context(), greg.ID); err != nil || len(got) != 1 ||
		got[0].ID != chat.ID || got[0].Role != "card" || got[0].SceneTitle != "Quiz" || got[0].Text != "Drew: Quiz" {
		t.Fatal("chat events did not migrate", got, err)
	}
	for backup, original := range backups {
		after, err := os.ReadFile(backup)
		if err != nil || !bytes.Equal(after, original) {
			t.Fatalf("backup %s changed: %v", filepath.Base(backup), err)
		}
	}
	if err := sessionStore(db).save(cake.DeviceID, "", true); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = testDB(t, path, dir)
	assertRows("sessions", 2)
	assertRows("chat_events", 2)
	var version int
	if err := db.QueryRow("SELECT MAX(version) FROM schema_version").Scan(&version); err != nil || version != 9 {
		t.Fatal("current schema version missing", version, err)
	}
}

func TestUpgradeDeployedSchemaFixture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "echoear.db")
	raw, err := sql.Open("turso", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// Unversioned production-era SQL shape, with synthetic rows only.
	for _, q := range []string{
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO meta VALUES ('migrated_json_v1','1'), ('migrated_face_svg_v1','1'),
			('migrated_cats_v1','1'), ('migrated_messages_v2_local','1'), ('migrated_messages_v2','1')`,
		`CREATE TABLE sessions (device_id TEXT PRIMARY KEY, session_id TEXT NOT NULL, updated_at INTEGER NOT NULL)`,
		`INSERT INTO sessions VALUES ('echoear-fixture','ses_fixture',42)`,
		`CREATE TABLE apps (name TEXT PRIMARY KEY, title TEXT NOT NULL, kind TEXT NOT NULL, svg TEXT NOT NULL DEFAULT '',
			code TEXT NOT NULL DEFAULT '', tick_ms INTEGER NOT NULL DEFAULT 0, max_ticks INTEGER NOT NULL DEFAULT 0,
			author TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL, template TEXT NOT NULL DEFAULT '', data_json TEXT NOT NULL DEFAULT '{}')`,
		`INSERT INTO apps (name,title,kind,updated_at,template,data_json) VALUES ('fixture','Saved','template',42,'quiz','{"round":1}')`,
		`CREATE TABLE messages (id TEXT PRIMARY KEY, device_id TEXT NOT NULL, role TEXT NOT NULL,
			sender TEXT NOT NULL, text TEXT NOT NULL, source TEXT NOT NULL, card_title TEXT NOT NULL DEFAULT '', ts_ms INTEGER NOT NULL)`,
		`CREATE INDEX messages_device_ts ON messages (device_id,ts_ms)`,
		`INSERT INTO messages VALUES ('msg_fixture','echoear-fixture','user','You','Keep me','typed','',42)`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	legacy := t.TempDir()
	if err := os.WriteFile(filepath.Join(legacy, "devices.json"), []byte(`invalid but already migrated`), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		db := testDB(t, path, legacy)
		for q, want := range map[string]string{
			`SELECT session_id FROM sessions`:                                    "ses_fixture",
			`SELECT contract_hash FROM sessions`:                                 "",
			`SELECT context_after_ms FROM sessions`:                              "42",
			`SELECT data_json FROM apps`:                                         `{"round":1}`,
			`SELECT text FROM messages`:                                          "Keep me",
			`SELECT count(*) FROM sqlite_schema WHERE name='messages_device_ts'`: "0",
			`SELECT count(*) FROM build_jobs`:                                    "0",
			`SELECT count(*) FROM parent_usage`:                                  "0",
		} {
			var got string
			if err := db.QueryRow(q).Scan(&got); err != nil || got != want {
				t.Fatalf("%s: got %q, want %q: %v", q, got, want, err)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSchemaResumesFailedStep(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "echoear.db")
	const id = "echoear-fixture"
	if err := writeMap(filepath.Join(dir, "devices.json"), map[string]peer{
		id: {DeviceID: id, Character: "Cake", IP: "127.0.0.21", Port: 80, LastSeen: 42},
	}); err != nil {
		t.Fatal(err)
	}
	voices := filepath.Join(dir, "voices.json")
	if err := os.WriteFile(voices, []byte(`not JSON`), 0600); err != nil {
		t.Fatal(err)
	}
	if db, err := openBridgeDB(t.Context(), path, dir, "", "", heartVoice); err == nil {
		db.Close()
		t.Fatal("invalid legacy JSON unexpectedly committed")
	}
	raw, err := sql.Open("turso", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var version, count int
	if err := raw.QueryRow(`SELECT MAX(version), (SELECT count(*) FROM devices) FROM schema_version`).Scan(&version, &count); err != nil ||
		version != baselineVersion || count != 0 {
		t.Fatalf("failed import not rolled back: version %d, devices %d: %v", version, count, err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeMap(voices, map[string]string{id: heartVoice}); err != nil {
		t.Fatal(err)
	}
	db := testDB(t, path, dir)
	if err := db.QueryRow(`SELECT MAX(version), (SELECT count(*) FROM cats) FROM schema_version`).Scan(&version, &count); err != nil ||
		version != 9 || count != 1 {
		t.Fatalf("import did not resume: version %d, cats %d: %v", version, count, err)
	}
}

func TestCloudOptionWorksOffline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "echoear.db")
	local := testDB(t, path, "")
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
	db, err := openBridgeDB(t.Context(), path, "", unavailable.URL, "test-token", heartVoice)
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
