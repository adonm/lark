package main

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"hash/fnv"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const turnTimeout = 90 * time.Second

// contractWait bounds how long a turn waits for the background MCP catalog
// refresh after a restart or a prompt/deploy change.
const contractWait = 30 * time.Second

type fixedEndpoint endpoint

func (e fixedEndpoint) get(bool) (endpoint, error) { return endpoint(e), nil }

func openCodeUpstream(client *http.Client, state, base, password string) (upstream, error) {
	if base == "" {
		return upstream{"OpenCode", client, &fileCache[endpoint]{
			path: filepath.Join(state, "opencode/service.json"), read: openCodeEndpoint,
		}}, nil
	}
	dst, err := localOpenCodeEndpoint(base, password)
	return upstream{"OpenCode", client, fixedEndpoint(dst)}, err
}

func openCodeEndpoint(raw []byte) (endpoint, error) {
	var info struct{ URL, Password string }
	if json.Unmarshal(raw, &info) != nil || info.Password == "" {
		return endpoint{}, errors.New("invalid OpenCode service configuration")
	}
	return localOpenCodeEndpoint(info.URL, info.Password)
}

func localOpenCodeEndpoint(base, password string) (endpoint, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" || u.User != nil || u.ForceQuery || u.RawQuery != "" ||
		u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return endpoint{}, errors.New("invalid OpenCode service URL")
	}
	host := "127.0.0.1"
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "0.0.0.0":
	case "::", "::1":
		host = "::1"
	default:
		return endpoint{}, errors.New("not a local OpenCode service URL")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return endpoint{}, errors.New("invalid OpenCode service port")
	}
	dst := endpoint{base: "http://" + net.JoinHostPort(host, u.Port())}
	if password != "" {
		dst.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte("opencode:"+password))
	}
	return dst, nil
}

type ocModel struct {
	Provider string `json:"providerID"`
	ID       string `json:"id"`
	Variant  string `json:"variant,omitempty"`
}

var defaultSmartModel = ocModel{Provider: "opencode", ID: "gemini-4-hydrogen", Variant: "low"}

func parseModelSpec(spec string) ocModel {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return ocModel{}
	}
	prov, rest, ok := strings.Cut(spec, "/")
	if !ok || prov == "" || rest == "" {
		return ocModel{}
	}
	id, variant, _ := strings.Cut(rest, "#")
	if id == "" {
		return ocModel{}
	}
	return ocModel{Provider: prov, ID: id, Variant: variant}
}

type ocPermission struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

type ocSettings struct {
	Agent       string         `json:"agent"`
	Model       ocModel        `json:"model"`
	Permissions []ocPermission `json:"permissions"`
}

type openCode struct {
	upstream
	feed     eventFeed
	settings ocSettings
	project  string
	db       *larkDB
	contract string
	store    *sessionStore
	turns    [64]chan struct{}

	mu           sync.Mutex
	sig          condSignal
	active       map[string]context.CancelFunc
	activeSource map[string]string
	interrupts   map[string]bool
	resets       map[string]*ocReset
	verified     map[string]string
	defaultModel ocModel
	sessionModel map[string]ocModel
	flushHistory func(string)
	contractWait time.Duration
}

type ocReset struct {
	count int
	done  chan struct{}
}

func newOpenCode(ctx context.Context, u upstream, db *larkDB, project string, settings ocSettings) *openCode {
	o := &openCode{upstream: u, project: project, settings: settings, db: db,
		store:  &sessionStore{db: db.DB, readDB: db.readDB()},
		active: make(map[string]context.CancelFunc), activeSource: make(map[string]string),
		interrupts: make(map[string]bool), resets: make(map[string]*ocReset),
		verified:     make(map[string]string),
		sessionModel: make(map[string]ocModel),
		contractWait: contractWait,
		feed:         eventFeed{bySession: make(map[string]map[chan ocEvent]struct{})}}
	o.feed.onConfigChange = func() { o.invalidateVerified() }
	for i := range o.turns {
		o.turns[i] = make(chan struct{}, 1)
	}
	go o.feed.run(ctx, u)
	return o
}

func (o *openCode) get(companionID string) (string, error) {
	sid, _, err := o.store.get(companionID)
	return sid, err
}

func (o *openCode) invalidateVerified(devices ...string) {
	o.mu.Lock()
	if len(devices) == 0 {
		clear(o.verified)
	} else {
		for _, dev := range devices {
			delete(o.verified, dev)
		}
	}
	o.mu.Unlock()
}

