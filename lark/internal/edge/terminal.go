package edge

import (
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"lark/internal/audio"
	"lark/internal/pixel"
	proto "lark/internal/proto"
)

type terminalMode uint8

const (
	modeIdle terminalMode = iota
	modeRecording
	modeListening
	modeThinking
	modeSpeaking
	modeMenu
	modeStory
)

func (m terminalMode) String() string {
	switch m {
	case modeIdle:
		return "idle"
	case modeRecording:
		return "recording"
	case modeListening:
		return "listening"
	case modeThinking:
		return "thinking"
	case modeSpeaking:
		return "speaking"
	case modeMenu:
		return "menu"
	case modeStory:
		return "story"
	}
	return "unknown"
}

type evKind uint8

const (
	evNone evKind = iota
	evRecStart
	evRecEnd
	evTTSAck
	evRender
	evFrameBuilt
	evSpeakStart
	evSpeakChunk
	evSpeakEnd
	evStopAudio
	evChime
	evPower
	evVolume
	evFiller
	evPreview
	evRecCancel
	evRecBegin
	evHits
	evClose
	evCaptureStart
	evCaptureStop
	evCaptureDone
	evTTSStreamDone
	evPCMStreamDone
	evMetronome
	evPlayTune
	evPointer
)

type termEvent struct {
	kind     evKind
	start    proto.ProtoRecStart
	end      proto.ProtoRecEnd
	ack      proto.ProtoTTSAck
	point    proto.ProtoPointer
	screen   Screen
	c        *ipcCommand
	reply    chan any
	frame    *frameBuilt
	capture  *captureState
	tts      *ttsStream
	pcmPlay  *pcmStream
	streamID uint64
	spec     audio.MetronomeSpec
	data     []byte
}

// terminalInfo is the immutable identity and capability snapshot from HELLO.
type terminalInfo struct {
	id          string
	board       string
	firmware    string
	companionID string
	width       int
	height      int
	shape       string
	display     uint8
	codecs      uint8
	caps        uint32
	token       [proto.ResumeTokenLen]byte
}

// terminalDeps are read-only process handles, not terminal state.
type terminalDeps struct {
	e   *Edge
	srv *terminalServer
}

// terminalConn keeps the socket, peer address and SSRC under one lock.
type terminalConn struct {
	mu     sync.RWMutex // guards ip and ssrc
	ip     string
	ssrc   uint32
	native atomic.Pointer[nativeConn]
}

// terminalLife owns goroutine lifecycle: queues, cancellation and close-once.
type terminalLife struct {
	events    chan termEvent
	jobs      chan renderJob
	done      chan struct{}
	closed    atomic.Bool
	replaced  atomic.Bool
	lastTouch atomic.Int64
	closeOnce sync.Once
}

// terminalScreen is owned by the actor goroutine and the render worker.
type terminalScreen struct {
	mode         terminalMode
	rev          uint64
	nativeRev    uint16
	actionRev    uint16
	hasActionRev bool
	hits         []pixel.HitBox
	actions      map[string]pixel.Action
	page         Screen
	kind         string
	gen          uint64
}

// terminalCapture holds the active recording and the lock-free done probe.
type terminalCapture struct {
	active  atomic.Pointer[captureState]
	pending atomic.Pointer[captureState]
}

// terminalPlayback tracks TTS/PCM streams and the acknowledgement they expect.
type terminalPlayback struct {
	tts      *ttsStream
	pcm      *pcmStream
	awaitAck uint64
	turnSeq  uint64
	asleep   atomic.Bool
}

// Terminal is one connected board. Its actor goroutine owns the render,
// capture and playback groups; the TCP write loop owns the socket.
type Terminal struct {
	info     terminalInfo
	deps     terminalDeps
	conn     terminalConn
	life     terminalLife
	screen   terminalScreen
	capture  terminalCapture
	playback terminalPlayback

	cache *clipCache

	// frames encodes render output; buffers it returns are released by the
	// write path after the payload leaves the socket.
	frames *pixel.FrameEncoder
}

