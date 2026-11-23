package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	imagecolor "image/color"
	"image/png"
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

	"golang.org/x/image/draw"
)

const (
	remotePath       = "/remote"
	remoteCookieName = "lark_remote"
	remotePoll       = 750 * time.Millisecond
	remoteKeepAlive  = 15 * time.Second
	remoteTokenBytes = 16
)

//go:embed web/remote/*.html web/remote/*.css web/remote/*.js
var remoteAssets embed.FS

type tunnelRunner func(context.Context, ...string) ([]byte, error)

func tunnelCLI(bin string) tunnelRunner {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
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
	Contact     string
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
		var key, hash string
		if err := rows.Scan(&key, &hash); err != nil {
			continue
		}
		rest, ok := strings.CutPrefix(key, "remote_token:")
		if !ok {
			continue
		}
		companionID, contact, ok := strings.Cut(rest, ":")
		if !ok || hash == "" {
			continue
		}
		m.mu.Lock()
		if _, exists := m.routes[companionID]; exists {
			m.tokens[hash] = remoteToken{CompanionID: companionID, Contact: contact}
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

func (m *remoteManager) issue(ctx context.Context, companionID, contact string) (string, error) {
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
	raw := randomHex(remoteTokenBytes)
	hash := tokenHash(raw)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", problem(503, "Remote controls shutting down")
	}
	for oldHash, tok := range m.tokens {
		if tok.CompanionID == companionID && tok.Contact == contact {
			delete(m.tokens, oldHash)
			m.deleteMeta(ctx, remoteTokenKey(companionID, contact))
		}
	}

	route := m.routes[companionID]
	if route == nil {
		scope, port, err := m.listen(companionID, 0)
		if err != nil {
			return "", problem(502, "Cannot start remote listener")
		}
		profile := "lark-" + randomHex(12)
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
		m.routes[companionID] = route
		m.live[companionID] = scope
	} else if _, err := m.ensureScopeLocked(companionID); err != nil {
		return "", err
	}

	m.tokens[hash] = remoteToken{CompanionID: companionID, Contact: contact}
	if err := m.saveToken(ctx, companionID, contact, hash); err != nil {
		delete(m.tokens, hash)
		return "", err
	}
	return strings.TrimRight(route.URL, "/") + "/remote?token=" + url.QueryEscape(raw), nil
}

func (m *remoteManager) revokeContact(ctx context.Context, companionID, contact string) (int, error) {
	if m == nil {
		return 0, nil
	}
	companionID = strings.ToLower(strings.TrimSpace(companionID))
	contact = strings.ToLower(strings.TrimSpace(contact))
	m.mu.Lock()
	defer m.mu.Unlock()
	for hash, tok := range m.tokens {
		if tok.CompanionID == companionID && tok.Contact == contact {
			delete(m.tokens, hash)
		}
	}
	m.deleteMeta(ctx, remoteTokenKey(companionID, contact))
	left := 0
	for _, tok := range m.tokens {
		if tok.CompanionID == companionID {
			left++
		}
	}
	if left == 0 {
		if err := m.stopLocked(ctx, companionID); err != nil {
			return 0, err
		}
	}
	return left, nil
}

func (m *remoteManager) stopCompanion(ctx context.Context, companionID string) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for hash, tok := range m.tokens {
		if tok.CompanionID == companionID {
			delete(m.tokens, hash)
		}
	}
	return m.stopLocked(ctx, companionID)
}

