package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
