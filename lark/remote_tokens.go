package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	remotePath       = "/remote"
	remoteCookieName = "lark_remote"
	remotePoll       = 750 * time.Millisecond
	remoteKeepAlive  = 15 * time.Second
	remoteTokenBytes = 16
	// opentunnel route add/down can block on systemd; bound one CLI call.
	tunnelTimeout = 90 * time.Second
	// History paging: a page is small, the query is clipped, and one search
	// term is bounded so LIKE cannot scan an unbounded string.
	remoteHistoryLimit    = 20
	remoteHistoryLimitMax = 50
	remoteHistoryQueryMax = 100
)

type tunnelRunner func(context.Context, ...string) ([]byte, error)

func tunnelCLI(bin string) tunnelRunner {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, tunnelTimeout)
		defer cancel()
		return exec.CommandContext(ctx, bin, args...).CombinedOutput()
	}
}

func findTunnelCLI(home string) string {
	for _, candidate := range []string{os.Getenv("LARK_OPENTUNNEL_BIN"), "opentunnel", filepath.Join(home, ".local/bin/opentunnel")} {
		if candidate == "" {
			continue
		}
		if bin, err := exec.LookPath(candidate); err == nil {
			return bin
		}
	}
	return ""
}

type remoteRoute struct {
	CompanionID string `json:"companion_id"`
	RouteName   string `json:"route_name"`
	URL         string `json:"url"`
	LocalPort   int    `json:"local_port"`
	Profile     string `json:"profile"`
	CreatedAt   string `json:"created_at,omitempty"`
}

type remoteToken struct {
	CompanionID string
	Contact     string // Lowercased Delta Chat address (or numeric contact key).
	ContactName string // Display name at issue time, used to scope history.
}

func (t remoteToken) matchesSender(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return name != "" && (name == t.Contact || (t.ContactName != "" && name == strings.ToLower(t.ContactName)))
}

type remoteScope struct {
	cancel context.CancelFunc
	server *http.Server
}

func (s *remoteScope) close() {
	s.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if s.server.Shutdown(ctx) != nil {
		_ = s.server.Close()
	}
}

type remoteManager struct {
	b      *larkServer
	bin    string
	runner tunnelRunner

	mu     sync.Mutex
	tokens map[string]remoteToken // sha256(token) -> scope
	routes map[string]*remoteRoute
	live   map[string]*remoteScope
	locks  map[string]*sync.Mutex // per-companion tunnel operations
	closed bool
}