func (o *openCode) prewarm(ctx context.Context, companionID, character string) {
	if o == nil || companionID == "" {
		return
	}
	o.mu.Lock()
	cached := o.verified[companionID]
	o.mu.Unlock()
	if cached != "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	binding, err := o.readBinding(ctx, companionID)
	if err != nil || binding.sid == "" {
		return
	}
	unlock, err := o.lock(ctx, companionID)
	if err != nil {
		return
	}
	defer unlock()
	_, _ = o.ensureSession(ctx, companionID, character, false)
}

func (o *openCode) forget(companionID string) error {
	o.invalidateVerified(companionID)
	err := o.store.delete(companionID)
	o.sig.notify()
	return err
}

func (o *openCode) lock(ctx context.Context, companionID string) (func(), error) {
	return o.lockGate(ctx, o.turnGate(companionID))
}

func (o *openCode) turnGate(companionID string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(companionID))
	return int(h.Sum32() % uint32(len(o.turns)))
}

func (o *openCode) lockGate(ctx context.Context, index int) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, o.unavailable(err)
	}
	gate := o.turns[index]
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, o.unavailable(ctx.Err())
	}
}

func (o *openCode) interrupt(ctx context.Context, sid string) error {
	if sid == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	err := o.jsonCall(ctx, http.MethodPost, "/api/session/"+sid+"/interrupt", nil, nil)
	var status *upstreamStatus
	if errors.As(err, &status) && status.code == http.StatusNotFound {
		return nil
	}
	return err // The API reports an idle no-op as success.
}

func (o *openCode) requestInterrupt(companionID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.resets[companionID] != nil {
		return false // The mapping change already owns the interrupt for the old session.
	}
	return o.markInterrupted(companionID)
}

func (o *openCode) markInterrupted(companionID string) bool {
	if cancel := o.active[companionID]; cancel != nil {
		cancel()
	}
	queued := o.interrupts[companionID]
	o.interrupts[companionID] = true
	return !queued
}

// Hold the turn gate through the interrupt. A failed interrupt stays pending
// so neither a replacement prompt nor a reset can race the old execution.
func (o *openCode) flushInterrupt(ctx context.Context, companionID string) error {
	o.mu.Lock()
	pending := o.interrupts[companionID]
	o.mu.Unlock()
	if !pending {
		return nil
	}
	sid, err := o.get(companionID)
	if err == nil {
		err = o.interrupt(ctx, sid)
	}
	if err == nil {
		o.mu.Lock()
		delete(o.interrupts, companionID)
		o.mu.Unlock()
	}
	return err
}

func (o *openCode) interruptDevice(companionID string) {
	if !o.requestInterrupt(companionID) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), turnTimeout)
		defer cancel()
		if unlock, err := o.lock(ctx, companionID); err == nil {
			defer unlock()
			_ = o.flushInterrupt(ctx, companionID)
		}
	}()
}

func (o *openCode) reset(ctx context.Context, companionID string) error {
	return o.prepareReset(companionID)(ctx)
}

func (o *openCode) prepareReset(companionID string) func(context.Context) error {
	run := o.prepareMappingChange(companionID)
	return func(ctx context.Context) error {
		return run(ctx, func() error { return o.forget(companionID) })
	}
}

// Block new prompts and interrupt the old mappings before changing them.
func (o *openCode) prepareMappingChange(devices ...string) func(context.Context, func() error) error {
	o.invalidateVerified(devices...)
	gates := make([]int, 0, len(devices))
	o.mu.Lock()
	for _, companionID := range devices {
		reset := o.resets[companionID]
		if reset == nil {
			reset = &ocReset{done: make(chan struct{})}
			o.resets[companionID] = reset
		}
		reset.count++
		o.markInterrupted(companionID)
		gates = append(gates, o.turnGate(companionID))
	}
	o.mu.Unlock()
	slices.Sort(gates)
	gates = slices.Compact(gates) // Two devices may hash to the same gate.
	return func(ctx context.Context, apply func() error) error {
		defer func() {
			o.mu.Lock()
			defer o.mu.Unlock()
			for _, companionID := range devices {
				reset := o.resets[companionID]
				reset.count--
				if reset.count == 0 {
					delete(o.resets, companionID)
					close(reset.done)
				}
			}
		}()
		for _, gate := range gates {
			unlock, err := o.lockGate(ctx, gate)
			if err != nil {
				return err
			}
			defer unlock()
		}
		for _, companionID := range devices {
			if err := o.flushInterrupt(ctx, companionID); err != nil {
				return err
			}
		}
		return apply()
	}
}

