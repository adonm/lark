package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"
)

type savedApp struct {
	Name, Title, Kind, SVG, Code, Author string
	Template                             string
	Data                                 json.RawMessage
	TickMS, MaxTicks                     int
	Version                              int64
}

const appColumns = "name,title,kind,svg,code,tick_ms,max_ticks,author,updated_at,template,data_json"

func scanApp(row rowScanner, extra ...any) (savedApp, error) {
	var a savedApp
	var data string
	fields := []any{&a.Name, &a.Title, &a.Kind, &a.SVG, &a.Code, &a.TickMS, &a.MaxTicks, &a.Author, &a.Version, &a.Template, &data}
	err := row.Scan(append(fields, extra...)...)
	a.Data = json.RawMessage(data)
	return a, err
}

func (a savedApp) scripted() bool { return a.Code != "" || a.Template != "" }

func (a savedApp) summary() object {
	out := object{"name": a.Name, "title": a.Title, "kind": a.Kind, "version": a.Version, "updated_at": a.Version,
		"tick_ms": a.TickMS, "max_ticks": a.MaxTicks}
	if a.Template != "" {
		out["template"] = a.Template
	}
	return out
}

func normalizeAppName(raw, field string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(raw))
	if !slugRE.MatchString(name) {
		return "", fieldError(field, "invalid saved app slug")
	}
	return name, nil
}

func (b *larkServer) savedAppState(ctx context.Context, name string) (object, error) {
	if b.db == nil {
		return nil, problem(503, "Apps unavailable")
	}
	var state string
	a, err := scanApp(b.db.readDB().QueryRowContext(ctx, `SELECT `+appColumns+`,
		(SELECT json_group_object(key,json(value_json)) FROM memory WHERE scope=?) FROM apps WHERE name=?`, "app:"+name, name), &state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, problem(404, "saved app not found")
	}
	if err != nil {
		return nil, problem(502, "Cannot read saved app")
	}
	out := a.summary()
	out["code"], out["svg"], out["state"] = a.Code, a.SVG, json.RawMessage(state)
	if a.Template != "" {
		out["data"] = a.Data
	}
	return out, nil
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
	_, err := b.writeApp(ctx, a)
	return err
}

func (b *larkServer) writeApp(ctx context.Context, a savedApp) (savedApp, error) {
	if b.db == nil {
		return a, problem(503, "Apps unavailable")
	}
	saved, err := scanApp(b.db.QueryRowContext(ctx, `INSERT INTO apps (`+appColumns+`)
		SELECT ?,?,?,?,?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM apps WHERE name=?) OR (SELECT count(*) FROM apps) < 128
		ON CONFLICT(name) DO UPDATE SET title=excluded.title,kind=excluded.kind,svg=excluded.svg,code=excluded.code,
		tick_ms=excluded.tick_ms,max_ticks=excluded.max_ticks,author=excluded.author,template=excluded.template,
		data_json=excluded.data_json,updated_at=max(apps.updated_at+1,excluded.updated_at)
		RETURNING `+appColumns,
		a.Name, a.Title, a.Kind, a.SVG, a.Code, a.TickMS, a.MaxTicks, a.Author, time.Now().UnixMilli(), a.Template, a.dataJSON(), a.Name))
	if errors.Is(err, sql.ErrNoRows) {
		return a, problem(409, "Saved app library is full")
	}
	return saved, err
}

func (a savedApp) dataJSON() string {
	if a.Template == "" || len(a.Data) == 0 {
		return "{}"
	}
	return string(a.Data)
}

