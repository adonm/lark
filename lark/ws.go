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

	"lark/internal/pixel"
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
	terminal      atomic.Pointer[terminalSession]
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
	theme pixel.Theme
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
	// Clients normally detach themselves and stop their actor; sweep anything
	// that was attached without a live connection so no goroutine leaks.
	h.mu.Lock()
	var rest []*terminalSession
	for _, d := range h.terminals {
		rest = append(rest, d)
	}
	clear(h.terminals)
	h.mu.Unlock()
	for _, d := range rest {
		d.close()
	}
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
	h.pushProto(d, msgVolume, encodeVolumeMsg(d.vol, persist, d.id, d.companionID.get()), mirror)
}

// activeLocked checks attachment. The caller holds the hub lock.
func (h *terminalHub) activeLocked(d *terminalSession) bool {
	return d != nil && !h.closed && h.terminals[d.key] == d && d.client.ctx.Err() == nil
}

// active checks attachment from outside the registry lock (session actors and
// request handlers).
func (h *terminalHub) active(d *terminalSession) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.activeLocked(d)
}

// live returns an attached session by ID, or nil.
func (h *terminalHub) live(id string) *terminalSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.terminals[strings.ToLower(id)]
	if !h.activeLocked(d) {
		return nil
	}
	return d
}

// liveAll snapshots every attached session.
func (h *terminalHub) liveAll() []*terminalSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*terminalSession, 0, len(h.terminals))
	for _, d := range h.terminals {
		if h.activeLocked(d) {
			out = append(out, d)
		}
	}
	return out
}

// liveFor snapshots the attached sessions of one companion.
func (h *terminalHub) liveFor(companionID string) []*terminalSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.terminalsFor(companionID)
}

// voiceOwnerIs reports whether d currently owns its companion's voice.
func (h *terminalHub) voiceOwnerIs(d *terminalSession) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.activeLocked(d) && d.companionID.get() != "" && h.voiceOwner(d.companionID.get()) == d
}

// statusState reads one companion's published status under the hub lock.
func (h *terminalHub) statusState(companionID string) (companionStatusState, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.companionStatuses[companionID]
	return st, ok
}

func (h *terminalHub) terminalsFor(companionID string) []*terminalSession {
	cid := strings.ToLower(companionID)
	if cid == "" {
		return nil
	}
	var out []*terminalSession
	for _, t := range h.terminals {
		if h.activeLocked(t) && t.companionID.get() == cid {
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
		if t := h.terminals[ownerID]; h.activeLocked(t) && t.companionID.get() == cid {
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
	h.mu.Lock()
	defer h.mu.Unlock()
	if d == nil || d.companionID.get() == "" || h.voiceOwners[d.companionID.get()] == d.id {
		return
	}
	h.voiceOwners[d.companionID.get()] = d.id
	h.broadcastVoiceOwner(d.companionID.get(), d.id)
}

func (h *terminalHub) fallbackVoiceOwnership(companionID, excludeID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fallbackVoiceOwnershipLocked(companionID, excludeID)
}

func (h *terminalHub) fallbackVoiceOwnershipLocked(companionID, excludeID string) {
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
	h.mu.Lock()
	defer h.mu.Unlock()
	h.setCompanionStatusLocked(companionID, state)
}

func (h *terminalHub) setCompanionStatusLocked(companionID string, state uint8) {
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
	if d == nil || d.companionID.get() == "" {
		return
	}
	if st, ok := h.statusState(d.companionID.get()); ok && (st.state == statusDone || st.state == statusError) {
		h.setCompanionStatus(d.companionID.get(), statusIdle)
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
	wasOwner := d.companionID.get() != "" && h.voiceOwners[d.companionID.get()] == d.id
	if d.hasResumeToken {
		h.pruneResumes(now)
		h.resumes[d.resumeToken] = &resumeEntry{
			terminalID:     d.id,
			companionID:    d.companionID.get(),
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
	if wasOwner {
		h.fallbackVoiceOwnershipLocked(d.companionID.get(), d.id)
	}
	h.broadcastPeersLocked()
	h.sig.notify()
}

func (h *terminalHub) detach(c *terminalClient) {
	h.mu.Lock()
	delete(h.clients, c)
	var d *terminalSession
	if d = c.terminal.Load(); d != nil && h.terminals[d.key] == d {
		h.teardownTerminal(d)
	} else {
		d = nil
	}
	h.mu.Unlock()
	if d != nil {
		d.close()
	}
}

func (h *terminalHub) disconnectTerminal(id string) {
	h.mu.Lock()
	d := h.terminals[strings.ToLower(id)]
	if d != nil {
		d.client.cancel()
		h.teardownTerminal(d)
	}
	h.mu.Unlock()
	if d != nil {
		d.close()
	}
}

func (h *terminalHub) attachTerminal(termID, compID string) {
	termID = strings.ToLower(termID)
	compID = strings.ToLower(compID)
	var companion companionState
	var voice string
	if compID != "" {
		companion = h.b.companionFor(compID)
		voice = h.b.voices.resolve(companion.Voice, companionTheme(companion.Name))
	} else {
		companion = blankCompanion()
		voice = h.b.voices.defaultFor("")
	}
	h.mu.Lock()
	d := h.terminals[termID]
	if d == nil || !h.activeLocked(d) {
		h.broadcastPeersLocked()
		h.mu.Unlock()
		return
	}
	if d.companionID.get() == compID {
		h.mu.Unlock()
		return
	}
	oldComp := d.companionID.get()
	wasOwner := oldComp != "" && h.voiceOwners[oldComp] == d.id
	d.companionID.set(compID)
	if compID != "" && h.voiceOwners[compID] == "" {
		h.voiceOwners[compID] = d.id
	}
	if wasOwner {
		h.fallbackVoiceOwnershipLocked(oldComp, d.id)
	}
	h.broadcastPeersLocked()
	h.mu.Unlock()
	d.post(terminalEvent{kind: evAttach, now: time.Now(),
		attach: terminalAttach{companion: companion, voice: voice}})
	if compID != "" && h.b.oc != nil {
		go h.b.oc.prewarm(d.client.ctx, companion)
	}
}