func (s *terminalServer) newTerminal(h proto.TerminalHello, remoteIP netip.Addr) *Terminal {
	t := &Terminal{
		info: terminalInfo{
			id: h.TerminalID, board: h.Board, firmware: h.Firmware,
			companionID: h.CompanionID, width: int(h.Width), height: int(h.Height),
			shape: "round", display: h.DisplayFormats, codecs: h.FrameCodecs, caps: h.CapsMask,
			token: randomToken(),
		},
		deps: terminalDeps{e: s.e, srv: s},
		life: terminalLife{
			events: make(chan termEvent, 128),
			jobs:   make(chan renderJob, 1),
			done:   make(chan struct{}),
		},
	}
	if h.Shape == proto.ShapeRect || t.info.width != t.info.height {
		t.info.shape = "rect"
	}
	t.conn.mu.Lock()
	t.conn.ip = remoteIP.String()
	t.conn.mu.Unlock()
	t.cache = s.e.clipCache()
	t.frames = new(pixel.FrameEncoder)
	return t
}

func (t *Terminal) start(nc *nativeConn, srv *terminalServer) {
	t.conn.native.Store(nc)
	go t.actor()
	go t.renderWorker()
}

func (t *Terminal) attach(nc *nativeConn) {
	t.conn.native.Store(nc)
	t.touchNow()
	select {
	case t.life.events <- termEvent{kind: evNone}:
	default:
	}
	t.deps.e.emit("terminal.connected", t.connectedEvent())
}

func (t *Terminal) detach(c *nativeConn) {
	if t.conn.native.CompareAndSwap(c, nil) {
		// A replaced or closing terminal must not report a disconnect after its
		// successor already connected under the same id.
		if !t.life.closed.Load() && !t.life.replaced.Load() {
			t.deps.e.emit("terminal.disconnected", map[string]any{"terminal_id": t.info.id})
		}
	}
	// Keep the actor and resume token warm for the protocol's resume window.
	time.AfterFunc(proto.ResumeWindowMS*time.Millisecond, func() {
		if t.native() == nil {
			t.close()
		}
	})
}

func (t *Terminal) native() *nativeConn { return t.conn.native.Load() }

func (t *Terminal) isClosed() bool { return t.life.closed.Load() }

func (t *Terminal) close() {
	t.life.closeOnce.Do(func() {
		t.life.closed.Store(true)
		close(t.life.done)
		if t.playback.tts != nil {
			t.playback.tts.cancel()
			t.playback.tts = nil
		}
		if t.playback.pcm != nil {
			t.playback.pcm.cancel()
			t.playback.pcm = nil
		}
		if active := t.capture.active.Swap(nil); active != nil {
			active.cancel(cancelUser)
		}
		if nc := t.native(); nc != nil {
			nc.close()
		}
		if t.deps.srv != nil {
			t.conn.mu.RLock()
			ssrc := t.conn.ssrc
			t.conn.mu.RUnlock()
			t.deps.srv.dropPeer(ssrc)
		}
		t.deps.e.unregister(t)
	})
}

func (t *Terminal) touch()    { t.life.lastTouch.Store(time.Now().UnixNano()) }
func (t *Terminal) touchNow() { t.touch() }

func (t *Terminal) pingInterval() time.Duration {
	if t.playback.asleep.Load() {
		return 30 * time.Second
	}
	return 12 * time.Second
}

func (t *Terminal) welcomePayload(udpPort uint16) []byte {
	var flags uint8
	if t.info.caps&proto.CapEdgeEndpoint != 0 {
		flags |= proto.WelcomeFlagEdgeEP
	}
	return proto.ProtoWelcome{
		ProtoVersion: proto.ProtoVersion1, Flags: flags, UdpPort: udpPort,
		Ssrc: t.conn.ssrc, MaxMsgBytes: proto.ProtoMaxPayload, ResumeWindowMS: proto.ResumeWindowMS,
		ResumeToken: t.info.token, TerminalID: t.info.id, CompanionID: t.info.companionID,
	}.Encode()
}

func (t *Terminal) reconnects() uint64 {
	if t.deps.srv == nil {
		return 0
	}
	return t.deps.srv.reconnectsFor(t.info.id)
}

func (t *Terminal) connectedEvent() map[string]any {
	t.conn.mu.RLock()
	ip := t.conn.ip
	t.conn.mu.RUnlock()
	return map[string]any{
		"terminal_id": t.info.id, "board": t.info.board, "firmware": t.info.firmware,
		"width": t.info.width, "height": t.info.height, "shape": t.info.shape, "ip": ip,
		"reconnects": t.reconnects(),
	}
}

func (t *Terminal) telemetry(m proto.ProtoTelemetry) {
	t.playback.asleep.Store(m.Flags&1 != 0)
	t.deps.e.emit("terminal.telemetry", map[string]any{
		"terminal_id": t.info.id, "wifi_rssi": int(m.WifiRssi), "battery_pct": int(m.BatteryPct),
		"charging": m.Charging != 0, "cpu_pct": int(m.CpuPct),
		"free_heap": m.FreeHeap, "uptime_s": m.UptimeS,
	})
}

