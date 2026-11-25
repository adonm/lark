// Package edge is Lark's real-time media edge: the latency-critical silicon
// path only. It speaks the firmware's existing v1 binary protocol on TCP/UDP,
// renders screens with internal/pixel, runs speech DSP with internal/audio,
// and exposes a small NDJSON command/event API to the TypeScript brain over a
// Unix socket.
//
// It deliberately has no knowledge of Delta Chat, OpenCode, SQLite, accounts
// or conversation state. The brain owns meaning; the edge owns timing.
package edge

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	DefaultAddr = ":8766"
	// DefaultSock is overridden by LARK_EDGE_SOCK. The directory is created
	// with 0700 and the socket with 0600: only the service user may attach.
	DefaultSock = "~/.local/share/lark/edge.sock"
)

// Config controls one edge process. Tests inject fakes; production leaves the
// model fields nil so Start loads the real Parakeet, Kokoro, Smart Turn and
// Silero models in the background.
type Config struct {
	Addr    string // TCP terminal address; ":0" picks an ephemeral port
	Sock    string // Unix IPC path; empty uses LARK_EDGE_SOCK or DefaultSock
	Token   string // LARK_TOKEN; empty also reads the environment
	DataDir string // model root; empty uses LARK_DATA_DIR or ~/.local/share/lark

	Transcriber  Transcriber // optional injection
	Synthesizer  Synthesizer // optional injection
	TurnDetector TurnDetector
	VADFactory   VADFactory

	DisableModels bool // start without touching model files (tests)
	Logf          func(format string, args ...any)
}

// Edge is the running service: terminal TCP/UDP server plus the brain socket.
type Edge struct {
	cfg  Config
	logf func(string, ...any)

	server *terminalServer
	ipc    *ipcServer
	models *Models

	terminalsMu sync.RWMutex
	terminals   map[string]*Terminal

	assetMu sync.Mutex
	assets  map[string]*assetEntry
	cache   *clipCache

	brainMu sync.RWMutex
	brains  map[*ipcClient]struct{}

	streamSeq atomic.Uint64
	closed    atomic.Bool
	closeOnce sync.Once
	done      chan struct{}
}

// New binds the terminal listener and the Unix socket but starts no model
// loading. Call Start to run. Binding is synchronous so callers can publish
// the real addresses before hardware connects.
func New(cfg Config) (*Edge, error) {
	if cfg.Addr == "" {
		cfg.Addr = DefaultAddr
	}
	if cfg.Token == "" {
		cfg.Token = os.Getenv("LARK_TOKEN")
	}
	if cfg.DataDir == "" {
		cfg.DataDir = defaultDataDir()
	}
	logf := cfg.Logf
	if logf == nil {
		logf = log.Printf
	}
	e := &Edge{
		cfg:       cfg,
		logf:      logf,
		models:    newModels(),
		terminals: make(map[string]*Terminal),
		brains:    make(map[*ipcClient]struct{}),
		assets:    make(map[string]*assetEntry),
		cache:     newClipCache(24 << 20),
		done:      make(chan struct{}),
	}
	if err := e.models.install(cfg); err != nil {
		return nil, err
	}
	srv, err := newTerminalServer(e, cfg.Addr)
	if err != nil {
		return nil, err
	}
	e.server = srv
	ipc, err := newIPCServer(e, resolveSock(cfg.Sock))
	if err != nil {
		srv.close()
		return nil, err
	}
	e.ipc = ipc
	return e, nil
}

// Start launches the listeners and background model loads. It returns
// immediately; ready events follow the actual model state.
func (e *Edge) Start() {
	e.server.start()
	e.ipc.start()
	e.emitReady()
	if !e.cfg.DisableModels {
		go e.loadModels()
	}
}

func (e *Edge) loadModels() {
	for _, name := range modelNames {
		name := name
		go func() {
			err := e.models.load(name, e.cfg.DataDir)
			if err != nil {
				e.models.setError(name, err)
				e.logf("lark-edge: %s: %v", name, err)
			} else {
				e.models.setReady(name)
			}
			e.emitReady()
		}()
	}
}

func (e *Edge) close() {
	e.closeOnce.Do(func() {
		e.closed.Store(true)
		close(e.done)
		if e.ipc != nil {
			e.ipc.close()
		}
		if e.server != nil {
			e.server.close()
		}
		e.models.close()
	})
}

