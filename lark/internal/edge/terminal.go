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
	evPointer
)

type termEvent struct {
	kind    evKind
	hello   proto.TerminalHello
	start   proto.ProtoRecStart
	end     proto.ProtoRecEnd
	ack     proto.ProtoTTSAck
	point   proto.ProtoPointer
	tele    proto.ProtoTelemetry
	screen  Screen
	c       *ipcCommand
	reply   chan any
	frame   *frameBuilt
	capture *captureState
	tts     *ttsStream
}

// Terminal is one connected board. Its actor goroutine owns mode, revisions,
// hit boxes, capture and playback state; the TCP write loop owns the socket.
type Terminal struct {
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

	e   *Edge
	srv *terminalServer

	mu   sync.RWMutex
	ip   string
	ssrc uint32
	conn atomic.Pointer[nativeConn]

	events    chan termEvent
	jobs      chan renderJob
	done      chan struct{}
	closed    atomic.Bool
	lastTouch atomic.Int64

	mode         terminalMode
	rev          uint64
	nativeRev    uint16
	actionRev    uint16
	hasActionRev bool
	hits         []pixel.HitBox
	actions      map[string]pixel.Action
	screen       Screen
	screenKind   string
	renderGen    uint64

	cap     atomic.Pointer[captureState]
	capture *captureState

	tts      *ttsStream
	awaitAck uint64
	turnSeq  uint64
	asleep   atomic.Bool

	cache     *clipCache
	closeOnce sync.Once
}

func (s *terminalServer) newTerminal(h proto.TerminalHello, remoteIP netip.Addr) *Terminal {
	t := &Terminal{
		id: h.TerminalID, board: h.Board, firmware: h.Firmware,
		companionID: h.CompanionID, width: int(h.Width), height: int(h.Height),
		shape: "round", display: h.DisplayFormats, codecs: h.FrameCodecs, caps: h.CapsMask,
		e: s.e, srv: s, events: make(chan termEvent, 128),
		jobs: make(chan renderJob, 1), done: make(chan struct{}),
		token: randomToken(),
	}
	if h.Shape == proto.ShapeRect || t.width != t.height {
		t.shape = "rect"
	}
	t.mu.Lock()
	t.ip = remoteIP.String()
	t.mu.Unlock()
	t.cache = s.e.clipCache()
	return t
}

func (t *Terminal) start(nc *nativeConn, srv *terminalServer) {
	t.conn.Store(nc)
	go t.actor()
	go t.renderWorker()
}

func (t *Terminal) attach(nc *nativeConn) {
	t.conn.Store(nc)
	t.touchNow()
	select {
	case t.events <- termEvent{kind: evNone}:
	default:
	}
	t.e.emit("terminal.connected", t.connectedEvent())
}

func (t *Terminal) detach(c *nativeConn) {
	if t.conn.CompareAndSwap(c, nil) {
		t.e.emit("terminal.disconnected", map[string]any{"terminal_id": t.id})
	}
	// Keep the actor and resume token warm for the protocol's resume window.
	time.AfterFunc(proto.ResumeWindowMS*time.Millisecond, func() {
		if t.native() == nil {
			t.close()
		}
	})
}

func (t *Terminal) native() *nativeConn { return t.conn.Load() }

func (t *Terminal) isClosed() bool { return t.closed.Load() }

func (t *Terminal) close() {
	t.closeOnce.Do(func() {
		t.closed.Store(true)
		close(t.done)
		if t.tts != nil {
			t.tts.cancel()
			t.tts = nil
		}
		if t.capture != nil {
			t.capture.cancel(cancelUser)
			t.capture = nil
		}
		if nc := t.native(); nc != nil {
			nc.close()
		}
		if t.srv != nil {
			t.mu.RLock()
			ssrc := t.ssrc
			t.mu.RUnlock()
			t.srv.dropPeer(ssrc)
		}
		t.e.unregister(t)
	})
}

func (t *Terminal) touch()    { t.lastTouch.Store(time.Now().UnixNano()) }
func (t *Terminal) touchNow() { t.touch() }

func (t *Terminal) pingInterval() time.Duration {
	if t.asleep.Load() {
		return 30 * time.Second
	}
	return 12 * time.Second
}

func (t *Terminal) welcomePayload(udpPort uint16) []byte {
	var flags uint8
	if t.caps&proto.CapBridgeEndpoint != 0 {
		flags |= proto.WelcomeFlagBridgeEP
	}
	return proto.ProtoWelcome{
		ProtoVersion: proto.ProtoVersion1, Flags: flags, UdpPort: udpPort,
		Ssrc: t.ssrc, MaxMsgBytes: proto.ProtoMaxPayload, ResumeWindowMS: proto.ResumeWindowMS,
		ResumeToken: t.token, TerminalID: t.id, CompanionID: t.companionID,
	}.Encode()
}

func (t *Terminal) connectedEvent() map[string]any {
	t.mu.RLock()
	ip := t.ip
	t.mu.RUnlock()
	return map[string]any{
		"terminal_id": t.id, "board": t.board, "firmware": t.firmware,
		"width": t.width, "height": t.height, "shape": t.shape, "ip": ip,
	}
}

