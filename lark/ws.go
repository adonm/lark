package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
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
	txSeq      atomic.Uint32
	kind       string // "hardware", "web", or "watcher"
	watch      string
	lastPeers  []byte
	terminal   *wsTerminal
	scope      *companionState
	pingMu     sync.Mutex
	pingSentAt time.Time
	rtt        *float64
}

type companionStatusState struct {
	seq      uint32
	state    uint8
	progress uint8
	kind     uint8
	text     string
}

type activePrompt struct {
	seq         uint32
	id          string
	companionID string
	kind        uint8
	text        string
	options     []string
	reply       chan string
	onReply     func(string)
}

type resumeEntry struct {
	terminalID     string
	companionID    string
	kind           uint8
	board          string
	firmware       string
	screen         screenGeometry
	displayFormats uint8
	frameCodecs    uint8
	caps           terminalCaps
	hadVoiceOwner  bool
	expiresAt      time.Time
}

// The hub lock orders state changes and control frames. Socket writes,
// playback queue waits, and backend requests never run under it.
type wsHub struct {
	b                 *larkServer
	mu                sync.Mutex
	sig               condSignal
	settingsMu        sync.Mutex
	conns             sync.WaitGroup
	clients           map[*wsClient]struct{}
	pending           int
	terminals         map[string]*wsTerminal
	closed            bool
	peersKey          string
	storedPeers       []peerDisplay
	storedCompanions  []companionState
	refreshing        bool
	refreshNeeded     bool
	nextCall          uint32
	atmosphere        bool
	udpPort           uint16
	stateSeq          uint32
	voiceOwners       map[string]string
	companionStatuses map[string]companionStatusState
	prompts           map[string]*activePrompt
	resumes           map[[resumeTokenLen]byte]*resumeEntry
}

func newWSHub(b *larkServer, atmosphere bool) *wsHub {
	h := &wsHub{
		b:                 b,
		atmosphere:        atmosphere,
		clients:           make(map[*wsClient]struct{}),
		terminals:         make(map[string]*wsTerminal),
		voiceOwners:       make(map[string]string),
		companionStatuses: make(map[string]companionStatusState),
		prompts:           make(map[string]*activePrompt),
		resumes:           make(map[[resumeTokenLen]byte]*resumeEntry),
	}
	go h.animate(b.root)
	return h
}

func (h *wsHub) nextSeq() uint32 {
	h.stateSeq++
	return h.stateSeq
}

