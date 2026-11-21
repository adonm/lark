package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
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

func randomHex(n int) string {
	raw := make([]byte, n)
	rand.Read(raw)
	return hex.EncodeToString(raw)
}

func tokenMatches(values []string, want string) bool {
	return want != "" && len(values) == 1 && subtle.ConstantTimeCompare([]byte(values[0]), []byte(want)) == 1
}

type tunnelRunner interface {
	run(context.Context, ...string) ([]byte, error)
}

type tunnelCLI string

func (bin tunnelCLI) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, string(bin), args...).CombinedOutput()
}

func findTunnelCLI(home string) string {
	for _, candidate := range []string{os.Getenv("LARK_OPENTUNNEL_BIN"), "opentunnel", filepath.Join(home, ".local/bin/opentunnel")} {
		if candidate != "" {
			if bin, err := exec.LookPath(candidate); err == nil {
				return bin
			}
		}
	}
	return ""
}

type tunnelRecord struct {
	DeviceID  string `json:"device_id"`
	RouteName string `json:"route_name"`
	URL       string `json:"url"`
	LocalPort int    `json:"local_port"`
	Token     string `json:"scoped_token"`
	Profile   string `json:"profile"`
	CreatedAt string `json:"created_at,omitempty"`
}

var (
	dnsLabel   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	dnsHost    = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{1,62}$`)
	routeURL   = regexp.MustCompile(`https://[a-z0-9.-]+`)
	scopeKey   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	profileKey = regexp.MustCompile(`^lark-[0-9a-f]{24}$`)
)

func routeAddress(raw string) (string, string) {
	for _, address := range routeURL.FindAllString(raw, -1) {
		host := strings.TrimPrefix(address, "https://")
		route, suffix, _ := strings.Cut(host, ".")
		if dnsHost.MatchString(host) && strings.Contains(suffix, ".") {
			return route, address
		}
	}
	return "", ""
}

func (v tunnelRecord) public(active bool) object {
	return object{"device_id": v.DeviceID, "route_name": v.RouteName, "url": v.URL,
		"share_url": v.URL + "/app?token=" + v.Token, "local_port": v.LocalPort, "active": active}
}

type tunnelManager struct {
	mu     sync.Mutex
	b      *bridge
	bin    string
	runner tunnelRunner
	store  *store[tunnelRecord] // manager mutex owns all access
	live   map[string]*scopeServer
	closed bool
}

func newTunnelManager(b *bridge, bin string, runner tunnelRunner) *tunnelManager {
	return &tunnelManager{b: b, bin: bin, runner: runner, live: make(map[string]*scopeServer), store: tunnelStore(b.db)}
}

func (m *tunnelManager) restore() {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := m.store.all()
	if err != nil {
		log.Print("Lark could not read saved tunnels")
		return
	}
	for id, record := range data {
		if v, ok := m.b.virtualByID(id); ok {
			if scope, _, err := m.listen(v, record.Token, record.LocalPort); err == nil {
				m.live[id] = scope
			} else {
				log.Printf("Lark could not restore scoped listener for %s", id)
			}
		}
	}
}

func (m *tunnelManager) info(id string) object {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok, _ := m.store.get(id); ok {
		out := v.public(m.live[id] != nil)
		delete(out, "device_id")
		delete(out, "local_port")
		return out
	}
	return nil
}

func (m *tunnelManager) status(ctx context.Context) object {
	out := object{"installed": m.runner != nil, "binary": m.bin, "service_running": false, "hostname": ""}
	if m.runner == nil {
		out["error"] = "OpenTunnel CLI not found"
		return out
	}
	raw, err := m.runner.run(ctx, "status")
	for line := range strings.SplitSeq(string(raw), "\n") {
		key, value, _ := strings.Cut(strings.TrimSpace(line), ":")
		value = strings.TrimSpace(value)
		switch key {
		case "Hostname":
			if dnsHost.MatchString(value) {
				out["hostname"] = value
			}
		case "Service":
			state := strings.Fields(strings.ToLower(value))
			out["service_running"] = len(state) != 0 && slices.Contains([]string{"running", "connected", "active"}, state[0])
		}
	}
	if err != nil {
		out["error"] = "OpenTunnel status failed"
	}
	return out
}

