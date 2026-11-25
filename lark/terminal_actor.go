package main

import (
	"context"
	"fmt"
	"image"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"lark/internal/stories"

	"lark/internal/pixel"
)

// terminalMode is the coarse session phase. Detailed state (capture phase, open
// drawer, playback generation) stays in its own field; mode exists so readers,
// logs, and tests share one name for the phase instead of inferring it from
// several booleans in different orders.
// atomicString lets routing code read a session's companion while its actor
// owns the session, without taking the hub lock.
type atomicString struct{ p *atomic.Pointer[string] }

func (a *atomicString) get() string {
	if a.p == nil {
		return ""
	}
	if p := a.p.Load(); p != nil {
		return *p
	}
	return ""
}

func (a *atomicString) set(s string) {
	if a.p == nil {
		a.p = new(atomic.Pointer[string])
	}
	a.p.Store(&s)
}

type atomicBool struct{ p *atomic.Bool }

func (a *atomicBool) load() bool { return a.p != nil && a.p.Load() }

func (a *atomicBool) store(v bool) {
	if a.p == nil {
		a.p = new(atomic.Bool)
	}
	a.p.Store(v)
}

type atomicUint64 struct{ p *atomic.Uint64 }

func (a *atomicUint64) load() uint64 {
	if a.p == nil {
		return 0
	}
	return a.p.Load()
}

func (a *atomicUint64) store(v uint64) {
	if a.p == nil {
		a.p = new(atomic.Uint64)
	}
	a.p.Store(v)
}

func atomicStr(s string) atomicString {
	var a atomicString
	a.set(s)
	return a
}

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

// modeTransitionAllowed encodes the orders the session actually reaches. A
// same-mode settle is always allowed; everything else must move through a
// phase the handler performs, not jump between incompatible capture states.
func modeTransitionAllowed(from, to terminalMode) bool {
	if from == to {
		return to <= modeStory
	}
	switch from {
	case modeIdle:
		return to == modeRecording || to == modeListening || to == modeThinking ||
			to == modeSpeaking || to == modeMenu || to == modeStory
	case modeRecording:
		return to == modeIdle || to == modeThinking || to == modeListening || to == modeMenu || to == modeStory
	case modeListening:
		return to == modeIdle || to == modeRecording || to == modeThinking || to == modeMenu || to == modeStory
	case modeThinking:
		return to == modeIdle || to == modeRecording || to == modeListening || to == modeSpeaking || to == modeMenu || to == modeStory
	case modeSpeaking:
		return to == modeIdle || to == modeRecording || to == modeListening || to == modeThinking || to == modeMenu || to == modeStory
	case modeMenu:
		return to != modeMenu && to <= modeStory
	case modeStory:
		return to != modeStory && to <= modeStory
	}
	return false
}

// deriveMode names the phase the detailed flags describe. Playback wins over a
// turn still in flight, because audio is what the board is showing and what
// gates the next capture.
func (d *terminalSession) deriveMode() terminalMode {
	switch {
	case d.ttsStarted || d.awaitAck:
		return modeSpeaking
	case d.recording.active() && d.recording.listen:
		return modeListening
	case d.recording.active():
		return modeRecording
	case d.busy:
		return modeThinking
	case d.drawer == pixel.DrawerStoryReader:
		return modeStory
	case d.drawer != "":
		return modeMenu
	}
	return modeIdle
}

func (d *terminalSession) transitionMode(to terminalMode) error {
	from := d.mode
	if !modeTransitionAllowed(from, to) {
		return fmt.Errorf("terminal %s: illegal mode transition %v -> %v", d.id, from, to)
	}
	d.mode = to
	return nil
}

// syncMode settles mode from the flags after a state change. An illegal order
// is a bug worth a log line, but the derived value still wins so a bad edit
// cannot wedge the session.
func (d *terminalSession) syncMode() {
	to := d.deriveMode()
	if to == d.mode {
		return
	}
	if err := d.transitionMode(to); err != nil {
		log.Print(err)
		d.mode = to
	}
}

type terminalEventKind uint8

const (
	evProto terminalEventKind = iota
	evTick
	evUpdate
	evStop
	evPeerRefresh
	evRender
	evCompanion
	evVoice
	evAttach
	evStoryOpen
	evStoryBG
	evMenuLoaded
	evNotice
	evTurnError
	evStartTurn
	evStopTurn
	evRecTimer
	evCapturePCM
	evEndpointDeadline
	evEndpointStop
	evFrameBuilt
	evCaption
)

