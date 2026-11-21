//go:build live

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const e2eDedicated = "http://127.0.0.1:49375"
const e2ePhotoPrompt = "Show the synthetic beach photos"
const e2eModelReply = "The sky looks blue because air scatters sunlight."

type e2eModel struct {
	url, alias  string
	client      *http.Client
	api         upstream
	settings    ocSettings
	permission  []ocPermission
	mu          sync.Mutex
	sessions    map[string]*e2eSession
	prompts     atomic.Int32
	mcpRequests atomic.Int32
	mcpCalls    atomic.Int32
	templates   []string
	mcpAudit    []string
	photo       map[string]bool
	build       chan struct{}
	followUp    chan struct{}
	events      chan ocEvent
	cost        float64
	usage       map[ocModel]int
	tools       map[string]int
	removed     int
}

type e2eSession struct {
	state object
	rows  []object
	wait  chan struct{}
}

func startE2EModel(t *testing.T, s *e2eSuite, home string) *e2eModel {
	t.Helper()
	m := &e2eModel{client: s.client, settings: openCodeSettings("echoear"), sessions: map[string]*e2eSession{},
		photo: map[string]bool{}, build: make(chan struct{}), followUp: make(chan struct{}),
		events: make(chan ocEvent, 64), usage: map[ocModel]int{}, tools: map[string]int{}}
	m.settings.Model = ocModel{Provider: "test", ID: "deterministic"}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { m.stub(w, r, s) })
	if s.real {
		pass := env("ECHOEAR_OPENCODE_PASSWORD", e2eInstallation(t, home, true)["ECHOEAR_OPENCODE_PASSWORD"])
		if pass == "" {
			t.Fatal("dedicated OpenCode password is missing")
		}
		var err error
		m.api, err = openCodeUpstream(s.client, "", e2eDedicated, pass)
		if err != nil {
			t.Fatal("cannot configure the dedicated OpenCode upstream")
		}
		m.alias = "echoear_e2e_" + randomHex(5)
		m.permission = []ocPermission{{"*", "*", "deny"}, {"execute", "*", "allow"},
			{m.alias + "_*", "*", "allow"}}
		q := e2eLocation(s.workspace)
		if err := m.api.jsonCall(t.Context(), "GET", "/api/location"+q, nil, nil); err != nil {
			t.Fatal("cannot initialize the disposable OpenCode location:", err)
		}
		until := time.Now().Add(8 * time.Second)
		for {
			configured, err := ocData[struct {
				ID    string
				Model ocModel
			}](t.Context(), m.api, "GET", "/api/agent/echoear"+q, nil)
			if err == nil && configured.ID == "echoear" && configured.Model.Provider != "" && configured.Model.ID != "" {
				m.settings.Model = configured.Model
				break
			}
			if time.Now().After(until) {
				t.Fatal("cannot read agents.echoear.model in the dedicated OpenCode location")
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Logf("dedicated model: %s/%s (%s); at most eight model prompts", m.settings.Model.Provider, m.settings.Model.ID, m.settings.Model.Variant)
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { m.real(w, r, s) })
		t.Cleanup(func() { m.cleanup(t) })
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	m.url = server.URL
	return m
}

func e2eLocation(workspace string) string {
	return "?" + url.Values{"location[directory]": {workspace}}.Encode()
}

func (m *e2eModel) connectMCP(t *testing.T, s *e2eSuite) {
	t.Helper()
	q := e2eLocation(s.workspace)
	route := "/api/experimental/mcp/" + m.alias
	// The dedicated server disallows project files. Register an in-memory,
	// unique MCP in this test location only; never overwrite its normal MCP.
	config := object{"config": object{"type": "remote", "url": m.url + "/mcp-test", "oauth": false,
		"headers": object{"X-EchoEar-Token": s.token}}}
	if err := m.api.jsonCall(t.Context(), "PUT", route+q, config, nil); err != nil {
		t.Fatal("cannot register isolated OpenCode MCP:", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := m.api.jsonCall(ctx, "DELETE", route+q, nil, nil); err != nil {
			t.Error("could not remove the temporary OpenCode MCP")
		}
	})
	until := time.Now().Add(8 * time.Second)
	for {
		rows, err := ocData[[]struct {
			Name   string
			Status struct{ Status string }
		}](t.Context(), m.api, "GET", "/api/mcp"+q, nil)
		if err == nil && m.mcpRequests.Load() > 0 && slices.ContainsFunc(rows, func(row struct {
			Name   string
			Status struct{ Status string }
		}) bool {
			return row.Name == m.alias && row.Status.Status == "connected"
		}) {
			return
		}
		if time.Now().After(until) {
			t.Fatal("isolated OpenCode MCP did not connect")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (m *e2eModel) cleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var ids []string
	m.mu.Lock()
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		path := "/api/session/" + id
		m.readUsage(ctx, id)
		_ = m.api.jsonCall(ctx, "POST", path+"/interrupt", nil, nil)
		if err := m.api.jsonCall(ctx, "DELETE", path, nil, nil); err != nil {
			t.Errorf("cannot delete an owned OpenCode session: %v", err)
		} else {
			m.removed++
		}
	}
	t.Logf("OpenCode: %d prompts, %d owned sessions deleted, reported message cost $%.6f (%v); %d isolated MCP calls, tools %v",
		m.prompts.Load(), m.removed, m.cost, m.usage, m.mcpCalls.Load(), m.tools)
}

func (m *e2eModel) readUsage(ctx context.Context, id string) {
	messages, err := ocData[[]struct {
		Model   ocModel
		Cost    float64
		Content []struct {
			Type, Name string
			State      struct{ Status string }
		}
	}](ctx, m.api, "GET", "/api/session/"+id+"/message?type=assistant&limit=50", nil)
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, message := range messages {
		m.cost += message.Cost
		m.usage[message.Model]++
		for _, part := range message.Content {
			if part.Type == "tool" {
				m.tools[part.Name+":"+part.State.Status]++
			}
		}
	}
}

func (m *e2eModel) real(w http.ResponseWriter, r *http.Request, s *e2eSuite) {
	path, method := r.URL.Path, r.Method
	if path == "/mcp-test" {
		m.relayMCP(w, r, s)
		return
	}
	if path == "/api/session" && method == "GET" {
		// No installed session can be discovered by this bridge's history importer.
		writeStubJSON(w, []object{})
		return
	}
	var body []byte
	writeInput := func(in object) { body, _ = json.Marshal(in) }
	readInput := func() object {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 256<<10))
		if err != nil {
			return nil
		}
		in, _ := decodeObject(raw)
		return in
	}
	create := path == "/api/session" && method == "POST"
	query := r.URL.Query()
	switch {
	case path == "/api/info" && method == "GET":
	case path == "/api/experimental/mcp/echoear/connect" && method == "POST":
		// The bridge may reconnect only this fixture's MCP in its own workspace.
		path = "/api/experimental/mcp/" + m.alias + "/connect"
		query.Set("location[directory]", s.workspace)
	case path == "/api/mcp" && method == "GET":
		rows, err := ocData[[]struct {
			Name   string
			Status struct{ Status string }
		}](r.Context(), m.api, "GET", "/api/mcp"+e2eLocation(s.workspace), nil)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		for _, row := range rows {
			if row.Name == m.alias {
				writeStubJSON(w, mcpServerStatus("echoear", row.Status.Status))
				return
			}
		}
		writeStubJSON(w, []object{})
		return
	case (path == "/api/agent/echoear" || path == "/api/agent/"+builderAgent || path == "/api/event") && method == "GET":
		query.Set("location[directory]", s.workspace)
	case create:
		in := readInput()
		location, _ := in["location"].(object)
		title, agent, id := str(in["title"]), str(in["agent"]), str(in["id"])
		foreground := agent == "echoear" && strings.HasPrefix(title, "EchoEar: "+s.prefix+" ") && id == ""
		builder := agent == builderAgent && title == builderTitle && strings.HasPrefix(id, builderSessionPrefix) && sessionIDRE.MatchString(id)
		if location["directory"] != s.workspace || !foreground && !builder {
			w.WriteHeader(403)
			return
		}
		if foreground {
			in["permissions"] = m.permission
		}
		writeInput(in)
	default:
		match := e2eSessionRoute.FindStringSubmatch(path)
		if len(match) == 0 {
			w.WriteHeader(404)
			return
		}
		id, action := match[1], match[2]
		m.mu.Lock()
		owned := m.sessions[id] != nil
		m.mu.Unlock()
		if !owned {
			w.WriteHeader(404)
			return
		}
		switch method + " " + action {
		case "GET ", "GET /message", "POST /wait", "POST /interrupt", "DELETE ":
		case "POST /prompt":
			in := readInput()
			text := str(in["text"])
			if len(text) == 0 || len(text) > 16000 || m.prompts.Add(1) > 8 {
				w.WriteHeader(429)
				return
			}
			m.rememberPhoto(text)
			writeInput(in)
		case "PATCH ":
			// The bridge reasserts its normal wider permissions on reused sessions.
			// Never forward that change to the dedicated server.
			w.WriteHeader(204)
			return
		default:
			w.WriteHeader(404)
			return
		}
	}
	dst, _ := m.api.config.get(false)
	if qs := query.Encode(); qs != "" {
		path += "?" + qs
	}
	// Admission may commit after the bridge abandons its request. Keep creation
	// alive briefly so its returned ID is still registered for cleanup.
	ctx := r.Context()
	if create {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
		defer cancel()
	}
	req, _ := http.NewRequestWithContext(ctx, method, dst.base+path, bytes.NewReader(body))
	req.Header.Set("Authorization", dst.auth)
	req.Header.Set("Accept", r.Header.Get("Accept"))
	if body != nil {
		req.Header.Set("Content-Type", jsonType)
	}
	if method == "DELETE" {
		m.readUsage(ctx, strings.TrimPrefix(r.URL.Path, "/api/session/"))
	}
	res, err := m.client.Do(req)
	if err != nil {
		w.WriteHeader(502)
		return
	}
	defer res.Body.Close()
	if create && res.StatusCode >= 200 && res.StatusCode < 300 {
		raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		var payload struct {
			Data struct {
				ID          string
				Location    struct{ Directory string }
				Permissions []ocPermission
			}
		}
		if err != nil || json.Unmarshal(raw, &payload) != nil || !sessionIDRE.MatchString(payload.Data.ID) {
			w.WriteHeader(502)
			return
		}
		m.mu.Lock()
		m.sessions[payload.Data.ID] = &e2eSession{}
		m.mu.Unlock()
		if payload.Data.Location.Directory != s.workspace ||
			(!slices.Equal(payload.Data.Permissions, m.permission) &&
				!slices.Equal(payload.Data.Permissions, []ocPermission{{"*", "*", "deny"}})) {
			w.WriteHeader(502)
			return
		}
		w.Header().Set("Content-Type", jsonType)
		w.WriteHeader(res.StatusCode)
		_, _ = w.Write(raw)
		return
	}
	if method == "DELETE" && res.StatusCode >= 200 && res.StatusCode < 300 {
		m.mu.Lock()
		delete(m.sessions, strings.TrimPrefix(r.URL.Path, "/api/session/"))
		m.removed++
		m.mu.Unlock()
	}
	w.Header().Set("Content-Type", res.Header.Get("Content-Type"))
	w.WriteHeader(res.StatusCode)
	if strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		_ = http.NewResponseController(w).Flush()
		buf := make([]byte, 8192)
		for {
			n, err := res.Body.Read(buf)
			if _, writeErr := w.Write(buf[:n]); writeErr != nil || err != nil {
				break
			}
			_ = http.NewResponseController(w).Flush()
		}
	} else {
		_, _ = io.Copy(w, res.Body)
	}
}

