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
	"sync/atomic"
	"time"

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

func sqliteDSN(path string, writer bool) string {
	if path == ":memory:" {
		return "file::memory:?_busy_timeout=5000&_cache_size=-32000&_foreign_keys=ON"
	}
	if writer {
		return path + "?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000&_cache_size=-32000&_foreign_keys=ON&_txlock=immediate"
	}
	return path + "?mode=ro&_query_only=1&_busy_timeout=5000&_cache_size=-32000"
}

var schemaBaseline = []string{
	`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS companions (id TEXT PRIMARY KEY, name TEXT NOT NULL, configured INTEGER NOT NULL DEFAULT 0, personality TEXT NOT NULL DEFAULT '', voice TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL, dc_account_id INTEGER NOT NULL DEFAULT 0, form TEXT NOT NULL DEFAULT '', portrait_id TEXT NOT NULL DEFAULT '', listen_seconds INTEGER NOT NULL DEFAULT 0)`,
	`CREATE TABLE IF NOT EXISTS terminals (id TEXT PRIMARY KEY, kind TEXT NOT NULL, companion_id TEXT REFERENCES companions(id) ON DELETE SET NULL, ip TEXT NOT NULL DEFAULT '', port INTEGER NOT NULL DEFAULT 0, last_seen INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL DEFAULT '')`,
	`CREATE TABLE IF NOT EXISTS terminal_prefs (terminal_id TEXT PRIMARY KEY, auto_volume INTEGER NOT NULL DEFAULT 1, manual_volume INTEGER NOT NULL DEFAULT 55)`,
	`CREATE TABLE IF NOT EXISTS sessions (companion_id TEXT PRIMARY KEY, session_id TEXT NOT NULL, updated_at INTEGER NOT NULL, contract_hash TEXT NOT NULL DEFAULT '', context_after_ms INTEGER NOT NULL DEFAULT 0)`,
	`CREATE TABLE IF NOT EXISTS messages (id TEXT PRIMARY KEY, companion_id TEXT NOT NULL, role TEXT NOT NULL, sender TEXT NOT NULL, text TEXT NOT NULL, source TEXT NOT NULL, channel TEXT NOT NULL DEFAULT '', card_title TEXT NOT NULL DEFAULT '', ts_ms INTEGER NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS messages_companion_ts_id ON messages (companion_id, ts_ms, id)`,
	`CREATE TABLE IF NOT EXISTS memory (scope TEXT NOT NULL, key TEXT NOT NULL, value_json TEXT NOT NULL, version INTEGER NOT NULL DEFAULT 1, updated_at INTEGER NOT NULL, PRIMARY KEY (scope, key))`,
	`CREATE TABLE IF NOT EXISTS stories (id TEXT PRIMARY KEY, title TEXT NOT NULL, twee TEXT NOT NULL, author TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL, passages INTEGER NOT NULL DEFAULT 0)`,
	`CREATE TABLE IF NOT EXISTS story_progress (companion_id TEXT NOT NULL, story_id TEXT NOT NULL, passage TEXT NOT NULL, page INTEGER NOT NULL DEFAULT 0, flags_json TEXT NOT NULL DEFAULT '{}', updated_at INTEGER NOT NULL, PRIMARY KEY (companion_id, story_id))`,
	`CREATE TABLE IF NOT EXISTS timers (id TEXT PRIMARY KEY, companion_id TEXT NOT NULL, label TEXT NOT NULL, say TEXT NOT NULL, chime TEXT NOT NULL, wake_agent INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, fire_at TEXT NOT NULL, notes_json TEXT NOT NULL DEFAULT '[]', app_name TEXT NOT NULL DEFAULT '')`,
}

type larkDB struct {
	*sql.DB
	readers    *sql.DB
	dataDir    string
	lastBackup atomic.Int64
}

func (d *larkDB) readDB() *sql.DB {
	if d.readers != nil {
		return d.readers
	}
	return d.DB
}

func (d *larkDB) Close() error {
	var err error
	if d.readers != nil && d.readers != d.DB {
		err = d.readers.Close()
	}
	if d.DB != nil {
		err = errors.Join(err, d.DB.Close())
	}
	return err
}