func ocData[T any](ctx context.Context, api upstream, method, path string, in any) (T, error) {
	var envelope struct{ Data *T }
	var zero T
	if err := api.jsonCall(ctx, method, path, in, &envelope); err != nil {
		return zero, err
	}
	if envelope.Data == nil {
		return zero, api.invalid()
	}
	return *envelope.Data, nil
}

func (o *openCode) ReconnectMCP(ctx context.Context, name string) error {
	o.invalidateVerified()
	query := ""
	if o.project != "" {
		query = "?" + url.Values{"location[directory]": {o.project}}.Encode()
	}
	if err := o.jsonCall(ctx, http.MethodPost, "/api/experimental/mcp/"+url.PathEscape(name)+"/connect"+query, nil, nil); err != nil {
		return err
	}
	// V2 returns 204 for a completed connect attempt even if MCP initialization failed.
	servers, err := ocData[[]struct {
		Name   string
		Status struct{ Status string }
	}](ctx, o.upstream, http.MethodGet, "/api/mcp"+query, nil)
	if err != nil {
		return err
	}
	for _, s := range servers {
		if s.Name == name && s.Status.Status == "connected" {
			return nil
		}
	}
	return problem(502, "OpenCode MCP catalog not connected")
}

func larkTitle(companionID, character string) string {
	if character = strings.TrimSpace(character); character != "" && character != companionID {
		return "Lark: " + character + " (" + companionID + ")"
	}
	return "Lark: " + companionID
}

func (o *openCode) configuredSettings(ctx context.Context) (ocSettings, error) {
	settings := o.settings
	if settings.Agent == "" || settings.Model != (ocModel{}) {
		return settings, nil
	}
	query := url.Values{"location[directory]": {o.project}}
	agent, err := ocData[struct {
		ID    string
		Model ocModel
	}](ctx, o.upstream, http.MethodGet, "/api/agent/"+url.PathEscape(settings.Agent)+"?"+query.Encode(), nil)
	if err != nil {
		return ocSettings{}, err
	}
	if agent.ID != settings.Agent {
		return ocSettings{}, o.invalid()
	}
	if agent.Model.Provider == "" || agent.Model.ID == "" {
		return ocSettings{}, problem(502, "Set the Lark agent model in the OpenCode configuration")
	}
	settings.Model = agent.Model
	o.mu.Lock()
	o.defaultModel = settings.Model
	o.mu.Unlock()
	return settings, nil
}

func (o *openCode) ensureModel(ctx context.Context, sid string, model ocModel) error {
	if model == (ocModel{}) {
		o.mu.Lock()
		model = o.defaultModel
		if model == (ocModel{}) {
			model = o.settings.Model
		}
		o.mu.Unlock()
	}
	if model == (ocModel{}) {
		return nil
	}
	o.mu.Lock()
	if o.sessionModel == nil {
		o.sessionModel = make(map[string]ocModel)
	}
	current, ok := o.sessionModel[sid]
	o.mu.Unlock()
	if ok && current == model {
		return nil
	}
	if err := o.jsonCall(ctx, http.MethodPost, "/api/session/"+sid+"/model", object{"model": model}, nil); err != nil {
		return err
	}
	o.mu.Lock()
	o.sessionModel[sid] = model
	o.mu.Unlock()
	return nil
}

func (o *openCode) fastSession(ctx context.Context, companionID, character string, replace bool) (string, error) {
	o.mu.Lock()
	cached := o.verified[companionID]
	o.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	return o.ensureSession(ctx, companionID, character, replace)
}

// awaitContract waits for runSchemaUpgrade to record the current MCP catalog
// hash. A turn that lands during a restart or a prompt change waits here
// instead of failing with "refresh pending".
func (o *openCode) awaitContract(ctx context.Context) error {
	wait := o.contractWait
	if wait <= 0 {
		wait = contractWait
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		catalog, err := o.db.getMeta(ctx, mcpSchemaMeta)
		if err != nil {
			return problem(502, "Cannot read OpenCode MCP contract")
		}
		if catalog == o.contract {
			return nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return problem(503, "OpenCode MCP catalog refresh pending")
		}
	}
}

