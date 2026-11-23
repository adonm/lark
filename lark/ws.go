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

const terminalBinary = 2

// protocolFrame is one terminal frame: a transport opcode followed by the
// payload. A slice avoids a second allocation for every rendered frame.
type protocolFrame []byte

func (f protocolFrame) op() byte {
	if len(f) == 0 {
		return 0
	}
	return f[0]
}

func (f protocolFrame) data() []byte {
	if len(f) < 2 {
		return nil
	}
	return f[1:]
}

func newProtocolFrame(op byte, data []byte) protocolFrame {
	f := make(protocolFrame, 1+len(data))
	f[0] = op
	copy(f[1:], data)
	return f
}

type terminalClient struct {
	native        *nativeClient
	ctx           context.Context
	cancel        context.CancelFunc
	terminal      *terminalSession
	activeCapture atomic.Pointer[endpointCapture]
	pingMu        sync.Mutex
	pingSentAt    time.Time
	rtt           *float64
}

type companionStatusState struct {
	state uint8
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
type terminalHub struct {
	b                 *larkServer
	mu                sync.Mutex
	sig               condSignal
	settingsMu        sync.Mutex
	conns             sync.WaitGroup
	clients           map[*terminalClient]struct{}
	terminals         map[string]*terminalSession
	closed            bool
	atmosphere        bool
	voiceOwners       map[string]string
	companionStatuses map[string]companionStatusState
	resumes           map[[resumeTokenLen]byte]*resumeEntry
	gate              *specGate
	assetsMu          sync.RWMutex
	assets            map[string]cachedAssets
	assetsClock       uint64
	assetsEpoch       map[string]uint64
}

// Keep every companion's decoded assets resident; the bound only guards
// against long histories of replaced portraits.
const assetsCacheLimit = 16

// debugScreenText builds the labelled hit trace that DOM tests wait on. It is
// off in production, where nothing reads svgScreen.source.
var debugScreenText = false

type cachedAssets struct {
	bg    *image.RGBA
	poses [4]*image.RGBA
	theme pixelTheme
	used  uint64
	// A portrait whose sheet is missing renders placeholders. Cache those too
	// so frames do not decode (and fail) per frame, but let them expire so a
	// portrait that finishes generating appears shortly after.
	expires time.Time
}

func newTerminalHub(b *larkServer, atmosphere bool) *terminalHub {
	h := &terminalHub{
		b:                 b,
		atmosphere:        atmosphere,
		clients:           make(map[*terminalClient]struct{}),
		terminals:         make(map[string]*terminalSession),
		voiceOwners:       make(map[string]string),
		companionStatuses: make(map[string]companionStatusState),
		resumes:           make(map[[resumeTokenLen]byte]*resumeEntry),
		gate:              &specGate{},
	}
	go h.animate(b.root)
	return h
}

func (h *terminalHub) shutdown() {
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

func (c *terminalClient) remoteAddr() net.Addr {
	if c.native != nil {
		return c.native.conn.RemoteAddr()
	}
	return nil
}

func (c *terminalClient) enqueueProto(msgType uint8, payload []byte) bool {
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

func (c *terminalClient) enqueueProtoWait(ctx context.Context, msgType uint8, payload []byte) bool {
	if c.native != nil {
		return c.native.enqueueWait(ctx, msgType, payload)
	}
	return false
}

func (h *terminalHub) pushProto(d *terminalSession, msgType uint8, payload []byte, mirror bool) {
	d.client.enqueueProto(msgType, payload)
}

func (h *terminalHub) pushRecStart(d *terminalSession, target string, bridgeEndpoint, listen bool) {
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

func (h *terminalHub) pushRecStop(d *terminalSession, target string, cancelled bool) {
	var op uint8 = recOpStop
	if cancelled {
		op = recOpCancel
	}
	var flags uint8
	if target != "" {
		flags |= recTargetInter
	}
	d.client.enqueueProto(msgRecCtrl, encodeRecCtrl(op, flags, d.recSession, target))
}

func (h *terminalHub) pushTTSAbort(d *terminalSession, audioID uint64) {
	d.client.enqueueProto(msgTTSCtrl, encodeTTSCtrl(ttsOpStop, audioID))
}

func (h *terminalHub) pushChime(d *terminalSession, kind string) {
	if code, ok := encodeChimePayload(kind); ok {
		d.client.enqueueProto(msgChime, []byte{code})
	}
}

func (h *terminalHub) pushPower(d *terminalSession, brightness int, mirror bool) {
	b := byte(min(100, max(0, brightness)))
	h.pushProto(d, msgPower, []byte{b}, mirror)
}

func (h *terminalHub) pushVolume(d *terminalSession, persist, mirror bool) {
	h.pushProto(d, msgVolume, encodeVolumeMsg(d.vol, persist, d.id, d.companionID), mirror)
}

func (h *terminalHub) active(d *terminalSession) bool {
	return d != nil && !h.closed && h.terminals[d.key] == d && d.client.ctx.Err() == nil
}

func (h *terminalHub) terminalsFor(companionID string) []*terminalSession {
	cid := strings.ToLower(companionID)
	if cid == "" {
		return nil
	}
	var out []*terminalSession
	for _, t := range h.terminals {
		if h.active(t) && t.companionID == cid {
			out = append(out, t)
		}
	}
	return out
}

func (h *terminalHub) voiceOwner(companionID string) *terminalSession {
	cid := strings.ToLower(companionID)
	if cid == "" {
		return nil
	}
	if ownerID := h.voiceOwners[cid]; ownerID != "" {
		if t := h.terminals[ownerID]; h.active(t) && t.companionID == cid {
			return t
		}
	}
	var best *terminalSession
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

func (h *terminalHub) broadcastVoiceOwner(companionID, terminalID string) {
	cid := strings.ToLower(companionID)
	if cid == "" {
		return
	}
	pkt := encodeVoiceOwner(cid, terminalID)
	for _, t := range h.terminalsFor(cid) {
		t.client.enqueueProto(msgVoiceOwner, pkt)
	}
}

func (h *terminalHub) takeVoiceOwnership(d *terminalSession) {
	if d == nil || d.companionID == "" {
		return
	}
	if h.voiceOwners[d.companionID] == d.id {
		return
	}
	h.voiceOwners[d.companionID] = d.id
	h.broadcastVoiceOwner(d.companionID, d.id)
}

func (h *terminalHub) fallbackVoiceOwnership(companionID, excludeID string) {
	cid := strings.ToLower(companionID)
	if cid == "" {
		return
	}
	var best *terminalSession
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

func (h *terminalHub) setCompanionStatus(companionID string, state uint8) {
	cid := strings.ToLower(companionID)
	if cid == "" {
		return
	}
	h.companionStatuses[cid] = companionStatusState{state: state}
	pkt := encodeStatus(state)
	for _, t := range h.terminalsFor(cid) {
		t.client.enqueueProto(msgStatus, pkt)
	}
}

func (h *terminalHub) clearDoneOrErrorStatus(d *terminalSession) {
	if d == nil || d.companionID == "" {
		return
	}
	if st, ok := h.companionStatuses[d.companionID]; ok && (st.state == statusDone || st.state == statusError) {
		h.setCompanionStatus(d.companionID, statusIdle)
	}
}

func newResumeToken() [resumeTokenLen]byte {
	var tok [resumeTokenLen]byte
	_, _ = rand.Read(tok[:])
	return tok
}

func (h *terminalHub) pruneResumes(now time.Time) {
	for k, e := range h.resumes {
		if now.After(e.expiresAt) {
			delete(h.resumes, k)
		}
	}
}

func (h *terminalHub) teardownTerminal(d *terminalSession) {
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

func (h *terminalHub) detach(c *terminalClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, c)
	if d := c.terminal; d != nil && h.terminals[d.key] == d {
		h.teardownTerminal(d)
	}
}

func (h *terminalHub) disconnectTerminal(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if d := h.terminals[strings.ToLower(id)]; d != nil {
		d.client.cancel()
		h.teardownTerminal(d)
	}
}

func (h *terminalHub) attachTerminal(termID, compID string) {
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
			go h.b.oc.prewarm(d.client.ctx, d.companion)
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