func (b *bridge) tunnelAPI(w http.ResponseWriter, r *http.Request, path string) (any, error) {
	m := b.tunnels
	if m == nil {
		return nil, problem(503, "Tunnel manager unavailable")
	}
	if path == "/api/tunnel/status" && r.Method == http.MethodGet {
		return m.status(r.Context()), nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, problem(503, "Tunnel manager shutting down")
	}
	switch {
	case path == "/api/tunnels" && r.Method == http.MethodGet:
		data, err := m.store.all()
		if err != nil {
			return nil, err
		}
		out := make([]object, 0, len(data))
		for _, v := range data {
			out = append(out, v.public(m.live[v.DeviceID] != nil))
		}
		slices.SortFunc(out, func(a, b object) int { return strings.Compare(str(a["device_id"]), str(b["device_id"])) })
		return object{"tunnels": out}, nil
	case path == "/api/tunnels" && r.Method == http.MethodPost:
		obj, err := jsonBody(r)
		if err != nil {
			return nil, err
		}
		info, err := m.addLocked(r.Context(), str(obj["device_id"]))
		if err != nil {
			return nil, err
		}
		sendJSON(w, 201, info)
		return nil, nil
	case strings.HasPrefix(path, "/api/tunnels/") && r.Method == http.MethodDelete:
		err := m.removeLocked(r.Context(), strings.TrimPrefix(path, "/api/tunnels/"), true)
		if err == nil {
			w.WriteHeader(204)
		}
		return nil, err
	}
	return nil, problem(404, "Not found")
}

func (m *tunnelManager) addLocked(ctx context.Context, id string) (object, error) {
	v, ok := m.b.virtualByID(id)
	if !ok || !virtualID.MatchString(id) {
		return nil, problem(404, "Tunnel requires an existing virtual client")
	}
	values, exists, err := m.store.get(id)
	if err != nil {
		return nil, err
	}
	if exists {
		if m.live[id] == nil {
			server, _, err := m.listen(v, values.Token, values.LocalPort)
			if err != nil {
				return nil, problem(409, "Persisted tunnel port is unavailable")
			}
			m.live[id] = server
		}
		return values.public(true), nil
	}
	if m.runner == nil {
		return nil, problem(503, "OpenTunnel CLI not found")
	}
	record := tunnelRecord{DeviceID: id, Token: randomHex(16), Profile: "lark-" + randomHex(12), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	server, port, err := m.listen(v, record.Token, 0)
	if err != nil {
		return nil, problem(502, "Cannot start scoped listener")
	}
	record.LocalPort = port
	// A fresh profile prevents route add from reusing a user's route for this target.
	raw, err := m.runner.run(ctx, "route", "add", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), "--profile", record.Profile)
	record.RouteName, record.URL = routeAddress(string(raw))
	if err != nil || record.RouteName == "" {
		server.close()
		return nil, problem(502, "OpenTunnel route add did not return a usable URL")
	}
	if err := m.store.save(id, record, false); err != nil {
		server.close()
		return nil, err
	}
	m.live[id] = server
	return record.public(true), nil
}

func (m *tunnelManager) removeLocked(ctx context.Context, id string, required bool) error {
	v, ok, err := m.store.get(id)
	if err != nil {
		return err
	}
	if !ok {
		if required {
			return problem(404, "Tunnel not found")
		}
		return nil
	}
	if m.runner == nil {
		return problem(503, "OpenTunnel CLI not found")
	}
	if _, err := m.runner.run(ctx, "route", "remove", v.RouteName, "--profile", v.Profile); err != nil {
		return problem(502, "OpenTunnel route remove failed")
	}
	if s := m.live[id]; s != nil {
		delete(m.live, id)
		s.close()
	}
	return m.store.save(id, tunnelRecord{}, true)
}