var (
	routeURL     = regexp.MustCompile(`https://[a-z0-9.-]+`)
	routeDNSHost = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{1,62}$`)
)

func newRemoteManager(b *larkServer, bin string) *remoteManager {
	var runner tunnelRunner
	if bin != "" {
		runner = tunnelCLI(bin)
	}
	return &remoteManager{
		b:      b,
		bin:    bin,
		runner: runner,
		tokens: make(map[string]remoteToken),
		routes: make(map[string]*remoteRoute),
		live:   make(map[string]*remoteScope),
	}
}

func (m *remoteManager) available() bool {
	return m != nil && m.runner != nil && m.bin != ""
}

func tokenHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func routeAddress(raw string) (string, string) {
	for _, address := range routeURL.FindAllString(raw, -1) {
		host := strings.TrimPrefix(address, "https://")
		if routeDNSHost.MatchString(host) && strings.Contains(host, ".") {
			return strings.Split(host, ".")[0], address
		}
	}
	return "", ""
}

func (m *remoteManager) restore(ctx context.Context) {
	if m == nil || m.b == nil || m.b.db == nil {
		return
	}
	rows, err := m.b.db.readDB().QueryContext(ctx, `SELECT key,value FROM meta WHERE key LIKE 'remote_route:%' ORDER BY key`)
	if err != nil {
		log.Printf("Lark could not read remote routes: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			continue
		}
		var route remoteRoute
		if json.Unmarshal([]byte(raw), &route) != nil || route.CompanionID == "" {
			continue
		}
		if _, ok := m.b.companionByID(route.CompanionID); !ok {
			continue
		}
		scope, _, err := m.listen(route.CompanionID, route.LocalPort)
		if err != nil {
			log.Printf("Lark could not restore remote route for %s: %v", route.CompanionID, err)
			continue
		}
		m.mu.Lock()
		m.routes[route.CompanionID] = &route
		m.live[route.CompanionID] = scope
		m.mu.Unlock()
	}
	if err := rows.Err(); err != nil {
		log.Printf("Lark could not read remote routes: %v", err)
		return
	}

	rows, err = m.b.db.readDB().QueryContext(ctx, `SELECT key,value FROM meta WHERE key LIKE 'remote_token:%' ORDER BY key`)
	if err != nil {
		log.Printf("Lark could not read remote tokens: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			continue
		}
		rest, ok := strings.CutPrefix(key, "remote_token:")
		if !ok {
			continue
		}
		companionID, contact, ok := strings.Cut(rest, ":")
		if !ok || raw == "" {
			continue
		}
		var meta remoteTokenMeta
		if json.Unmarshal([]byte(raw), &meta) != nil {
			// Pre-scope rows stored the hash as a bare string; they carry no
			// contact name, so Delta Chat history stays hidden until reissued.
			meta = remoteTokenMeta{Hash: raw}
		}
		if meta.Hash == "" {
			continue
		}
		m.mu.Lock()
		if _, exists := m.routes[companionID]; exists {
			m.tokens[meta.Hash] = remoteToken{CompanionID: companionID, Contact: contact, ContactName: meta.Name}
		}
		m.mu.Unlock()
	}
}

func (m *remoteManager) listen(companionID string, port int) (*remoteScope, int, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, 0, err
	}
	ctx, cancel := context.WithCancel(m.b.root)
	s := &remoteScope{cancel: cancel}
	s.server = &http.Server{
		Handler:           m.scopedHandler(companionID),
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	go func() { _ = s.server.Serve(ln) }()
	return s, ln.Addr().(*net.TCPAddr).Port, nil
}

func (m *remoteManager) ensureScopeLocked(companionID string) (*remoteRoute, error) {
	route := m.routes[companionID]
	if route == nil {
		return nil, nil
	}
	if m.live[companionID] != nil {
		return route, nil
	}
	scope, _, err := m.listen(companionID, route.LocalPort)
	if err != nil {
		return nil, problem(409, "Saved remote port is unavailable")
	}
	m.live[companionID] = scope
	return route, nil
}

// lockCompanion serializes route/token operations for one companion without
// holding m.mu across the opentunnel CLI (which can block for 90 seconds).
func (m *remoteManager) lockCompanion(companionID string) func() {
	m.mu.Lock()
	if m.locks == nil {
		m.locks = make(map[string]*sync.Mutex)
	}
	lock := m.locks[companionID]
	if lock == nil {
		lock = &sync.Mutex{}
		m.locks[companionID] = lock
	}
	m.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func (m *remoteManager) shuttingDown() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

func (m *remoteManager) issue(ctx context.Context, companionID, contact, contactName string) (string, error) {
	if m == nil || m.b == nil {
		return "", problem(503, "Remote controls unavailable")
	}
	companionID = strings.ToLower(strings.TrimSpace(companionID))
	if _, ok := m.b.companionByID(companionID); !ok {
		return "", problem(404, "Companion not found")
	}
	if !m.available() {
		return "", problem(503, "Remote controls need opentunnel; install it from opentunnel.xyz")
	}
	contact = strings.ToLower(strings.TrimSpace(contact))
	raw, err := randomHex(remoteTokenBytes)
	if err != nil {
		return "", problem(500, "Cannot create a remote token")
	}
	hash := tokenHash(raw)

	unlock := m.lockCompanion(companionID)
	defer unlock()
	if m.shuttingDown() {
		return "", problem(503, "Remote controls shutting down")
	}

	m.mu.Lock()
	route := m.routes[companionID]
	live := m.live[companionID] != nil
	m.mu.Unlock()
	if route == nil {
		scope, port, err := m.listen(companionID, 0)
		if err != nil {
			return "", problem(502, "Cannot start remote listener")
		}
		profileID, err := randomHex(12)
		if err != nil {
			scope.close()
			return "", problem(500, "Cannot create a tunnel profile")
		}
		profile := "lark-" + profileID
		rawOut, err := m.runner(ctx, "route", "add", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), "--profile", profile)
		routeName, publicURL := routeAddress(string(rawOut))
		if routeName == "" || publicURL == "" {
			scope.close()
			return "", problem(502, "OpenTunnel did not return a usable remote URL")
		}
		if err != nil {
			// Route add can return a nonzero status after enabling the systemd
			// unit in some environments; a usable URL means the route exists.
			log.Printf("OpenTunnel route add for %s reported: %v", companionID, err)
		}
		route = &remoteRoute{CompanionID: companionID, RouteName: routeName, URL: publicURL,
			LocalPort: port, Profile: profile, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		if err := m.saveRoute(ctx, route); err != nil {
			scope.close()
			return "", err
		}
		m.mu.Lock()
		m.routes[companionID] = route
		if m.live[companionID] == nil {
			m.live[companionID] = scope
		} else {
			scope.close()
		}
		m.mu.Unlock()
	} else if !live {
		m.mu.Lock()
		_, err := m.ensureScopeLocked(companionID)
		m.mu.Unlock()
		if err != nil {
			return "", err
		}
	}

	// Persist the replacement token before retiring the old one, so a failed
	// route add or save leaves the existing link working.
	if err := m.saveToken(ctx, companionID, contact, hash, contactName); err != nil {
		return "", err
	}
	m.mu.Lock()
	for oldHash, tok := range m.tokens {
		if tok.CompanionID == companionID && tok.Contact == contact {
			delete(m.tokens, oldHash)
		}
	}
	m.tokens[hash] = remoteToken{CompanionID: companionID, Contact: contact, ContactName: contactName}
	m.mu.Unlock()
	return strings.TrimRight(route.URL, "/") + "/remote?token=" + url.QueryEscape(raw), nil
}

func (m *remoteManager) revokeContact(ctx context.Context, companionID, contact string) (int, error) {
	if m == nil {
		return 0, nil
	}
	companionID = strings.ToLower(strings.TrimSpace(companionID))
	contact = strings.ToLower(strings.TrimSpace(contact))
	unlock := m.lockCompanion(companionID)
	defer unlock()
	// Delete the durable token first: if that fails the link stays valid and
	// the caller can retry instead of resurrecting it after a restart.
	if err := m.deleteMeta(ctx, remoteTokenKey(companionID, contact)); err != nil {
		return 0, problem(502, "Cannot revoke the remote token")
	}
	m.mu.Lock()
	for hash, tok := range m.tokens {
		if tok.CompanionID == companionID && tok.Contact == contact {
			delete(m.tokens, hash)
		}
	}
	left := 0
	for _, tok := range m.tokens {
		if tok.CompanionID == companionID {
			left++
		}
	}
	m.mu.Unlock()
	if left == 0 {
		if err := m.stop(ctx, companionID); err != nil {
			return 0, err
		}
	}
	return left, nil
}

func (m *remoteManager) stopCompanion(ctx context.Context, companionID string) error {
	if m == nil {
		return nil
	}
	companionID = strings.ToLower(strings.TrimSpace(companionID))
	unlock := m.lockCompanion(companionID)
	defer unlock()
	return m.stop(ctx, companionID)
}

// stop removes a companion's route and tokens. The caller holds its
// per-companion operation lock; m.mu is released across the CLI calls.
func (m *remoteManager) stop(ctx context.Context, companionID string) error {
	m.mu.Lock()
	route := m.routes[companionID]
	scope := m.live[companionID]
	for hash, tok := range m.tokens {
		if tok.CompanionID == companionID {
			delete(m.tokens, hash)
		}
	}
	delete(m.live, companionID)
	delete(m.routes, companionID)
	m.mu.Unlock()
	if route == nil {
		return nil
	}
	if m.runner != nil {
		if _, err := m.runner(ctx, "route", "remove", route.RouteName, "--profile", route.Profile); err != nil {
			log.Printf("Lark could not remove remote route for %s: %v", companionID, err)
		}
		if _, err := m.runner(ctx, "down", "--profile", route.Profile); err != nil {
			log.Printf("Lark could not stop the remote tunnel for %s: %v", companionID, err)
		}
	}
	if scope != nil {
		scope.close()
	}
	if m.b == nil || m.b.db == nil {
		return nil
	}
	if _, err := m.b.db.ExecContext(ctx, `DELETE FROM meta WHERE key=? OR key LIKE ?`,
		remoteRouteKey(companionID), "remote_token:"+companionID+":%"); err != nil {
		return problem(502, "Cannot delete saved remote route")
	}
	return nil
}

func (m *remoteManager) close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for _, scope := range m.live {
		scope.close()
	}
	clear(m.live)
}

func remoteRouteKey(companionID string) string {
	return "remote_route:" + strings.ToLower(companionID)
}

func remoteTokenKey(companionID, contact string) string {
	return "remote_token:" + strings.ToLower(companionID) + ":" + strings.ToLower(contact)
}

func (m *remoteManager) saveRoute(ctx context.Context, route *remoteRoute) error {
	raw, err := json.Marshal(route)
	if err != nil {
		return err
	}
	if m.b != nil && m.b.db != nil {
		if err := m.b.db.setMeta(ctx, remoteRouteKey(route.CompanionID), string(raw)); err != nil {
			return problem(502, "Cannot save remote route")
		}
	}
	return nil
}

type remoteTokenMeta struct {
	Hash string `json:"hash"`
	Name string `json:"name,omitempty"`
}

func (m *remoteManager) saveToken(ctx context.Context, companionID, contact, hash, name string) error {
	if m.b == nil || m.b.db == nil {
		return nil
	}
	raw, err := json.Marshal(remoteTokenMeta{Hash: hash, Name: name})
	if err != nil {
		return problem(502, "Cannot save remote token")
	}
	if err := m.b.db.setMeta(ctx, remoteTokenKey(companionID, contact), string(raw)); err != nil {
		return problem(502, "Cannot save remote token")
	}
	return nil
}

// deleteMeta returns the delete error so a revocation that could not be made
// durable fails loudly instead of letting the token reappear after restart.
func (m *remoteManager) deleteMeta(ctx context.Context, key string) error {
	if m == nil || m.b == nil || m.b.db == nil {
		return nil
	}
	_, err := m.b.db.ExecContext(ctx, `DELETE FROM meta WHERE key=?`, key)
	return err
}

func (m *remoteManager) authorize(r *http.Request) (remoteToken, string, bool) {
	if m == nil {
		return remoteToken{}, "", false
	}
	raw := ""
	if values := r.Header.Values("X-Lark-Remote-Token"); len(values) == 1 {
		raw = values[0]
	}
	if raw == "" {
		raw = r.URL.Query().Get("token")
	}
	if raw == "" {
		if cookie, err := r.Cookie(remoteCookieName); err == nil {
			raw = cookie.Value
		}
	}
	if raw == "" {
		return remoteToken{}, "", false
	}
	hash := tokenHash(raw)
	m.mu.Lock()
	tok, ok := m.tokens[hash]
	m.mu.Unlock()
	return tok, raw, ok
}

func (m *remoteManager) scopedHandler(companionID string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		path := r.URL.EscapedPath()
		if !strings.HasPrefix(path, remotePath) {
			sendError(w, problem(404, "Not found"))
			return
		}
		if publicRemoteAsset(path) {
			m.b.serveRemoteAsset(w, path)
			return
		}
		tok, raw, ok := m.authorize(r)
		if !ok || tok.CompanionID != companionID {
			sendError(w, problem(401, "Unauthorized"))
			return
		}
		m.handleAuthorized(w, r, path, tok, raw)
	})
}

func publicRemoteAsset(path string) bool {
	switch path {
	case "/remote/page.css", "/remote/app.js":
		return true
	}
	return false
}

func (b *larkServer) remoteHTTP(w http.ResponseWriter, r *http.Request, path string) {
	if b.remote == nil {
		sendError(w, problem(503, "Remote controls unavailable"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if publicRemoteAsset(path) {
		b.serveRemoteAsset(w, path)
		return
	}
	tok, raw, ok := b.remote.authorize(r)
	if !ok {
		sendError(w, problem(401, "Unauthorized"))
		return
	}
	b.remote.handleAuthorized(w, r, path, tok, raw)
}

func (m *remoteManager) handleAuthorized(w http.ResponseWriter, r *http.Request, path string, tok remoteToken, raw string) {
	switch {
	case path == remotePath && r.Method == http.MethodGet:
		m.b.serveRemotePage(w, r, tok, raw)
	case path == remotePath+"/events" && r.Method == http.MethodGet:
		m.b.serveRemoteEvents(w, r, tok.CompanionID)
	case strings.HasPrefix(path, remotePath+"/api/"):
		m.b.serveRemoteAPI(w, r, path, tok, raw)
	case strings.HasPrefix(path, remotePath+"/pwa/"):
		m.b.serveRemotePWA(w, r, path, tok, raw)
	default:
		sendError(w, problem(404, "Not found"))
	}
}

func (b *larkServer) serveRemoteEvents(w http.ResponseWriter, r *http.Request, companionID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		sendError(w, problem(500, "Streaming unavailable"))
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	_, _ = fmt.Fprint(w, "retry: 2000\n\n")
	flusher.Flush()

	send := func(snapshot object) bool {
		data, err := json.Marshal(snapshot)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	sendCapture := func(status object) bool {
		data, err := json.Marshal(status)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: capture\ndata: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	initial := b.remoteSnapshot(r.Context(), companionID, true)
	if !send(initial) {
		return
	}
	last := remoteSnapshotKey(initial)
	captureStatus := b.captures.status(companionID)
	if !sendCapture(captureStatus) {
		return
	}
	lastCapture := remoteSnapshotKey(captureStatus)
	ticker := time.NewTicker(remotePoll)
	defer ticker.Stop()
	keep := time.NewTicker(remoteKeepAlive)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			snapshot := b.remoteSnapshot(r.Context(), companionID, false)
			key := remoteSnapshotKey(snapshot)
			if key != last {
				last = key
				if !send(snapshot) {
					return
				}
			}
			status := b.captures.status(companionID)
			captureKey := remoteSnapshotKey(status)
			if captureKey == lastCapture {
				continue
			}
			lastCapture = captureKey
			if !sendCapture(status) {
				return
			}
		case <-keep.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func remoteSnapshotKey(snapshot object) string {
	comparable := object{}
	for key, value := range snapshot {
		comparable[key] = value
	}
	if comp, ok := comparable["companion"].(object); ok {
		withoutPortrait := object{}
		for key, value := range comp {
			if key != "portrait" {
				withoutPortrait[key] = value
			}
		}
		comparable["companion"] = withoutPortrait
	}
	data, _ := json.Marshal(comparable)
	return string(data)
}

func (b *larkServer) remoteSnapshot(ctx context.Context, companionID string, portrait bool) object {
	c := b.companionFor(companionID)
	comp := object{
		"id":         c.ID,
		"name":       c.Name,
		"form":       c.Form,
		"voice":      b.voiceFor(c.ID),
		"listening":  c.Listening,
		"live_state": b.companionLiveState(c.ID),
	}
	if portrait {
		if pngBytes, err := companionAvatarPNG(b, c); err == nil {
			comp["portrait"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes)
		}
	}

	terminals := []object{}
	seen := make(map[string]bool)
	if dbTerms, err := b.terminalsForCompanion(companionID); err == nil {
		for _, dt := range dbTerms {
			te := b.terminalEntry(dt, true)
			entry := object{
				"id":     dt.ID,
				"name":   dt.ID,
				"online": te["online"],
				"volume": te["volume"],
			}
			if pct, ok := te["battery_pct"]; ok && pct != nil {
				entry["battery_pct"] = pct
			}
			if chg, ok := te["charging"]; ok && chg != nil {
				entry["charging"] = chg
			}
			terminals = append(terminals, entry)
			seen[strings.ToLower(dt.ID)] = true
		}
	}
	if b.hub != nil {
		for _, t := range b.hub.companionTerminals(companionID) {
			tid := strings.ToLower(str(t["id"]))
			if !seen[tid] {
				terminals = append(terminals, t)
			}
		}
	}
	slices.SortFunc(terminals, func(a, b object) int { return strings.Compare(str(a["id"]), str(b["id"])) })

	progressByStory := make(map[string]storyProgress)
	if plist, err := b.listStoryProgress(ctx, companionID); err == nil {
		for _, p := range plist {
			progressByStory[p.StoryID] = p
		}
	}
	stories := []object{}
	if list, err := b.listStorySummaries(ctx); err == nil {
		for _, st := range list {
			item := object{"name": st.ID, "title": st.Title, "passages": st.Passages, "progress": 0}
			if prog, ok := progressByStory[st.ID]; ok {
				pct := 10
				if cs, err := b.compiledStoryByID(ctx, st.ID); err == nil && len(cs.Passages) > 0 {
					if idx, exists := cs.ByName[prog.Passage]; exists {
						if cs.Passages[idx].End {
							pct = 100
						} else {
							pct = max(5, min(95, (idx+1)*100/len(cs.Passages)))
						}
					}
				}
				item["progress"] = pct
				item["passage"] = prog.Passage
			}
			stories = append(stories, item)
		}
	}

	voices := []object{}
	for _, v := range voiceCatalog {
		voices = append(voices, object{"id": v.ID, "name": v.Name})
	}
	return object{"companion": comp, "terminals": terminals, "stories": stories, "voices": voices}
}

func (b *larkServer) serveRemoteAPI(w http.ResponseWriter, r *http.Request, path string, tok remoteToken, raw string) {
	switch {
	case path == remotePath+"/api/capture" && r.Method == http.MethodPost:
		value, err := b.remoteCaptureStart(r, tok.CompanionID)
		sendResult(w, value, err)
	case path == remotePath+"/api/captures" && r.Method == http.MethodGet:
		value, err := b.remoteCaptureList(tok.CompanionID)
		sendResult(w, value, err)
	case strings.HasPrefix(path, remotePath+"/api/captures/"):
		name := strings.TrimPrefix(path, remotePath+"/api/captures/")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(name, "/transcribe"):
			value, err := b.remoteCaptureTranscribe(r, tok.CompanionID, strings.TrimSuffix(name, "/transcribe"))
			sendResult(w, value, err)
		case r.Method == http.MethodGet && strings.HasSuffix(name, ".wav"):
			b.remoteCaptureWAV(w, r, tok.CompanionID, strings.TrimSuffix(name, ".wav"))
		case r.Method == http.MethodDelete:
			b.remoteCaptureDelete(w, tok.CompanionID, name)
		default:
			sendError(w, problem(404, "Not found"))
		}
	case path == remotePath+"/api/history" && r.Method == http.MethodGet:
		page, err := b.remoteHistory(r.Context(), tok, r.URL.Query())
		sendResult(w, page, err)
	case path == remotePath+"/api/say" && r.Method == http.MethodPost:
		value, err := b.remoteSay(r, tok.CompanionID)
		sendResult(w, value, err)
	case path == remotePath+"/api/listen" && r.Method == http.MethodPost:
		value, err := b.remoteListen(r, tok.CompanionID)
		sendResult(w, value, err)
	case path == remotePath+"/api/volume" && r.Method == http.MethodPost:
		value, err := b.remoteVolume(r, tok.CompanionID)
		sendResult(w, value, err)
	case path == remotePath+"/api/voice" && r.Method == http.MethodPost:
		value, err := b.remoteVoice(r, tok.CompanionID)
		sendResult(w, value, err)
	case path == remotePath+"/api/story/open" && r.Method == http.MethodPost:
		value, err := b.remoteStory(r, tok.CompanionID, false)
		sendResult(w, value, err)
	case path == remotePath+"/api/story/restart" && r.Method == http.MethodPost:
		value, err := b.remoteStory(r, tok.CompanionID, true)
		sendResult(w, value, err)
	case path == remotePath+"/api/revoke" && r.Method == http.MethodPost:
		_, err := b.remote.revokeContact(r.Context(), tok.CompanionID, tok.Contact)
		if err == nil {
			http.SetCookie(w, &http.Cookie{Name: remoteCookieName, Value: "", Path: remotePath, MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
			sendJSON(w, 200, object{"ok": true, "message": "Remote controls off."})
			return
		}
		sendError(w, err)
	default:
		sendError(w, problem(404, "Not found"))
	}
}

func remoteStateResponse(b *larkServer, ctx context.Context, companionID, message string) object {
	return object{"ok": true, "message": message, "state": b.remoteSnapshot(ctx, companionID, false)}
}

func (b *larkServer) remoteSay(r *http.Request, companionID string) (any, error) {
	var req remoteSayRequest
	if err := decodeRequest(r, &req); err != nil {
		return nil, err
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return nil, problem(400, "Enter something to say")
	}
	clean, err := spokenText(text)
	if err != nil {
		return nil, problem(400, err.Error())
	}
	b.sayOnCompanion(r.Context(), companionID, "Remote", 0, clean)
	return remoteStateResponse(b, r.Context(), companionID, "Said it."), nil
}

func (b *larkServer) remoteListen(r *http.Request, companionID string) (any, error) {
	var req remoteListenRequest
	if err := decodeRequest(r, &req); err != nil {
		return nil, err
	}
	if req.Listening == nil {
		return nil, problem(400, "Expected listening to be true or false")
	}
	if err := b.setListening(r.Context(), companionID, *req.Listening); err != nil {
		return nil, err
	}
	return remoteStateResponse(b, r.Context(), companionID, "Listening mode updated."), nil
}

func (b *larkServer) remoteVolume(r *http.Request, companionID string) (any, error) {
	var req remoteVolumeRequest
	if err := decodeRequest(r, &req); err != nil {
		return nil, err
	}
	terminalID := strings.ToLower(strings.TrimSpace(req.Terminal))
	if terminalID == "" {
		return nil, problem(400, "Terminal is required")
	}
	t, ok := b.terminalByID(terminalID)
	if !ok {
		return nil, problem(404, "Terminal not found")
	}
	if !strings.EqualFold(t.CompanionID, companionID) {
		return nil, problem(403, "Terminal out of scope")
	}
	if req.Volume == nil {
		return nil, problem(400, `Expected {"volume":0..100}`)
	}
	if _, err := b.setVolume(r.Context(), terminalID, *req.Volume); err != nil {
		return nil, err
	}
	return remoteStateResponse(b, r.Context(), companionID, "Volume updated."), nil
}

func (b *larkServer) remoteVoice(r *http.Request, companionID string) (any, error) {
	var req remoteVoiceRequest
	if err := decodeRequest(r, &req); err != nil {
		return nil, err
	}
	if err := b.setVoice(r.Context(), companionID, req.Voice, true); err != nil {
		return nil, err
	}
	b.previewVoiceOnCompanion(companionID, req.Voice)
	return remoteStateResponse(b, r.Context(), companionID, "Voice updated."), nil
}

func (b *larkServer) remoteStory(r *http.Request, companionID string, restart bool) (any, error) {
	var req remoteStoryRequest
	if err := decodeRequest(r, &req); err != nil {
		return nil, err
	}
	storyID := strings.TrimSpace(req.Story)
	if storyID == "" {
		return nil, problem(400, "Missing story ID")
	}
	if restart {
		if _, err := b.restartStory(r.Context(), companionID, storyID); err != nil {
			return nil, err
		}
		return remoteStateResponse(b, r.Context(), companionID, "Started over on device."), nil
	}
	if _, err := b.openStory(r.Context(), companionID, storyID); err != nil {
		return nil, err
	}
	return remoteStateResponse(b, r.Context(), companionID, "Opened on device."), nil
}

func encodeHistoryCursor(c historyCursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(c.TS, 10) + ":" + c.ID))
}

func decodeHistoryCursor(raw string) (*historyCursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	tsRaw, id, ok := strings.Cut(string(data), ":")
	if !ok || id == "" {
		return nil, fmt.Errorf("invalid cursor")
	}
	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return nil, err
	}
	return &historyCursor{TS: ts, ID: id}, nil
}

func (b *larkServer) remoteHistory(ctx context.Context, tok remoteToken, query url.Values) (object, error) {
	if b.db == nil {
		return nil, problem(503, "History unavailable")
	}
	companionID := tok.CompanionID
	if b.history != nil {
		b.history.flush(companionID)
	}
	limit := remoteHistoryLimit
	if raw := query.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return nil, problem(400, "Expected a positive history limit")
		}
		limit = min(n, remoteHistoryLimitMax)
	}
	channel := strings.TrimSpace(query.Get("channel"))
	switch channel {
	case "", "all", "in_person", "deltachat":
	default:
		return nil, problem(400, "Unknown history filter")
	}
	q := strings.TrimSpace(query.Get("q"))
	if len(q) > remoteHistoryQueryMax {
		q = q[:remoteHistoryQueryMax]
	}
	var cursor *historyCursor
	if raw := query.Get("cursor"); raw != "" {
		var err error
		cursor, err = decodeHistoryCursor(raw)
		if err != nil {
			return nil, problem(400, "Invalid history cursor")
		}
	}

	// A scoped link shows in-person notes and only the Delta Chat messages
	// this contact sent. Companion replies and intercom traffic stay private.
	querySQL := `SELECT id,role,sender,text,channel,ts_ms FROM messages WHERE companion_id=?`
	args := []any{strings.ToLower(companionID)}
	switch channel {
	case "in_person":
		querySQL += ` AND channel='in_person'`
	case "deltachat":
		querySQL += ` AND channel='deltachat' AND role='user' AND LOWER(sender) IN (?,?)`
		args = append(args, tok.Contact, strings.ToLower(strings.TrimSpace(tok.ContactName)))
	default:
		querySQL += ` AND (channel='in_person' OR (channel='deltachat' AND role='user' AND LOWER(sender) IN (?,?)))`
		args = append(args, tok.Contact, strings.ToLower(strings.TrimSpace(tok.ContactName)))
	}
	if q != "" {
		pat := "%" + escapeLike(strings.ToLower(q)) + "%"
		querySQL += ` AND (LOWER(text) LIKE ? ESCAPE '\' OR LOWER(sender) LIKE ? ESCAPE '\')`
		args = append(args, pat, pat)
	}
	if cursor != nil {
		querySQL += ` AND ts_ms<=? AND (ts_ms<? OR (ts_ms=? AND id<?))`
		args = append(args, cursor.TS, cursor.TS, cursor.TS, cursor.ID)
	}
	querySQL += ` ORDER BY ts_ms DESC,id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := b.db.readDB().QueryContext(ctx, querySQL, args...)
	if err != nil {
		return nil, problem(502, "Cannot read history")
	}
	defer rows.Close()
	c := b.companionFor(companionID)
	items := []object{}
	for rows.Next() {
		var id, role, sender, text, rowChannel string
		var ts int64
		if err := rows.Scan(&id, &role, &sender, &text, &rowChannel, &ts); err != nil {
			return nil, problem(502, "Cannot read history")
		}
		items = append(items, object{
			"id":      id,
			"role":    role,
			"speaker": historySpeaker(role, sender, rowChannel, c.Name),
			"channel": rowChannel,
			"text":    strings.TrimSpace(stripLarkHeader(text)),
			"ts_ms":   ts,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, problem(502, "Cannot read history")
	}
	out := object{"items": items, "has_more": false}
	if len(items) > limit {
		items = items[:limit]
		out["items"] = items
		out["has_more"] = true
		oldest := items[len(items)-1]
		ts, _ := oldest["ts_ms"].(int64)
		out["next_cursor"] = encodeHistoryCursor(historyCursor{TS: ts, ID: str(oldest["id"])})
	}
	return out, nil
}

// sayOnCompanion and previewVoiceOnCompanion are shared by the Delta Chat
// commands and the remote page.
func (b *larkServer) sayOnCompanion(ctx context.Context, companionID, senderName string, chatID uint32, words string) {
	if b == nil {
		return
	}
	who := b.companionFor(companionID)
	b.chat(companionID, newChat("user", senderName, "/say "+words, "deltachat"), true)
	if b.hub != nil {
		n := intercomNotice(who, []string{companionID}, words, nil, b.voiceFor(companionID))
		n.intro = ""
		if chatID != 0 {
			n.reply = &turnReply{name: senderName, companionID: companionID, chatID: chatID}
			n.cardReply = senderName
		}
		b.hub.deliverNotice(ctx, n)
	}
}

func (b *larkServer) previewVoiceOnCompanion(companionID, voiceID string) {
	if b == nil || b.hub == nil {
		return
	}
	h := b.hub
	previewText := voicePreviewText(voiceID)
	h.mu.Lock()
	var targets []*terminalSession
	for _, d := range h.terminalsFor(companionID) {
		if h.active(d) {
			targets = append(targets, d)
		}
	}
	if len(targets) == 0 {
		h.mu.Unlock()
		return
	}
	if pcm := b.clips.peek(voiceID, previewText); len(pcm) > 0 {
		for _, d := range targets {
			h.playPCM(d, pcm)
		}
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()

	if w := b.worker; w != nil && w.ready.Load() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), ttsWait)
			defer cancel()
			pcm, err := b.clips.get(ctx, w, voiceID, previewText)
			if err != nil || len(pcm) == 0 {
				return
			}
			h.mu.Lock()
			for _, d := range h.terminalsFor(companionID) {
				if h.active(d) && b.voiceFor(companionID) == voiceID {
					h.playPCM(d, pcm)
				}
			}
			h.mu.Unlock()
		}()
		return
	}
	h.mu.Lock()
	for _, d := range targets {
		if h.active(d) {
			h.chime(d, "tap")
		}
	}
	h.mu.Unlock()
}

func (b *larkServer) clearStoryProgress(ctx context.Context, companionID, storyID string) error {
	if b == nil || b.db == nil {
		return nil
	}
	_, err := b.db.ExecContext(ctx, `DELETE FROM story_progress WHERE companion_id=? AND story_id=?`, companionID, storyID)
	return err
}

func (b *larkServer) companionLiveState(companionID string) string {
	if b == nil || b.hub == nil {
		return "offline"
	}
	b.hub.mu.Lock()
	defer b.hub.mu.Unlock()
	terms := b.hub.terminalsFor(companionID)
	if len(terms) == 0 {
		return "offline"
	}
	best := b.hub.voiceOwner(companionID)
	if best == nil {
		best = terms[0]
	}
	switch {
	case best.asleep:
		return "sleeping"
	case best.recording.active():
		return "listening"
	case best.ttsStarted || best.awaitAck:
		return "speaking"
	case best.busy:
		return "thinking"
	case best.pose.mood == "listening" || best.pose.mood == "thinking" || best.pose.mood == "speaking" || best.pose.mood == "sleepy":
		if best.pose.mood == "sleepy" {
			return "sleeping"
		}
		return best.pose.mood
	default:
		return "idle"
	}
}

// remoteTunnelAPI exposes admin-only tunnel state for `lark tunnels`.
func (b *larkServer) remoteTunnelAPI(w http.ResponseWriter, r *http.Request, path string) (any, error) {
	m := b.remote
	if m == nil {
		return nil, problem(503, "Remote controls unavailable")
	}
	switch {
	case path == "/api/remote/tunnels" && r.Method == http.MethodGet:
		m.mu.Lock()
		defer m.mu.Unlock()
		list := make([]object, 0, len(m.routes))
		for id, route := range m.routes {
			contacts := 0
			for _, tok := range m.tokens {
				if tok.CompanionID == id {
					contacts++
				}
			}
			list = append(list, object{
				"companion_id": id,
				"url":          route.URL,
				"route_name":   route.RouteName,
				"active":       m.live[id] != nil,
				"contacts":     contacts,
			})
		}
		slices.SortFunc(list, func(a, b object) int { return strings.Compare(str(a["companion_id"]), str(b["companion_id"])) })
		return object{"tunnels": list}, nil
	case strings.HasPrefix(path, "/api/remote/tunnels/") && r.Method == http.MethodDelete:
		id := strings.ToLower(strings.TrimPrefix(path, "/api/remote/tunnels/"))
		if !validCompanionID(id) {
			return nil, problem(400, "Invalid companion ID")
		}
		if err := m.stopCompanion(r.Context(), id); err != nil {
			return nil, err
		}
		w.WriteHeader(204)
		return nil, nil
	}
	return nil, problem(404, "Not found")
}