var e2eSessionRoute = regexp.MustCompile(`^/api/(?:experimental/)?session/(ses[-_][A-Za-z0-9_-]{1,120})(/message|/wait|/prompt|/interrupt)?$`)
var e2ePhotoAccess = regexp.MustCompile(` · local photos access: ([a-zA-Z0-9_-]+)\]`)

func (m *e2eModel) rememberPhoto(text string) string {
	if !strings.Contains(text, e2ePhotoPrompt) {
		return ""
	}
	match := e2ePhotoAccess.FindStringSubmatch(text)
	if cat := e2ePhotoDevice.FindStringSubmatch(text); len(cat) != 0 {
		m.mu.Lock()
		m.photo[cat[1]] = len(match) > 0
		m.mu.Unlock()
	}
	if len(match) != 0 {
		return match[1]
	}
	return ""
}

func (m *e2eModel) relayMCP(w http.ResponseWriter, r *http.Request, s *e2eSuite) {
	if !tokenMatches(r.Header.Values("X-EchoEar-Token"), s.token) || r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 512<<10))
	in, _ := decodeObject(raw)
	if err != nil || in == nil {
		w.WriteHeader(400)
		return
	}
	m.mcpRequests.Add(1)
	p, _ := in["params"].(object)
	calling := in["method"] == "tools/call"
	if calling {
		m.mcpCalls.Add(1)
	}
	if calling && p["name"] != "state" && p["name"] != "app" {
		m.auditMCP("isolated MCP rejected an unapproved tool")
		w.WriteHeader(403)
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), "POST", s.base+"/mcp", bytes.NewReader(raw))
	req.Header.Set("X-EchoEar-Token", s.token)
	req.Header.Set("Content-Type", jsonType)
	req.Header.Set("Accept", r.Header.Get("Accept"))
	res, err := m.client.Do(req)
	if err != nil {
		w.WriteHeader(502)
		return
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if calling {
		envelope, _ := decodeObject(out)
		result, _ := envelope["result"].(object)
		value, _ := result["structuredContent"].(object)
		args, _ := p["arguments"].(object)
		template := str(args["template"])
		m.auditMCP(fmt.Sprintf("%s(%s) HTTP %d error=%v: %s", p["name"], e2eRedact(template), res.StatusCode,
			result["isError"], e2eRedact(str(value["error"]))))
		if p["name"] == "app" && res.StatusCode == 200 && result != nil && result["isError"] != true &&
			value["ok"] == true && value["layout_warnings"] == nil {
			m.mu.Lock()
			m.templates = append(m.templates, template)
			m.mu.Unlock()
		}
	}
	w.Header().Set("Content-Type", res.Header.Get("Content-Type"))
	w.WriteHeader(res.StatusCode)
	_, _ = w.Write(out)
}

