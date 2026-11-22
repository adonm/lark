package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var parentControls = template.Must(template.ParseFS(webFiles, "web/parent-controls.html"))

func parentFragmentRequest(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true" && r.Header.Get("Accept") == "text/html"
}

func parentForm(r *http.Request) (object, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, 2048)
	if err := r.ParseForm(); err != nil {
		return nil, problem(400, "Invalid parent settings form")
	}
	fields := r.PostForm
	args := object{}
	for key, values := range fields {
		if len(values) != 1 {
			return nil, problem(400, "Invalid parent settings form")
		}
		switch key {
		case "timezone":
			args[key] = strings.TrimSpace(values[0])
		case "minutes", "daily_minutes":
			args[key] = json.Number(values[0])
		default:
			args[key] = values[0]
		}
	}
	return args, nil
}

type parentUsageView struct {
	Settings                        parentSettings
	Status, Variant, Summary, Until string
	History                         string
	Used, Limit, High               int
	Activity                        object
}

func parentUsage(row object, now time.Time) parentUsageView {
	s := row["settings"].(parentSettings)
	secs := row["used_seconds"].(int)
	reason := row["reason"].(parentReason)
	view := parentUsageView{Settings: s, Status: "Available", Variant: "success", Activity: row["recent"].(object),
		History: "/app?companion_id=" + url.QueryEscape(str(row["companion_id"])) + "#chat"}
	switch reason {
	case parentQuiet:
		view.Status, view.Variant = "Quiet hours", "warning"
	case parentLimit:
		view.Status, view.Variant = "Daily time used", "warning"
	case parentPause:
		view.Status, view.Variant = "Paused", "warning"
	case parentUnavailable:
		view.Status, view.Variant = "Controls unavailable", "danger"
	default:
		if s.OverrideUntil > now.UnixMilli() {
			view.Status, view.Variant = "Override active", "warning"
		}
	}
	view.Summary = fmt.Sprintf("%d min %d sec used", secs/60, secs%60)
	if s.DailyMinutes > 0 {
		view.Limit = s.DailyMinutes * 60
		view.High = view.Limit * 9 / 10
		view.Used = min(secs, view.Limit)
		view.Summary += fmt.Sprintf(" of %d min", s.DailyMinutes)
	} else {
		view.Summary += " · No daily limit"
	}
	view.Summary += fmt.Sprintf(" · %s (%s)", row["day"], s.Timezone)
	until, label := s.OverrideUntil, "Overridden"
	if s.PauseUntil > now.UnixMilli() {
		until, label = s.PauseUntil, "Paused"
	}
	if until > now.UnixMilli() {
		if loc, err := time.LoadLocation(s.Timezone); err == nil {
			view.Until = fmt.Sprintf("%s until %s (%s).", label, time.UnixMilli(until).In(loc).Format("2 Jan 2006, 15:04"), s.Timezone)
		}
	}
	return view
}

func (b *larkServer) routeParent(w http.ResponseWriter, r *http.Request, id string) (any, error) {
	value, err := b.parentAPI(r, id)
	if err != nil || !parentFragmentRequest(r) {
		return value, err
	}
	overview := value.(object)
	if r.Method == http.MethodPost {
		overview, err = b.parentOverview(r.Context())
		if err != nil {
			return nil, err
		}
	} else {
		id = r.URL.Query().Get("companion_id")
	}
	rows := overview["companions"].([]object)
	var selected object
	if len(rows) > 0 {
		selected = rows[0]
		for _, row := range rows {
			if row["companion_id"] == id {
				selected = row
				break
			}
		}
		id = str(selected["companion_id"])
	}
	data := struct {
		Devices  []object
		Selected string
		Usage    *parentUsageView
	}{Devices: rows, Selected: id}
	if selected != nil {
		view := parentUsage(selected, time.Now())
		data.Usage = &view
	}
	var html bytes.Buffer
	if err := parentControls.Execute(&html, data); err != nil {
		return nil, err
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; form-action 'none'; base-uri 'none'")
	w.Header().Set("Vary", "HX-Request, Accept")
	sendBytes(w, 200, "text/html; charset=utf-8", html.Bytes())
	return nil, nil
}
