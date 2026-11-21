package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const wsConnections = 64
const wsCompanionOccupied = 4001

type wsEvent struct {
	Event     string  `json:"ev"`
	Role      string  `json:"role"`
	DeviceID  string  `json:"device_id"`
	Character string  `json:"character"`
	Firmware  string  `json:"firmware"`
	Proto     int     `json:"proto"`
	Port      *int    `json:"port"`
	Width     *int    `json:"width"`
	Height    *int    `json:"height"`
	Shape     string  `json:"shape"`
	Board     string  `json:"board"`
	Caps      wsCaps  `json:"caps"`
	ID        string  `json:"id"`
	X         int     `json:"x"`
	Y         int     `json:"y"`
	RevLo     *uint16 `json:"rev_lo"`
	Target    string  `json:"target"`
	Text      string  `json:"text"`
	State     string  `json:"state"`
	Audio     uint64  `json:"audio"`
	Call      uint32  `json:"call"`
	Cancelled bool    `json:"cancelled"`
	Enabled   *bool   `json:"enabled"`
	IfFree    bool    `json:"if_free"` // A reconnect must not replace a live virtual cat.
	wsStatus
}

type wsClient struct {
	wire       *websocket
	ctx        context.Context
	cancel     context.CancelFunc
	closeCode  atomic.Uint32
	out        chan wsFrame
	outMu      sync.Mutex
	drained    chan struct{}
	role       string
	watch      string
	lastPeers  wsFrame
	device     *wsDevice
	scope      *virtualClient
	pingMu     sync.Mutex
	pingSentAt time.Time
	rtt        *float64
}

// The hub lock orders state changes and control frames. Socket writes,
// playback queue waits, and backend requests never run under it.
type wsHub struct {
	b             *bridge
	mu            sync.Mutex
	settingsMu    sync.Mutex
	conns         sync.WaitGroup
	clients       map[*wsClient]struct{}
	devices       map[string]*wsDevice
	parents       map[string]*parentRuntime
	closed        bool
	peers         string
	storedPeers   []peerDisplay
	refreshing    bool
	refreshNeeded bool
	nextCall      uint32
	atmosphere    bool
}

func newWSHub(b *bridge, atmosphere bool) *wsHub {
	h := &wsHub{b: b, atmosphere: atmosphere, clients: make(map[*wsClient]struct{}), devices: make(map[string]*wsDevice)}
	go h.animate(b.root)
	return h
}

func (h *wsHub) shutdown() {
	h.mu.Lock()
	if !h.closed {
		h.closed = true
		now := time.Now()
		for key, p := range h.parents {
			if err := h.parentAccount(key, p, now, true); err != nil {
				log.Printf("Lark parent usage checkpoint for %s: %v", key, err)
			}
		}
		for c := range h.clients {
			c.cancel()
		}
	}
	h.mu.Unlock()
	h.conns.Wait()
}

func (h *wsHub) serve(w http.ResponseWriter, r *http.Request) error {
	if !tokenHeaderMatches(r.Header, "Token", h.b.token) && !tokenMatches(r.URL.Query()["token"], h.b.token) {
		return problem(401, "Unauthorized")
	}
	return h.serveScope(w, r, nil)
}

func (h *wsHub) serveScope(w http.ResponseWriter, r *http.Request, scope *virtualClient) error {
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" ||
			(u.Scheme != "http" && u.Scheme != "https") || !strings.EqualFold(u.Host, r.Host) {
			return problem(403, "Unexpected WebSocket origin")
		}
	}
	h.mu.Lock()
	full := h.closed || len(h.clients) >= wsConnections
	h.mu.Unlock()
	if full {
		return problem(503, "WebSocket hub unavailable")
	}
	wire, err := upgradeWebSocket(w, r)
	if err != nil || wire == nil {
		return err
	}
	ctx, cancel := context.WithCancel(r.Context())
	if scope != nil || !libraryLAN(r) {
		ctx = withoutPhotoAccess(ctx)
	}
	c := &wsClient{wire: wire, ctx: ctx, cancel: cancel, out: make(chan wsFrame, 192), scope: scope}
	wire.onPong = c.pong
	h.mu.Lock()
	if closed := h.closed; closed || len(h.clients) >= wsConnections {
		h.mu.Unlock()
		cancel()
		code := uint16(1013)
		if closed {
			code = 1001
		}
		_ = wire.write(wsCloseFrame(code))
		wire.conn.Close()
		return nil
	}
	h.clients[c] = struct{}{}
	h.conns.Add(1)
	h.mu.Unlock()
	go c.write()
	defer h.conns.Done()
	defer func() {
		c.wire.conn.Close()
		cancel()
		h.detach(c)
	}()
	for {
		f, err := wire.read()
		if err != nil || c.ctx.Err() != nil {
			return nil
		}
		switch f.op {
		case wsText:
			var ev wsEvent
			if json.Unmarshal(f.data, &ev) == nil {
				h.handle(c, ev)
			}
		case wsBinary:
			h.micChunk(c, f.data)
		}
	}
}