// Close stops listeners, terminals and models. It is safe to call twice.
func (e *Edge) Close() { e.close() }

// TCPAddr is the bound terminal TCP address (useful with ":0").
func (e *Edge) TCPAddr() net.Addr {
	if e.server == nil || e.server.tcp == nil {
		return nil
	}
	return e.server.tcp.Addr()
}

// UDPAddr is the bound terminal UDP address.
func (e *Edge) UDPAddr() net.Addr {
	if e.server == nil || e.server.udp == nil {
		return nil
	}
	return e.server.udp.LocalAddr()
}

// SockPath is the Unix socket the brain connects to.
func (e *Edge) SockPath() string {
	if e.ipc == nil {
		return ""
	}
	return e.ipc.path
}

// Terminal returns a connected terminal by id.
func (e *Edge) Terminal(id string) *Terminal {
	e.terminalsMu.RLock()
	defer e.terminalsMu.RUnlock()
	return e.terminals[id]
}

// Terminals returns a snapshot of connected terminals.
func (e *Edge) Terminals() []*Terminal {
	e.terminalsMu.RLock()
	defer e.terminalsMu.RUnlock()
	out := make([]*Terminal, 0, len(e.terminals))
	for _, t := range e.terminals {
		out = append(out, t)
	}
	return out
}

func (e *Edge) register(t *Terminal) bool {
	e.terminalsMu.Lock()
	defer e.terminalsMu.Unlock()
	if _, exists := e.terminals[t.id]; exists {
		return false
	}
	e.terminals[t.id] = t
	return true
}

func (e *Edge) unregister(t *Terminal) {
	e.terminalsMu.Lock()
	if e.terminals[t.id] == t {
		delete(e.terminals, t.id)
	}
	e.terminalsMu.Unlock()
	e.emit("terminal.disconnected", map[string]any{"terminal_id": t.id})
}

func (e *Edge) clipCache() *clipCache { return e.cache }

func (e *Edge) nextStream() uint64 {
	for {
		n := e.streamSeq.Add(1)
		if n != 0 {
			return n
		}
	}
}

// emitReady broadcasts the whole model state. Reconnect snapshotting reuses
// it, so the brain always sees a complete map even while some model loads.
func (e *Edge) emitReady() {
	e.emit("ready", map[string]any{"models": e.models.snapshot()})
}

func (e *Edge) emit(kind string, fields map[string]any) {
	msg := make(map[string]any, len(fields)+1)
	msg["type"] = kind
	for k, v := range fields {
		msg[k] = v
	}
	e.brainMu.RLock()
	for c := range e.brains {
		c.send(msg)
	}
	e.brainMu.RUnlock()
}

func (e *Edge) addBrain(c *ipcClient) {
	e.brainMu.Lock()
	e.brains[c] = struct{}{}
	e.brainMu.Unlock()
}

func (e *Edge) removeBrain(c *ipcClient) {
	e.brainMu.Lock()
	delete(e.brains, c)
	e.brainMu.Unlock()
}

// snapshot sends ready plus every connected terminal, in one ordered burst.
func (e *Edge) snapshot(c *ipcClient) {
	c.send(map[string]any{"type": "ready", "models": e.models.snapshot()})
	for _, t := range e.Terminals() {
		ev := t.connectedEvent()
		ev["type"] = "terminal.connected"
		c.send(ev)
	}
}

func defaultDataDir() string {
	if v := os.Getenv("LARK_DATA_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".lark"
	}
	return filepath.Join(home, ".local", "share", "lark")
}

func resolveSock(path string) string {
	if path == "" {
		path = os.Getenv("LARK_EDGE_SOCK")
	}
	if path == "" {
		path = DefaultSock
	}
	if len(path) > 1 && path[0] == '~' {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[1:])
		}
	}
	return path
}

var errClosed = errors.New("edge closed")

func (e *Edge) ensureOpen() error {
	if e.closed.Load() {
		return fmt.Errorf("edge: %w", errClosed)
	}
	return nil
}

// waitIdle is a test helper: wait until no terminal exists or timeout.
func (e *Edge) waitIdle(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(e.Terminals()) == 0 {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}
