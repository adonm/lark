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
	IfFree    bool    `json:"if_free"` // A reconnect must not replace a live virtual companion.
	wsStatus
}

type wsClient struct {
	wire       *websocket
	native     *nativeClient
	ctx        context.Context
	cancel     context.CancelFunc
	closeOnce  sync.Once
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
	pending       int
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
	full := h.closed || len(h.clients)+h.pending >= wsConnections
	if !full {
		h.pending++
	}
	h.mu.Unlock()
	if full {
		return problem(503, "WebSocket hub unavailable")
	}
	wire, err := upgradeWebSocket(w, r)
	if err != nil || wire == nil {
		h.mu.Lock()
		h.pending--
		h.mu.Unlock()
		return err
	}
	ctx, cancel := context.WithCancel(r.Context())
	c := &wsClient{wire: wire, ctx: ctx, cancel: cancel, out: make(chan wsFrame, 192), scope: scope}
	wire.onPong = c.pong
	h.mu.Lock()
	h.pending--
	if h.closed {
		h.mu.Unlock()
		cancel()
		_ = wire.write(wsCloseFrame(1001))
		c.close()
		return nil
	}
	h.clients[c] = struct{}{}
	h.conns.Add(1)
	h.mu.Unlock()
	go c.write()
	defer h.conns.Done()
	defer func() {
		c.close()
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

func (c *wsClient) close() {
	if c.wire != nil {
		c.closeOnce.Do(func() {
			c.wire.conn.Close()
		})
	}
}

func (c *wsClient) write() {
	defer c.close()
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
		c.close()
		return false
	}
}

func enqueueWaitChan[T any](ctx, clientCtx context.Context, mu *sync.Mutex, out chan T, drained *chan struct{}, val T) bool {
	for {
		mu.Lock()
		if clientCtx.Err() != nil || ctx.Err() != nil {
			mu.Unlock()
			return false
		}
		// Keep one FIFO, but leave room for display and interrupt controls
		// rather than letting a burst of PCM fill it.
		if len(out) < min(8, cap(out)) {
			out <- val
			mu.Unlock()
			return true
		}
		if *drained == nil {
			*drained = make(chan struct{})
		}
		wait := *drained
		mu.Unlock()
		select {
		case <-wait:
		case <-clientCtx.Done():
			return false
		case <-ctx.Done():
			return false
		}
	}
}

func (c *wsClient) enqueueWait(ctx context.Context, f wsFrame) bool {
	if c.native != nil {
		if f.op == wsBinary && len(f.data) > 0 && f.data[0] == pcmPrefix {
			return c.native.enqueueWait(ctx, msgTTSPCM, f.data[1:])
		}
		return true
	}
	return enqueueWaitChan(ctx, c.ctx, &c.outMu, c.out, &c.drained, f)
}

func textFrame(v object) wsFrame {
	raw, _ := json.Marshal(v)
	return wsFrame{wsText, raw}
}

func (h *wsHub) mirrorWatchers(d *wsDevice, v object) {
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

func (h *wsHub) pushWS(d *wsDevice, v object, mirror bool) {
	if d.client.native == nil {
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
		h.mirrorWatchers(d, v)
	}
}

func (h *wsHub) pushWakeWord(d *wsDevice, enabled bool) {
	if d.client.native != nil {
		var b [1]byte
		if enabled {
			b[0] = 1
		}
		d.client.native.enqueue(msgWakeWordCtrl, b[:])
		return
	}
	d.client.enqueue(textFrame(object{"op": "wake_word", "enabled": enabled, "device_id": d.who.DeviceID}))
}

func (h *wsHub) pushRecStart(d *wsDevice, target string, bridgeEndpoint bool) {
	if d.client.native != nil {
		var t uint8
		if target != "" {
			t |= recTargetInter
		}
		if bridgeEndpoint {
			t |= recTargetBridgeE
		}
		d.client.native.enqueue(msgRecCtrl, encodeRecCtrl(recOpStart, t, d.recSession))
		return
	}
	start := object{"op": "rec", "action": "start", "target": target}
	if bridgeEndpoint {
		start["endpoint"] = "bridge"
	}
	d.client.enqueue(textFrame(start))
}

func (h *wsHub) pushRecStop(d *wsDevice, target string, cancelled bool) {
	if d.client.native != nil {
		op := recOpStop
		if cancelled {
			op = recOpCancel
		}
		var t uint8
		if target != "" {
			t |= recTargetInter
		}
		d.client.native.enqueue(msgRecCtrl, encodeRecCtrl(op, t, d.recSession))
		return
	}
	v := object{"op": "rec", "action": "stop", "target": target}
	if cancelled {
		v["cancelled"] = true
	}
	d.client.enqueue(textFrame(v))
}

func (h *wsHub) pushTTSAbort(d *wsDevice, audioID uint64) {
	if d.client.native != nil {
		d.client.native.enqueue(msgTTSCtrl, encodeIDCtrl(ttsOpStop, audioID))
		return
	}
	d.client.enqueue(textFrame(object{"op": "tts", "state": "abort", "audio": audioID}))
}

func (h *wsHub) pushCallStart(d *wsDevice) {
	if d.client.native != nil {
		d.client.native.enqueue(msgCallCtrl, encodeIDCtrl(callOpStart, uint64(d.callID)))
		return
	}
	msg := object{"op": "call", "state": "start", "call": d.callID, "muted": false}
	if d.caps.CallPCM {
		msg["mic_prefix"] = callMicPrefix
	}
	d.client.enqueue(textFrame(msg))
}

func (h *wsHub) pushCallMute(d *wsDevice) {
	if d.client.native != nil {
		op := callOpUnmute
		if d.callMuted {
			op = callOpMute
		}
		d.client.native.enqueue(msgCallCtrl, encodeIDCtrl(op, uint64(d.callID)))
		return
	}
	d.client.enqueue(textFrame(object{"op": "call", "state": "mute", "call": d.callID, "muted": d.callMuted}))
}

func (h *wsHub) pushCallEnd(d *wsDevice, callID uint32, reason string) {
	if d.client.native != nil {
		d.client.native.enqueue(msgCallCtrl, encodeIDCtrl(callOpEnd, uint64(callID)))
		return
	}
	d.client.enqueue(textFrame(object{"op": "call", "state": "end", "call": callID, "reason": reason}))
}

func (h *wsHub) pushChime(d *wsDevice, kind string) {
	if d.client.native != nil {
		if code, ok := encodeChimePayload(kind); ok {
			d.client.native.enqueue(msgChime, []byte{code})
		}
		return
	}
	d.client.enqueue(textFrame(object{"op": "chime", "kind": kind}))
}

func (h *wsHub) pushPower(d *wsDevice, brightness int, mirror bool) {
	if d.client.native != nil {
		d.client.native.enqueue(msgPower, []byte{encodePowerPayload(brightness)})
	} else {
		d.client.enqueue(textFrame(object{"op": "power", "brightness": brightness}))
	}
	if mirror {
		h.mirrorWatchers(d, object{"op": "power", "brightness": brightness})
	}
}

func (h *wsHub) pushVolume(d *wsDevice, persist, mirror bool) {
	if d.client.native != nil {
		var p byte
		if persist {
			p = 1
		}
		d.client.native.enqueue(msgVolume, []byte{byte(min(100, max(0, d.vol))), p})
		if mirror {
			h.mirrorWatchers(d, d.volumeFrame(persist))
		}
		return
	}
	h.pushWS(d, d.volumeFrame(persist), mirror)
}

func (h *wsHub) active(d *wsDevice) bool {
	return !h.closed && h.devices[d.key] == d && d.client.ctx.Err() == nil
}

func (h *wsHub) teardownDevice(d *wsDevice) {
	h.endCall(d, "disconnected")
	h.dropApp(d)
	delete(h.devices, d.key)
	d.recording.clear()
	h.cancelTurn(d, "disconnect")
	h.stopPlayback(d, false)
	h.parentFinishActivity(d.key, time.Now())
	h.broadcastPeers()
}

func (h *wsHub) detach(c *wsClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, c)
	if d := c.device; d != nil && h.devices[d.key] == d {
		h.teardownDevice(d)
	}
}

func (h *wsHub) disconnectDevice(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if d := h.devices[id]; d != nil {
		d.client.cancel()
		h.teardownDevice(d)
	}
}
