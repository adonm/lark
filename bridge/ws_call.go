package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

type callState uint8

const (
	callNone callState = iota
	callRingingOut
	callRingingIn
	callActive
)

const (
	callRingEvery = 2500 * time.Millisecond
	callRingLimit = 25 * time.Second
	callNoticeFor = 4 * time.Second
	callPCMBytes  = 2 * sampleRate * 40 / 1000
)

// Call state and control/PCM ordering are owned by the hub lock.
func (h *wsHub) callPeer(d *wsDevice) *wsDevice {
	p := h.devices[d.callPeer]
	if d.callID == 0 || p == nil || p.callID != d.callID || p.callPeer != d.key {
		return nil
	}
	return p
}

func (h *wsHub) canCall(d *wsDevice, peerID string) bool {
	p := h.devices[strings.ToLower(peerID)]
	return d.caps.Call && p != nil && p != d && h.active(p) && p.caps.Call
}

func (h *wsHub) interruptForCall(d *wsDevice, now time.Time) {
	h.cancelTurn(d, "call")
	h.stopPlayback(d, true)
	h.abortCallRecording(d)
	h.clearView(d)
	h.wake(d, now)
}

func (h *wsHub) abortCallRecording(d *wsDevice) {
	rec := &d.recording
	if rec.active() {
		h.push(d, object{"op": "rec", "action": "stop", "target": rec.target, "cancelled": true}, false)
		rec.clear()
	}
	rec.ignoreStart = false
}

func (h *wsHub) callNotice(d *wsDevice, message string, now time.Time) {
	view := noticeScene("Can't call", message)
	d.view = &view
	h.render(d, now, false)
	time.AfterFunc(callNoticeFor, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.active(d) && d.view == &view {
			d.view = nil
			h.render(d, time.Now(), false)
		}
	})
}

func (h *wsHub) startCall(caller *wsDevice, peerID string) {
	if err := h.ringCall(caller, peerID, callRingLimit); err != nil && h.active(caller) && caller.parentReason == "" && caller.callState == callNone {
		now := time.Now()
		h.interruptForCall(caller, now)
		h.callNotice(caller, err.Error(), now)
	}
}

func (h *wsHub) ringCall(caller *wsDevice, peerID string, limit time.Duration) error {
	if !h.active(caller) {
		return problem(409, "Caller is not connected.")
	}
	key := strings.ToLower(peerID)
	callee := h.devices[key]
	now := time.Now()
	if r := h.parentRefresh(caller.key, now); r != "" {
		return r.err()
	}
	if callee != nil && h.active(callee) && h.parentRefresh(key, now) != "" {
		return problem(423, callee.companion.Name+" is resting right now. Try again later.")
	}
	// The first request already installed both halves. A reciprocal request
	// from the ringing callee is an answer, not a second call.
	if caller.callState == callRingingIn && callee != nil && h.callPeer(caller) == callee &&
		h.active(callee) && callee.callState == callRingingOut {
		h.answerCall(caller)
		return nil
	}
	if caller.callState != callNone {
		return problem(409, "Caller is already on a call.")
	}
	name := peerID
	if callee != nil {
		name = callee.companion.Name
	} else {
		for _, p := range h.storedPeers {
			if strings.EqualFold(p.DeviceID, key) {
				name = p.Character
				break
			}
		}
	}
	switch {
	case !caller.caps.Call:
		return problem(409, "This companion needs call support (firmware 0.9.2 or newer).")
	case callee == caller:
		return problem(400, "Choose another companion to call.")
	case callee == nil || !h.active(callee):
		return problem(409, name+" is offline.")
	case !callee.caps.Call:
		return problem(409, name+" needs firmware 0.9.2 or newer to call.")
	case callee.callState != callNone:
		return problem(409, name+" is busy.")
	}
	h.nextCall++
	if h.nextCall == 0 {
		h.nextCall++
	}
	id := h.nextCall
	ctx, cancel := context.WithTimeout(caller.client.ctx, limit)
	h.interruptForCall(caller, now)
	h.abortCallRecording(callee)
	h.wake(callee, now)
	caller.callState, callee.callState = callRingingOut, callRingingIn
	caller.callPeer, callee.callPeer = callee.key, caller.key
	caller.callID, callee.callID = id, id
	caller.callOrigin = true
	caller.callCancel = cancel
	logCall("start", caller, callee, "", 0)
	for _, d := range []*wsDevice{caller, callee} {
		h.chime(d, "ring")
		// A new call may have the same artwork as the one that just ended.
		h.render(d, now, true)
	}
	go h.ringUntilAnswered(ctx, caller, id)
	return nil
}