func (o *openCode) ensureSession(ctx context.Context, companionID, character string, replace bool) (string, error) {
	if o.contract != "" {
		catalog, err := o.db.getMeta(ctx, mcpSchemaMeta)
		if err != nil {
			return "", problem(502, "Cannot read OpenCode MCP contract")
		}
		if catalog != o.contract {
			// The catalog refreshes in the background right after the listener
			// comes up (and after any prompt/deploy change). A turn that
			// arrives first waits for it instead of failing, which is what made
			// the first turn after a restart silently fail.
			if err := o.awaitContract(ctx); err != nil {
				return "", err
			}
		}
	}
	binding, err := o.readBinding(ctx, companionID)
	if err != nil {
		return "", err
	}
	sid := binding.sid
	stale := sid != "" && binding.contract != o.contract
	if stale && !replace {
		return "", problem(503, "OpenCode session refresh pending a user turn")
	}
	type session struct {
		ID string `json:"id"`
		ocSettings
	}
	var settings ocSettings
	if sid != "" && !stale {
		if !sessionIDRE.MatchString(sid) {
			return "", problem(502, "Invalid stored OpenCode session ID")
		}
		path := "/api/session/" + sid
		// Both reads precede any writes. Keep the live session permission check,
		// but fetch the user-owned model selection alongside it.
		var configuredErr error
		loadSettings := sync.WaitGroup{}
		loadSettings.Go(func() { settings, configuredErr = o.configuredSettings(ctx) })
		current, err := ocData[session](ctx, o.upstream, http.MethodGet, path, nil)
		loadSettings.Wait()
		if configuredErr != nil {
			return "", configuredErr
		}
		if err == nil {
			if current.ID != sid {
				return "", o.invalid()
			}
			s := settings
			if s.Agent != current.Agent {
				if err := o.jsonCall(ctx, http.MethodPost, path+"/agent", object{"agent": s.Agent}, nil); err != nil {
					return "", err
				}
			}
			if s.Model != current.Model {
				if err := o.jsonCall(ctx, http.MethodPost, path+"/model", object{"model": s.Model}, nil); err != nil {
					return "", err
				}
			}
			if !slices.Equal(s.Permissions, current.Permissions) {
				if err := o.jsonCall(ctx, http.MethodPatch, path, object{"permissions": s.Permissions}, nil); err != nil {
					return "", err
				}
			}
			o.mu.Lock()
			if o.verified != nil {
				o.verified[companionID] = sid
			}
			if o.sessionModel != nil {
				o.sessionModel[sid] = settings.Model
			}
			o.mu.Unlock()
			return sid, nil
		}
		var status *upstreamStatus
		if !replace || !errors.As(err, &status) || status.code != http.StatusNotFound ||
			status.tag != "SessionNotFoundError" || status.sessionID != sid {
			return "", err
		}
	} else {
		settings, err = o.configuredSettings(ctx)
		if err != nil {
			return "", err
		}
	}
	var seed string
	if sid != "" {
		seed, err = o.recentDialogue(ctx, companionID, binding.after)
		if err != nil {
			return "", err
		}
	}
	after := binding.after
	if after == 0 || sid == "" {
		after = time.Now().UnixMilli()
	}
	in := struct {
		Title    string `json:"title"`
		Location object `json:"location"`
		ocSettings
	}{larkTitle(companionID, character), object{"directory": o.project}, settings}
	created, err := ocData[session](ctx, o.upstream, http.MethodPost, "/api/session", in)
	if err != nil {
		return "", err
	}
	if !sessionIDRE.MatchString(created.ID) || created.ID == sid {
		return "", o.invalid()
	}
	if seed != "" {
		if err := o.seedDialogue(ctx, created.ID, seed); err != nil {
			return "", err
		}
	}
	if err := o.replaceBinding(ctx, companionID, binding, created.ID, after); err != nil {
		return "", err
	}
	o.mu.Lock()
	if o.verified != nil {
		o.verified[companionID] = created.ID
	}
	if o.sessionModel != nil {
		o.sessionModel[created.ID] = settings.Model
	}
	o.mu.Unlock()
	return created.ID, nil
}

func sessionMatchesDevice(title, deviceID string) bool {
	name, ok := strings.CutPrefix(title, "Lark: ")
	if !ok || deviceID == "" {
		return false
	}
	if strings.EqualFold(name, deviceID) {
		return true
	}
	if id, ok := strings.CutPrefix(name, "scope-"); ok && strings.EqualFold(id, deviceID) {
		return true
	}
	if name, ok := strings.CutSuffix(name, ")"); ok {
		if i := strings.LastIndex(name, " ("); i >= 0 {
			return strings.EqualFold(name[i+2:], deviceID)
		}
	}
	return false
}