func (m *remoteManager) stopLocked(ctx context.Context, companionID string) error {
	route := m.routes[companionID]
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
	if scope := m.live[companionID]; scope != nil {
		scope.close()
	}
	delete(m.live, companionID)
	delete(m.routes, companionID)
	m.deleteMeta(ctx, remoteRouteKey(companionID))
	if m.b == nil || m.b.db == nil {
		return nil
	}
	rows, err := m.b.db.QueryContext(ctx, `SELECT key FROM meta WHERE key LIKE ?`, "remote_token:"+companionID+":%")
	if err == nil {
		var keys []string
		for rows.Next() {
			var key string
			if rows.Scan(&key) == nil {
				keys = append(keys, key)
			}
		}
		rows.Close()
		for _, key := range keys {
			m.deleteMeta(ctx, key)
		}
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

func (m *remoteManager) saveToken(ctx context.Context, companionID, contact, hash string) error {
	if m.b == nil || m.b.db == nil {
		return nil
	}
	if err := m.b.db.setMeta(ctx, remoteTokenKey(companionID, contact), hash); err != nil {
		return problem(502, "Cannot save remote token")
	}
	return nil
}

func (m *remoteManager) deleteMeta(ctx context.Context, key string) {
	if m == nil || m.b == nil || m.b.db == nil {
		return
	}
	_, _ = m.b.db.ExecContext(ctx, `DELETE FROM meta WHERE key=?`, key)
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
	case "/remote/style.css", "/remote/app.js", "/remote/oat.min.css", "/remote/oat.min.js":
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

func (b *larkServer) serveRemoteAsset(w http.ResponseWriter, path string) {
	name := strings.TrimPrefix(path, remotePath+"/")
	switch name {
	case "style.css", "app.js", "oat.min.css", "oat.min.js", "icon.svg":
	default:
		sendError(w, problem(404, "Not found"))
		return
	}
	data, err := remoteAssets.ReadFile("web/remote/" + name)
	if err != nil {
		sendError(w, problem(404, "Not found"))
		return
	}
	kind := "application/octet-stream"
	switch filepath.Ext(name) {
	case ".css":
		kind = "text/css; charset=utf-8"
	case ".js":
		kind = "text/javascript; charset=utf-8"
	case ".svg":
		kind = "image/svg+xml"
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; manifest-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	sendBytes(w, 200, kind, data)
}

func (b *larkServer) serveRemotePage(w http.ResponseWriter, r *http.Request, tok remoteToken, raw string) {
	index, err := remoteAssets.ReadFile("web/remote/index.html")
	if err != nil {
		sendError(w, problem(500, "Remote page unavailable"))
		return
	}
	query := url.QueryEscape(raw)
	page := strings.ReplaceAll(string(index), "{{TOKEN}}", query)
	page = strings.ReplaceAll(page, "{{TOKEN_HTML}}", htmlEscape(raw))
	page = strings.ReplaceAll(page, "{{NAME}}", htmlEscape(remoteDisplayName(b.companionFor(tok.CompanionID))))
	if secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"); secure {
		http.SetCookie(w, &http.Cookie{Name: remoteCookieName, Value: raw, Path: remotePath,
			HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	} else {
		http.SetCookie(w, &http.Cookie{Name: remoteCookieName, Value: raw, Path: remotePath,
			HttpOnly: true, SameSite: http.SameSiteLaxMode})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; manifest-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.WriteHeader(200)
	_, _ = w.Write([]byte(page))
}

func htmlEscape(s string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;")
	return replacer.Replace(s)
}

func remoteDisplayName(c companionState) string {
	name := strings.TrimSpace(c.Name)
	if name == "" {
		name = strings.TrimSpace(c.ID)
	}
	if name == "" {
		return "Lark"
	}
	return name
}

func (b *larkServer) serveRemotePWA(w http.ResponseWriter, r *http.Request, path string, tok remoteToken, raw string) {
	c := b.companionFor(tok.CompanionID)
	name := remoteDisplayName(c)
	switch path {
	case remotePath + "/pwa/manifest.webmanifest":
		query := url.QueryEscape(raw)
		icons := []object{}
		for _, icon := range []struct {
			name string
			size int
			use  string
		}{
			{"icon-192.png", 192, "any"},
			{"icon-512.png", 512, "any"},
			{"maskable-512.png", 512, "maskable"},
		} {
			icons = append(icons, object{
				"src":     fmt.Sprintf("%s/pwa/%s?token=%s", remotePath, icon.name, query),
				"type":    "image/png",
				"sizes":   fmt.Sprintf("%dx%d", icon.size, icon.size),
				"purpose": icon.use,
			})
		}
		data, _ := json.Marshal(object{
			"id":               remotePath + "?companion=" + url.QueryEscape(c.ID),
			"start_url":        remotePath + "?token=" + query,
			"scope":            remotePath,
			"name":             name,
			"short_name":       name,
			"display":          "standalone",
			"orientation":      "portrait",
			"theme_color":      "#6f5bd3",
			"background_color": "#faf7f2",
			"icons":            icons,
		})
		w.Header().Set("Content-Type", "application/manifest+json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		sendBytes(w, 200, "application/manifest+json", data)
		return
	case remotePath + "/pwa/icon-192.png", remotePath + "/pwa/icon-512.png", remotePath + "/pwa/maskable-512.png":
		size := 512
		if strings.Contains(path, "192") {
			size = 192
		}
		icon, err := b.remoteIcon(c, size, strings.Contains(path, "maskable"))
		if err != nil {
			sendError(w, problem(502, "Cannot render remote icon"))
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		sendBytes(w, 200, "image/png", icon)
		return
	}
	sendError(w, problem(404, "Not found"))
}

func (b *larkServer) remoteIcon(c companionState, size int, maskable bool) ([]byte, error) {
	raw, err := companionAvatarPNG(b, c)
	if err != nil {
		return nil, err
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
	if maskable {
		bg := imagecolor.NRGBA{R: 250, G: 247, B: 242, A: 255}
		draw.Draw(dst, dst.Bounds(), &image.Uniform{C: bg}, image.Point{}, draw.Src)
		pad := size / 10
		draw.CatmullRom.Scale(dst, image.Rect(pad, pad, size-pad, size-pad), src, src.Bounds(), draw.Over, nil)
	} else {
		draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	}
	var out bytes.Buffer
	if err := png.Encode(&out, dst); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
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

	initial := b.remoteSnapshot(r.Context(), companionID, true)
	if !send(initial) {
		return
	}
	last := remoteSnapshotKey(initial)
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
			if key == last {
				continue
			}
			last = key
			if !send(snapshot) {
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
		"id":             c.ID,
		"name":           c.Name,
		"form":           c.Form,
		"voice":          b.voiceFor(c.ID),
		"listen_seconds": c.ListenSeconds,
		"live_state":     b.companionLiveState(c.ID),
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
			if auto, ok := te["auto_volume"]; ok {
				entry["auto_volume"] = auto
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
	if b.ws != nil {
		for _, t := range b.ws.companionTerminals(companionID) {
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
	if list, err := b.listStories(ctx); err == nil {
		for _, st := range list {
			item := object{"name": st.ID, "title": st.Title, "passages": st.Passages, "progress": 0}
			if prog, ok := progressByStory[st.ID]; ok {
				pct := 10
				if cs, err := parseTwee(st.Twee); err == nil && len(cs.Passages) > 0 {
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
	case path == remotePath+"/api/history" && r.Method == http.MethodGet:
		page, err := b.remoteHistory(r.Context(), tok.CompanionID, r.URL.Query())
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
			http.SetCookie(w, &http.Cookie{Name: remoteCookieName, Value: "", Path: remotePath, MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
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
	args, err := jsonBody(r)
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(str(args["text"]))
	if text == "" {
		return nil, problem(400, "Enter something to say.")
	}
	clean, err := spokenText(text)
	if err != nil {
		return nil, problem(400, err.Error())
	}
	b.sayOnCompanion(r.Context(), companionID, "Remote", clean)
	return remoteStateResponse(b, r.Context(), companionID, "Said it."), nil
}

func (b *larkServer) remoteListen(r *http.Request, companionID string) (any, error) {
	args, err := jsonBody(r)
	if err != nil {
		return nil, err
	}
	seconds, ok := companionInt(args["seconds"])
	if !ok || !slices.Contains([]int{0, 30, 60, 120, 300}, seconds) {
		return nil, problem(400, "Invalid listening duration.")
	}
	if _, err := b.patchCompanion(r.Context(), companionID, object{"listen_seconds": seconds}); err != nil {
		return nil, err
	}
	return remoteStateResponse(b, r.Context(), companionID, "Listening mode updated."), nil
}

func (b *larkServer) remoteVolume(r *http.Request, companionID string) (any, error) {
	args, err := jsonBody(r)
	if err != nil {
		return nil, err
	}
	terminalID := strings.ToLower(strings.TrimSpace(str(args["terminal"])))
	if terminalID == "" {
		return nil, problem(400, "Terminal is required.")
	}
	t, ok := b.terminalByID(terminalID)
	if !ok || !strings.EqualFold(t.CompanionID, companionID) {
		return nil, problem(403, "Terminal out of scope")
	}
	if b.ws == nil {
		return nil, problem(503, "Terminal hub unavailable")
	}
	volArgs := object{}
	if raw, exists := args["auto"]; exists {
		auto, ok := raw.(bool)
		if !ok {
			return nil, problem(400, "Expected boolean auto.")
		}
		volArgs["auto_volume"] = auto
	}
	if raw, exists := args["volume"]; exists {
		level, ok := companionInt(raw)
		if !ok || level < 0 || level > 100 {
			return nil, problem(400, "Volume must be 0..100.")
		}
		volArgs["volume"] = level
	}
	if len(volArgs) == 0 {
		return nil, problem(400, `Expected {"volume":0..100} or {"auto":true}`)
	}
	if _, err := b.ws.terminalVolumeControl(r.Context(), terminalID, volArgs); err != nil {
		return nil, err
	}
	return remoteStateResponse(b, r.Context(), companionID, "Volume updated."), nil
}

func (b *larkServer) remoteVoice(r *http.Request, companionID string) (any, error) {
	args, err := jsonBody(r)
	if err != nil {
		return nil, err
	}
	voice := str(args["voice"])
	if !b.voices.allowed(voice) {
		return nil, problem(400, "Unknown voice.")
	}
	if _, err := b.patchCompanion(r.Context(), companionID, object{"voice": voice}); err != nil {
		return nil, err
	}
	b.previewVoiceOnCompanion(companionID, voice)
	return remoteStateResponse(b, r.Context(), companionID, "Voice updated."), nil
}

func (b *larkServer) remoteStory(r *http.Request, companionID string, restart bool) (any, error) {
	args, err := jsonBody(r)
	if err != nil {
		return nil, err
	}
	storyID := strings.TrimSpace(str(args["story"]))
	if storyID == "" {
		storyID = strings.TrimSpace(str(args["app"]))
	}
	if storyID == "" {
		return nil, problem(400, "Missing story ID.")
	}
	st, err := b.loadStory(r.Context(), storyID)
	if err != nil {
		return nil, problem(404, "Story not found.")
	}
	if restart {
		if err := b.clearStoryProgress(r.Context(), companionID, st.ID); err != nil {
			return nil, problem(502, "Could not reset progress.")
		}
	}
	if b.ws != nil {
		b.ws.mu.Lock()
		for _, d := range b.ws.terminalsFor(companionID) {
			if b.ws.active(d) {
				b.ws.openStory(d, st, false)
			}
		}
		b.ws.mu.Unlock()
	}
	message := "Opened on device."
	if restart {
		message = "Started over on device."
	}
	return remoteStateResponse(b, r.Context(), companionID, message), nil
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

func (b *larkServer) remoteHistory(ctx context.Context, companionID string, query url.Values) (object, error) {
	if b.db == nil {
		return nil, problem(503, "History unavailable")
	}
	if b.history != nil {
		b.history.flush(companionID)
	}
	limit := 20
	if raw := query.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return nil, problem(400, "Expected a positive history limit.")
		}
		limit = min(n, 50)
	}
	channel := strings.TrimSpace(query.Get("channel"))
	switch channel {
	case "", "all", "in_person", "deltachat", "intercom":
	default:
		return nil, problem(400, "Unknown history filter.")
	}
	q := strings.TrimSpace(query.Get("q"))
	if len(q) > 100 {
		q = q[:100]
	}
	var cursor *historyCursor
	if raw := query.Get("cursor"); raw != "" {
		var err error
		cursor, err = decodeHistoryCursor(raw)
		if err != nil {
			return nil, problem(400, "Invalid history cursor.")
		}
	}
	if cursor == nil && query.Get("before_ts") != "" && query.Get("before_id") != "" {
		ts, err := strconv.ParseInt(query.Get("before_ts"), 10, 64)
		if err != nil {
			return nil, problem(400, "Invalid history cursor.")
		}
		cursor = &historyCursor{TS: ts, ID: query.Get("before_id")}
	}

	querySQL := `SELECT id,role,sender,text,source,channel,ts_ms FROM messages WHERE companion_id=?`
	args := []any{strings.ToLower(companionID)}
	switch channel {
	case "in_person":
		querySQL += ` AND (channel='in_person' OR (channel='' AND source IN ('voice','typed','tap','text')))`
	case "deltachat":
		querySQL += ` AND (channel='deltachat' OR (channel='' AND source='deltachat'))`
	case "intercom":
		querySQL += ` AND (channel='intercom' OR (channel='' AND source='intercom'))`
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
		return nil, problem(502, "Cannot read history.")
	}
	defer rows.Close()
	c := b.companionFor(companionID)
	items := []object{}
	for rows.Next() {
		var id, role, sender, text, source, rowChannel string
		var ts int64
		if err := rows.Scan(&id, &role, &sender, &text, &source, &rowChannel, &ts); err != nil {
			return nil, problem(502, "Cannot read history.")
		}
		if rowChannel == "" {
			rowChannel = channelForSource(source)
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
		return nil, problem(502, "Cannot read history.")
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
func (b *larkServer) sayOnCompanion(ctx context.Context, companionID, senderName, words string) {
	if b == nil {
		return
	}
	who := b.companionFor(companionID)
	b.chat(companionID, newChat("user", senderName, "/say "+words, "deltachat"), true)
	if b.ws != nil {
		b.ws.deliverNotice(ctx, terminalNotice{
			recipientIDs: []string{companionID},
			sender:       who,
			text:         words,
			speakText:    words,
			voice:        b.voiceFor(companionID),
			chime:        "intercom",
			showCard:     true,
			replyTo:      senderName,
		})
	}
}

func (b *larkServer) previewVoiceOnCompanion(companionID, voiceID string) {
	if b == nil || b.ws == nil {
		return
	}
	h := b.ws
	previewText := voicePreviewText(voiceID)
	h.mu.Lock()
	var targets []*wsTerminal
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
	if b == nil || b.ws == nil {
		return "offline"
	}
	b.ws.mu.Lock()
	defer b.ws.mu.Unlock()
	terms := b.ws.terminalsFor(companionID)
	if len(terms) == 0 {
		return "offline"
	}
	best := b.ws.voiceOwner(companionID)
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
