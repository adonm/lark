package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var (
	slugRE      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,47}$`)
	memoryKeyRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)
)

func validScope(scope string) bool {
	if name, ok := strings.CutPrefix(scope, "companion:"); ok {
		return validCompanionID(name)
	}
	for _, prefix := range []string{"game:", "app:"} {
		if name, ok := strings.CutPrefix(scope, prefix); ok {
			return slugRE.MatchString(name)
		}
	}
	return scope == "shared"
}

type memoryWrite struct{ scope, key, value string }

func parseMemoryWrite(args object) (memoryWrite, error) {
	scope, key := str(args["scope"]), str(args["key"])
	if !validScope(scope) || !memoryKeyRE.MatchString(key) {
		return memoryWrite{}, problem(400, "Invalid memory scope or key")
	}
	value, ok := args["value"]
	if !ok {
		return memoryWrite{}, problem(400, "Memory value is required (may be null)")
	}
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > 16384 {
		return memoryWrite{}, problem(400, "Memory value exceeds 16384 JSON bytes")
	}
	return memoryWrite{scope, key, string(raw)}, nil
}

type versionConflict struct{ version int64 }

func (e *versionConflict) Error() string { return "version_conflict" }

type memoryEdit struct {
	memoryWrite
	expected *uint64
	delete   bool
}

var errMemoryAccess = problem(502, "Cannot access saved memory")

func (d *larkDB) setMemory(ctx context.Context, w memoryWrite, expected *uint64) (int64, error) {
	now := time.Now().UnixMilli()
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	entry, err := editMemory(ctx, tx, memoryEdit{memoryWrite: w, expected: expected}, now)
	if err != nil {
		return 0, err
	}
	return entry["version"].(int64), tx.Commit()
}

func editMemory(ctx context.Context, tx *sql.Tx, edit memoryEdit, now int64) (object, error) {
	var current int64
	err := tx.QueryRowContext(ctx, "SELECT version FROM memory WHERE scope=? AND key=?", edit.scope, edit.key).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if edit.expected != nil && uint64(current) != *edit.expected {
		return nil, &versionConflict{current}
	}
	if edit.delete {
		if current != 0 {
			if _, err := tx.ExecContext(ctx, "DELETE FROM memory WHERE scope=? AND key=?", edit.scope, edit.key); err != nil {
				return nil, err
			}
		}
		return object{"deleted": current != 0}, nil
	}
	var result sql.Result
	if current == 0 {
		result, err = tx.ExecContext(ctx, `INSERT INTO memory (scope,key,value_json,updated_at)
			SELECT ?,?,?,? WHERE (SELECT count(*) FROM memory WHERE scope=?) < 64
			ON CONFLICT(scope,key) DO NOTHING`, edit.scope, edit.key, edit.value, now, edit.scope)
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE memory SET value_json=?,version=version+1,updated_at=?
			WHERE scope=? AND key=? AND version=?`, edit.value, now, edit.scope, edit.key, current)
	}
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed == 0 {
		return nil, problem(409, "Memory scope already has 64 keys")
	}
	return object{"value": json.RawMessage(edit.value), "version": current + 1, "updated_at": now}, nil
}

func (b *larkServer) memoryEntries(ctx context.Context, scope string) ([]object, error) {
	entries := []object{}
	if !validScope(scope) {
		return nil, problem(400, "Invalid memory scope")
	}
	if b.db == nil {
		return entries, nil
	}
	rows, err := b.db.readDB().QueryContext(ctx, "SELECT key,value_json,version,updated_at FROM memory WHERE scope=? ORDER BY key", scope)
	if err != nil {
		return nil, errMemoryAccess
	}
	defer rows.Close()
	for rows.Next() {
		var k, raw string
		var version, at int64
		if err := rows.Scan(&k, &raw, &version, &at); err != nil {
			return nil, errMemoryAccess
		}
		entries = append(entries, object{"key": k, "value": json.RawMessage(raw), "version": version, "updated_at": at})
	}
	if rows.Err() != nil {
		return nil, errMemoryAccess
	}
	return entries, nil
}