func (h *wsHub) mcpCall(from, to string) (object, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	caller := h.devices[strings.ToLower(from)]
	if caller == nil || caller.who.DeviceID != from {
		return nil, problem(409, "Caller is not connected.")
	}
	if callee := h.devices[strings.ToLower(to)]; callee == nil || callee.who.DeviceID != to {
		return nil, problem(409, "Recipient is not connected.")
	}
	if err := h.ringCall(caller, to, callRingLimit); err != nil {
		return nil, err
	}
	state := "ringing"
	if caller.callState == callActive {
		state = "connected"
	}
	return object{"id": caller.callID, "state": state}, nil
}

func (h *wsHub) callSnapshot(d *wsDevice) object {
	if d.callState == callNone {
		return nil
	}
	state := "ringing"
	if d.callState == callActive {
		state = "connected"
	}
	peerID := d.callPeer
	if p := h.callPeer(d); p != nil {
		peerID = p.who.DeviceID
	}
	direction := "incoming"
	if d.callOrigin {
		direction = "outgoing"
	}
	out := object{"id": d.callID, "state": state, "direction": direction, "peer_device_id": peerID, "muted": d.callMuted}
	if d.callState == callActive {
		out["started_at"] = d.callStarted.UTC()
	}
	return out
}

func (h *wsHub) mcpHangup(id string, expected *uint64) (object, []object) {
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.devices[strings.ToLower(id)]
	skip := func(reason string) (object, []object) {
		return nil, []object{deliveryReceipt("hangup", id, reason)}
	}
	if d == nil || d.who.DeviceID != id || !h.active(d) {
		return skip("not_connected")
	}
	if d.callState == callNone {
		return skip("no_call")
	}
	if expected != nil && uint64(d.callID) != *expected {
		return skip("call_changed")
	}
	callID, p := d.callID, h.callPeer(d)
	h.endCall(d, "hung up")
	receipts := []object{}
	for _, side := range []*wsDevice{d, p} {
		if side != nil {
			reason := ""
			if !h.active(side) {
				reason = "not_connected"
			}
			receipts = append(receipts, deliveryReceipt("hangup", side.who.DeviceID, reason))
		}
	}
	return object{"id": callID, "state": "ended"}, receipts
}

func (h *wsHub) ringUntilAnswered(ctx context.Context, caller *wsDevice, id uint32) {
	tick := time.NewTicker(callRingEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				h.mu.Lock()
				if caller.callID == id && caller.callState == callRingingOut {
					h.endCall(caller, "no answer")
				}
				h.mu.Unlock()
			}
			return
		case <-tick.C:
			h.mu.Lock()
			peer := h.callPeer(caller)
			if ctx.Err() != nil || caller.callID != id || caller.callState != callRingingOut || peer == nil {
				h.mu.Unlock()
				return
			}
			now := time.Now()
			if h.parentRefresh(caller.key, now) != "" || h.parentRefresh(peer.key, now) != "" {
				h.mu.Unlock()
				return
			}
			h.chime(caller, "ring")
			h.chime(peer, "ring")
			h.mu.Unlock()
		}
	}
}

func (h *wsHub) answerCall(callee *wsDevice) {
	if callee.callState != callRingingIn {
		return
	}
	caller := h.callPeer(callee)
	if caller == nil || !h.active(caller) {
		h.endCall(callee, "disconnected")
		return
	}
	now := time.Now()
	if h.parentRefresh(callee.key, now) != "" || h.parentRefresh(caller.key, now) != "" {
		return
	}
	caller.callCancel()
	caller.callCancel = nil
	logCall("answer", caller, callee, "", 0)
	for _, d := range []*wsDevice{caller, callee} {
		h.interruptForCall(d, now)
		d.callState, d.callStarted = callActive, now
		msg := object{"op": "call", "state": "start", "call": d.callID, "muted": false}
		if d.caps.CallPCM {
			msg["mic_prefix"] = callMicPrefix
		}
		h.push(d, msg, false)
		h.render(d, now, false)
	}
}