func (t *Terminal) pointer(m proto.ProtoPointer) {
	t.event(evPointer, func(e *termEvent) { e.point = m })
}

func (t *Terminal) event(kind evKind, fill func(*termEvent)) {
	ev := termEvent{kind: kind}
	if fill != nil {
		fill(&ev)
	}
	select {
	case t.life.events <- ev:
	case <-t.life.done:
	}
}

func (t *Terminal) actor() {
	defer t.close()
	for {
		select {
		case <-t.life.done:
			return
		case ev := <-t.life.events:
			t.handle(ev)
		}
	}
}

func (t *Terminal) handle(ev termEvent) {
	switch ev.kind {
	case evPointer:
		t.handlePointer(ev.point)
	case evRecStart:
		t.recStartEvent(ev.start)
	case evRecEnd:
		t.recEndEvent(ev.end)
	case evTTSAck:
		t.handleAck(ev.ack)
	case evTTSStreamDone:
		if t.playback.tts == ev.tts {
			t.playback.tts = nil
			t.playback.awaitAck = ev.tts.id
			t.screen.mode = modeSpeaking
			t.ordered(proto.MsgTTSCtrl, proto.ProtoTTSCtrl{Op: proto.TtsOpEnd, AudioID: ev.tts.id}.Encode())
		}
	case evPCMStreamDone:
		if t.playback.pcm == ev.pcmPlay {
			t.playback.pcm = nil
			t.playback.awaitAck = ev.pcmPlay.id
			t.screen.mode = modeSpeaking
			t.ordered(proto.MsgTTSCtrl, proto.ProtoTTSCtrl{Op: proto.TtsOpEnd, AudioID: ev.pcmPlay.id}.Encode())
		}
	case evMetronome:
		t.playMetronome(ev.streamID, ev.spec)
	case evPlayTune:
		t.playTune(ev.streamID, ev.data)
	case evRender:
		t.render(ev.screen, true)
	case evFrameBuilt:
		t.commitFrame(ev.frame)
	case evSpeakStart:
		t.speakStart(ev)
	case evSpeakChunk:
		if t.playback.tts != nil {
			t.playback.tts.push(ev.c.Text)
		}
	case evSpeakEnd:
		if t.playback.tts != nil {
			t.playback.tts.endInput()
		}
	case evStopAudio:
		t.stopAudio(true)
	case evChime:
		if code, ok := chimeCode(ev.c.Kind); ok {
			t.ctrl(proto.MsgChime, []byte{code})
		}
	case evPower:
		level := uint8(max(0, min(100, ev.c.Brightness)))
		if level == 0 {
			t.playback.asleep.Store(true)
		} else {
			t.playback.asleep.Store(false)
		}
		t.ctrl(proto.MsgPower, proto.ProtoPower{Level: level}.Encode())
	case evVolume:
		level := max(0, min(100, ev.c.Level))
		t.ctrl(proto.MsgVolume, proto.ProtoVolume{
			Level: uint8(level), Persist: 1, TerminalID: t.info.id, CompanionID: t.info.companionID,
		}.Encode())
	case evFiller:
		t.playEphemeral(ev.c.Voice, "Mm-hmm.", true)
	case evPreview:
		t.playEphemeral(ev.c.Voice, "Hi, this is how I sound.", true)
	case evRecCancel:
		if active := t.capture.active.Load(); active != nil {
			active.cancel(cancelUser)
		}
	case evRecBegin:
		if ev.c != nil {
			t.beginCapture(ev.c.ListenWindow, ev.c.WindowS, ev.c.FollowUpGraceMS > 0, ev.c.CompanionName)
		}
	case evHits:
		t.replyHits(ev.reply)
	case evCaptureStart:
		if ev.c != nil {
			t.startDeviceCapture(ev.c)
		}
	case evCaptureStop:
		if active := t.capture.active.Load(); active != nil {
			active.cancel(cancelUser)
		}
	case evCaptureDone:
		if t.capture.active.CompareAndSwap(ev.capture, nil) {
			t.screen.mode = modeIdle
		}
	case evClose:
		t.close()
	}
}