type terminalRender struct {
	full bool
}

type terminalProto struct {
	msgType uint8
	payload []byte
}

type terminalTick struct {
	checkGrade bool
}

type terminalCompanion struct {
	companion companionState
	voice     string
	clearView bool
}

type terminalTurnError struct {
	id      uint64
	message string
}

type terminalAttach struct {
	companion companionState
	voice     string
}

type terminalRecTimer struct {
	id uint64
}

type terminalEndpoint struct {
	c        *endpointCapture
	discards bool
}

type terminalFrameBuilt struct {
	job    frameJob
	screen svgScreen
	view   pixel.Scene
	err    error
	native protocolFrame
	raw    int
	render time.Duration
	send   bool
}

type terminalEvent struct {
	kind terminalEventKind
	now  time.Time

	proto     terminalProto
	tick      terminalTick
	render    struct{ full bool }
	companion terminalCompanion
	attach    terminalAttach
	voice     string
	story     terminalStory
	menu      terminalMenu
	queued    queuedNotice
	turnErr   terminalTurnError
	startTurn terminalStartTurn
	recTimer  terminalRecTimer
	endpoint  terminalEndpoint
	frame     terminalFrameBuilt
	caption   terminalCaption
	reason    string

	fn   func(*terminalSession)
	done chan struct{}
}

type terminalStory struct {
	st      stories.Saved
	narrate bool
	idx     int
	bg      *image.RGBA
	passage string
	storyID string
}

type terminalMenu struct {
	drawer  string
	pending *menuContent
	menu    menuContent
	err     error
}

type terminalStartTurn struct {
	keepView bool
	source   string
	run      func(context.Context, uint64)
	started  *bool
}

type terminalCaption struct {
	view    *pixel.Scene
	oldView *pixel.Scene
}

// terminalActor serializes one session's state. The queue is unbounded and the
// wake channel is level-triggered, so a producer never blocks while holding the
// hub lock; only the goroutine in run() touches the session.
type terminalActor struct {
	mu      sync.Mutex
	queue   []terminalEvent
	wake    chan struct{}
	done    chan struct{}
	stopped bool
}

func newTerminalActor() *terminalActor {
	return &terminalActor{wake: make(chan struct{}, 1), done: make(chan struct{})}
}

func (d *terminalSession) startActor(h *terminalHub) {
	if d.actor != nil {
		return
	}
	d.recording.init()
	d.actor = newTerminalActor()
	d.frames = newFrameWorker(h, d)
	go h.runTerminal(d)
}

// post enqueues an event. A closed actor drops it instead of blocking.
func (d *terminalSession) post(ev terminalEvent) bool {
	a := d.actor
	if a == nil {
		return false
	}
	return a.enqueue(ev)
}

func (a *terminalActor) enqueue(ev terminalEvent) bool {
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return false
	}
	a.queue = append(a.queue, ev)
	a.mu.Unlock()
	select {
	case a.wake <- struct{}{}:
	default:
	}
	return true
}

// update runs fn in the actor and waits. Callers must not hold the hub lock or
// run inside another terminal's actor; fn itself must not block.
func (d *terminalSession) update(fn func(*terminalSession)) bool {
	if d.actor == nil {
		fn(d)
		return true
	}
	done := make(chan struct{})
	if !d.post(terminalEvent{kind: evUpdate, fn: fn, done: done}) {
		return false
	}
	<-done
	return true
}

func audioStreamFrom(pcm []byte) *AudioStream {
	stream := newAudioStream()
	if len(pcm) != 0 {
		stream.append(pcm)
	}
	stream.finish(nil)
	return stream
}

// terminalState is a point-in-time copy of the session fields that out-of-band
// readers (HTTP, MCP, diag, tests) may inspect. Pointers inside it are the live
// ones; callers use it for a consistent read, not for mutation.
type terminalState struct {
	mode         terminalMode
	recording    deviceCapture
	recordingPCM int

	drawer    string
	menuPage  int
	homePage  int
	menuData  *menuContent
	story     *storySession
	storyPage int
	statsAt   time.Time

	scene         pixel.Scene
	view          *pixel.Scene
	pending       *pixel.Scene
	card          *pixel.Scene
	svg           svgScreen
	frame         *diagFrame
	pose          companionPose
	moodAt        time.Time
	last          time.Time
	nameFreeUntil time.Time
	meter         time.Time
	asleep        bool

	reply       *turnReply
	replySpoken time.Time
	replyUntil  time.Time

	queuedVoice   *endpointCapture
	queuedNotices int
	turn          uint64
	finished      uint64
	trace         *turnTrace
	busy          bool
	stopTaps      int
	stopTapAt     time.Time
	agentSetScene bool

	companion companionState
	vol       int
	pct       int
	charging  bool
	telemetry terminalTelemetry
	voice     string

	audio      uint64
	replyAudio *turnAudio
	stopAudio  bool
	ttsStarted bool
	awaitAck   bool
	audioUntil time.Time

	nativeRev     uint16
	actionRev     uint16
	hasActionRev  bool
	actionHitsKey string
}