var e2eSecrets = regexp.MustCompile(`[[:alnum:]_.+-]{24,}`)

func e2eRedact(s string) string { return truncate(e2eSecrets.ReplaceAllString(s, "[redacted]"), 160) }

func (m *e2eModel) auditMCP(note string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.mcpAudit) < 32 {
		m.mcpAudit = append(m.mcpAudit, note)
	}
}

func (m *e2eModel) stub(w http.ResponseWriter, r *http.Request, s *e2eSuite) {
	switch r.Method + " " + r.URL.Path {
	case "GET /api/info":
		writeStubJSON(w, object{})
		return
	case "POST /api/experimental/mcp/echoear/connect":
		w.WriteHeader(204)
		return
	case "GET /api/mcp":
		writeStubJSON(w, mcpServerStatus("echoear", "connected"))
		return
	case "GET /api/event":
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		_, _ = io.WriteString(w, ": connected\n\n")
		f.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case event := <-m.events:
				data, _ := json.Marshal(event)
				_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
				f.Flush()
			}
		}
	case "GET /api/session":
		writeStubJSON(w, []object{})
		return
	case "GET /api/agent/echoear":
		writeStubJSON(w, object{"id": "echoear", "model": m.settings.Model})
		return
	case "GET /api/agent/" + builderAgent:
		writeStubJSON(w, object{"id": builderAgent})
		return
	case "POST /api/session":
		var in object
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			w.WriteHeader(400)
			return
		}
		id := str(in["id"])
		if id == "" {
			id = "ses-e2e-" + randomHex(6)
		}
		in["id"] = id
		m.mu.Lock()
		m.sessions[id] = &e2eSession{state: in}
		m.mu.Unlock()
		writeStubJSON(w, in)
		return
	}
	match := e2eSessionRoute.FindStringSubmatch(r.URL.Path)
	if len(match) == 0 {
		w.WriteHeader(404)
		return
	}
	id, action := match[1], match[2]
	m.mu.Lock()
	session := m.sessions[id]
	if session == nil {
		m.mu.Unlock()
		w.WriteHeader(404)
		return
	}
	switch r.Method + " " + action {
	case "GET ":
		state := session.state
		m.mu.Unlock()
		writeStubJSON(w, state)
	case "DELETE ":
		delete(m.sessions, id)
		m.mu.Unlock()
		w.WriteHeader(204)
	case "POST /interrupt", "PATCH ":
		m.mu.Unlock()
		w.WriteHeader(204)
	case "POST /prompt":
		var in struct{ Text string }
		err := json.NewDecoder(r.Body).Decode(&in)
		if err != nil || in.Text == "" {
			m.mu.Unlock()
			w.WriteHeader(400)
			return
		}
		n := m.prompts.Add(1)
		key := fmt.Sprintf("e2e-%d", n)
		session.rows = append(session.rows, object{"id": "user-" + key, "type": "user", "text": in.Text})
		session.wait = make(chan struct{})
		done := session.wait
		m.mu.Unlock()
		// Respond to admission before doing tool work, matching the real API.
		writeStubJSON(w, object{"id": "user-" + key})
		go m.finishStub(s, session, id, key, in.Text, done)
	case "POST /wait":
		done := session.wait
		m.mu.Unlock()
		select {
		case <-done:
			w.WriteHeader(204)
		case <-r.Context().Done():
		}
	case "GET /message":
		for _, row := range session.rows {
			if row["type"] == "assistant" && row["time"] == nil {
				// The adapter has admitted/persisted the user before reading the
				// final projection. Make that ordering unambiguous in this stub.
				row["time"] = object{"created": time.Now().Add(time.Millisecond).UnixMilli()}
			}
		}
		rows := slices.Clone(session.rows)
		m.mu.Unlock()
		if kind := r.URL.Query().Get("type"); kind != "" {
			rows = slices.DeleteFunc(rows, func(row object) bool { return row["type"] != kind })
		}
		if r.URL.Query().Get("order") == "desc" {
			slices.Reverse(rows)
		}
		if n := e2eNumber(r.URL.Query().Get("limit")); n > 0 && n < len(rows) {
			rows = rows[:n]
		}
		writeStubJSON(w, rows)
	default:
		m.mu.Unlock()
		w.WriteHeader(404)
	}
}

