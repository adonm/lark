package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	turso "turso.tech/database/tursogo"
)

const createHistoryBackfill = `CREATE TABLE IF NOT EXISTS history_backfill (
	device_id TEXT NOT NULL, session_id TEXT NOT NULL, next TEXT NOT NULL, source TEXT NOT NULL,
	PRIMARY KEY (device_id, session_id))`

var schemaBaseline = []string{
	`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS devices (device_id TEXT PRIMARY KEY, character TEXT NOT NULL, ip TEXT NOT NULL, port INTEGER NOT NULL, last_seen INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS sessions (device_id TEXT PRIMARY KEY, session_id TEXT NOT NULL, updated_at INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS virtual_clients (id TEXT PRIMARY KEY, character TEXT NOT NULL, theme TEXT NOT NULL, voice TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS voices (device_id TEXT PRIMARY KEY, voice TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS device_prefs (device_id TEXT PRIMARY KEY, auto_volume INTEGER NOT NULL DEFAULT 1, manual_volume INTEGER NOT NULL DEFAULT 55)`,
	`CREATE TABLE IF NOT EXISTS cats (device_id TEXT PRIMARY KEY, name TEXT NOT NULL, configured INTEGER NOT NULL DEFAULT 0, face_bg TEXT NOT NULL, mark_color TEXT NOT NULL, accent_color TEXT NOT NULL, personality TEXT NOT NULL DEFAULT '', extras_json TEXT NOT NULL DEFAULT '[]', updated_at INTEGER NOT NULL, face_svg TEXT NOT NULL DEFAULT '', svg_extras TEXT NOT NULL DEFAULT '')`,
	`CREATE TABLE IF NOT EXISTS saved_cats (slug TEXT PRIMARY KEY, name TEXT NOT NULL, face_bg TEXT NOT NULL, mark_color TEXT NOT NULL, accent_color TEXT NOT NULL, personality TEXT NOT NULL DEFAULT '', voice TEXT NOT NULL DEFAULT '', extras_json TEXT NOT NULL DEFAULT '[]', updated_at INTEGER NOT NULL, face_svg TEXT NOT NULL DEFAULT '', svg_extras TEXT NOT NULL DEFAULT '')`,
	`CREATE TABLE IF NOT EXISTS tunnels (device_id TEXT PRIMARY KEY, route_name TEXT NOT NULL, url TEXT NOT NULL, local_port INTEGER NOT NULL, scoped_token TEXT NOT NULL, profile TEXT NOT NULL, created_at TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS chat_events (id TEXT PRIMARY KEY, device_id TEXT NOT NULL, role TEXT NOT NULL, sender TEXT NOT NULL, text TEXT NOT NULL, source TEXT NOT NULL, scene_title TEXT NOT NULL DEFAULT '', tool_summary TEXT NOT NULL DEFAULT '', ts_ms INTEGER NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS chat_device_ts ON chat_events (device_id, ts_ms)`,
	`CREATE TABLE IF NOT EXISTS messages (id TEXT PRIMARY KEY, device_id TEXT NOT NULL, role TEXT NOT NULL, sender TEXT NOT NULL, text TEXT NOT NULL, source TEXT NOT NULL, card_title TEXT NOT NULL DEFAULT '', ts_ms INTEGER NOT NULL)`,
	// Extend the old device/timestamp index with the cursor's tie-breaker so
	// LIMIT can stop the ordered scan instead of sorting the whole transcript.
	`CREATE INDEX IF NOT EXISTS messages_device_ts_id ON messages (device_id, ts_ms, id)`,
	`CREATE TABLE IF NOT EXISTS photos (id TEXT PRIMARY KEY, device_id TEXT NOT NULL, mime TEXT NOT NULL, data BLOB NOT NULL, caption TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS memory (scope TEXT NOT NULL, key TEXT NOT NULL, value_json TEXT NOT NULL, version INTEGER NOT NULL DEFAULT 1, updated_at INTEGER NOT NULL, PRIMARY KEY (scope, key))`,
	`CREATE TABLE IF NOT EXISTS apps (name TEXT PRIMARY KEY, title TEXT NOT NULL, kind TEXT NOT NULL, svg TEXT NOT NULL DEFAULT '', code TEXT NOT NULL DEFAULT '', tick_ms INTEGER NOT NULL DEFAULT 0, max_ticks INTEGER NOT NULL DEFAULT 0, author TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL, template TEXT NOT NULL DEFAULT '', data_json TEXT NOT NULL DEFAULT '{}')`,
	`CREATE TABLE IF NOT EXISTS timers (id TEXT PRIMARY KEY, device_id TEXT NOT NULL, label TEXT NOT NULL, say TEXT NOT NULL, chime TEXT NOT NULL, wake_agent INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, fire_at TEXT NOT NULL, notes_json TEXT NOT NULL DEFAULT '[]', app_name TEXT NOT NULL DEFAULT '')`,
	`CREATE TABLE IF NOT EXISTS build_jobs (id TEXT PRIMARY KEY, device_id TEXT NOT NULL, state TEXT NOT NULL, spec TEXT NOT NULL,
		created_at INTEGER NOT NULL, result TEXT NOT NULL DEFAULT '', announced INTEGER NOT NULL DEFAULT 0, cleanup INTEGER NOT NULL DEFAULT 0)`,
	`CREATE INDEX IF NOT EXISTS build_jobs_state ON build_jobs (state, device_id)`,
	`CREATE TABLE IF NOT EXISTS parent_settings (device_id TEXT PRIMARY KEY, timezone TEXT NOT NULL, quiet_start TEXT NOT NULL, quiet_end TEXT NOT NULL, daily_minutes INTEGER NOT NULL, pause_until_ms INTEGER NOT NULL, override_until_ms INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS parent_usage (device_id TEXT NOT NULL, day TEXT NOT NULL, used_ms INTEGER NOT NULL, PRIMARY KEY (device_id, day))`,
	createHistoryBackfill,
}

