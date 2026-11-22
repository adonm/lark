package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	imagecolor "image/color"
	"strconv"
	"strings"
	"time"
)

func parseHexRGBA(s string, fallback imagecolor.RGBA) imagecolor.RGBA {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "#") || len(s) != 7 {
		return fallback
	}
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return fallback
	}
	return imagecolor.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 255}
}

type savedApp struct {
	Name, Title, Kind, Code, Author string
	Template                        string
	Data                            json.RawMessage
	TickMS, MaxTicks                int
	Version                         int64
}

const appColumns = "name,title,kind,code,tick_ms,max_ticks,author,updated_at,template,data_json"

func scanApp(row rowScanner, extra ...any) (savedApp, error) {
	var a savedApp
	var data string
	fields := []any{&a.Name, &a.Title, &a.Kind, &a.Code, &a.TickMS, &a.MaxTicks, &a.Author, &a.Version, &a.Template, &data}
	err := row.Scan(append(fields, extra...)...)
	a.Data = json.RawMessage(data)
	return a, err
}

func bookTextAndCover(a savedApp, fallbackColor imagecolor.RGBA) (string, imagecolor.RGBA) {
	cover := fallbackColor
	if cover.A == 0 {
		cover = pxMint
	}
	if len(a.Data) == 0 {
		return "", cover
	}
	var obj map[string]any
	if json.Unmarshal(a.Data, &obj) != nil {
		return "", cover
	}
	if c, ok := obj["cover"].(string); ok && strings.HasPrefix(c, "#") {
		cover = parseHexRGBA(c, cover)
	}
	if txt, ok := obj["text"].(string); ok && strings.TrimSpace(txt) != "" {
		return strings.TrimSpace(txt), cover
	}
	if pages, ok := obj["pages"].([]any); ok {
		var parts []string
		for _, p := range pages {
			switch v := p.(type) {
			case string:
				if s := strings.TrimSpace(v); s != "" {
					parts = append(parts, s)
				}
			case map[string]any:
				var seg []string
				if t, ok := v["title"].(string); ok && strings.TrimSpace(t) != "" {
					seg = append(seg, strings.TrimSpace(t))
				}
				if t, ok := v["text"].(string); ok && strings.TrimSpace(t) != "" {
					seg = append(seg, strings.TrimSpace(t))
				}
				if len(seg) > 0 {
					parts = append(parts, strings.Join(seg, "\n"))
				}
			}
		}
		return strings.Join(parts, "\n\n"), cover
	}
	return "", cover
}

func (a savedApp) summary() object {
	out := object{"name": a.Name, "title": a.Title, "kind": a.Kind, "version": a.Version, "updated_at": a.Version}
	return out
}

func (b *larkServer) loadApp(ctx context.Context, name string) (savedApp, error) {
	a := savedApp{Name: name}
	if b.db == nil {
		return a, problem(503, "Apps unavailable")
	}
	if !slugRE.MatchString(name) {
		return a, problem(400, "Invalid app name")
	}
	a, err := scanApp(b.db.readDB().QueryRowContext(ctx, "SELECT "+appColumns+" FROM apps WHERE name=?", name))
	if errors.Is(err, sql.ErrNoRows) {
		err = problem(404, "App not found")
	}
	return a, err
}

func (b *larkServer) saveApp(ctx context.Context, a savedApp) error {
	if b.db == nil {
		return problem(503, "Apps unavailable")
	}
	data := "{}"
	if a.Kind == "book" && len(a.Data) > 0 {
		data = string(a.Data)
	}
	_, err := b.db.ExecContext(ctx, `INSERT INTO apps (`+appColumns+`)
		SELECT ?,?,?,?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM apps WHERE name=?) OR (SELECT count(*) FROM apps) < 128
		ON CONFLICT(name) DO UPDATE SET title=excluded.title,kind=excluded.kind,code=excluded.code,
		tick_ms=excluded.tick_ms,max_ticks=excluded.max_ticks,author=excluded.author,template=excluded.template,
		data_json=excluded.data_json,updated_at=max(apps.updated_at+1,excluded.updated_at)`,
		a.Name, a.Title, a.Kind, a.Code, a.TickMS, a.MaxTicks, a.Author, time.Now().UnixMilli(), a.Template, data, a.Name)
	return err
}

func (b *larkServer) listApps(ctx context.Context, includeCode bool) ([]object, error) {
	out := []object{}
	if b.db == nil {
		return out, nil
	}
	columns := "name,title,kind,author,updated_at,tick_ms,max_ticks,template"
	if includeCode {
		columns += ",code,data_json"
	}
	rows, err := b.db.readDB().QueryContext(ctx, "SELECT "+columns+" FROM apps ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a savedApp
		var data string
		fields := []any{&a.Name, &a.Title, &a.Kind, &a.Author, &a.Version, &a.TickMS, &a.MaxTicks, &a.Template}
		if includeCode {
			fields = append(fields, &a.Code, &data)
		}
		if err := rows.Scan(fields...); err != nil {
			return nil, err
		}
		entry := a.summary()
		entry["author"] = a.Author
		if a.Kind == "book" && includeCode {
			entry["data"] = json.RawMessage(data)
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}