func (o *openCode) ListDeviceSessions(ctx context.Context, deviceID, activeSID string) ([]string, error) {
	return o.listDeviceSessions(ctx, deviceID, activeSID, true)
}

func (o *openCode) listDeviceSessions(ctx context.Context, deviceID, activeSID string, fallback bool) ([]string, error) {
	type session struct {
		ID, Title string
		Time      struct{ Created int64 }
	}
	type page struct {
		Data   []session
		Cursor struct{ Next string }
	}
	var matches []session
	seenIDs, seenCursors := make(map[string]bool), make(map[string]bool)
	params := url.Values{"limit": {"200"}}
	for {
		var result page
		if err := o.jsonCall(ctx, http.MethodGet, "/api/session?"+params.Encode(), nil, &result); err != nil {
			if fallback && activeSID != "" {
				return []string{activeSID}, nil
			}
			return nil, err
		}
		for _, s := range result.Data {
			if sessionIDRE.MatchString(s.ID) && !seenIDs[s.ID] && (s.ID == activeSID || sessionMatchesDevice(s.Title, deviceID)) {
				matches = append(matches, s)
				seenIDs[s.ID] = true
			}
		}
		next := result.Cursor.Next
		if next == "" || seenCursors[next] {
			break
		}
		seenCursors[next] = true
		params.Set("cursor", next)
	}
	slices.SortFunc(matches, func(a, b session) int {
		return cmp.Or(cmp.Compare(a.Time.Created, b.Time.Created), strings.Compare(a.ID, b.ID))
	})
	ids := make([]string, 0, len(matches))
	for _, s := range matches {
		ids = append(ids, s.ID)
	}
	if activeSID != "" && !seenIDs[activeSID] {
		ids = append(ids, activeSID)
	}
	return ids, nil
}

type ocMessage struct {
	ID, Type, Finish, Outcome string
	Text                      string
	Time                      struct{ Created int64 }
	Metadata                  struct{ LarkSource string }
	Content                   []*ocMessagePart
	Parts                     []*ocMessagePart
	Error                     *struct{ Status int }
}

type ocMessagePart struct {
	Type, Name, Tool string
	Text             *string
	State            struct{ Input struct{ Title string } }
}

func (m ocMessage) chat(source string) chatEntry {
	entry := chatEntry{ID: m.ID, Role: m.Type, Source: source, Channel: channelForSource(source), Timestamp: time.UnixMilli(m.Time.Created).In(time.Local)}
	switch m.Type {
	case "user":
		entry.Sender, entry.Text = "You", m.Text
	case "assistant":
		entry.Sender = "Lark"
	case "system":
		entry.Sender, entry.Text = "System", m.Text
	default:
		return chatEntry{}
	}
	parts := m.Content
	if parts == nil {
		parts = m.Parts
	}
	var text, notes []string
	for _, p := range parts {
		if p == nil {
			continue
		}
		if p.Type == "text" && p.Text != nil {
			text = append(text, *p.Text)
		}
		name := p.Name
		if name == "" {
			name = p.Tool
		}
		if action, ok := strings.CutPrefix(name, "lark_"); p.Type == "tool" && ok {
			label := map[string]string{"state": "Checked live state", "patch": "Changed saved or companionID state",
				"emit": "Sent a signal", "library": "Searched the local library"}[action]
			if label != "" && !slices.Contains(notes, label) {
				notes = append(notes, label)
			}
		}
	}
	if entry.Text == "" {
		entry.Text = strings.Join(text, "\n")
	}
	if m.Type == "user" {
		entry.Text = stripLarkHeader(entry.Text)
	}
	if m.Type == "assistant" {
		entry.Text = fenced.ReplaceAllString(entry.Text, "")
	}
	entry.Text = strings.TrimSpace(entry.Text)
	entry.ToolSummary = strings.Join(notes, "; ")
	if entry.Text == "" && entry.ToolSummary != "" {
		entry.Source = "mcp"
	}
	return entry
}

type ocMessagePage struct {
	Data   []ocMessage
	Cursor struct{ Next string }
}