func snapshotState(d *terminalSession) terminalState {
	mu := d.recording.pcmMu
	if mu != nil {
		mu.Lock()
	}
	rec := d.recording
	pcmLen := len(rec.pcm)
	if mu != nil {
		mu.Unlock()
	}
	return terminalState{
		mode: d.mode, recording: rec, recordingPCM: pcmLen,
		drawer: d.drawer, menuPage: d.menuPage, homePage: d.homePage, menuData: d.menuData,
		story: d.story, storyPage: d.storyPage, statsAt: d.statsAt,
		scene: d.scene, view: d.view, pending: d.pending, card: d.card, svg: d.svg, frame: d.frame,
		pose: d.pose, moodAt: d.moodAt, last: d.last, nameFreeUntil: d.nameFreeUntil, meter: d.meter, asleep: d.asleep,
		reply: d.reply, replySpoken: d.replySpoken, replyUntil: d.replyUntil,
		queuedVoice: d.queuedVoice, queuedNotices: len(d.queuedNotices),
		turn: d.turn, finished: d.finished, trace: d.trace, busy: d.busy,
		stopTaps: d.stopTaps, stopTapAt: d.stopTapAt, agentSetScene: d.agentSetScene,
		companion: d.companion, vol: d.vol, pct: d.pct, charging: d.charging, telemetry: d.telemetry, voice: d.voice,
		audio: d.audio, replyAudio: d.replyAudio, stopAudio: d.stopAudio != nil,
		ttsStarted: d.ttsStarted, awaitAck: d.awaitAck, audioUntil: d.audioUntil,
		nativeRev: d.nativeRev, actionRev: d.actionRev, hasActionRev: d.hasActionRev, actionHitsKey: d.actionHitsKey,
	}
}

// state reads a consistent snapshot from the actor.
func (d *terminalSession) state() terminalState {
	if d.actor == nil {
		return snapshotState(d)
	}
	var out terminalState
	d.update(func(s *terminalSession) { out = snapshotState(s) })
	return out
}

// close stops the actor after everything already queued. It must be called
// without the hub lock held.
func (d *terminalSession) close() {
	d.closeWith("disconnect")
}

func (d *terminalSession) closeWith(reason string) {
	a := d.actor
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return
	}
	// Stop is the last event: post() drops anything that arrives after it.
	a.queue = append(a.queue, terminalEvent{kind: evStop, reason: reason})
	a.stopped = true
	a.mu.Unlock()
	select {
	case a.wake <- struct{}{}:
	default:
	}
	<-a.done
	if d.frames != nil {
		d.frames.close()
		d.frames = nil
	}
}

func (h *terminalHub) runTerminal(d *terminalSession) {
	a := d.actor
	defer close(a.done)
	for {
		<-a.wake
		for {
			a.mu.Lock()
			if len(a.queue) == 0 {
				a.mu.Unlock()
				break
			}
			ev := a.queue[0]
			a.queue[0] = terminalEvent{}
			a.queue = a.queue[1:]
			a.mu.Unlock()
			if ev.kind == evStop {
				h.stopTerminal(d, ev.reason)
				return
			}
			h.handleTerminal(d, ev)
		}
	}
}