func (m *e2eModel) finishStub(s *e2eSuite, session *e2eSession, id, key, text string, done chan struct{}) {
	reply := e2eModelReply
	switch {
	case session.state["agent"] == builderAgent:
		<-m.build
		reply = quizProposal
	case strings.Contains(text, "Virtual test hold"):
		// The test cancels this through the actual WS gesture while /wait is blocked.
		return
	case strings.Contains(text, "Virtual test queued follow-up"):
		<-m.followUp
	case strings.Contains(text, e2ePhotoPrompt):
		if token := m.rememberPhoto(text); token != "" {
			match := e2ePhotoDevice.FindStringSubmatch(text)
			if len(match) == 0 || !m.stubPhotos(s, match[1], token) {
				reply = "I could not open the synthetic photos."
			}
		}
	}
	m.mu.Lock()
	session.rows = append(session.rows, object{"id": "assistant-" + key, "type": "assistant", "agent": session.state["agent"], "model": m.settings.Model,
		"finish": "stop", "content": []object{{"type": "text", "text": reply}}})
	m.mu.Unlock()
	close(done)
	if session.state["agent"] != builderAgent {
		m.events <- ocEvent{Type: "session.text.delta", Data: ocEventData{
			SessionID: id, AssistantMessageID: "assistant-" + key, Delta: reply + " ",
		}}
	}
	m.events <- ocEvent{Type: "session.execution.succeeded", Data: ocEventData{SessionID: id}}
}