type bridgeDB struct {
	*sql.DB
	cloud      *turso.TursoSyncDb
	lastBackup atomic.Int64
}

// VACUUM INTO produces a standalone consistent SQLite file, including WAL data,
// without copying live database files or pausing the sync worker.
func (d *bridgeDB) backup(ctx context.Context, dir string, now time.Time) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	target := filepath.Join(dir, "echoear-"+now.Format(time.DateOnly)+".db")
	_, err := os.Lstat(target)
	if os.IsNotExist(err) {
		staging, err := os.MkdirTemp(dir, ".backup-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(staging)
		tmp := filepath.Join(staging, "echoear.db")
		// Turso 0.8.4 requires a literal and does not unescape doubled quotes
		// in the VACUUM path, but accepts either quote delimiter.
		quote := "'"
		if strings.Contains(tmp, quote) {
			quote = `"`
			if strings.Contains(tmp, quote) {
				return errors.New("backup path contains both SQL quote delimiters")
			}
		}
		if _, err := d.ExecContext(ctx, "VACUUM INTO "+quote+tmp+quote); err != nil {
			return err
		}
		if err := os.Chmod(tmp, 0o600); err != nil {
			return err
		}
		copyDB, err := sql.Open("turso", tmp)
		if err != nil {
			return err
		}
		var check string
		err = copyDB.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&check)
		if err = errors.Join(err, copyDB.Close()); err != nil {
			return err
		}
		if check != "ok" {
			return fmt.Errorf("backup quick_check: %s", check)
		}
		f, err := os.Open(tmp)
		if err != nil {
			return err
		}
		if err := errors.Join(f.Sync(), f.Close()); err != nil {
			return err
		}
		if err := os.Rename(tmp, target); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	info, err := os.Lstat(target)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("daily backup path is not a regular file")
	}
	d.lastBackup.Store(info.ModTime().Unix())
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var daily []string
	for _, entry := range entries {
		date, isBackup := strings.CutPrefix(entry.Name(), "echoear-")
		if !entry.Type().IsRegular() || !isBackup || !strings.HasSuffix(date, ".db") {
			continue
		}
		if _, err := time.Parse(time.DateOnly, strings.TrimSuffix(date, ".db")); err == nil {
			daily = append(daily, entry.Name())
		}
	}
	for _, name := range daily[:max(0, len(daily)-7)] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

func (d *bridgeDB) backupPeriodically(ctx context.Context, dir string) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		run, cancel := context.WithTimeout(ctx, time.Minute)
		err := d.backup(run, dir, time.Now())
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("EchoEar daily backup: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (d *bridgeDB) getMeta(ctx context.Context, key string) (string, error) {
	var value string
	err := d.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return value, err
}

