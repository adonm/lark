package main

import (
	"log"
	"sync"
	"time"
)

type capturePhase uint8

const (
	captureIdle capturePhase = iota
	captureRequested
	captureOpen
	captureStoppingRequested
	captureStoppingOpen
)

// Owned by the session actor. Stopping still accepts PCM after the board has
// acknowledged rec_start. The raw PCM buffer is the one exception: UDP audio
// arrives on the read loop and appends into the preallocated buffer under
// pcmMu so the fast path stays allocation-free.
type deviceCapture struct {
	pcmMu       *sync.Mutex
	accepting   atomicBool
	phase       capturePhase
	session     uint16 // rec_start session this capture was requested with
	started     time.Time
	target      string
	pcm         []byte
	stopTimer   *time.Timer
	endpoint    *endpointCapture
	ignoreStart bool
	listen      bool
	listenUntil time.Time
	listenSpan  time.Duration
	debug       bool // Capture raw PCM for the microphone audit, never a turn.
	nameGate    bool // Auto-opened listening window: require the name after 10 s.
}

func (c *deviceCapture) active() bool { return c.phase != captureIdle }
func (c *deviceCapture) open() bool {
	return c.phase == captureOpen || c.phase == captureStoppingOpen
}

// init allocates the producer-side primitives before the actor starts, so the
// UDP read loop never races their first assignment.
func (c *deviceCapture) init() {
	if c.pcmMu == nil {
		c.pcmMu = new(sync.Mutex)
	}
	c.accepting.store(false)
}

func (c *deviceCapture) lock() *sync.Mutex {
	if c.pcmMu == nil {
		c.pcmMu = new(sync.Mutex)
	}
	return c.pcmMu
}

func (c *deviceCapture) start(now time.Time, target string, request bool) {
	mu := c.lock()
	c.phase = captureOpen
	if request {
		c.phase = captureRequested
	}
	mu.Lock()
	c.pcm = nil
	mu.Unlock()
	c.accepting.store(true)
	c.started, c.target, c.ignoreStart = now, target, false
	c.nameGate = false
	c.session = 0
}

func (c *deviceCapture) acceptsStart() bool { return !c.open() && !c.ignoreStart }

func (c *deviceCapture) acknowledge() {
	switch c.phase {
	case captureRequested:
		c.phase = captureOpen
	case captureStoppingRequested:
		c.phase = captureStoppingOpen
	}
}

func (c *deviceCapture) requestStop() bool {
	switch c.phase {
	case captureRequested:
		c.phase = captureStoppingRequested
	case captureOpen:
		c.phase = captureStoppingOpen
	default:
		return false
	}
	return true
}

func (c *deviceCapture) suppressLateStart() { c.ignoreStart = !c.open() }

func (c *deviceCapture) receivedEnd() { c.ignoreStart = false }

func (c *deviceCapture) clear() {
	if e := c.endpoint; e != nil {
		c.endpoint = nil
		if e.owner != nil {
			e.owner.activeCapture.CompareAndSwap(e, nil)
		}
		if e.deadline != nil {
			e.deadline.Stop()
		}
		if e.cancel != nil && !e.committing {
			if debugEndpoint {
				log.Printf("capture cleared/cancelled companion=%s phase=%d", e.companionID, c.phase)
			}
			e.cancel()
		}
		if e.listening != nil {
			close(e.listening)
		}
	}
	if c.stopTimer != nil {
		c.stopTimer.Stop()
		c.stopTimer = nil
	}
	c.accepting.store(false)
	if mu := c.pcmMu; mu != nil {
		mu.Lock()
		c.pcm = nil
		mu.Unlock()
	}
	c.phase, c.started, c.target = captureIdle, time.Time{}, ""
	c.listen, c.listenUntil, c.listenSpan, c.debug, c.nameGate = false, time.Time{}, 0, false, false
	c.session = 0
}
