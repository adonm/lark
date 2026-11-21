package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/dop251/goja"
)

func (a *savedApp) setTiming(args object) error {
	for _, o := range []struct {
		key       string
		dest      *int
		low, high uint64
	}{
		{"tick_ms", &a.TickMS, 250, 10000}, {"max_ticks", &a.MaxTicks, 1, 240},
	} {
		n, err := mcpUint(args, o.key)
		if err != nil {
			return err
		}
		if n != nil {
			if a.Template != "" {
				return fieldError(o.key, "templates manage their own ticks")
			}
			*o.dest = int(min(o.high, max(o.low, *n)))
		}
	}
	if a.TickMS != 0 && a.MaxTicks == 0 {
		a.MaxTicks = 240
	}
	return nil
}

func (b *larkServer) updateSavedApp(ctx context.Context, tx *sql.Tx, edit object) (savedApp, error) {
	name, err := normalizeAppName(str(edit["name"]), "apps.update.name")
	if err != nil {
		return savedApp{}, err
	}
	expected, err := mcpUint(edit, "expected_version")
	if err != nil {
		return savedApp{}, err
	}
	if len(edit) == 1 || len(edit) == 2 && expected != nil {
		return savedApp{}, fieldError("apps.update", "expected a field to change")
	}
	a, err := scanApp(tx.QueryRowContext(ctx, "SELECT "+appColumns+" FROM apps WHERE name=?", name))
	if errors.Is(err, sql.ErrNoRows) {
		return a, problem(404, "saved app not found: "+name)
	}
	if err != nil {
		return a, errSavedAppEdit
	}
	if expected != nil && uint64(a.Version) != *expected {
		return a, &versionConflict{a.Version}
	}
	if err := applyAppEdit(&a, edit); err != nil {
		return a, err
	}
	if _, ok := edit["data"]; ok {
		if _, _, err := initApp(a, []appPlayer{{id: "preview"}}, nil, nil, nil, buildScreen); err != nil {
			return a, fieldError("apps.update.data", "%s", err)
		}
	}
	return storeAppEdit(ctx, tx, a)
}

func applyAppEdit(a *savedApp, edit object) error {
	if raw, ok := edit["title"]; ok {
		a.Title = shortText(raw, 60, false)
		if a.Title == "" {
			return fieldError("apps.update.title", "expected a non-empty title")
		}
	}
	if raw, ok := edit["code"]; ok {
		code, isString := raw.(string)
		if !isString || len(code) > 16<<10 {
			return fieldError("apps.update.code", "expected code up to 16 KB")
		}
		if code != "" {
			if _, err := goja.Compile("app.js", code, false); err != nil {
				return fieldError("apps.update.code", "%s", err)
			}
		}
		a.Code, a.Template, a.Data = code, "", nil
	}
	if raw, ok := edit["svg"]; ok {
		svg, isString := raw.(string)
		if !isString || a.Template != "" {
			return fieldError("apps.update.svg", "expected an SVG string for a custom app")
		}
		a.SVG = ""
		if svg != "" {
			root, err := parseSVG(svg)
			if err != nil {
				return fieldError("apps.update.svg", "%s", err)
			}
			a.SVG = svgString(root)
		}
	}
	if raw, ok := edit["data"]; ok {
		if a.Template == "" || raw == nil {
			return fieldError("apps.update.data", "expected a data object for a template")
		}
		if fields, ok := raw.(object); ok && edit["title"] == nil {
			if title := shortText(fields["title"], 60, false); title != "" {
				a.Title = title
			}
		}
		if err := a.setTemplate(a.Template, raw); err != nil {
			return err
		}
	}
	switch {
	case a.scripted():
		a.Kind = "app"
	case a.SVG != "":
		a.Kind = "drawing"
	default:
		return fieldError("apps.update", "cannot remove the entire app definition")
	}
	return a.setTiming(edit)
}

func storeAppEdit(ctx context.Context, tx *sql.Tx, a savedApp) (savedApp, error) {
	a, err := scanApp(tx.QueryRowContext(ctx, `UPDATE apps SET title=?,kind=?,svg=?,code=?,tick_ms=?,max_ticks=?,template=?,
		data_json=?,updated_at=max(updated_at+1,?) WHERE name=? RETURNING `+appColumns,
		a.Title, a.Kind, a.SVG, a.Code, a.TickMS, a.MaxTicks, a.Template, a.dataJSON(), time.Now().UnixMilli(), a.Name))
	if err != nil {
		return a, errSavedAppEdit
	}
	return a, nil
}

func (b *larkServer) saveCurrentApp(ctx context.Context, id string, raw any) (object, error) {
	args, ok := raw.(object)
	if !ok {
		return nil, fieldError("save_current", "expected an object with a name")
	}
	name, err := normalizeAppName(str(args["name"]), "save_current.name")
	if err != nil {
		return nil, err
	}
	title := shortText(args["title"], 60, false)
	if _, ok := args["title"]; ok && title == "" {
		return nil, fieldError("save_current.title", "expected a non-empty title")
	}
	h := b.ws
	h.mu.Lock()
	d := h.devices[strings.ToLower(id)]
	if d == nil || !h.active(d) || d.app == nil {
		h.mu.Unlock()
		return nil, problem(409, "No running app on "+id)
	}
	a := d.app.spec
	a.Name = name
	if title != "" {
		a.Title = title
	}
	out := object{"ok": true, "devices": []string{d.who.DeviceID}, "title": d.app.spec.Title, "hit_ids": appHitIDs(d.appView),
		"saved_as": name}
	if state := d.app.snapshot; state != nil {
		out["state"] = state
	}
	if warnings := appLayoutWarnings([]svgScreen{d.appView}, d.screen.shape == "round"); len(warnings) > 0 {
		out["layout_warnings"] = warnings
	}
	saved, err := b.writeApp(ctx, a)
	h.mu.Unlock()
	if err != nil {
		return nil, err
	}
	b.appsChanged()
	out["saved"] = saved.summary()
	return out, nil
}