func (t *Terminal) telemetry(m proto.ProtoTelemetry) {
	t.asleep.Store(m.Flags&1 != 0)
	t.e.emit("terminal.telemetry", map[string]any{
		"terminal_id": t.id, "wifi_rssi": int(m.WifiRssi), "battery_pct": int(m.BatteryPct),
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
	case t.events <- ev:
	case <-t.done:
	}
}

func (t *Terminal) actor() {
	defer t.close()
	for {
		select {
		case <-t.done:
			return
		case ev := <-t.events:
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
		if t.tts == ev.tts {
			t.tts = nil
			t.awaitAck = ev.tts.id
			t.mode = modeSpeaking
			t.ordered(proto.MsgTTSCtrl, proto.ProtoTTSCtrl{Op: proto.TtsOpEnd, AudioID: ev.tts.id}.Encode())
		}
	case evRender:
		t.render(ev.screen, true)
	case evFrameBuilt:
		t.commitFrame(ev.frame)
	case evSpeakStart:
		t.speakStart(ev)
	case evSpeakChunk:
		if t.tts != nil {
			t.tts.push(ev.c.Text)
		}
	case evSpeakEnd:
		if t.tts != nil {
			t.tts.endInput()
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
			t.asleep.Store(true)
		} else {
			t.asleep.Store(false)
		}
		t.ctrl(proto.MsgPower, proto.ProtoPower{Level: level}.Encode())
	case evVolume:
		level := max(0, min(100, ev.c.Level))
		t.ctrl(proto.MsgVolume, proto.ProtoVolume{
			Level: uint8(level), Persist: 1, TerminalID: t.id, CompanionID: t.companionID,
		}.Encode())
	case evFiller:
		t.playEphemeral(ev.c.Voice, "Mm-hmm.", true)
	case evPreview:
		t.playEphemeral(ev.c.Voice, "Hi, this is how I sound.", true)
	case evRecCancel:
		if t.capture != nil {
			t.capture.cancel(cancelUser)
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
		if t.capture != nil {
			t.capture.cancel(cancelUser)
		}
	case evCaptureDone:
		if t.capture == ev.capture {
			t.capture = nil
			t.mode = modeIdle
		}
	case evClose:
		t.close()
	}
}

func (t *Terminal) handlePointer(m proto.ProtoPointer) {
	if m.Kind != proto.PtrTap || len(t.hits) == 0 {
		return
	}
	id := pixel.HitID(t.hits, int(m.X), int(m.Y))
	if id == "" {
		return
	}
	act := t.actions[id]
	if act.Kind == "" {
		act.Kind = id
	}
	switch act.Kind {
	case "listen":
		t.beginCapture(false, 0, true, "")
	case "rec-send":
		if t.capture != nil {
			t.capture.cancel(cancelSend)
		}
	case "rec-cancel":
		if t.capture != nil {
			t.capture.cancel(cancelUser)
		}
	case "stop", "listen-stop":
		t.stopAudio(true)
	}
	t.e.emit("pointer.action", map[string]any{
		"terminal_id": t.id, "action": id, "value": act.Value,
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
		nc.enqueueFrame(payload)
	}
}

func (t *Terminal) render(s Screen, full bool) {
	if s.Kind == "" {
		return
	}
	t.screen = s
	t.screenKind = s.Kind
	t.setModeFromScreen(s)
	t.renderGen++
	job := renderJob{gen: t.renderGen, screen: s, mode: t.mode, full: full}
	select {
	case t.jobs <- job:
	default:
		select {
		case <-t.jobs:
		default:
		}
		t.jobs <- job
	}
}

func (t *Terminal) setModeFromScreen(s Screen) {
	switch {
	case t.awaitAck != 0 || t.tts != nil:
		t.mode = modeSpeaking
	case t.capture != nil && t.capture.listen:
		t.mode = modeListening
	case t.capture != nil:
		t.mode = modeRecording
	case s.Kind == "menu" || s.Kind == "list":
		t.mode = modeMenu
	case s.Kind == "reader":
		t.mode = modeStory
	default:
		t.mode = modeIdle
	}
}

func (t *Terminal) commitFrame(fb *frameBuilt) {
	if fb == nil || fb.gen != t.renderGen || t.isClosed() {
		return
	}
	t.rev = fb.rev
	t.nativeRev = uint16(fb.rev)
	t.hits = fb.hits
	t.actions = fb.actions
	t.actionRev = t.nativeRev
	t.hasActionRev = true
	t.frame(fb.payload)
}

func (t *Terminal) replyHits(reply chan any) {
	resp := map[string]any{
		"rev": t.nativeRev, "screen": t.screenKind, "mode": t.mode.String(),
		"hits": t.hits,
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
	t.turnSeq++
	return t.turnSeq
}

// logf avoids importing log in every helper file.
func (t *Terminal) logf(format string, args ...any) { t.e.logf(format, args...) }