func (h *wsHub) shutdown() {
	h.mu.Lock()
	if !h.closed {
		h.closed = true
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

func (h *wsHub) serveScope(w http.ResponseWriter, r *http.Request, scope *companionState) error {
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
		if f.op != wsBinary {
			continue
		}
		msgType, _, payload, ok := unpackProtoPacket(f.data)
		if !ok {
			continue
		}
		if msgType == msgPing && len(payload) == 4 {
			c.enqueueProto(msgPong, payload)
			continue
		}
		if msgType == msgPong && len(payload) == 4 {
			c.pingMu.Lock()
			if !c.pingSentAt.IsZero() && binary.LittleEndian.Uint32(payload) == uint32(c.pingSentAt.UnixMilli()) {
				c.rtt = new(milliseconds(time.Since(c.pingSentAt)))
				c.pingSentAt = time.Time{}
			}
			c.pingMu.Unlock()
			continue
		}
		h.handleProto(c, msgType, payload)
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

func (c *wsClient) enqueueProto(msgType uint8, payload []byte) bool {
	if c.native != nil {
		return c.native.enqueue(msgType, payload)
	}
	seq := uint16(c.txSeq.Add(1) - 1)
	return c.enqueue(wsFrame{wsBinary, packProtoPacket(msgType, seq, payload)})
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

func (c *wsClient) enqueueProtoWait(ctx context.Context, msgType uint8, payload []byte) bool {
	if c.native != nil {
		return c.native.enqueueWait(ctx, msgType, payload)
	}
	seq := uint16(c.txSeq.Add(1) - 1)
	f := wsFrame{wsBinary, packProtoPacket(msgType, seq, payload)}
	return enqueueWaitChan(ctx, c.ctx, &c.outMu, c.out, &c.drained, f)
}

func (h *wsHub) mirrorWatchers(d *wsTerminal, msgType uint8, payload []byte) {
	for c := range h.clients {
		if c.watch == d.companionID && d.companionID != "" || c.watch == d.key {
			c.enqueueProto(msgType, payload)
		}
	}
}

func (h *wsHub) pushProto(d *wsTerminal, msgType uint8, payload []byte, mirror bool) {
	d.client.enqueueProto(msgType, payload)
	if mirror {
		h.mirrorWatchers(d, msgType, payload)
	}
}

func (h *wsHub) pushWakeWord(d *wsTerminal, enabled bool) {
	d.client.enqueueProto(msgWakeWordCtrl, encodeWakeWordCtrl(enabled, d.id, d.companionID))
}

func (h *wsHub) pushRecStart(d *wsTerminal, target string, bridgeEndpoint bool) {
	var flags uint8
	if target != "" {
		flags |= recTargetInter
	}
	if bridgeEndpoint {
		flags |= recTargetBridgeE
	}
	d.client.enqueueProto(msgRecCtrl, encodeRecCtrl(recOpStart, flags, d.recSession, target))
}

func (h *wsHub) pushRecStop(d *wsTerminal, target string, cancelled bool) {
	op := recOpStop
	if cancelled {
		op = recOpCancel
	}
	var flags uint8
	if target != "" {
		flags |= recTargetInter
	}
	d.client.enqueueProto(msgRecCtrl, encodeRecCtrl(op, flags, d.recSession, target))
}

func (h *wsHub) pushTTSAbort(d *wsTerminal, audioID uint64) {
	d.client.enqueueProto(msgTTSCtrl, encodeTTSCtrl(ttsOpStop, audioID))
}

func (h *wsHub) pushCallStart(d *wsTerminal) {
	d.client.enqueueProto(msgCallCtrl, encodeCallCtrl(callOpStart, d.callID, ""))
}

func (h *wsHub) pushCallMute(d *wsTerminal) {
	op := callOpUnmute
	if d.callMuted {
		op = callOpMute
	}
	d.client.enqueueProto(msgCallCtrl, encodeCallCtrl(op, d.callID, ""))
}

func (h *wsHub) pushCallEnd(d *wsTerminal, callID uint32, reason string) {
	d.client.enqueueProto(msgCallCtrl, encodeCallCtrl(callOpEnd, callID, reason))
}

func (h *wsHub) pushChime(d *wsTerminal, kind string) {
	if code, ok := encodeChimePayload(kind); ok {
		d.client.enqueueProto(msgChime, []byte{code})
	}
}

func (h *wsHub) pushPower(d *wsTerminal, brightness int, mirror bool) {
	b := byte(min(100, max(0, brightness)))
	h.pushProto(d, msgPower, []byte{b}, mirror)
}

func (h *wsHub) pushVolume(d *wsTerminal, persist, mirror bool) {
	h.pushProto(d, msgVolume, encodeVolumeMsg(d.vol, persist, d.autoVol, d.id, d.companionID), mirror)
}

func (h *wsHub) active(d *wsTerminal) bool {
	return d != nil && !h.closed && h.terminals[d.key] == d && d.client.ctx.Err() == nil
}

func (h *wsHub) terminalsFor(companionID string) []*wsTerminal {
	cid := strings.ToLower(companionID)
	if cid == "" {
		return nil
	}
	var out []*wsTerminal
	for _, t := range h.terminals {
		if h.active(t) && t.companionID == cid {
			out = append(out, t)
		}
	}
	return out
}

func (h *wsHub) voiceOwner(companionID string) *wsTerminal {
	cid := strings.ToLower(companionID)
	if cid == "" {
		return nil
	}
	if ownerID := h.voiceOwners[cid]; ownerID != "" {
		if t := h.terminals[ownerID]; h.active(t) && t.companionID == cid {
			return t
		}
	}
	var best *wsTerminal
	for _, t := range h.terminalsFor(cid) {
		if best == nil || t.last.After(best.last) {
			best = t
		}
	}
	if best != nil {
		h.voiceOwners[cid] = best.id
	} else {
		delete(h.voiceOwners, cid)
	}
	return best
}

func (h *wsHub) broadcastVoiceOwner(companionID, terminalID string) {
	cid := strings.ToLower(companionID)
	if cid == "" {
		return
	}
	pkt := encodeVoiceOwner(h.nextSeq(), cid, terminalID)
	for _, t := range h.terminalsFor(cid) {
		t.client.enqueueProto(msgVoiceOwner, pkt)
	}
	for c := range h.clients {
		if c.watch == cid {
			c.enqueueProto(msgVoiceOwner, pkt)
		}
	}
}

func (h *wsHub) takeVoiceOwnership(d *wsTerminal) {
	if d == nil || d.companionID == "" {
		return
	}
	if h.voiceOwners[d.companionID] == d.id {
		return
	}
	h.voiceOwners[d.companionID] = d.id
	h.broadcastVoiceOwner(d.companionID, d.id)
}

func (h *wsHub) fallbackVoiceOwnership(companionID, excludeID string) {
	cid := strings.ToLower(companionID)
	if cid == "" {
		return
	}
	var best *wsTerminal
	for _, t := range h.terminalsFor(cid) {
		if t.id == excludeID {
			continue
		}
		if best == nil || t.last.After(best.last) {
			best = t
		}
	}
	if best != nil {
		h.voiceOwners[cid] = best.id
		h.broadcastVoiceOwner(cid, best.id)
	} else {
		delete(h.voiceOwners, cid)
	}
}

func (h *wsHub) setCompanionStatus(companionID string, state, progress, kind uint8, text string) {
	cid := strings.ToLower(companionID)
	if cid == "" {
		return
	}
	seq := h.nextSeq()
	h.companionStatuses[cid] = companionStatusState{
		seq:      seq,
		state:    state,
		progress: progress,
		kind:     kind,
		text:     text,
	}
	pkt := encodeStatus(seq, state, progress, kind, text)
	for _, t := range h.terminalsFor(cid) {
		t.client.enqueueProto(msgStatus, pkt)
	}
	for c := range h.clients {
		if c.watch == cid {
			c.enqueueProto(msgStatus, pkt)
		}
	}
}

func (h *wsHub) clearDoneOrErrorStatus(d *wsTerminal) {
	if d == nil || d.companionID == "" {
		return
	}
	if st, ok := h.companionStatuses[d.companionID]; ok && (st.state == statusDone || st.state == statusError) {
		h.setCompanionStatus(d.companionID, statusIdle, 0, promptNone, "")
	}
}

func (h *wsHub) openPrompt(companionID string, kind uint8, id, text string, options []string, onReply func(string)) *activePrompt {
	cid := strings.ToLower(companionID)
	if cid == "" {
		return nil
	}
	seq := h.nextSeq()
	p := &activePrompt{
		seq:         seq,
		id:          id,
		companionID: cid,
		kind:        kind,
		text:        text,
		options:     options,
		reply:       make(chan string, 1),
		onReply:     onReply,
	}
	h.prompts[cid] = p
	pkt := encodePrompt(seq, kind, id, text, options)
	for _, t := range h.terminalsFor(cid) {
		t.client.enqueueProto(msgPrompt, pkt)
	}
	for c := range h.clients {
		if c.watch == cid {
			c.enqueueProto(msgPrompt, pkt)
		}
	}
	h.setCompanionStatus(cid, statusBlocked, 0, kind, text)
	return p
}

func (h *wsHub) resolvePrompt(companionID, id, choice string) bool {
	cid := strings.ToLower(companionID)
	p := h.prompts[cid]
	if p == nil || p.id != id {
		return false
	}
	delete(h.prompts, cid)
	select {
	case p.reply <- choice:
	default:
	}
	if p.onReply != nil {
		go p.onReply(choice)
	}
	pkt := encodePromptEnded(h.nextSeq(), id)
	for _, t := range h.terminalsFor(cid) {
		t.client.enqueueProto(msgPromptEnded, pkt)
	}
	for c := range h.clients {
		if c.watch == cid {
			c.enqueueProto(msgPromptEnded, pkt)
		}
	}
	h.setCompanionStatus(cid, statusIdle, 0, promptNone, "")
	return true
}

func newResumeToken() [resumeTokenLen]byte {
	var tok [resumeTokenLen]byte
	_, _ = rand.Read(tok[:])
	return tok
}

func (h *wsHub) pruneResumes(now time.Time) {
	for k, e := range h.resumes {
		if now.After(e.expiresAt) {
			delete(h.resumes, k)
		}
	}
}

func (h *wsHub) teardownTerminal(d *wsTerminal) {
	now := time.Now()
	wasOwner := d.companionID != "" && h.voiceOwners[d.companionID] == d.id
	if d.hasResumeToken {
		h.pruneResumes(now)
		h.resumes[d.resumeToken] = &resumeEntry{
			terminalID:     d.id,
			companionID:    d.companionID,
			kind:           d.protoKind,
			board:          d.board,
			firmware:       d.firmware,
			screen:         d.screen,
			displayFormats: d.displayFormats,
			frameCodecs:    d.frameCodecs,
			caps:           d.caps,
			hadVoiceOwner:  wasOwner,
			expiresAt:      now.Add(time.Duration(resumeWindowMS) * time.Millisecond),
		}
	}
	h.endCall(d, "disconnected")
	h.dropApp(d)
	delete(h.terminals, d.key)
	d.recording.clear()
	h.cancelTurn(d, "disconnect")
	h.stopPlayback(d, false)
	if wasOwner {
		h.fallbackVoiceOwnership(d.companionID, d.id)
	}
	h.broadcastPeers()
	h.sig.notify()
}

func (h *wsHub) detach(c *wsClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, c)
	if d := c.terminal; d != nil && h.terminals[d.key] == d {
		h.teardownTerminal(d)
	}
}

func (h *wsHub) disconnectTerminal(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if d := h.terminals[strings.ToLower(id)]; d != nil {
		d.client.cancel()
		h.teardownTerminal(d)
	}
}

func (h *wsHub) attachTerminal(termID, compID string) {
	termID = strings.ToLower(termID)
	compID = strings.ToLower(compID)
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.terminals[termID]
	if d == nil || !h.active(d) {
		h.broadcastPeers()
		return
	}
	if d.companionID == compID {
		return
	}
	oldComp := d.companionID
	wasOwner := oldComp != "" && h.voiceOwners[oldComp] == d.id
	h.endCall(d, "reassigned")
	h.dropApp(d)
	d.recording.clear()
	h.cancelTurn(d, "reassigned")
	h.stopPlayback(d, true)
	if wasOwner {
		h.fallbackVoiceOwnership(oldComp, d.id)
	}
	d.companionID = compID
	if compID != "" {
		d.companion = h.b.companionFor(compID)
		d.voice = h.b.voices.resolve(d.companion.Voice, companionTheme(d.companion.Name))
		if h.voiceOwners[compID] == "" {
			h.voiceOwners[compID] = d.id
		}
	} else {
		d.companion = blankCompanion()
		d.voice = h.b.voices.defaultFor("")
	}
	h.clearView(d)
	now := time.Now()
	h.render(d, now, true)
	h.sendSnapshotState(d)
	h.broadcastPeers()
}
