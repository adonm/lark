package main

import "time"

type capturePhase uint8

const (
	captureIdle capturePhase = iota
	captureRequested
	captureOpen
	captureStoppingRequested
	captureStoppingOpen
)

// Owned by the hub lock. Stopping still accepts PCM after the board has
// acknowledged rec_start.
type deviceCapture struct {
	phase       capturePhase
	started     time.Time
	target      string
	pcm         []byte
	stopTimer   *time.Timer
	endpoint    *endpointCapture
	ignoreStart bool
}

func (c *deviceCapture) active() bool { return c.phase != captureIdle }
func (c *deviceCapture) open() bool {
	return c.phase == captureOpen || c.phase == captureStoppingOpen
}

func (c *deviceCapture) start(now time.Time, target string, request bool) {
	c.phase = captureOpen
	if request {
		c.phase = captureRequested
	}
	c.started, c.target, c.pcm, c.ignoreStart = now, target, nil, false
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
		e.deadline.Stop()
		e.cancel()
		close(e.listening)
	}
	if c.stopTimer != nil {
		c.stopTimer.Stop()
		c.stopTimer = nil
	}
	c.phase, c.started, c.target, c.pcm = captureIdle, time.Time{}, "", nil
}