func (o *openCode) GetSessionMessages(ctx context.Context, sid string) ([]chatEntry, error) {
	entries := []chatEntry{}
	cursor, source := "", "text"
	seen := make(map[string]bool)
	for {
		page, err := o.sessionMessagePage(ctx, sid, cursor, source)
		if err != nil {
			return nil, err
		}
		entries = append(entries, page.entries...)
		if page.next == "" || seen[page.next] {
			return entries, nil
		}
		seen[page.next] = true
		cursor, source = page.next, page.source
	}
}

type sessionHistoryPage struct {
	entries      []chatEntry
	next, source string
}

func (o *openCode) sessionMessagePage(ctx context.Context, sid, cursor, source string) (sessionHistoryPage, error) {
	if !sessionIDRE.MatchString(sid) {
		return sessionHistoryPage{}, problem(400, "Invalid session ID")
	}
	params := url.Values{"limit": {"50"}}
	if cursor == "" {
		params.Set("order", "asc")
	} else {
		params.Set("cursor", cursor)
	}
	var result ocMessagePage
	if err := o.jsonCall(ctx, http.MethodGet, "/api/session/"+sid+"/message?"+params.Encode(), nil, &result); err != nil {
		return sessionHistoryPage{}, err
	}
	page := sessionHistoryPage{source: source}
	for _, message := range result.Data {
		if message.Type == "assistant" && message.Finish != "" && message.Finish != "stop" && message.Finish != "length" {
			continue
		}
		if message.Type == "user" {
			switch message.Metadata.LarkSource {
			case "typed", "voice", "tap":
				page.source = message.Metadata.LarkSource
			default:
				page.source = "text"
			}
		}
		if entry := message.chat(page.source); entry.ID != "" && (entry.Text != "" || entry.ToolSummary != "") {
			page.entries = append(page.entries, entry)
		}
	}
	if len(result.Data) != 0 {
		page.next = result.Cursor.Next
	}
	return page, nil
}

func (o *openCode) lockPrompt(ctx context.Context, companionID string) (context.Context, func(), error) {
	for {
		unlock, err := o.lock(ctx, companionID)
		if err != nil {
			return nil, nil, err
		}
		if err := o.flushInterrupt(ctx, companionID); err != nil {
			unlock()
			return nil, nil, err
		}
		o.mu.Lock()
		if reset := o.resets[companionID]; reset != nil {
			o.mu.Unlock()
			unlock()
			select {
			case <-reset.done:
				continue
			case <-ctx.Done():
				return nil, nil, o.unavailable(ctx.Err())
			}
		}
		if !o.interrupts[companionID] {
			turn, cancel := context.WithCancel(ctx)
			o.active[companionID] = cancel
			o.mu.Unlock()
			return turn, func() {
				defer unlock()
				o.mu.Lock()
				delete(o.active, companionID)
				delete(o.activeSource, companionID)
				o.mu.Unlock()
				cancel()
				_ = o.flushInterrupt(context.WithoutCancel(turn), companionID)
			}, nil
		}
		o.mu.Unlock()
		unlock()
	}
}

func (o *openCode) askPrepared(ctx context.Context, companionID string, prepare func() (string, string), stream turnStream, source string, begin func() func(), onChat func(chatEntry)) (reply, sid string, err error) {
	return o.askPreparedWithModel(ctx, companionID, prepare, ocModel{}, stream, source, begin, onChat)
}

func (o *openCode) askPreparedWithModel(ctx context.Context, companionID string, prepare func() (string, string), model ocModel, stream turnStream, source string, begin func() func(), onChat func(chatEntry)) (reply, sid string, err error) {
	waiting := time.Now()
	trace := traceFrom(ctx)
	ctx, unlock, err := o.lockPrompt(ctx, companionID)
	if err != nil {
		return "", "", err
	}
	admitted := time.Now()
	trace.measure(diagGate, admitted.Sub(waiting))
	defer unlock()
	character, prompt := prepare()
	sid, err = o.fastSession(ctx, companionID, character, source != "timer")
	if err != nil {
		return "", "", err
	}
	if err := o.ensureModel(ctx, sid, model); err != nil {
		o.invalidateVerified(companionID)
		return "", "", err
	}
	if begin != nil {
		finish := begin()
		defer finish()
	}
	trace.measure(diagPromptPrep, time.Since(admitted))
	reply, err = o.runTurnStreamingWithModel(ctx, sid, prompt, model, stream, source, onChat)
	if err != nil {
		o.invalidateVerified(companionID)
		o.mu.Lock()
		delete(o.sessionModel, sid)
		o.mu.Unlock()
		o.requestInterrupt(companionID)
	}
	return reply, sid, err
}
