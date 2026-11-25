package main

import (
	"context"
	"os"
	"sync"
	"time"
)

var debugEndpoint = os.Getenv("LARK_DEBUG_ENDPOINT") != ""

type endpointSettings struct {
	mu        sync.RWMutex
	newVAD    func() endpointVAD
	smartTurn func([]byte) (float32, error)
}

// smart returns the semantic endpoint function, or nil while Smart Turn is
// still loading in the background.
func (e *endpointSettings) smart() func([]byte) (float32, error) {
	if e == nil {
		return nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.smartTurn
}

func (e *endpointSettings) setSmartTurn(fn func([]byte) (float32, error)) {
	e.mu.Lock()
	e.smartTurn = fn
	e.mu.Unlock()
}

const (
	endpointRingSlots = 256
	endpointSlotBytes = rtpPCMBytes // one firmware record chunk (20 ms)
)

type endpointRing [endpointRingSlots][endpointSlotBytes]byte

var endpointRings = sync.Pool{New: func() any { return new(endpointRing) }}

type endpointCapture struct {
	ctx         context.Context
	cancel      context.CancelFunc
	owner       *terminalClient
	packets     chan micPacket
	free        chan int
	slotsMu     sync.Mutex // Guards the pool hand-off between push and captureEndpoint.
	slots       *endpointRing
	stop        chan struct{}
	result      chan captureResult
	listening   chan struct{}
	deadline    *time.Timer // Hub owns and stops the wall-clock watchdog.
	commitAt    time.Time   // Hub writes before closing stop.
	listen      bool
	debug       bool // Raw capture for lark capture; no VAD decisions or turn.
	nameGate    bool // Auto-opened listening window: require the name after 10 s.
	committing  bool // finishEndpoint closed stop; let the worker deliver its result.
	limitBytes  int  // PCM cap for this capture's full window
	limitSpan   time.Duration
	started     time.Time // Capture start; a read-out after this is not its answer.
	reply       *turnReply
	companionID string
}

type micPacket struct {
	pcm  []byte
	at   time.Time
	slot int
}

func (c *endpointCapture) push(pcm []byte, at time.Time) bool {
	c.slotsMu.Lock()
	defer c.slotsMu.Unlock()
	if c.slots == nil {
		return false
	}
	if c.free == nil {
		select {
		case c.packets <- micPacket{pcm: append([]byte(nil), pcm...), at: at, slot: -1}:
			return true
		default:
			return false
		}
	}
	select {
	case idx := <-c.free:
		if len(pcm) <= endpointSlotBytes {
			n := copy(c.slots[idx][:], pcm)
			c.packets <- micPacket{pcm: c.slots[idx][:n], at: at, slot: idx}
			return true
		}
		// Rare oversized chunk: keep the slot and copy out of band.
		c.free <- idx
		select {
		case c.packets <- micPacket{pcm: append([]byte(nil), pcm...), at: at, slot: -1}:
			return true
		default:
			return false
		}
	default:
		return false
	}
}

type captureResult struct {
	pcm              []byte
	text             string
	err              error
	stt, endToCommit time.Duration
	sttQueue, sttRun time.Duration
	vadQueue         time.Duration
	hasEnd           bool
	listen           bool
	reply            *turnReply
	spec             *specTurn
	speech, audio    time.Duration
	speechAt         time.Time // wall time of the first voiced packet
	nameGate         bool
}

type pauseTranscript struct {
	at               time.Duration
	from             time.Duration // start of the decoded audio within the clip
	audioLen         int
	windowed         bool
	text             string
	err              error
	stt              time.Duration
	sttQueue, sttRun time.Duration
}

type smartTurnResult struct {
	prob   float32
	wordAt time.Duration
}

func (h *terminalHub) startEndpoint(d *terminalSession) {
	ctx, cancel := context.WithCancel(d.client.ctx)
	limit := endpointLimit
	var reply *turnReply
	if d.recording.listen && d.recording.listenSpan > 0 {
		limit = d.recording.listenSpan
	}
	// A read-out arms the reply for the next spoken turn, whether the child taps
	// to talk or answers inside the listening window.
	reply = d.pendingReply(time.Now())
	free := make(chan int, endpointRingSlots)
	for i := range endpointRingSlots {
		free <- i
	}
	c := &endpointCapture{
		ctx:         ctx,
		cancel:      cancel,
		owner:       d.client,
		packets:     make(chan micPacket, endpointRingSlots),
		free:        free,
		slots:       endpointRings.Get().(*endpointRing),
		stop:        make(chan struct{}),
		result:      make(chan captureResult, 1),
		listening:   make(chan struct{}),
		listen:      d.recording.listen,
		debug:       d.recording.debug,
		nameGate:    d.recording.nameGate,
		limitSpan:   limit,
		limitBytes:  utteranceBytes(limit),
		started:     time.Now(),
		reply:       reply,
		companionID: d.companionID.get(),
	}
	d.recording.endpoint = c
	d.client.activeCapture.Store(c)
	c.deadline = time.AfterFunc(limit, func() {
		d.post(terminalEvent{kind: evEndpointDeadline, now: time.Now(), endpoint: terminalEndpoint{c: c}})
	})
	go h.captureEndpoint(d, c)
}

func (h *terminalHub) endpointDeadline(d *terminalSession, c *endpointCapture, now time.Time) {
	if !h.active(d) || d.recording.endpoint != c {
		return
	}
	if c.listen {
		h.b.diag.listen.closeCap.Add(1)
	}
	if c.debug {
		h.finishEndpoint(d, now)
		return
	}
	h.recordingControl(d, now, c.listen)
}

func (h *terminalHub) endpointStop(d *terminalSession, c *endpointCapture, discards bool, now time.Time) {
	// Don't stop on an old decision while more speech is queued.
	if h.active(d) && d.recording.endpoint == c && d.recording.phase == captureOpen && len(c.packets) == 0 {
		h.recordingControl(d, now, discards)
	}
}

func (h *terminalHub) captureEndpoint(d *terminalSession, c *endpointCapture) {
	defer close(c.result)
	r := h.newEndpointRun(d, c)
	defer r.close()
	r.loop()
}

func (h *terminalHub) finishEndpoint(d *terminalSession, now time.Time) {
	c := d.recording.endpoint
	d.recording.endpoint = nil
	if d.client != nil {
		d.client.activeCapture.CompareAndSwap(c, nil)
	}
	c.deadline.Stop()
	if c.commitAt.IsZero() {
		c.commitAt = now
	}
	// The worker owns the final decode. Mark the commit before closing stop so
	// clear() does not cancel the context and race it out of sending a result.
	c.committing = true
	if c.stop != nil {
		close(c.stop)
	} else {
		close(c.packets)
	}
	close(c.listening)
	d.recording.clear()
	if c.debug {
		h.render(d, now, false)
		return
	}
	d.queuedVoice = c
	if !h.maybeStartVoice(d, now) {
		h.render(d, now, false)
	}
}

// Only one captured follow-up may wait for the current reply. It enters the
// same regular prompt gate as any other turn, without interrupt or retry.
func (h *terminalHub) maybeStartVoice(d *terminalSession, now time.Time) bool {
	c := d.queuedVoice
	if c == nil || d.busy || d.stopAudio != nil || d.awaitAck || !h.active(d) {
		return false
	}
	d.queuedVoice = nil
	h.startTurn(d, now, false, "voice", func(ctx context.Context, id uint64) {
		// Explicit Stop also cancels any outstanding work for this capture.
		stop := context.AfterFunc(ctx, c.cancel)
		defer stop()
		defer c.cancel()
		var result captureResult
		select {
		case result = <-c.result:
		case <-ctx.Done():
			return
		}
		if len(result.pcm) == 0 {
			h.finishTurnAsync(d, id, nil, nil)
			return
		}
		trace := traceFrom(ctx)
		trace.measure(diagSTT, result.stt)
		if result.sttRun > 0 || result.sttQueue > 0 {
			trace.measure(diagSTTQueue, result.sttQueue)
			trace.measure(diagSTTRun, result.sttRun)
		}
		trace.measure(diagVADQueue, result.vadQueue)
		if result.hasEnd {
			trace.measure(diagEndpoint, result.endToCommit)
		}
		if result.err != nil {
			h.turnError(d, id, "Could not hear that. Try again.")
			return
		}
		if result.speech > 0 {
			words, _, _ := inspectTranscript(result.text)
			h.b.diag.listen.speech.Add(1)
			h.b.diag.addSegment(diagSegment{
				TS: time.Now().UnixMilli(), Terminal: d.id, Companion: d.companionID.get(),
				DurationMS: result.audio.Milliseconds(), SpeechMS: result.speech.Milliseconds(),
				TextLen: len(result.text), Words: words,
			})
		}
		if result.nameGate && h.nameRequired(d, result.speechAt) {
			rest, named := splitName(result.text, h.b.companionFor(d.companionID.get()).Name)
			if !named {
				// Background chatter after the follow-up grace period is
				// ignored without a spoken reply.
				h.b.diag.listen.nameRequired.Add(1)
				h.finishTurnAsync(d, id, nil, nil)
				return
			}
			result.text = rest
		}
		if result.text == "" || isFillerTranscript(result.text) {
			if result.text == "" {
				h.b.diag.listen.emptyTranscript.Add(1)
			} else {
				h.b.diag.listen.fillerOnly.Add(1)
			}
			// A cough or a door does not deserve a spoken apology; a segment
			// with real speech and no usable words does.
			if result.speech >= time.Second {
				h.replyNotCaught(ctx, d, id)
				return
			}
			h.finishTurnAsync(d, id, nil, nil)
			return
		}
		h.b.diag.listen.committed.Add(1)
		if result.reply == nil {
			h.dropStaleReply(d, c.started)
		}
		h.runTurnChannel(ctx, d, id, result.text, "voice", result.pcm, turnChannel{reply: result.reply, spec: result.spec})
	})
	return true
}
