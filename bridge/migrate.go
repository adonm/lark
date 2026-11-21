package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const baselineVersion = 2

func (d *bridgeDB) upgradeSchema(ctx context.Context, legacy, fallbackVoice string) error {
	// Append versions; never insert into or renumber the sequence.
	steps := []struct {
		name string
		run  func(context.Context, *sql.Tx) error
	}{
		{"create tables", createBaseline},
		{"upgrade unversioned columns", upgradeLegacyColumns},
		{"import JSON", func(ctx context.Context, tx *sql.Tx) error { return d.importLegacyJSON(ctx, tx, legacy) }},
		{"seed face SVG", migrateFaceSVG},
		{"backfill local messages", migrateLocalMessages},
		{"seed cats", func(ctx context.Context, tx *sql.Tx) error {
			return migrateCats(ctx, tx, deviceVoiceStore(d, fallbackVoice))
		}},
		{"seed device preferences", func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO device_prefs (device_id)
				SELECT lower(device_id) FROM cats UNION SELECT lower(device_id) FROM devices UNION SELECT lower(id) FROM virtual_clients WHERE true
				ON CONFLICT DO NOTHING`)
			return err
		}},
		{"add OpenCode history checkpoints", func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, createHistoryBackfill)
			return err
		}},
		{"version companion session bindings", func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE sessions ADD COLUMN contract_hash TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `ALTER TABLE sessions ADD COLUMN context_after_ms INTEGER NOT NULL DEFAULT 0`); err != nil {
				return err
			}
			// The last binding write is the earliest safe boundary for legacy
			// bindings; older visible messages may predate a reset or reassignment.
			_, err := tx.ExecContext(ctx, `UPDATE sessions SET context_after_ms=updated_at`)
			return err
		}},
	}
	version, err := d.initSchema(ctx)
	if err != nil {
		return fmt.Errorf("initialize EchoEar schema: %w", err)
	}
	if version > len(steps) {
		return fmt.Errorf("EchoEar schema version %d is newer than supported version %d", version, len(steps))
	}
	for i := version; i < len(steps); i++ {
		if err := d.applySchemaStep(ctx, i+1, steps[i].run); err != nil {
			return fmt.Errorf("EchoEar schema version %d (%s): %w", i+1, steps[i].name, err)
		}
	}
	return nil
}

func (d *bridgeDB) initSchema(ctx context.Context) (int, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER PRIMARY KEY)`); err != nil {
		return 0, err
	}
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return 0, err
	}
	if version == 0 {
		var existing bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sqlite_schema
			WHERE type='table' AND name<>'schema_version' AND name NOT GLOB 'sqlite_*')`).Scan(&existing); err != nil {
			return 0, err
		}
		if !existing {
			if err := createBaseline(ctx, tx); err != nil {
				return 0, err
			}
			version = baselineVersion
			if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version VALUES (?)`, version); err != nil {
				return 0, err
			}
		}
	}
	return version, tx.Commit()
}

func (d *bridgeDB) applySchemaStep(ctx context.Context, version int, run func(context.Context, *sql.Tx) error) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := run(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version VALUES (?)`, version); err != nil {
		return err
	}
	return tx.Commit()
}

func createBaseline(ctx context.Context, tx *sql.Tx) error {
	for _, q := range schemaBaseline {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func upgradeLegacyColumns(ctx context.Context, tx *sql.Tx) error {
	for _, column := range []struct{ table, name, def string }{
		{"cats", "svg_extras", "''"}, {"cats", "face_svg", "''"},
		{"saved_cats", "svg_extras", "''"}, {"saved_cats", "face_svg", "''"},
		{"timers", "notes_json", "'[]'"}, {"timers", "app_name", "''"},
		{"apps", "template", "''"}, {"apps", "data_json", "'{}'"},
	} {
		rows, err := tx.QueryContext(ctx, `PRAGMA table_info(`+column.table+`)`)
		if err != nil {
			return err
		}
		found := false
		for rows.Next() {
			var name string
			var cid, kind, notNull, defaultVal, pk any
			if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultVal, &pk); err != nil {
				rows.Close()
				return err
			}
			found = found || name == column.name
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if !found {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE `+column.table+` ADD COLUMN `+column.name+` TEXT NOT NULL DEFAULT `+column.def); err != nil {
				return err
			}
		}
	}
	_, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS messages_device_ts`)
	return err
}

func legacyMigrationDone(ctx context.Context, tx *sql.Tx, marker string) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM meta WHERE key=?)`, marker).Scan(&exists)
	return exists, err
}

