package main

import (
	"context"
	"encoding/json"
	"log"
	"net"
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
	native     *nativeClient
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

func (c *wsClient) remoteAddr() net.Addr {
	if c.native != nil {
		return c.native.conn.RemoteAddr()
	}
	return c.wire.conn.RemoteAddr()
}

func (c *wsClient) enqueue(f wsFrame) bool {
	if c.native != nil {
		if f.op == wsBinary && len(f.data) > 0 {
			switch f.data[0] {
			case frameTag:
				return c.native.enqueue(msgFrame, f.data)
			case pcmPrefix:
				return c.native.enqueue(msgTTSPCM, f.data[1:])
			}
		}
		return true
	}
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
	if c.native != nil {
		if f.op == wsBinary && len(f.data) > 0 && f.data[0] == pcmPrefix {
			return c.native.enqueueWait(ctx, msgTTSPCM, f.data[1:])
		}
		return true
	}
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
	if d.client.native != nil {
		h.pushNative(d, v)
	} else {
		f := textFrame(v)
		d.client.enqueue(f)
		if mirror {
			for c := range h.clients {
				if c.watch == d.key {
					c.enqueue(f)
				}
			}
		}
		return
	}
	if mirror {
		var f wsFrame
		for c := range h.clients {
			if c.watch == d.key {
				if f.data == nil {
					f = textFrame(v)
				}
				c.enqueue(f)
			}
		}
	}
}

func (h *wsHub) pushNative(d *wsDevice, v object) {
	nc := d.client.native
	switch str(v["op"]) {
	case "wake_word":
		var b [1]byte
		if enabled, _ := v["enabled"].(bool); enabled {
			b[0] = 1
		}
		nc.enqueue(msgWakeWordCtrl, b[:])
	case "rec":
		var op uint8
		switch str(v["action"]) {
		case "start":
			op = recOpStart
		default:
			if cancelled, _ := v["cancelled"].(bool); cancelled {
				op = recOpCancel
			} else {
				op = recOpStop
			}
		}
		var target uint8
		if str(v["target"]) != "" {
			target |= recTargetInter
		}
		if str(v["endpoint"]) == "bridge" {
			target |= recTargetBridgeE
		}
		nc.enqueue(msgRecCtrl, encodeRecCtrl(op, target, d.recSession))
	case "tts":
		var op uint8
		switch str(v["state"]) {
		case "start":
			op = ttsOpStart
		case "end":
			op = ttsOpEnd
		default:
			op = ttsOpStop
		}
		var id uint64
		switch n := v["audio"].(type) {
		case uint64:
			id = n
		case int:
			id = uint64(n)
		}
		nc.enqueue(msgTTSCtrl, encodeIDCtrl(op, id))
	case "call":
		var op uint8
		switch str(v["state"]) {
		case "start":
			op = callOpStart
		case "mute":
			if muted, _ := v["muted"].(bool); muted {
				op = callOpMute
			} else {
				op = callOpUnmute
			}
		default:
			op = callOpEnd
		}
		var id uint64
		switch n := v["call"].(type) {
		case uint32:
			id = uint64(n)
		case uint64:
			id = n
		case int:
			id = uint64(n)
		}
		nc.enqueue(msgCallCtrl, encodeIDCtrl(op, id))
	case "chime":
		if code, ok := encodeChimePayload(str(v["kind"])); ok {
			nc.enqueue(msgChime, []byte{code})
		}
	case "power":
		bright, _ := v["brightness"].(int)
		nc.enqueue(msgPower, []byte{encodePowerPayload(bright)})
	case "volume":
		level, _ := v["level"].(int)
		persist, _ := v["persist"].(bool)
		var p byte
		if persist {
			p = 1
		}
		nc.enqueue(msgVolume, []byte{byte(min(100, max(0, level))), p})
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