func (m *tunnelManager) shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for _, s := range m.live {
		s.close()
	}
	clear(m.live)
}

type scopeServer struct {
	cancel context.CancelFunc
	server *http.Server
}

func (s *scopeServer) close() {
	s.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if s.server.Shutdown(ctx) != nil {
		_ = s.server.Close()
	}
}

func (m *tunnelManager) listen(v virtualClient, token string, port int) (*scopeServer, int, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, 0, err
	}
	ctx, cancel := context.WithCancel(m.b.root)
	s := &scopeServer{cancel: cancel}
	s.server = &http.Server{
		Handler:           m.b.scoped(v, token),
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 105 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	go func() { _ = s.server.Serve(ln) }()
	return s, ln.Addr().(*net.TCPAddr).Port, nil
}

func (b *bridge) scoped(v virtualClient, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		path := r.URL.EscapedPath()
		if path == "/app/parent.js" {
			sendError(w, problem(404, "Not found"))
			return
		}
		if r.Method == http.MethodGet {
			if path == "/" {
				location := "/app"
				if tokenMatches(r.URL.Query()["token"], token) {
					location += "?token=" + url.QueryEscape(token)
				}
				http.Redirect(w, r, location, http.StatusFound)
				return
			}
			if b.servePhonePWA(w, r, path, &v, token) || serveApp(w, path) {
				return
			}
		}
		messageID, messages := messagesDevice(path)
		voiceID, voice := voiceDevice(path)
		volumeID, volume := deviceSettingID(path, "volume")
		readPortrait := strings.HasPrefix(path, "/api/portraits/")
		timers := r.Method == http.MethodGet && path == "/api/timers" ||
			r.Method == http.MethodDelete && strings.HasPrefix(path, "/api/timers/")
		get := r.Method == http.MethodGet && (path == "/ws" || path == "/api/scope" || messages || readPortrait)
		post := r.Method == http.MethodPost && (path == "/api/chat" || voice || volume)
		if !get && !post && !timers {
			sendError(w, problem(404, "Not found"))
			return
		}
		if !tokenHeaderMatches(r.Header, "Token", token) && !tokenMatches(r.URL.Query()["token"], token) {
			sendError(w, problem(401, "Unauthorized"))
			return
		}
		switch {
		case get && readPortrait:
			sendResult(w, nil, b.servePortrait(w, r, path, v.ID))
		case timers:
			value, err := b.timerAPI(w, r, path, v.ID)
			sendResult(w, value, err)
		case path == "/api/scope":
			companion := b.companionFor(peer{DeviceID: v.ID, Character: v.Character})
			out := object{"scoped": true, "device_id": v.ID, "character": companion.Name, "companion": companion.companionLook, "voices": voiceCatalog,
				"voice": b.voices.get(v.ID, deviceTheme(companion.Name))}
			if b.ws != nil {
				b.ws.deviceStatus(out, v.ID, false)
			} else {
				out["auto_volume"], out["volume"] = b.volumePrefs(v.ID)
			}
			sendJSON(w, 200, out)
		case path == "/ws" && b.ws != nil:
			if err := b.ws.serveScope(w, r, &v); err != nil {
				sendError(w, err)
			}
		case get && messages:
			if messageID != v.ID {
				sendError(w, problem(403, "Device out of scope"))
				return
			}
			value, err := b.messages(r, messageID)
			sendResult(w, value, err)
		case post && voice:
			if voiceID != v.ID {
				sendError(w, problem(403, "Device out of scope"))
				return
			}
			value, err := b.voiceAPI(r, voiceID)
			sendResult(w, value, err)
		case post && volume:
			if volumeID != v.ID {
				sendError(w, problem(403, "Device out of scope"))
				return
			}
			value, err := b.volumeAPI(r, volumeID)
			sendResult(w, value, err)
		case post:
			value, err := b.chatAPI(r, v.ID)
			sendResult(w, value, err)
		default:
			sendError(w, problem(404, "Not found"))
		}
	})
}