var e2ePhotoDevice = regexp.MustCompile(`\(([a-z0-9-]+)\) ·`)

func (m *e2eModel) stubPhotos(s *e2eSuite, id, token string) bool {
	call := func(name string, args object) object {
		raw, _ := json.Marshal(object{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": object{"name": name, "arguments": args}})
		req, _ := http.NewRequest("POST", s.base+"/mcp", bytes.NewReader(raw))
		req.Header.Set("X-EchoEar-Token", s.token)
		req.Header.Set("Content-Type", jsonType)
		res, err := m.client.Do(req)
		if err != nil {
			return nil
		}
		defer res.Body.Close()
		var v object
		_ = json.NewDecoder(res.Body).Decode(&v)
		r, _ := v["result"].(map[string]any)
		out, _ := r["structuredContent"].(map[string]any)
		return out
	}
	found := call("state", object{"device_id": id, "photos": object{"query": "beach", "access": token}})
	photos, _ := found["photos"].(map[string]any)
	items, _ := photos["items"].([]any)
	var handles []string
	for _, item := range items {
		value, _ := item.(map[string]any)
		handles = append(handles, str(value["handle"]))
	}
	return len(handles) == 2 && call("app", object{"devices": id, "template": "slideshow", "data": object{"handles": handles, "access": token}})["ok"] == true
}