func (h *terminalHub) handleTerminal(d *terminalSession, ev terminalEvent) {
	switch ev.kind {
	case evUpdate:
		if ev.fn != nil {
			ev.fn(d)
		}
		if ev.done != nil {
			close(ev.done)
		}
	case evProto:
		h.handleProtoEvent(d, ev.proto.msgType, ev.proto.payload, ev.now)
	case evTick:
		h.tickTerminal(d, ev.now, ev.tick.checkGrade)
	case evRender:
		if h.active(d) {
			h.render(d, ev.now, ev.render.full)
		}
	case evPeerRefresh:
		if h.active(d) && (d.drawer == "intercom" || d.card.HasIntercom() || d.view.HasIntercom()) {
			h.render(d, ev.now, false)
		}
	case evCompanion:
		c := ev.companion
		d.companion, d.voice = c.companion, c.voice
		if c.clearView {
			h.clearView(d)
			d.agentSetScene = d.busy
		}
		if !c.companion.Listening && d.recording.active() && d.recording.listen {
			h.stopListeningWindow(d, ev.now)
		}
		h.render(d, ev.now, true)
	case evVoice:
		d.voice = ev.voice
		d.companion.Voice = ev.voice
		if d.drawer != "" {
			h.render(d, ev.now, false)
		}
	case evAttach:
		d.recording.clear()
		h.cancelTurn(d, "reassigned")
		h.stopPlayback(d, true)
		d.companion, d.voice = ev.attach.companion, ev.attach.voice
		h.clearView(d)
		h.render(d, ev.now, true)
		h.sendSnapshotState(d)
	case evStoryOpen:
		h.openStoryEvent(d, ev.story.st, ev.story.narrate)
	case evStoryBG:
		if d.drawer == pixel.DrawerStoryReader && d.story != nil && d.story.id == ev.story.storyID &&
			d.story.currentPassageName() == ev.story.passage {
			d.story.bgImg = ev.story.bg
			h.render(d, ev.now, false)
		}
	case evMenuLoaded:
		h.menuLoaded(d, ev.menu, ev.now)
	case evNotice:
		h.deliverNoticeEvent(d, ev.queued, ev.now)
		if ev.done != nil {
			close(ev.done)
		}
	case evCaption:
		h.captionEvent(d, ev.caption, ev.now)
	case evTurnError:
		h.turnFailed(d, ev.turnErr.id, ev.turnErr.message)
	case evStartTurn:
		started := h.startTurn(d, ev.now, ev.startTurn.keepView, ev.startTurn.source, ev.startTurn.run)
		if ev.startTurn.started != nil {
			*ev.startTurn.started = started
		}
		if ev.done != nil {
			close(ev.done)
		}
	case evStopTurn:
		h.stopTurnNow(d, ev.now)
	case evRecTimer:
		h.captureStopTimer(d, ev.recTimer.id, ev.now)
	case evCapturePCM:
		h.captureLimit(d, ev.now)
	case evEndpointDeadline:
		h.endpointDeadline(d, ev.endpoint.c, ev.now)
	case evEndpointStop:
		h.endpointStop(d, ev.endpoint.c, ev.endpoint.discards, ev.now)
	case evFrameBuilt:
		h.handleFrameBuilt(d, ev.frame)
	}
	d.syncMode()
}

// stopTerminal is the actor's last turn: it cancels capture, a turn in flight,
// and playback before the goroutine exits. The hub lock is never held here.
func (h *terminalHub) stopTerminal(d *terminalSession, reason string) {
	d.recording.clear()
	h.cancelTurn(d, reason)
	h.stopPlayback(d, false)
	d.syncMode()
}

// tickTerminal applies one 40 ms animate decision to a session.
func (h *terminalHub) tickTerminal(d *terminalSession, now time.Time, checkGrade bool) {
	if !h.active(d) {
		return
	}
	turnActive := d.recording.active() || d.busy || d.ttsStarted || d.awaitAck
	if checkGrade && !turnActive && !(d.asleep && d.caps.PanelSleep) &&
		d.svg.grade != h.frameGrade(d, now) {
		h.render(d, now, false)
	}
	switch {
	case d.recording.active() && !d.recording.listen && d.recording.stopTimer == nil && now.Sub(d.recording.started) > captureFailsafe:
		h.stopRecordingAtLimit(d, now)
	case d.awaitAck && !d.ttsStarted && now.After(d.audioUntil.Add(ttsAckFallback)) &&
		(!losslessTTSAcks(d.firmware) || d.ackClient != d.client):
		h.playbackDone(d, now, false)
	case !d.asleep && d.drawer != pixel.DrawerPair && !turnActive && now.Sub(d.last) >= idleSleep:
		h.setSleeping(d, true)
		h.render(d, now, false)
	case d.asleep && d.caps.PanelSleep:
		// The battery board's panel is off; skip idle updates.
	case d.drawer == "stats" && !d.asleep && !turnActive && now.Sub(d.statsAt) >= diagInterval:
		h.render(d, now, false)
	case d.stopTapsExpired(now):
		h.render(d, now, false)
	}
	d.syncMode()
}