func (b *larkServer) listApps(ctx context.Context, includeCode bool) ([]object, error) {
	out := []object{}
	if b.db == nil {
		return out, nil
	}
	columns := "name,title,kind,author,updated_at,tick_ms,max_ticks,template"
	if includeCode {
		columns += ",svg,code,data_json"
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
			fields = append(fields, &a.SVG, &a.Code, &data)
		}
		if err := rows.Scan(fields...); err != nil {
			return nil, err
		}
		entry := a.summary()
		entry["author"] = a.Author
		if a.Template != "" && includeCode {
			entry["data"] = json.RawMessage(data)
		}
		if a.SVG != "" {
			entry["svg"] = a.SVG
		}
		if a.Code != "" {
			entry["code"] = a.Code
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

func (b *larkServer) appState(ctx context.Context, includeCode bool) (object, error) {
	list, err := b.listApps(ctx, includeCode)
	if err != nil {
		return nil, err
	}
	out := object{}
	for _, a := range list {
		delete(a, "author")
		out[str(a["name"])] = a
	}
	return out, nil
}

func (b *larkServer) deleteSavedApp(ctx context.Context, name string) (bool, error) {
	if !slugRE.MatchString(name) {
		return false, problem(400, "Invalid app name")
	}
	out, err := b.editSavedApps(ctx, []string{name}, nil, nil)
	if err != nil {
		return false, err
	}
	deleted := out["deleted"].([]string)
	return len(deleted) != 0, err
}

type appRename struct{ from, to, title string }

func (b *larkServer) patchSavedApps(ctx context.Context, args object) (object, error) {
	var names []string
	if raw, ok := args["delete"]; ok {
		var err error
		names, err = stringList(raw, "apps.delete")
		if err != nil {
			return nil, err
		}
		for i, name := range names {
			names[i], err = normalizeAppName(name, "apps.delete")
			if err != nil {
				return nil, err
			}
		}
	}
	var renames []appRename
	if raw, ok := args["rename"]; ok {
		items, ok := raw.([]any)
		if !ok {
			return nil, fieldError("apps.rename", "expected an array of objects")
		}
		for _, item := range items {
			rename, ok := item.(object)
			if !ok {
				return nil, fieldError("apps.rename", "expected a rename object")
			}
			from, err := normalizeAppName(str(rename["from"]), "apps.rename.from")
			if err != nil {
				return nil, err
			}
			edit := appRename{from: from, to: from}
			to, hasTo := rename["to"]
			title, hasTitle := rename["title"]
			if !hasTo && !hasTitle {
				return nil, fieldError("apps.rename", "expected to or title")
			}
			if hasTo {
				edit.to, err = normalizeAppName(str(to), "apps.rename.to")
				if err != nil {
					return nil, err
				}
			}
			if hasTitle {
				edit.title = shortText(title, 60, false)
				if edit.title == "" {
					return nil, fieldError("apps.rename.title", "expected a non-empty title")
				}
			}
			renames = append(renames, edit)
		}
	}
	var updates []object
	if raw, exists := args["update"]; exists {
		items, ok := raw.([]any)
		if !ok {
			items = []any{raw}
		}
		for _, item := range items {
			update, ok := item.(object)
			if !ok {
				return nil, fieldError("apps.update", "expected an update object")
			}
			updates = append(updates, update)
		}
	}
	if len(names)+len(renames)+len(updates) == 0 {
		return nil, fieldError("apps", "expected at least one delete, rename, or update")
	}
	return b.editSavedApps(ctx, names, renames, updates)
}

var errSavedAppEdit = problem(502, "Cannot edit saved apps")

func (b *larkServer) editSavedApps(ctx context.Context, names []string, renames []appRename, updates []object) (object, error) {
	if b.db == nil {
		return nil, problem(503, "Apps unavailable")
	}
	out, err := b.transactSavedAppEdits(ctx, names, renames, updates)
	if err == nil && len(out["deleted"].([]string))+len(out["renamed"].([]object))+len(updates) != 0 {
		b.appsChanged()
	}
	return out, err
}

func (b *larkServer) transactSavedAppEdits(ctx context.Context, names []string, renames []appRename, updates []object) (object, error) {
	// Slug changes must be atomic with device app installation. Updates only
	// touch disk; rendering their preview must not block the hub.
	if b.ws != nil && len(names)+len(renames) > 0 {
		b.ws.mu.Lock()
		defer b.ws.mu.Unlock()
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, errSavedAppEdit
	}
	defer tx.Rollback()
	deleted, renamed := []string{}, []object{}
	for _, name := range names {
		result, err := tx.ExecContext(ctx, "DELETE FROM apps WHERE name=?", name)
		if err != nil {
			return nil, errSavedAppEdit
		}
		n, err := result.RowsAffected()
		if err != nil {
			return nil, errSavedAppEdit
		}
		if n == 0 {
			continue
		}
		if b.ws.appRunning(name) {
			return nil, problem(409, "saved app is running: "+name)
		}
		var armed bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM timers WHERE app_name=?)", name).Scan(&armed); err != nil {
			return nil, errSavedAppEdit
		}
		if armed {
			return nil, problem(409, "saved app is used by an armed timer: "+name)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM memory WHERE scope=?", "app:"+name); err != nil {
			return nil, errSavedAppEdit
		}
		deleted = append(deleted, name)
	}
	for _, edit := range renames {
		result, err := b.renameSavedApp(ctx, tx, edit)
		if err != nil {
			return nil, err
		}
		renamed = append(renamed, result)
	}
	updated := []object{}
	for _, edit := range updates {
		saved, err := b.updateSavedApp(ctx, tx, edit)
		if err != nil {
			return nil, err
		}
		updated = append(updated, saved.summary())
	}
	if err := tx.Commit(); err != nil {
		return nil, errSavedAppEdit
	}
	return object{"deleted": deleted, "renamed": renamed, "updated": updated}, nil
}

func (h *wsHub) appRunning(name string) bool {
	if h != nil {
		for _, d := range h.terminals {
			if d.app != nil && d.app.spec.Name == name {
				return true
			}
		}
	}
	return false
}