func (d *bridgeDB) setMeta(ctx context.Context, key, value string) error {
	_, err := d.ExecContext(ctx, `INSERT INTO meta VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func openBridgeDB(ctx context.Context, path, legacy, remote, token, fallbackVoice string) (_ *bridgeDB, err error) {
	if path != ":memory:" {
		path, err = filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		if strings.Contains(path, "?") {
			return nil, errors.New("database path cannot contain '?'")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		chmod := f.Chmod(0o600)
		if err := errors.Join(chmod, f.Close()); err != nil {
			return nil, err
		}
	}
	d := &bridgeDB{}
	if path != ":memory:" && remote != "" && token != "" {
		// Local-first: an unavailable cloud endpoint must not hold up boot.
		bootstrap := false
		d.cloud, err = turso.NewTursoSyncDb(ctx, turso.TursoSyncDbConfig{
			Path: path, RemoteUrl: remote, AuthToken: token, BootstrapIfEmpty: &bootstrap,
		})
		if err != nil {
			return nil, errors.New("cannot initialize optional Turso Cloud sync")
		}
		d.DB, err = d.cloud.Connect(ctx)
	} else {
		d.DB, err = sql.Open("turso", path)
	}
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			d.Close()
		}
	}()
	// Serialize SQL writers and keep one connection alive for :memory:.
	d.SetMaxOpenConns(1)
	if err := d.upgradeSchema(ctx, legacy, fallbackVoice); err != nil {
		return nil, err
	}
	return d, nil
}

const initialAutoVolume = 55

func (b *bridge) volumePrefs(id string) (auto bool, level int) {
	auto, level = true, initialAutoVolume
	if b.db != nil {
		err := b.db.QueryRow(`SELECT auto_volume,manual_volume FROM device_prefs WHERE device_id=?`, strings.ToLower(id)).Scan(&auto, &level)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Print("EchoEar could not read volume preferences")
		}
	}
	return auto, level
}

func (b *bridge) saveVolumePrefs(ctx context.Context, id string, auto bool, level int) error {
	if b.db == nil {
		return problem(503, "Volume storage unavailable")
	}
	_, err := b.db.ExecContext(ctx, `INSERT INTO device_prefs (device_id,auto_volume,manual_volume) VALUES (?,?,?)
		ON CONFLICT(device_id) DO UPDATE SET auto_volume=excluded.auto_volume,manual_volume=excluded.manual_volume`,
		strings.ToLower(id), auto, level)
	if err != nil {
		return problem(502, "Cannot save volume")
	}
	return nil
}

func (d *bridgeDB) syncCloud(ctx context.Context) error {
	if d.cloud == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := d.cloud.Push(ctx); err != nil {
		return err
	}
	_, err := d.cloud.Pull(ctx)
	return err
}

func (d *bridgeDB) syncPeriodically(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := d.syncCloud(ctx); err != nil && ctx.Err() == nil {
			log.Print("EchoEar Turso Cloud sync failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type rowScanner interface{ Scan(...any) error }

// Table definitions share the same SQL codecs between migration and runtime.
type store[T any] struct {
	mu                  sync.Mutex
	db                  *sql.DB
	table, key, columns string
	upsert              string
	scan                func(rowScanner) (string, T, error)
	values              func(string, T) []any
}

func (s *store[T]) all() (map[string]T, error) {
	return s.allContext(context.Background())
}

func (s *store[T]) allContext(ctx context.Context) (map[string]T, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+s.columns+" FROM "+s.table)
	if err != nil {
		return nil, problem(502, "Cannot read "+s.table)
	}
	defer rows.Close()
	out := make(map[string]T)
	for rows.Next() {
		id, value, err := s.scan(rows)
		if err != nil {
			return nil, problem(502, "Cannot read "+s.table)
		}
		out[id] = value
	}
	if rows.Err() != nil {
		return nil, problem(502, "Cannot read "+s.table)
	}
	return out, nil
}

func (s *store[T]) get(id string) (T, bool, error) {
	return s.getContext(context.Background(), id)
}

func (s *store[T]) getContext(ctx context.Context, id string) (T, bool, error) {
	_, value, err := s.scan(s.db.QueryRowContext(ctx, "SELECT "+s.columns+" FROM "+s.table+" WHERE "+s.key+"=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return value, false, nil
	}
	if err != nil {
		return value, false, problem(502, "Cannot read "+s.table)
	}
	return value, true, nil
}

func (s *store[T]) save(id string, value T, remove bool) error {
	var err error
	if remove {
		_, err = s.db.Exec("DELETE FROM "+s.table+" WHERE "+s.key+"=?", id)
	} else {
		_, err = s.db.Exec("INSERT INTO "+s.table+" ("+s.columns+") VALUES ("+placeholders(len(s.values(id, value)))+") ON CONFLICT("+s.key+") DO UPDATE SET "+s.upsert, s.values(id, value)...)
	}
	if err != nil {
		return problem(502, "Cannot save "+s.table)
	}
	return nil
}

func placeholders(n int) string { return strings.Repeat("?,", n-1) + "?" }

func deviceStore(d *bridgeDB) *store[peer] {
	return &store[peer]{db: d.DB, table: "devices", key: "device_id", columns: "device_id,character,ip,port,last_seen",
		upsert: "character=excluded.character,ip=excluded.ip,port=excluded.port,last_seen=excluded.last_seen",
		scan: func(r rowScanner) (string, peer, error) {
			var p peer
			err := r.Scan(&p.DeviceID, &p.Character, &p.IP, &p.Port, &p.LastSeen)
			return p.DeviceID, p, err
		}, values: func(id string, p peer) []any { return []any{id, p.Character, p.IP, p.Port, p.LastSeen} }}
}

func voiceStore(d *bridgeDB) *store[string] {
	return &store[string]{db: d.DB, table: "voices", key: "device_id", columns: "device_id,voice", upsert: "voice=excluded.voice",
		scan: scanString, values: func(id, voice string) []any { return []any{id, voice} }}
}

func scanString(r rowScanner) (string, string, error) {
	var id, value string
	err := r.Scan(&id, &value)
	return id, value, err
}

func sessionStore(d *bridgeDB) *store[string] {
	return &store[string]{db: d.DB, table: "sessions", key: "device_id", columns: "device_id,session_id,updated_at",
		upsert: "session_id=excluded.session_id,updated_at=excluded.updated_at,contract_hash='',context_after_ms=excluded.updated_at",
		scan: func(r rowScanner) (string, string, error) {
			var id, sid string
			var at int64
			err := r.Scan(&id, &sid, &at)
			return id, sid, err
		}, values: func(id, sid string) []any { return []any{id, sid, time.Now().UnixMilli()} }}
}

func virtualStore(d *bridgeDB) *store[virtualClient] {
	return &store[virtualClient]{db: d.DB, table: "virtual_clients", key: "id", columns: "id,character,theme,voice,created_at",
		upsert: "character=excluded.character,theme=excluded.theme,voice=excluded.voice,created_at=excluded.created_at",
		scan: func(r rowScanner) (string, virtualClient, error) {
			var v virtualClient
			var at string
			err := r.Scan(&v.ID, &v.Character, &v.Theme, &v.Voice, &at)
			if err == nil {
				v.CreatedAt, err = time.Parse(time.RFC3339Nano, at)
			}
			return v.ID, v, err
		}, values: func(id string, v virtualClient) []any {
			return []any{id, v.Character, v.Theme, v.Voice, v.CreatedAt.Format(time.RFC3339Nano)}
		}}
}

func tunnelStore(d *bridgeDB) *store[tunnelRecord] {
	return &store[tunnelRecord]{db: d.DB, table: "tunnels", key: "device_id", columns: "device_id,route_name,url,local_port,scoped_token,profile,created_at",
		upsert: "route_name=excluded.route_name,url=excluded.url,local_port=excluded.local_port,scoped_token=excluded.scoped_token,profile=excluded.profile",
		scan: func(r rowScanner) (string, tunnelRecord, error) {
			var v tunnelRecord
			err := r.Scan(&v.DeviceID, &v.RouteName, &v.URL, &v.LocalPort, &v.Token, &v.Profile, &v.CreatedAt)
			return v.DeviceID, v, err
		}, values: func(id string, v tunnelRecord) []any {
			return []any{id, v.RouteName, v.URL, v.LocalPort, v.Token, v.Profile, v.CreatedAt}
		}}
}