func migrateFaceSVG(ctx context.Context, tx *sql.Tx) error {
	for _, table := range []struct{ name, key, configured string }{
		{"cats", "device_id", "configured"}, {"saved_cats", "slug", "1"},
	} {
		rows, err := tx.QueryContext(ctx, `SELECT `+table.key+`,face_bg,mark_color,accent_color,`+table.configured+
			` FROM `+table.name+` WHERE face_svg=''`)
		if err != nil {
			return err
		}
		type seed struct{ key, svg string }
		var seeds []seed
		for rows.Next() {
			var key, bg, mark, accent string
			var configured int
			if err := rows.Scan(&key, &bg, &mark, &accent, &configured); err != nil {
				rows.Close()
				return err
			}
			seeds = append(seeds, seed{key, defaultFaceArtwork(bg, mark, accent, configured != 0, true)})
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, s := range seeds {
			if _, err := tx.ExecContext(ctx, `UPDATE `+table.name+` SET face_svg=? WHERE `+table.key+`=?`, s.svg, s.key); err != nil {
				return err
			}
		}
		// Overlays could mask the editable eyes (Cake's closed-eye overlay did).
		if _, err := tx.ExecContext(ctx, `UPDATE `+table.name+` SET svg_extras='', extras_json='[]'
			WHERE (svg_extras<>'' OR extras_json<>'[]') AND NOT EXISTS (SELECT 1 FROM meta WHERE key='migrated_face_svg_v1')`); err != nil {
			return err
		}
	}
	return nil
}

func migrateMap[T any](ctx context.Context, tx *sql.Tx, dir string, s *store[T], valid func(string, T) bool) error {
	return legacyRows(dir, s.table, func(id string, raw json.RawMessage) error {
		var value T
		if json.Unmarshal(raw, &value) != nil || !valid(id, value) {
			return nil
		}
		if v, ok := any(value).(tunnelRecord); ok && v.CreatedAt == "" {
			v.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			value = any(v).(T)
		}
		args := s.values(id, value)
		_, err := tx.ExecContext(ctx, "INSERT INTO "+s.table+" ("+s.columns+") VALUES ("+placeholders(len(args))+") ON CONFLICT DO NOTHING", args...)
		return err
	})
}

func legacyRows(dir, name string, visit func(string, json.RawMessage) error) error {
	raw, err := os.ReadFile(filepath.Join(dir, name+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s.json: %w", name, err)
	}
	var rows map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil || rows == nil {
		return fmt.Errorf("invalid %s.json", name)
	}
	for id, row := range rows {
		if deviceIDRE.MatchString(id) {
			if err := visit(id, row); err != nil {
				return fmt.Errorf("import %s.json: %w", name, err)
			}
		}
	}
	return nil
}

func (d *bridgeDB) importLegacyJSON(ctx context.Context, tx *sql.Tx, legacy string) error {
	if legacy == "" {
		return nil
	}
	if done, err := legacyMigrationDone(ctx, tx, "migrated_json_v1"); err != nil || done {
		return err
	}
	if err := migrateMap(ctx, tx, legacy, deviceStore(d), validLegacyPeer); err != nil {
		return err
	}
	if err := migrateMap(ctx, tx, legacy, sessionStore(d), func(_ string, sid string) bool { return sessionIDRE.MatchString(sid) }); err != nil {
		return err
	}
	// Preserve alias rows as well as the canonical mapping. Prefer an existing LAN session.
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (device_id,session_id,updated_at)
		SELECT substr(device_id,7),session_id,updated_at FROM sessions
		WHERE device_id GLOB 'scope-virt-[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]' ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	if err := migrateMap(ctx, tx, legacy, virtualStore(d), validLegacyVirtual); err != nil {
		return err
	}
	if err := migrateMap(ctx, tx, legacy, voiceStore(d), func(id, voice string) bool {
		return id == strings.ToLower(id) && voice != ""
	}); err != nil {
		return err
	}
	if err := migrateMap(ctx, tx, legacy, tunnelStore(d), validLegacyTunnel); err != nil {
		return err
	}
	if err := legacyRows(legacy, "chat_events", func(id string, raw json.RawMessage) error {
		var entries []json.RawMessage
		if json.Unmarshal(raw, &entries) != nil {
			return nil
		}
		for _, item := range entries {
			var e chatEntry
			if json.Unmarshal(item, &e) == nil && e.ID != "" && !e.Timestamp.IsZero() {
				var fields struct {
					SceneTitle  string `json:"scene_title"`
					ToolSummary string `json:"tool_summary"`
				}
				if json.Unmarshal(item, &fields) == nil {
					if e.SceneTitle == "" {
						e.SceneTitle = fields.SceneTitle
					}
					e.ToolSummary = fields.ToolSummary
				}
				if _, err := tx.ExecContext(ctx, insertChat, chatValues(id, e)...); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

type backfillMessage struct {
	id    string
	entry chatEntry
}

func migrateLocalMessages(ctx context.Context, tx *sql.Tx) error {
	if done, err := legacyMigrationDone(ctx, tx, "migrated_messages_v2_local"); err != nil || done {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT device_id,id,role,sender,text,source,scene_title,ts_ms FROM chat_events`)
	if err != nil {
		return err
	}
	var batch []backfillMessage
	for rows.Next() {
		var row backfillMessage
		e := &row.entry
		var ts int64
		if err := rows.Scan(&row.id, &e.ID, &e.Role, &e.Sender, &e.Text, &e.Source, &e.SceneTitle, &ts); err != nil {
			rows.Close()
			return err
		}
		e.Timestamp = time.UnixMilli(ts)
		batch = append(batch, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	return insertBackfill(ctx, tx, batch)
}

func insertBackfill(ctx context.Context, tx *sql.Tx, batch []backfillMessage) error {
	for _, row := range batch {
		if e, ok := canonicalMessage(row.entry); ok {
			if _, err := tx.ExecContext(ctx, insertMessage, messageValues(row.id, e)...); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *bridgeDB) historyDeviceIDs(ctx context.Context) ([]string, error) {
	rows, err := d.QueryContext(ctx, `SELECT lower(device_id) FROM devices
		UNION SELECT lower(id) FROM virtual_clients
		UNION SELECT lower(device_id) FROM cats
		UNION SELECT lower(CASE WHEN device_id LIKE 'scope-virt-%' THEN substr(device_id,7) ELSE device_id END) FROM sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if deviceIDRE.MatchString(id) {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

func (b *bridge) migrateSessionMessages(ctx context.Context) error {
	const marker = "migrated_messages_v2"
	var done string
	err := b.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, marker).Scan(&done)
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	ids, err := b.db.historyDeviceIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		// The empty session ID marks a finished device. On retry there is no
		// need to list that device's sessions again.
		var done bool
		if err := b.db.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM history_backfill WHERE device_id=? AND session_id='')`, id).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		active, _, err := b.oc.store.getContext(ctx, id)
		if err != nil {
			return err
		}
		alias, _, err := b.oc.store.getContext(ctx, "scope-"+id)
		if err != nil {
			return err
		}
		sessions, err := b.oc.listDeviceSessions(ctx, id, active, false)
		if err != nil {
			return err
		}
		if alias != "" && !slices.Contains(sessions, alias) {
			sessions = append(sessions, alias)
		}
		who, err := b.lookupDevice(id)
		if err != nil {
			who = peer{DeviceID: id, Character: "EchoEar"}
		}
		who.DeviceID = id
		for _, sid := range sessions {
			if err := b.backfillSession(ctx, who, sid); err != nil {
				return err
			}
		}
		if _, err := b.db.ExecContext(ctx, `INSERT INTO history_backfill VALUES (?,'','','')`, id); err != nil {
			return err
		}
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM history_backfill`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta VALUES (?, '1') ON CONFLICT DO NOTHING`, marker); err != nil {
		return err
	}
	return tx.Commit()
}

func (b *bridge) backfillSession(ctx context.Context, who peer, sid string) error {
	var cursor, source string
	err := b.db.QueryRowContext(ctx, `SELECT next,source FROM history_backfill WHERE device_id=? AND session_id=?`,
		who.DeviceID, sid).Scan(&cursor, &source)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		source = "text"
	case err != nil || cursor == "":
		return err
	}
	seen := map[string]bool{cursor: true}
	for {
		page, err := b.oc.sessionMessagePage(ctx, sid, cursor, source)
		var status *upstreamStatus
		if err != nil && !(errors.As(err, &status) && status.code == http.StatusNotFound) {
			return err
		}
		if seen[page.next] {
			page.next = ""
		}
		var batch []backfillMessage
		for _, e := range page.entries {
			if e.Role != "user" && e.Role != "assistant" {
				continue
			}
			if e.Role == "assistant" {
				e.Sender = who.Character
			}
			batch = append(batch, backfillMessage{who.DeviceID, e})
		}
		if err := b.db.saveHistoryPage(ctx, who.DeviceID, sid, page, batch); err != nil || page.next == "" {
			return err
		}
		seen[page.next] = true
		cursor, source = page.next, page.source
	}
}

func (d *bridgeDB) saveHistoryPage(ctx context.Context, id, sid string, page sessionHistoryPage, batch []backfillMessage) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertBackfill(ctx, tx, batch); err != nil {
		return err
	}
	// A page becomes visible together with its cursor and speaker source, or
	// not at all. Reopening after a deadline cannot skip or repeat that page.
	if _, err := tx.ExecContext(ctx, `INSERT INTO history_backfill VALUES (?,?,?,?)
		ON CONFLICT(device_id,session_id) DO UPDATE SET next=excluded.next,source=excluded.source`,
		id, sid, page.next, page.source); err != nil {
		return err
	}
	return tx.Commit()
}