// VACUUM INTO produces a standalone consistent SQLite file, including WAL data,
// without copying live database files or pausing readers.
func (d *larkDB) backup(ctx context.Context, dir string, now time.Time) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	target := filepath.Join(dir, "lark-"+now.Format(time.DateOnly)+".db")
	_, err := os.Lstat(target)
	if os.IsNotExist(err) {
		staging, err := os.MkdirTemp(dir, ".backup-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(staging)
		tmp := filepath.Join(staging, "lark.db")
		// SQLite VACUUM INTO requires a string literal path; pick a quote delimiter not in tmp.
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
		copyDB, err := sql.Open("lark-sqlite3", tmp)
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
		date, isBackup := strings.CutPrefix(entry.Name(), "lark-")
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

func (d *larkDB) backupPeriodically(ctx context.Context, dir string) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		run, cancel := context.WithTimeout(ctx, time.Minute)
		err := d.backup(run, dir, time.Now())
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("Lark daily backup: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (d *larkDB) getMeta(ctx context.Context, key string) (string, error) {
	var value string
	err := d.readDB().QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return value, err
}

func (d *larkDB) setMeta(ctx context.Context, key, value string) error {
	_, err := d.ExecContext(ctx, `INSERT INTO meta VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func openLarkDB(ctx context.Context, path string) (_ *larkDB, err error) {
	var dataDir string
	if path != ":memory:" {
		path, err = filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		if strings.Contains(path, "?") {
			return nil, errors.New("database path cannot contain '?'")
		}
		dataDir = filepath.Dir(path)
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
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
	d := &larkDB{dataDir: dataDir}
	d.DB, err = sql.Open("lark-sqlite3", sqliteDSN(path, true))
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
	if err := d.initSchema(ctx); err != nil {
		return nil, err
	}
	if path == ":memory:" {
		d.readers = d.DB
	} else {
		d.readers, err = sql.Open("lark-sqlite3", sqliteDSN(path, false))
		if err != nil {
			return nil, err
		}
		d.readers.SetMaxOpenConns(8)
		if err := d.readers.PingContext(ctx); err != nil {
			return nil, err
		}
	}
	return d, nil
}

func (d *larkDB) initSchema(ctx context.Context) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("initialize Lark schema: %w", err)
	}
	defer tx.Rollback()
	for _, q := range schemaBaseline {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("initialize Lark schema: %w", err)
		}
	}
	return tx.Commit()
}

const initialAutoVolume = 55

func (b *larkServer) volumePrefs(id string) (auto bool, level int) {
	auto, level = true, initialAutoVolume
	if b.db == nil {
		return auto, level
	}
	err := b.db.readDB().QueryRow(`SELECT auto_volume,manual_volume FROM terminal_prefs WHERE terminal_id=?`, strings.ToLower(id)).Scan(&auto, &level)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Print("Lark could not read volume preferences")
		return auto, level
	}
	return auto, level
}

func (b *larkServer) saveVolumePrefs(ctx context.Context, id string, auto bool, level int) error {
	if b.db == nil {
		return problem(503, "Volume storage unavailable")
	}
	_, err := b.db.ExecContext(ctx, `INSERT INTO terminal_prefs (terminal_id,auto_volume,manual_volume) VALUES (?,?,?)
		ON CONFLICT(terminal_id) DO UPDATE SET auto_volume=excluded.auto_volume,manual_volume=excluded.manual_volume`,
		strings.ToLower(id), auto, level)
	if err != nil {
		return problem(502, "Cannot save volume")
	}
	return nil
}

type rowScanner interface{ Scan(...any) error }

// Table definitions share the same SQL codecs between migration and runtime.
type store[T any] struct {
	db, readDB          *sql.DB
	table, key, columns string
	upsert              string
	scan                func(rowScanner) (string, T, error)
	values              func(string, T) []any
}

func (s *store[T]) all() (map[string]T, error) {
	return s.allContext(context.Background())
}

func (s *store[T]) allContext(ctx context.Context) (map[string]T, error) {
	rdb := s.readDB
	if rdb == nil {
		rdb = s.db
	}
	rows, err := rdb.QueryContext(ctx, "SELECT "+s.columns+" FROM "+s.table)
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
	rdb := s.readDB
	if rdb == nil {
		rdb = s.db
	}
	_, value, err := s.scan(rdb.QueryRowContext(ctx, "SELECT "+s.columns+" FROM "+s.table+" WHERE "+s.key+"=?", id))
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

func terminalStore(d *larkDB) *store[terminalRecord] {
	return &store[terminalRecord]{db: d.DB, readDB: d.readDB(), table: "terminals", key: "id", columns: "id,kind,companion_id,ip,port,last_seen,created_at",
		upsert: "kind=excluded.kind,companion_id=excluded.companion_id,ip=excluded.ip,port=excluded.port,last_seen=excluded.last_seen,created_at=excluded.created_at",
		scan: func(r rowScanner) (string, terminalRecord, error) {
			var t terminalRecord
			var cid sql.NullString
			err := r.Scan(&t.ID, &t.Kind, &cid, &t.IP, &t.Port, &t.LastSeen, &t.CreatedAt)
			t.CompanionID = cid.String
			return t.ID, t, err
		}, values: func(id string, t terminalRecord) []any {
			var cid any
			if t.CompanionID != "" {
				cid = t.CompanionID
			}
			return []any{id, t.Kind, cid, t.IP, t.Port, t.LastSeen, t.CreatedAt}
		}}
}

// sessionStore only reads and deletes the OpenCode binding. Sessions are
// written by replaceBinding so the contract hash and dialogue watermark land
// atomically with the session ID.
type sessionStore struct {
	db, readDB *sql.DB
}

func (s *sessionStore) get(id string) (string, bool, error) {
	return s.getContext(context.Background(), id)
}

func (s *sessionStore) getContext(ctx context.Context, id string) (string, bool, error) {
	rdb := s.readDB
	if rdb == nil {
		rdb = s.db
	}
	var sid string
	err := rdb.QueryRowContext(ctx, `SELECT session_id FROM sessions WHERE companion_id=?`, id).Scan(&sid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, problem(502, "Cannot read sessions")
	}
	return sid, true, nil
}

func (s *sessionStore) allContext(ctx context.Context) (map[string]string, error) {
	rdb := s.readDB
	if rdb == nil {
		rdb = s.db
	}
	rows, err := rdb.QueryContext(ctx, `SELECT companion_id,session_id FROM sessions`)
	if err != nil {
		return nil, problem(502, "Cannot read sessions")
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var id, sid string
		if err := rows.Scan(&id, &sid); err != nil {
			return nil, problem(502, "Cannot read sessions")
		}
		out[id] = sid
	}
	if rows.Err() != nil {
		return nil, problem(502, "Cannot read sessions")
	}
	return out, nil
}

func (s *sessionStore) delete(id string) error {
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE companion_id=?`, id); err != nil {
		return problem(502, "Cannot delete session")
	}
	return nil
}