func (b *larkServer) renameSavedApp(ctx context.Context, tx *sql.Tx, edit appRename) (object, error) {
	var title string
	err := tx.QueryRowContext(ctx, "SELECT title FROM apps WHERE name=?", edit.from).Scan(&title)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, problem(404, "saved app not found: "+edit.from)
	}
	if err != nil {
		return nil, errSavedAppEdit
	}
	if edit.to != edit.from {
		if b.ws.appRunning(edit.from) {
			return nil, problem(409, "saved app is running: "+edit.from)
		}
		var appExists, stateExists bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM apps WHERE name=?),
			EXISTS(SELECT 1 FROM memory WHERE scope=?)`, edit.to, "app:"+edit.to).Scan(&appExists, &stateExists)
		if err != nil {
			return nil, errSavedAppEdit
		}
		if appExists {
			return nil, problem(409, "saved app already exists: "+edit.to)
		}
		if stateExists {
			return nil, problem(409, "saved app state already exists: "+edit.to)
		}
		if b.ws.appRunning(edit.to) {
			return nil, problem(409, "saved app is running: "+edit.to)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE memory SET scope=? WHERE scope=?", "app:"+edit.to, "app:"+edit.from); err != nil {
			return nil, errSavedAppEdit
		}
		if _, err := tx.ExecContext(ctx, "UPDATE timers SET app_name=? WHERE app_name=?", edit.to, edit.from); err != nil {
			return nil, errSavedAppEdit
		}
	}
	if edit.title != "" {
		title = edit.title
	}
	if _, err := tx.ExecContext(ctx, "UPDATE apps SET name=?,title=?,updated_at=max(updated_at+1,?) WHERE name=?",
		edit.to, title, time.Now().UnixMilli(), edit.from); err != nil {
		return nil, errSavedAppEdit
	}
	return object{"from": edit.from, "to": edit.to, "title": title}, nil
}

func (b *larkServer) appAPI(w http.ResponseWriter, r *http.Request, path string) (any, error) {
	ctx := r.Context()
	switch {
	case path == "/api/apps" && r.Method == http.MethodGet:
		list, err := b.listApps(ctx, false)
		return object{"apps": list}, err
	case path == "/api/apps/run" && r.Method == http.MethodPost:
		data, err := jsonBody(r)
		if err != nil {
			return nil, err
		}
		return b.runSavedApp(ctx, strings.ToLower(str(data["companion_id"])), str(data["name"]), nil)
	case r.Method == http.MethodDelete:
		name := strings.TrimPrefix(path, "/api/apps/")
		_, err := b.deleteSavedApp(ctx, name)
		if err != nil {
			return nil, err
		}
		w.WriteHeader(204)
		return nil, nil
	}
	return nil, problem(404, "Not found")
}

type appPlayer struct {
	id, name string
	index    int
}

type appEffect struct {
	kind, text string
	value      json.RawMessage
	pcm        []byte
	all        bool
}

const appEffectLimit = 8

// One worker owns the VM and lastScreens after initialization. The hub owns
// device membership, queued events, and tick scheduling.
type appInstance struct {
	spec                                savedApp
	players                             []appPlayer
	terminals []*wsTerminal
	vm                                  *goja.Runtime
	self, state                         goja.Value
	render, parse, stringify, errorText goja.Callable
	merge                               goja.Callable
	hooks                               map[string]goja.Callable
	scope                               string
	memory                              map[string]json.RawMessage
	effects                             []appEffect
	allowEffects                        bool
	geometry                            []screenGeometry
	gradeSources                        []*atomic.Uint32 // Immutable; the hub can remove entries from devices.
	buildScreen                         func(string, bool, screenGeometry) (svgScreen, error)
	lastScreens                         []svgScreen
	ctx                                 context.Context
	cancel                              context.CancelFunc
	events                              []appEvent
	running, tickQueued                 bool
	nextTick, timeOfSpeech              time.Time
	remaining                           int
	snapshot                            json.RawMessage // Immutable JSON published under the hub lock.
}

type appEvent struct {
	i, x, y  int
	kind, id string
	now      time.Time
	patch    json.RawMessage
	done     chan appPatchResult
}

type appPatchResult struct {
	out object
	err error
}

func (a *appInstance) runJS(fn func() (goja.Value, error)) (goja.Value, error) {
	vm := a.vm
	done := make(chan struct{})
	timer := time.AfterFunc(50*time.Millisecond, func() {
		vm.Interrupt("timeout")
		close(done)
	})
	defer func() {
		if !timer.Stop() {
			<-done
		}
		vm.ClearInterrupt()
	}()
	v, err := fn()
	if ex, ok := err.(*goja.Exception); ok {
		var out bytes.Buffer
		if msg, textErr := a.errorText(goja.Undefined(), ex.Value()); textErr == nil {
			if s, ok := msg.(goja.String); ok {
				out.WriteString(s.Substring(0, min(512, s.Length())).String())
			}
		}
		if out.Len() == 0 {
			out.WriteString("JS exception")
		}
		for _, frame := range ex.Stack()[:min(8, len(ex.Stack()))] {
			out.WriteString("\n\tat ")
			frame.Write(&out)
		}
		return v, errors.New(out.String())
	}
	if trace, ok := err.(interface{ String() string }); ok {
		return v, errors.New(strings.TrimSpace(trace.String()))
	}
	return v, err
}

func (a *appInstance) call(fn goja.Callable, args ...goja.Value) (goja.Value, error) {
	return a.runJS(func() (goja.Value, error) { return fn(a.self, args...) })
}

func (a *appInstance) player(i int) goja.Value {
	p := a.players[i]
	// Copy primitives into a native JS object, not a reflected Go map.
	v := a.vm.NewObject()
	for k, value := range map[string]any{"id": p.id, "name": p.name, "index": p.index} {
		_ = v.DefineDataProperty(k, a.vm.ToValue(value), goja.FLAG_FALSE, goja.FLAG_FALSE, goja.FLAG_TRUE)
	}
	return v
}

func initApp(spec savedApp, players []appPlayer, memory map[string]json.RawMessage, terminals []*wsTerminal, initial json.RawMessage, renderer func(string, bool, screenGeometry) (svgScreen, error)) (*appInstance, []svgScreen, error) {
	a := &appInstance{spec: spec, players: players, memory: memory, terminals: terminals, scope: "app:" + spec.Name,
		geometry: make([]screenGeometry, len(players)), gradeSources: make([]*atomic.Uint32, len(players)),
		buildScreen: renderer, lastScreens: make([]svgScreen, len(players))}
	for i := range players {
		// Geometry and the client role do not change after hello.
		if terminals == nil {
			a.geometry[i] = roundScreen
		} else if !terminals[i].virtual() {
			a.geometry[i] = terminals[i].screen
			a.gradeSources[i] = &terminals[i].grade
		}
	}
	if spec.scripted() {
		if len(spec.Code) > 16<<10 {
			return nil, nil, problem(400, "App code exceeds 16 KB")
		}
		if err := a.initVM(initial); err != nil {
			return nil, nil, err
		}
	}
	screens, err := a.screens(nil)
	if err == nil {
		a.snapshot, err = a.stateJSON()
	}
	return a, screens, err
}

func (a *appInstance) initVM(initial json.RawMessage) error {
	vm := goja.New()
	a.vm = vm
	vm.SetMaxCallStackSize(512)
	a.errorText, _ = goja.AssertFunction(vm.Get("String"))
	hostJSON := vm.Get("JSON").ToObject(vm)
	a.parse, _ = goja.AssertFunction(hostJSON.Get("parse"))
	a.stringify, _ = goja.AssertFunction(hostJSON.Get("stringify"))
	merge, err := vm.RunString(`(function (state, patch) {
		if (state === null || typeof state !== "object" || Array.isArray(state)) {
			throw new TypeError("state_patch requires an object app state");
		}
		return {...state, ...patch};
	})`)
	if err != nil {
		return err
	}
	a.merge, _ = goja.AssertFunction(merge)
	bad := func(err error) { panic(vm.NewTypeError("%s", err.Error())) }
	asString := func(v goja.Value) (string, bool) {
		s, ok := v.(goja.String)
		if !ok || s.Length() > 16384 {
			return "", false
		}
		return s.String(), true
	}
	checkEffectLimit := func() {
		if len(a.effects) >= appEffectLimit {
			panic(vm.NewTypeError("Too many app effects"))
		}
	}
	run := func(name string, f func(goja.FunctionCall) goja.Value) {
		_ = vm.Set(name, func(c goja.FunctionCall) goja.Value {
			if !a.allowEffects {
				panic(vm.NewTypeError("%s is only available in onTap/onDrag/onUp/onTick", name))
			}
			return f(c)
		})
	}
	addEffect := func(e appEffect) goja.Value {
		checkEffectLimit()
		a.effects = append(a.effects, e)
		return goja.Undefined()
	}
	for _, name := range []string{"play", "playAll"} {
		run(name, func(c goja.FunctionCall) goja.Value {
			checkEffectLimit()
			var notes any
			v := c.Argument(0)
			if s, ok := asString(v); ok {
				notes = s
			} else if obj, ok := v.(*goja.Object); ok && obj.ClassName() == "Array" && obj.Get("length").ToInteger() <= 32 {
				list := []any{}
				for i := range int(obj.Get("length").ToInteger()) {
					s, ok := asString(obj.Get(fmt.Sprint(i)))
					if !ok {
						panic(vm.NewTypeError("Expected note strings"))
					}
					list = append(list, s)
				}
				notes = list
			}
			_, pcm, err := melodyInput(notes, "notes")
			if err != nil {
				bad(err)
			}
			return addEffect(appEffect{kind: "pcm", pcm: pcm, all: name == "playAll"})
		})
	}
	isAppChime := func(kind string) bool { return kind == "cancel" || validChime(kind) }
	warnedSFX := false
	for _, name := range []string{"sfx", "sfxAll"} {
		run(name, func(c goja.FunctionCall) goja.Value {
			kind, _ := asString(c.Argument(0))
			all := name == "sfxAll"
			if isAppChime(kind) {
				return addEffect(appEffect{kind: "chime", text: kind, all: all})
			}
			if _, ok := sfxPresets[kind]; !ok {
				if !warnedSFX {
					warnedSFX = true
					slog.Warn("Unknown app sound ignored", "app", a.spec.Name, "function", name, "sound", truncate(kind, 80))
				}
				return goja.Undefined()
			}
			checkEffectLimit()
			pcm, err := sfxPCM(kind)
			if err != nil {
				bad(err)
			}
			return addEffect(appEffect{kind: "pcm", pcm: pcm, all: all})
		})
	}
	run("chime", func(c goja.FunctionCall) goja.Value {
		kind, _ := asString(c.Argument(0))
		if !isAppChime(kind) {
			panic(vm.NewTypeError("Unknown chime kind"))
		}
		return addEffect(appEffect{kind: "chime", text: kind})
	})
	installAppUI(vm, func() {
		if a.allowEffects && len(a.effects) < appEffectLimit {
			addEffect(appEffect{kind: "chime", text: "tap"})
		}
	})
	run("say", func(c goja.FunctionCall) goja.Value {
		text, _ := asString(c.Argument(0))
		if clean := shortText(text, 100, true); clean != "" {
			return addEffect(appEffect{kind: "say", text: clean})
		}
		return goja.Undefined()
	})
	run("exit", func(goja.FunctionCall) goja.Value {
		return addEffect(appEffect{kind: "exit"})
	})
	if a.spec.Template == "timer" {
		run("templateTick", func(c goja.FunctionCall) goja.Value {
			if c.Argument(0).ToBoolean() {
				return addEffect(appEffect{kind: "start_ticks"})
			}
			return addEffect(appEffect{kind: "stop_ticks"})
		})
	}
	_ = vm.Set("storeGet", func(c goja.FunctionCall) goja.Value {
		key, _ := asString(c.Argument(0))
		if !memoryKeyRE.MatchString(key) {
			panic(vm.NewTypeError("Invalid app memory key"))
		}
		value := a.memory[key]
		for _, e := range slices.Backward(a.effects) {
			if e.kind == "store" && e.text == key {
				value = e.value
				break
			}
		}
		if len(value) == 0 {
			return goja.Undefined()
		}
		v, err := a.parse(goja.Undefined(), vm.ToValue(string(value)))
		if err != nil {
			bad(err)
		}
		return v
	})
	run("storeSet", func(c goja.FunctionCall) goja.Value {
		checkEffectLimit()
		key, _ := asString(c.Argument(0))
		if !memoryKeyRE.MatchString(key) {
			panic(vm.NewTypeError("Invalid app memory key"))
		}
		raw, err := a.stringify(goja.Undefined(), c.Argument(1))
		if err != nil {
			bad(err)
		}
		s, ok := asString(raw)
		if !ok || len(s) > 16384 {
			panic(vm.NewTypeError("Expected a JSON value of up to 16384 bytes"))
		}
		return addEffect(appEffect{kind: "store", text: key, value: json.RawMessage(s)})
	})
	_ = vm.Set("app", goja.Undefined())
	source := a.spec.Code
	if a.spec.Template != "" {
		source, err = a.installTemplate()
		if err != nil {
			return err
		}
	}
	if _, err := a.runJS(func() (goja.Value, error) { return vm.RunScript("app.js", source) }); err != nil {
		return err
	}
	methods, err := a.runJS(func() (goja.Value, error) {
		return vm.RunScript("app-contract.js", `(function() {
			if (app && typeof app === "object") return [app, app.init, app.render, app.onTap, app.onDrag, app.onUp, app.onTick];
			return [void 0, typeof init === "function" ? init : void 0, typeof render === "function" ? render : void 0,
				typeof onTap === "function" ? onTap : void 0, typeof onDrag === "function" ? onDrag : void 0,
				typeof onUp === "function" ? onUp : void 0, typeof onTick === "function" ? onTick : void 0];
		})()`)
	})
	if err != nil {
		return err
	}
	obj := methods.(*goja.Object)
	a.self = obj.Get("0")
	init, _ := goja.AssertFunction(obj.Get("1"))
	a.render, _ = goja.AssertFunction(obj.Get("2"))
	a.hooks = make(map[string]goja.Callable, 4)
	for i, kind := range []string{"tap", "drag", "up", "tick"} {
		a.hooks[kind], _ = goja.AssertFunction(obj.Get(fmt.Sprint(i + 3)))
	}
	if a.render == nil {
		return fmt.Errorf("app.js must define render(state, player)")
	}
	ctx := vm.NewObject()
	players := make([]any, len(a.players))
	for i := range players {
		players[i] = a.player(i)
	}
	_ = ctx.DefineDataProperty("players", vm.NewArray(players...), goja.FLAG_FALSE, goja.FLAG_FALSE, goja.FLAG_TRUE)
	a.state = vm.NewObject()
	if initial != nil {
		a.state, err = a.call(a.parse, vm.ToValue(string(initial)))
		if err != nil {
			return err
		}
	}
	_ = ctx.DefineDataProperty("initial_state", a.state, goja.FLAG_FALSE, goja.FLAG_FALSE, goja.FLAG_TRUE)
	_ = ctx.DefineDataProperty("state", a.state, goja.FLAG_TRUE, goja.FLAG_FALSE, goja.FLAG_TRUE)
	if init != nil {
		state, err := a.call(init, ctx)
		if err != nil {
			return err
		}
		if !goja.IsUndefined(state) {
			a.state = state
		}
	}
	return nil
}

func (a *appInstance) stateJSON() (json.RawMessage, error) {
	if a.vm == nil {
		return nil, nil
	}
	v, err := a.call(a.stringify, a.state)
	if err != nil {
		return nil, fmt.Errorf("app state must be JSON: %w", err)
	}
	if s, ok := v.(goja.String); ok && s.Length() <= 64<<10 {
		if raw := s.String(); len(raw) <= 64<<10 {
			return json.RawMessage(raw), nil
		}
	}
	return nil, problem(400, "App state must fit in 64 KB of JSON")
}

func (a *appInstance) applyStatePatch(raw json.RawMessage, connected []bool) ([]svgScreen, json.RawMessage, error) {
	patch, err := a.call(a.parse, a.vm.ToValue(string(raw)))
	if err != nil {
		return nil, nil, err
	}
	next, err := a.call(a.merge, a.state, patch)
	if err != nil {
		return nil, nil, err
	}
	before := a.state
	a.state = next
	views, err := a.screens(connected)
	var snapshot json.RawMessage
	if err == nil {
		snapshot, err = a.stateJSON()
	}
	if err != nil {
		a.state = before
	}
	return views, snapshot, err
}

func (a *appInstance) screens(connected []bool) ([]svgScreen, error) {
	out := make([]svgScreen, len(a.players))
	for i := range out {
		if connected != nil && !connected[i] {
			continue
		}
		raw := a.spec.SVG
		if a.vm != nil {
			value, err := a.call(a.render, a.state, a.player(i))
			if err != nil {
				return nil, err
			}
			s, ok := value.(goja.String)
			if !ok {
				return nil, fmt.Errorf("app.js render must return an SVG string")
			}
			if s.Length() > maxSVG {
				return nil, fmt.Errorf("app.js render: SVG exceeds 64 KB")
			}
			raw = s.String()
		}
		grade := gradeDay
		if src := a.gradeSources[i]; src != nil {
			grade = gradeKey(src.Load())
		}
		if last := a.lastScreens[i]; last.source != "" && last.raw == raw && last.grade == grade {
			out[i] = last
			continue
		}
		var screen svgScreen
		var err error
		if grade == gradeDay {
			screen, err = a.buildScreen(raw, true, a.geometry[i])
		} else {
			screen, err = screens.buildGraded(raw, true, a.geometry[i], grade)
		}
		if err != nil {
			return nil, fmt.Errorf("render for player %d: %w", i, err)
		}
		a.lastScreens[i], out[i] = screen, screen
	}
	return out, nil
}

func (a *appInstance) update(ev appEvent, connected []bool) ([]svgScreen, error) {
	fn := a.hooks[ev.kind]
	if fn == nil {
		return nil, nil
	}
	args := []goja.Value{a.state}
	if ev.i >= 0 {
		args = append(args, a.player(ev.i), a.vm.ToValue(ev.id), a.vm.ToValue(ev.x), a.vm.ToValue(ev.y))
	}
	a.effects = nil
	a.allowEffects = true
	state, err := a.call(fn, args...)
	a.allowEffects = false
	if err != nil {
		return nil, err
	}
	if !goja.IsUndefined(state) {
		a.state = state
	}
	if slices.ContainsFunc(a.effects, func(e appEffect) bool { return e.kind == "exit" }) {
		return nil, nil
	}
	return a.screens(connected)
}

func appCompanions(raw any) ([]string, error) {
	var ids []string
	switch v := raw.(type) {
	case string:
		ids = []string{strings.ToLower(v)}
	case []any:
		for _, item := range v {
			ids = append(ids, strings.ToLower(str(item)))
		}
	}
	if len(ids) == 0 || len(ids) > 8 {
		return nil, problem(400, "Expected 1–8 companion IDs")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !validCompanionID(id) || seen[id] {
			return nil, problem(400, "Invalid or duplicate companion ID")
		}
		seen[id] = true
	}
	return ids, nil
}

func (b *larkServer) runSavedApp(ctx context.Context, id, name string, menu *menuContent) (object, error) {
	return b.launchApp(ctx, object{"companions": id, "load": name}, menu)
}

func (b *larkServer) runApp(ctx context.Context, args object) (object, error) {
	return b.launchApp(ctx, args, nil)
}

func (b *larkServer) launchApp(ctx context.Context, args object, menu *menuContent) (object, error) {
	if name, present := args["delete_saved"]; present {
		if len(args) != 1 {
			return nil, problem(400, "Use delete_saved without other app fields")
		}
		deleted, err := b.deleteSavedApp(ctx, str(name))
		return object{"ok": true, "delete_saved": name, "deleted": deleted, "companions": []string{}, "title": "", "hit_ids": []string{}}, err
	}
	ids, err := appCompanions(args["companions"])
	if err != nil {
		return nil, err
	}
	h := b.ws
	if h == nil {
		return nil, problem(503, "Terminal hub unavailable")
	}
	if raw, present := args["save_current"]; present {
		if len(args) != 2 || len(ids) != 1 {
			return nil, problem(400, "Use save_current with one companion only")
		}
		return b.saveCurrentApp(ctx, ids[0], raw)
	}
	if raw, present := args["state_patch"]; present {
		if len(args) != 2 {
			return nil, problem(400, "Use state_patch with companions only")
		}
		patch, err := appJSONObject(raw, "state_patch")
		if err != nil {
			return nil, err
		}
		return h.patchRunningApp(ctx, ids, patch)
	}
	if args["exit"] == true && len(args) != 2 {
		return nil, problem(400, "Use exit with companions only")
	}
	h.mu.Lock()
	players := make([]appPlayer, 0, len(ids))
	devices := make([]*wsTerminal, 0, len(ids))
	gens := make([]uint64, 0, len(ids))
	for _, id := range ids {
		terms := h.terminalsFor(id)
		if len(terms) == 0 {
			h.mu.Unlock()
			return nil, problem(409, "Companion is not connected: "+id)
		}
		if args["exit"] != true {
			if r := h.parentRefresh(id, time.Now()); r != "" {
				h.mu.Unlock()
				return nil, r.err()
			}
		}
		for _, d := range terms {
			if args["exit"] != true && d.callState != callNone {
				h.mu.Unlock()
				return nil, problem(409, "Companion is on a call: "+id)
			}
			if menu != nil && !d.canLaunchMenuGame(menu) {
				h.mu.Unlock()
				return nil, problem(409, "Games menu changed")
			}
			devices = append(devices, d)
			gens = append(gens, d.appGen)
			players = append(players, appPlayer{d.companionID, d.companion.Name, len(players)})
		}
	}
	h.mu.Unlock()
	if args["exit"] == true {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, d := range devices {
			if h.active(d) && d.app != nil {
				h.closeApp(d.app, time.Now())
			}
		}
		return object{"ok": true, "companions": ids, "title": "", "hit_ids": []string{}}, nil
	}
	a := savedApp{SVG: str(args["svg"]), Code: str(args["code"]), Title: shortText(args["title"], 60, false), Author: players[0].name}
	if raw, present := args["screen"]; present {
		for _, key := range []string{"load", "code", "svg", "template", "data"} {
			if _, incompatible := args[key]; incompatible {
				return nil, fieldError("screen", "cannot combine with %s", key)
			}
		}
		spec, err := readUIValue[declarativeScreen](raw, declarativeScreenSchema, "screen")
		if err != nil {
			return nil, err
		}
		a.SVG, err = spec.svg()
		if err != nil {
			return nil, err
		}
		if a.Title == "" {
			a.Title = shortText(spec.Title, 60, false)
		}
	}
	if _, present := args["template"]; present {
		for _, key := range []string{"load", "code", "svg"} {
			if _, incompatible := args[key]; incompatible {
				return nil, fieldError("template", "cannot combine with %s", key)
			}
		}
		data, hasData := args["data"]
		if hasData && data == nil {
			return nil, fieldError("data", "expected an object")
		}
		if err := a.setTemplate(str(args["template"]), data); err != nil {
			return nil, err
		}
	} else if _, present := args["data"]; present {
		return nil, fieldError("data", "requires a template")
	}
	var loaded string
	if name, provided := args["load"]; provided {
		if a.SVG != "" || a.Code != "" {
			return nil, problem(400, "Use load or svg/code, not both")
		}
		a, err = b.loadApp(ctx, str(name))
		if err != nil {
			return nil, err
		}
		loaded = a.Name
		if title := shortText(args["title"], 60, false); title != "" {
			a.Title = title
		}
	}
	save := str(args["save_as"])
	if _, present := args["save_as"]; present {
		if !slugRE.MatchString(save) {
			return nil, problem(400, "Invalid save_as slug")
		}
		a.Name = save
	}
	if a.Title == "" {
		a.Title = "Lark App"
	}
	if a.Name == "" {
		a.Name = truncate(companionSlug(a.Title), 48)
		if a.Name == "" {
			a.Name = "untitled"
		}
	}
	switch {
	case a.scripted():
		a.Kind = "app"
	case a.SVG != "":
		a.Kind = "drawing"
	default:
		return nil, problem(400, "Expected screen, template, svg, code, or load")
	}
	if err := a.setTiming(args); err != nil {
		return nil, err
	}
	if a.SVG != "" {
		clean, err := parseSVG(a.SVG)
		if err != nil {
			return nil, problem(400, err.Error())
		}
		a.SVG = svgString(clean)
		if a.scripted() {
			var geometry screenGeometry
			if slices.ContainsFunc(devices, func(d *wsTerminal) bool { return !d.virtual() }) {
				geometry = roundScreen
			}
			if _, err := buildScreen(a.SVG, false, geometry); err != nil {
				return nil, problem(400, err.Error())
			}
		}
	}
	memory := map[string]json.RawMessage{}
	var initial json.RawMessage
	if raw, present := args["initial_state"]; present {
		if !a.scripted() {
			return nil, problem(400, "initial_state requires a JS app")
		}
		initial, err = appJSONObject(raw, "initial_state")
		if err != nil {
			return nil, err
		}
	}
	if a.scripted() {
		values, err := b.memoryEntries(ctx, "app:"+a.Name)
		if err != nil {
			return nil, err
		}
		for _, value := range values {
			memory[str(value["key"])] = value["value"].(json.RawMessage)
		}
	}
	inst, screens, err := initApp(a, players, memory, devices, initial, buildScreen)
	if err != nil {
		h.mu.Lock()
		for i, d := range devices {
			if h.active(d) && d.appGen == gens[i] {
				d.lastAppError = appFailure(a, "launch", err, time.Now())
			}
		}
		h.mu.Unlock()
		return nil, problem(400, err.Error())
	}
	out := object{"ok": true, "companions": ids, "title": a.Title, "hit_ids": appHitIDs(screens...)}
	if inst.snapshot != nil {
		out["state"] = inst.snapshot
	}
	round := slices.ContainsFunc(devices, func(d *wsTerminal) bool { return d.screen.shape == "round" })
	if warnings := appLayoutWarnings(screens, round); len(warnings) > 0 {
		out["layout_warnings"] = warnings
	}
	h.mu.Lock()
	for i, d := range devices {
		if !h.active(d) {
			h.mu.Unlock()
			return nil, problem(409, "Terminal disconnected: "+d.id)
		}
		if r := h.parentRefresh(d.companionID, time.Now()); r != "" {
			h.mu.Unlock()
			return nil, r.err()
		}
		if d.appGen != gens[i] {
			h.mu.Unlock()
			return nil, problem(409, "Companion view changed: "+d.companionID)
		}
		if d.callState != callNone {
			h.mu.Unlock()
			return nil, problem(409, "Companion is on a call: "+d.companionID)
		}
		if menu != nil && !d.canLaunchMenuGame(menu) {
			h.mu.Unlock()
			return nil, problem(409, "Games menu changed")
		}
	}
	if loaded == a.Name {
		// Rendering runs outside the hub lock; the library may have changed.
		current, err := b.loadApp(ctx, loaded)
		if err != nil {
			h.mu.Unlock()
			return nil, err
		}
		if current.Version != a.Version {
			h.mu.Unlock()
			return nil, &versionConflict{current.Version}
		}
	}
	if save != "" {
		saved, err := b.writeApp(ctx, a)
		if err != nil {
			h.mu.Unlock()
			return nil, err
		}
		inst.spec = saved
		out["saved"] = saved.summary()
	}
	inst.ctx, inst.cancel = context.WithCancel(b.root)
	now := time.Now()
	if inst.hooks["tick"] != nil && a.TickMS != 0 && a.Template != "timer" {
		inst.remaining = a.MaxTicks
		inst.nextTick = now.Add(time.Duration(a.TickMS) * time.Millisecond)
	}
	for _, d := range devices {
		if d.app == nil {
			continue
		}
		old := d.app
		for _, other := range old.terminals {
			if other != nil {
				h.clearView(other)
				if !slices.Contains(devices, other) {
					h.render(other, now, false)
				}
			}
		}
	}
	for i, d := range devices {
		h.clearView(d)
		d.app, d.appView, d.agentSetScene = inst, screens[i], d.busy
		d.lastAppError = nil
		h.wake(d, now)
		h.render(d, now, false)
	}
	h.mu.Unlock()
	if save != "" {
		b.appsChanged()
	}
	for _, p := range players {
		text := "Started: " + a.Title
		if a.Kind == "drawing" {
			text = "Drew: " + a.Title
		}
		entry := newChat("card", p.name, text, "app")
		entry.SceneTitle = a.Title
		b.chat(p.id, entry, true)
	}
	if save != "" {
		out["saved_as"] = save
	}
	return out, nil
}

func appJSONObject(raw any, field string) (json.RawMessage, error) {
	if _, ok := raw.(object); !ok {
		return nil, fieldError(field, "expected a JSON object")
	}
	data, err := json.Marshal(raw)
	if err != nil || len(data) > 64<<10 {
		return nil, fieldError(field, "expected at most 64 KB of JSON")
	}
	return data, nil
}

func appHitIDs(screens ...svgScreen) []string {
	seen, hits := map[string]bool{}, []string{}
	for _, screen := range screens {
		for _, hit := range screen.hits {
			if !seen[hit.id] {
				seen[hit.id] = true
				hits = append(hits, hit.id)
			}
		}
	}
	return hits
}

func appLayoutWarnings(views []svgScreen, round bool) []string {
	seen := map[string]bool{}
	var warnings []string
	for _, screen := range views {
		for _, warning := range slices.Concat(screen.hitWarnings, layoutWarnings(screen.hits, round)) {
			if !seen[warning] {
				seen[warning] = true
				warnings = append(warnings, warning)
			}
		}
	}
	return warnings
}

func appFailure(spec savedApp, action string, err error, now time.Time) object {
	return object{"name": spec.Name, "title": spec.Title, "action": action, "error": truncate(err.Error(), 1024), "at": now.UTC()}
}

func (h *wsHub) activeAppState(a *appInstance) object {
	ids := []string{}
	for _, d := range a.terminals {
		if d != nil && h.active(d) && !slices.Contains(ids, d.companionID) {
			ids = append(ids, d.companionID)
		}
	}
	out := object{"name": a.spec.Name, "title": a.spec.Title, "kind": a.spec.Kind, "companions": ids, "ticks_left": a.remaining}
	if a.spec.Template != "" {
		out["template"] = a.spec.Template
	}
	if a.snapshot != nil {
		out["state"] = a.snapshot
	}
	return out
}

func (h *wsHub) patchRunningApp(ctx context.Context, ids []string, patch json.RawMessage) (object, error) {
	h.mu.Lock()
	var a *appInstance
	for _, id := range ids {
		terms := h.terminalsFor(id)
		if len(terms) == 0 || h.parentRefresh(id, time.Now()) != "" {
			h.mu.Unlock()
			return nil, problem(409, "No running JS app on "+id)
		}
		for _, d := range terms {
			if d.app == nil || d.app.vm == nil {
				h.mu.Unlock()
				return nil, problem(409, "No running JS app on "+id)
			}
			if a != nil && d.app != a {
				h.mu.Unlock()
				return nil, problem(409, "Companions must belong to the same running JS app")
			}
			a = d.app
		}
	}
	done := make(chan appPatchResult, 1)
	ev := appEvent{kind: "patch", i: -1, now: time.Now(), patch: patch, done: done}
	ok := h.updateApp(a, ev)
	h.mu.Unlock()
	if !ok {
		return nil, problem(409, "App event queue is full")
	}
	select {
	case result := <-done:
		return result.out, result.err
	case <-a.ctx.Done():
		return nil, problem(409, "App closed before applying state_patch")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *larkServer) appsChanged() {
	if b.ws == nil {
		return
	}
	b.ws.mu.Lock()
	defer b.ws.mu.Unlock()
	pkt := encodeEventMsg(b.ws.nextSeq(), eventSavedApps, "", "", "")
	for c := range b.ws.clients {
		if (c.kind == "web" || c.kind == "watcher") && c.scope == nil {
			c.enqueueProto(msgEvent, pkt)
		}
	}
}

func (h *wsHub) dropApp(d *wsTerminal) {
	if a := d.app; a != nil {
		for i, p := range a.terminals {
			if p == d {
				a.terminals[i] = nil
				break
			}
		}
		d.app, d.appView = nil, svgScreen{}
		if !slices.ContainsFunc(a.terminals, func(d *wsTerminal) bool { return d != nil }) {
			a.cancel()
		}
	}
}

func (h *wsHub) closeApp(a *appInstance, now time.Time) {
	for _, d := range a.terminals {
		if d != nil {
			h.clearView(d)
			h.render(d, now, false)
		}
	}
}

func (h *wsHub) tickApps(now time.Time) {
	seen := map[*appInstance]bool{}
	for _, d := range h.terminals {
		a := d.app
		if a == nil || seen[a] || a.remaining == 0 || a.tickQueued || now.Before(a.nextTick) || !h.active(d) {
			continue
		}
		seen[a] = true
		if h.updateApp(a, appEvent{kind: "tick", i: -1, now: now}) {
			a.remaining--
			a.nextTick = now.Add(time.Duration(a.spec.TickMS) * time.Millisecond)
		}
	}
}

func (h *wsHub) updateApp(a *appInstance, ev appEvent) bool {
	if ev.kind == "tap" && isExitNode(ev.id) {
		h.closeApp(a, ev.now)
		return true
	}
	if ev.kind != "patch" && a.hooks[ev.kind] == nil {
		return false
	}
	if ev.kind == "drag" {
		for i := len(a.events) - 1; i >= 0; i-- {
			prior := a.events[i]
			if prior.i != ev.i {
				continue
			}
			if prior.kind == "drag" {
				a.events = slices.Delete(a.events, i, i+1)
				break
			}
			if prior.kind == "up" || prior.kind == "tap" {
				break
			}
		}
	}
	if len(a.events) >= 32 {
		if ev.kind == "drag" {
			return false
		}
		i := slices.IndexFunc(a.events, func(p appEvent) bool { return p.kind == "drag" })
		if i < 0 {
			return false
		}
		a.events = slices.Delete(a.events, i, i+1)
	}
	a.events = append(a.events, ev)
	if ev.kind == "tick" {
		a.tickQueued = true
	}
	if !a.running {
		a.running = true
		go h.runAppEvents(a)
	}
	return true
}

func (h *wsHub) runAppEvents(a *appInstance) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for len(a.events) != 0 && !h.closed && a.ctx.Err() == nil {
		ev := a.events[0]
		a.events[0] = appEvent{}
		a.events = a.events[1:]
		if ev.i >= 0 && (a.terminals[ev.i] == nil || !h.active(a.terminals[ev.i])) {
			continue
		}
		var connected [8]bool
		for i, d := range a.terminals {
			connected[i] = d != nil && h.active(d)
		}
		h.mu.Unlock()
		if ev.kind == "patch" {
			views, snapshot, err := a.applyStatePatch(ev.patch, connected[:len(a.terminals)])
			h.mu.Lock()
			if err != nil {
				ev.done <- appPatchResult{err: problem(400, err.Error())}
			} else if !h.closed && a.ctx.Err() == nil {
				a.snapshot = snapshot
				h.publishAppViews(a, views)
				active := h.activeAppState(a)
				out := object{"ok": true, "companions": active["companions"], "title": a.spec.Title, "state": snapshot, "hit_ids": appHitIDs(views...)}
				if warnings := appLayoutWarnings(views, slices.Contains(a.geometry, roundScreen)); len(warnings) > 0 {
					out["layout_warnings"] = warnings
				}
				ev.done <- appPatchResult{out: out}
			} else {
				ev.done <- appPatchResult{err: problem(409, "App closed before applying state_patch")}
			}
			continue
		}
		views, err := a.update(ev, connected[:len(a.terminals)])
		var snapshot json.RawMessage
		if err == nil && views != nil {
			snapshot, err = a.stateJSON()
		}
		if err == nil {
			err = h.storeAppEffects(a)
		}
		h.mu.Lock()
		if ev.kind == "tick" {
			a.tickQueued = false
		}
		if !h.closed && a.ctx.Err() == nil {
			if snapshot != nil {
				a.snapshot = snapshot
			}
			h.commitAppEvent(a, ev, views, err)
		}
	}
	a.running, a.events = false, nil
}

func (h *wsHub) storeAppEffects(a *appInstance) error {
	effects := a.effects
	if i := slices.IndexFunc(effects, func(e appEffect) bool { return e.kind == "exit" }); i >= 0 {
		effects = effects[:i]
	}
	if a.ctx.Err() != nil || !slices.ContainsFunc(effects, func(e appEffect) bool { return e.kind == "store" }) {
		return nil
	}
	if h.b.db == nil {
		return errors.New("App storage unavailable")
	}
	ctx, cancel := context.WithTimeout(a.ctx, 200*time.Millisecond)
	defer cancel()
	tx, err := h.b.db.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("Lark could not open storage for app %q: %v", a.spec.Name, err)
		return errors.New("Could not save app storage")
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	for _, e := range effects {
		if e.kind != "store" {
			continue
		}
		edit := memoryEdit{memoryWrite: memoryWrite{a.scope, e.text, string(e.value)}}
		if _, err := editMemory(ctx, tx, edit, now); err != nil {
			log.Printf("Lark could not persist app %q key %q: %v", a.spec.Name, e.text, err)
			return fmt.Errorf("Could not save app key %q", e.text)
		}
	}
	if err := tx.Commit(); err != nil {
		log.Printf("Lark could not commit storage for app %q: %v", a.spec.Name, err)
		return errors.New("Could not save app storage")
	}
	for _, e := range effects {
		if e.kind == "store" {
			a.memory[e.text] = e.value
		}
	}
	return nil
}

func (h *wsHub) commitAppEvent(a *appInstance, ev appEvent, views []svgScreen, err error) {
	now := ev.now
	if err != nil {
		for _, d := range a.terminals {
			if d != nil && h.active(d) {
				d.lastAppError = appFailure(a.spec, ev.kind, err, now)
				h.setCompanionStatus(d.companionID, statusError, 0, promptNone, err.Error())
			}
		}
		h.closeApp(a, now)
		return
	}
	effects := a.effects
	a.effects = nil
	for _, e := range effects {
		if e.kind == "exit" {
			h.closeApp(a, now)
			return
		}
		if e.kind == "store" {
			continue
		}
		switch e.kind {
		case "start_ticks":
			a.remaining = a.spec.MaxTicks
			a.nextTick = time.Now().Add(time.Duration(a.spec.TickMS) * time.Millisecond)
			continue
		case "stop_ticks":
			a.remaining = 0
			continue
		}
		if e.kind == "say" {
			if now.Sub(a.timeOfSpeech) < 3*time.Second {
				continue
			}
			a.timeOfSpeech = now
		}
		for j, d := range a.terminals {
			if d == nil || !h.active(d) || ev.i >= 0 && j != ev.i && !e.all || (d.companionID != "" && h.parentRefresh(d.companionID, time.Now()) != "") {
				continue
			}
			if h.voiceOwner(d.companionID) != d {
				continue
			}
			switch e.kind {
			case "chime":
				h.chime(d, e.text)
			case "pcm":
				h.playPCM(d, e.pcm)
			case "say":
				h.speakLocal(d, e.text)
			}
		}
	}
	h.publishAppViews(a, views)
}

func (h *wsHub) publishAppViews(a *appInstance, views []svgScreen) {
	for j, screen := range views {
		if d := a.terminals[j]; d != nil && h.active(d) && screen.source != "" && d.appView.raw != screen.raw {
			d.appView = screen
			h.render(d, time.Now(), false)
		}
	}
}

func withCompanion(args object, id string) object {
	out := maps.Clone(args)
	out["companion_id"] = id
	return out
}