func (b *larkServer) memoryState(ctx context.Context, scopes []string) (object, error) {
	out := object{}
	for _, scope := range scopes {
		if _, exists := out[scope]; exists {
			continue
		}
		entries, err := b.memoryEntries(ctx, scope)
		if err != nil {
			return nil, err
		}
		byKey := object{}
		for _, entry := range entries {
			key := str(entry["key"])
			delete(entry, "key")
			byKey[key] = entry
		}
		out[scope] = byKey
	}
	return out, nil
}

func (b *larkServer) patchMemory(ctx context.Context, raw any) (object, error) {
	if b.db == nil {
		return nil, problem(503, "Memory unavailable")
	}
	items, ok := raw.([]any)
	if !ok {
		items = []any{raw}
	}
	if len(items) == 0 {
		return nil, fieldError("memory", "expected at least one edit")
	}
	edits := make([]memoryEdit, 0, len(items))
	for _, item := range items {
		args, ok := item.(object)
		if !ok {
			return nil, fieldError("memory", "expected an edit object")
		}
		expected, err := mcpUint(args, "expected_version")
		if err != nil {
			return nil, err
		}
		op, supplied := args["op"]
		if !supplied {
			op = "set"
		}
		var w memoryWrite
		switch op {
		case "set":
			w, err = parseMemoryWrite(args)
			if err != nil {
				return nil, err
			}
		case "delete":
			w.scope, w.key = str(args["scope"]), str(args["key"])
			if !validScope(w.scope) || !memoryKeyRE.MatchString(w.key) {
				return nil, problem(400, "Invalid memory scope or key")
			}
		default:
			return nil, fieldError("memory.op", "expected set or delete")
		}
		edits = append(edits, memoryEdit{w, expected, op == "delete"})
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, errMemoryAccess
	}
	defer tx.Rollback()
	now, out := time.Now().UnixMilli(), object{}
	for _, edit := range edits {
		entry, err := editMemory(ctx, tx, edit, now)
		if err != nil {
			var conflict *versionConflict
			var api *apiError
			if errors.As(err, &conflict) || errors.As(err, &api) {
				return nil, err
			}
			return nil, errMemoryAccess
		}
		if out[edit.scope] == nil {
			out[edit.scope] = object{}
		}
		out[edit.scope].(object)[edit.key] = entry
	}
	if err := tx.Commit(); err != nil {
		return nil, errMemoryAccess
	}
	return out, nil
}

func deleteRow(ctx context.Context, d *larkDB, query string, args ...any) (bool, error) {
	result, err := d.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n != 0, err
}

func (b *larkServer) memoryAPI(w http.ResponseWriter, r *http.Request, path string) (any, error) {
	if b.db == nil {
		return nil, problem(503, "Memory unavailable")
	}
	q := r.URL.Query()
	scope, key := q.Get("scope"), q.Get("key")
	switch {
	case r.Method == http.MethodGet && path == "/api/memory":
		entries, err := b.memoryEntries(r.Context(), scope)
		return object{"scope": scope, "entries": entries}, err
	case r.Method == http.MethodDelete && path == "/api/memory":
		_, err := b.patchMemory(r.Context(), object{"op": "delete", "scope": scope, "key": key})
		if err == nil {
			w.WriteHeader(204)
		}
		return nil, err
	case r.Method == http.MethodGet && path == "/api/memory/scopes":
		dbError := problem(502, "Cannot list memory scopes")
		rows, err := b.db.readDB().QueryContext(r.Context(), "SELECT scope,count(*) FROM memory GROUP BY scope ORDER BY scope")
		if err != nil {
			return nil, dbError
		}
		defer rows.Close()
		scopes := []object{}
		for rows.Next() {
			var scope string
			var count int
			if err := rows.Scan(&scope, &count); err != nil {
				return nil, dbError
			}
			scopes = append(scopes, object{"scope": scope, "count": count})
		}
		if rows.Err() != nil {
			return nil, dbError
		}
		return object{"scopes": scopes}, nil
	}
	return nil, problem(404, "Not found")
}
