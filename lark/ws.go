package main

import (
	"context"
	"crypto/rand"
	"image"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	wsBinary = 2
	wsPing   = 9
)

type wsFrame struct {
	op   byte
	data []byte
}

type wsClient struct {
	native        *nativeClient
	ctx           context.Context
	cancel        context.CancelFunc
	out           chan wsFrame
	outMu         sync.Mutex
	drained       chan struct{}
	txSeq         atomic.Uint32
	kind          string // "hardware"
	terminal      *wsTerminal
	activeCapture atomic.Pointer[endpointCapture]
	pingMu        sync.Mutex
	pingSentAt    time.Time
	rtt           *float64
}

type companionStatusState struct {
	seq      uint32
	state    uint8
	progress uint8
	kind     uint8
	text     string
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
	atmosphere        bool
	udpPort           uint16
	stateSeq          uint32
	voiceOwners       map[string]string
	companionStatuses map[string]companionStatusState
	resumes           map[[resumeTokenLen]byte]*resumeEntry
	assetsMu          sync.RWMutex
	assets            map[string]cachedAssets
}

type cachedAssets struct {
	bg    *image.RGBA
	poses [4]*image.RGBA
	theme pixelTheme
}

func newWSHub(b *larkServer, atmosphere bool) *wsHub {
	h := &wsHub{
		b:                 b,
		atmosphere:        atmosphere,
		clients:           make(map[*wsClient]struct{}),
		terminals:         make(map[string]*wsTerminal),
		voiceOwners:       make(map[string]string),
		companionStatuses: make(map[string]companionStatusState),
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

func (c *wsClient) stop(code uint16) {
	c.cancel()
}

func (c *wsClient) remoteAddr() net.Addr {
	if c.native != nil {
		return c.native.conn.RemoteAddr()
	}
	return nil
}

func (c *wsClient) enqueueProto(msgType uint8, payload []byte) bool {
	if c.native != nil {
		return c.native.enqueue(msgType, payload)
	}
	return false
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
	return false
}

func (h *wsHub) pushProto(d *wsTerminal, msgType uint8, payload []byte, mirror bool) {
	d.client.enqueueProto(msgType, payload)
}

func (h *wsHub) pushRecStart(d *wsTerminal, target string, bridgeEndpoint, listen bool) {
	var flags uint8
	if target != "" {
		flags |= recTargetInter
	}
	if bridgeEndpoint {
		flags |= recTargetBridgeE
	}
	if listen {
		flags |= recTargetListen
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
}

func (h *wsHub) clearDoneOrErrorStatus(d *wsTerminal) {
	if d == nil || d.companionID == "" {
		return
	}
	if st, ok := h.companionStatuses[d.companionID]; ok && (st.state == statusDone || st.state == statusError) {
		h.setCompanionStatus(d.companionID, statusIdle, 0, promptNone, "")
	}
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
		if h.b.oc != nil {
			go h.b.oc.prewarm(d.client.ctx, compID, d.companion.Name)
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