func (t *Terminal) handlePointer(m proto.ProtoPointer) {
	if m.Kind != proto.PtrTap || len(t.screen.hits) == 0 {
		return
	}
	id := pixel.HitID(t.screen.hits, int(m.X), int(m.Y))
	if id == "" {
		return
	}
	act := t.screen.actions[id]
	if act.Kind == "" {
		act.Kind = id
	}
	switch act.Kind {
	case "listen":
		t.beginCapture(false, 0, true, "")
	case "rec-send":
		if active := t.capture.active.Load(); active != nil {
			active.cancel(cancelSend)
		}
	case "rec-cancel":
		if active := t.capture.active.Load(); active != nil {
			active.cancel(cancelUser)
		}
	case "stop", "listen-stop":
		t.stopAudio(true)
	}
	t.deps.e.emit("pointer.action", map[string]any{
		"terminal_id": t.info.id, "action": id, "value": act.Value,
	})
}

func (t *Terminal) ctrl(msgType uint8, payload []byte) bool {
	nc := t.native()
	if nc == nil {
		return false
	}
	return nc.enqueueCtrl(msgType, payload)
}

func (t *Terminal) ordered(msgType uint8, payload []byte) bool {
	nc := t.native()
	if nc == nil {
		return false
	}
	select {
	case nc.pcmOut <- outMsg{msgType, payload}:
		return true
	default:
		return false
	}
}

func (t *Terminal) pcm(payload []byte) bool {
	nc := t.native()
	if nc == nil {
		return false
	}
	return nc.enqueuePCM(payload)
}

func (t *Terminal) frame(payload []byte) {
	if nc := t.native(); nc != nil {
		if nc.enqueueFrame(payload) {
			return
		}
	}
	t.releaseFrame(payload)
}

// releaseFrame returns an encoded frame buffer to the renderer's free list.
func (t *Terminal) releaseFrame(payload []byte) {
	if t.frames != nil {
		t.frames.Release(payload)
	}
}

func (t *Terminal) render(s Screen, full bool) {
	if s.Kind == "" {
		return
	}
	t.screen.page = s
	t.screen.kind = s.Kind
	t.setModeFromScreen(s)
	t.screen.gen++
	job := renderJob{gen: t.screen.gen, screen: s, mode: t.screen.mode, full: full}
	select {
	case t.life.jobs <- job:
	default:
		select {
		case <-t.life.jobs:
		default:
		}
		t.life.jobs <- job
	}
}

func (t *Terminal) setModeFromScreen(s Screen) {
	active := t.capture.active.Load()
	switch {
	case t.playback.awaitAck != 0 || t.playback.tts != nil || t.playback.pcm != nil:
		t.screen.mode = modeSpeaking
	case active != nil && active.listen:
		t.screen.mode = modeListening
	case active != nil:
		t.screen.mode = modeRecording
	case s.Kind == "menu" || s.Kind == "list":
		t.screen.mode = modeMenu
	case s.Kind == "reader":
		t.screen.mode = modeStory
	default:
		t.screen.mode = modeIdle
	}
}

func (t *Terminal) commitFrame(fb *frameBuilt) {
	if fb == nil {
		return
	}
	if fb.gen != t.screen.gen || t.isClosed() {
		t.releaseFrame(fb.payload)
		return
	}
	t.screen.rev = fb.rev
	t.screen.nativeRev = uint16(fb.rev)
	t.screen.hits = fb.hits
	t.screen.actions = fb.actions
	t.screen.actionRev = t.screen.nativeRev
	t.screen.hasActionRev = true
	t.frame(fb.payload)
}

func (t *Terminal) replyHits(reply chan any) {
	resp := map[string]any{
		"rev": t.screen.nativeRev, "screen": t.screen.kind, "mode": t.screen.mode.String(),
		"hits": t.screen.hits,
	}
	if reply != nil {
		reply <- resp
	}
}

func chimeCode(kind string) (byte, bool) {
	switch kind {
	case "tap", "":
		return proto.ChimeTap, true
	case "think":
		return proto.ChimeThink, true
	case "cancel":
		return proto.ChimeCancel, true
	case "intercom", "celebrate":
		return proto.ChimeIntercom, true
	case "ring":
		return proto.ChimeRing, true
	}
	return 0, false
}

func (t *Terminal) voice(id string) audio.Voice {
	v := audio.ByID(id)
	if v.ID == "" {
		v = audio.ByID(audio.HeartVoice)
	}
	return v
}

func (t *Terminal) nextTurn() uint64 {
	t.playback.turnSeq++
	return t.playback.turnSeq
}

// logf avoids importing log in every helper file.
func (t *Terminal) logf(format string, args ...any) { t.deps.e.logf(format, args...) }