func (h *wsHub) declineCall(d *wsDevice) {
	if d.callState == callRingingIn {
		h.endCall(d, "declined")
	}
}

func (h *wsHub) endCall(d *wsDevice, reason string) {
	if d.callState == callNone {
		return
	}
	p := h.callPeer(d)
	now := time.Now()
	var elapsed time.Duration
	if d.callState == callActive {
		elapsed = now.Sub(d.callStarted)
	}
	logCall("end", d, p, reason, elapsed)
	if p != nil && d.callState == callActive {
		duration := callDuration(elapsed)
		dID, pID, dName, pName := d.who.DeviceID, p.who.DeviceID, d.companion.Name, p.companion.Name
		go func() {
			h.b.chat(dID, newChat("card", pName, "Voice call with "+pName+" ("+duration+")", "call"), true)
			h.b.chat(pID, newChat("card", dName, "Voice call with "+dName+" ("+duration+")", "call"), true)
		}()
	}
	for _, side := range []*wsDevice{d, p} {
		if side == nil {
			continue
		}
		if side.callCancel != nil {
			side.callCancel()
		}
		id, answered, outgoing := side.callID, side.callState == callActive, side.callOrigin
		side.callState, side.callID, side.callPeer = callNone, 0, ""
		side.callMuted, side.callStarted, side.callSecond, side.callCancel = false, time.Time{}, 0, nil
		side.callOrigin, side.callFramesIn, side.callFramesOut = false, 0, 0
		if h.active(side) {
			h.push(side, object{"op": "call", "state": "end", "call": id, "reason": reason}, false)
			if answered && side.parentReason == "" {
				h.chime(side, "cancel")
			}
			h.wake(side, now)
			if outgoing && p != nil && reason == "declined" {
				h.callNotice(side, d.companion.Name+" declined", now)
				continue
			}
			if outgoing && p != nil && reason == "no answer" {
				h.callNotice(side, "No answer from "+p.companion.Name, now)
				continue
			}
			if side.parentReason == "" && reason == "Companion is resting" {
				h.callNotice(side, "The other companion is resting. Try again later.", now)
				continue
			}
			h.render(side, now, false)
		}
	}
}

func logCall(event string, d, p *wsDevice, reason string, duration time.Duration) {
	caller, callee := d.key, d.callPeer
	in, out := d.callFramesIn, d.callFramesOut
	if p != nil {
		in, out = in+p.callFramesIn, out+p.callFramesOut
	}
	if !d.callOrigin {
		caller, callee = callee, caller
	}
	log.Printf("ws call %s id=%d caller=%s callee=%s reason=%q duration=%s frames_in=%d frames_out=%d",
		event, d.callID, caller, callee, reason, duration.Round(time.Millisecond), in, out)
}

func (h *wsHub) toggleCallMute(d *wsDevice) {
	if d.callState == callActive {
		d.callMuted = !d.callMuted
		h.push(d, object{"op": "call", "state": "mute", "call": d.callID, "muted": d.callMuted}, false)
		h.render(d, time.Now(), false)
	}
}

func (h *wsHub) relayCallPCM(d *wsDevice, packet []byte) {
	if len(packet) != callPCMBytes+1 {
		return
	}
	d.callFramesIn++
	if d.callMuted {
		return
	}
	p := h.callPeer(d)
	if p == nil || !h.active(p) || p.callState != callActive || h.parentRefresh(p.key, time.Now()) != "" {
		return
	}
	c := p.client
	c.outMu.Lock()
	defer c.outMu.Unlock()
	if c.ctx.Err() == nil && len(c.out) < min(6, cap(c.out)) {
		// The read loop transfers ownership of the unmasked packet to the queue.
		packet[0] = pcmPrefix
		c.out <- wsFrame{wsBinary, packet}
		d.callFramesOut++
	}
}

func callDuration(elapsed time.Duration) string {
	seconds := max(0, int(elapsed/time.Second))
	return fmt.Sprintf("%d:%02d", seconds/60, seconds%60)
}