func (c *wsClient) write() {
	defer c.wire.conn.Close()
	ping := time.NewTicker(12 * time.Second)
	defer ping.Stop()
	for c.ctx.Err() == nil {
		select {
		case <-c.ctx.Done():
		case f := <-c.out:
			c.outMu.Lock()
			if c.drained != nil {
				close(c.drained)
				c.drained = nil
			}
			c.outMu.Unlock()
			if c.wire.write(f) != nil {
				return
			}
		case <-ping.C:
			if c.wire.write(c.ping(time.Now())) != nil {
				return
			}
		}
	}
	code := uint16(c.closeCode.Load())
	if code == 0 {
		code = 1001
	}
	_ = c.wire.write(wsCloseFrame(code))
}

func (c *wsClient) stop(code uint16) {
	c.closeCode.Store(uint32(code))
	c.cancel()
}

func (c *wsClient) enqueue(f wsFrame) bool {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	if c.ctx.Err() != nil {
		return false
	}
	select {
	case c.out <- f:
		return true
	default:
		c.cancel()
		c.wire.conn.Close()
		return false
	}
}

func (c *wsClient) enqueueWait(ctx context.Context, f wsFrame) bool {
	for {
		c.outMu.Lock()
		if c.ctx.Err() != nil || ctx.Err() != nil {
			c.outMu.Unlock()
			return false
		}
		// Keep one FIFO, but leave room for display and interrupt controls
		// rather than letting a burst of PCM fill it.
		if len(c.out) < min(8, cap(c.out)) {
			c.out <- f
			c.outMu.Unlock()
			return true
		}
		if c.drained == nil {
			c.drained = make(chan struct{})
		}
		drained := c.drained
		c.outMu.Unlock()
		select {
		case <-drained:
		case <-c.ctx.Done():
			return false
		case <-ctx.Done():
			return false
		}
	}
}

func textFrame(v object) wsFrame {
	raw, _ := json.Marshal(v)
	return wsFrame{wsText, raw}
}

func (h *wsHub) push(d *wsDevice, v object, mirror bool) {
	f := textFrame(v)
	d.client.enqueue(f)
	if mirror {
		for c := range h.clients {
			if c.watch == d.key {
				c.enqueue(f)
			}
		}
	}
}

func (h *wsHub) active(d *wsDevice) bool {
	return !h.closed && h.devices[d.key] == d && d.client.ctx.Err() == nil
}

func (h *wsHub) detach(c *wsClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, c)
	if d := c.device; d != nil && h.devices[d.key] == d {
		h.endCall(d, "disconnected")
		h.dropApp(d)
		delete(h.devices, d.key)
		d.recording.clear()
		h.cancelTurn(d, "disconnect")
		h.stopPlayback(d, false)
		h.parentFinishActivity(d.key, time.Now())
		h.broadcastPeers()
	}
}

func (h *wsHub) disconnectDevice(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if d := h.devices[id]; d != nil {
		d.client.cancel()
		h.endCall(d, "disconnected")
		h.dropApp(d)
		delete(h.devices, id)
		d.recording.clear()
		h.cancelTurn(d, "disconnect")
		h.stopPlayback(d, false)
		h.parentFinishActivity(d.key, time.Now())
		h.broadcastPeers()
	}
}
