package main

import (
	"image"
	"testing"
	"time"

	"lark/internal/pixel"
)

func TestNativeTapRequiresCurrentFrameRevision(t *testing.T) {
	t.Parallel()
	b, base := wsHarness(t, nil)
	d := testConnectWS(t, base, "unit", false)
	const id = "vocat-0001"
	_, rev, _ := decodeDisplay(t, d.helloHardware(t, id, "", capPointerRev|capDrag))

	d.recStart(t, "")
	waitSVG(t, b.hub, id, "rec-send")
	dev := b.hub.live(id)
	rev = dev.state().nativeRev

	d.tap(t, 247, 290, rev-1)
	d.barrier(t)
	st := dev.state()
	stillActive, rev := st.recording.active(), st.nativeRev
	if !stillActive {
		t.Fatal("stale revision stopped recording")
	}
	d.tap(t, 247, 290, rev)
	d.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpStop
	})
}

func TestDestructiveNativeRevisionWrapAndBrowser(t *testing.T) {
	t.Parallel()
	d := terminalSession{caps: terminalCaps{PointerRev: true}}
	for _, action := range []string{"stop", "rec-cancel", "rec-send", "listen", "reply"} {
		wrapped, old := uint16(0), uint16(65535)
		if !d.acceptPointerRevision(action, &wrapped) || d.acceptPointerRevision(action, &old) {
			t.Fatalf("%s accepts the wrong revision at wrap", action)
		}
	}
	d.caps.PointerRev = false
	if !d.acceptPointerRevision("stop", nil) {
		t.Fatal("browser virtual clients must continue to work without rev_lo")
	}
}

func TestNativeSayMoreRequiresCurrentFrameRevision(t *testing.T) {
	t.Parallel()
	b, base := wsHarness(t, func(b *larkServer, _ string) { b.endpoint = testEndpointSettings() })
	d := testConnectWS(t, base, "unit", false)
	const id = "vocat-0002"
	_, idleRev, _ := decodeDisplay(t, d.helloHardware(t, id, "", capPointerRev|capBridgeEndpoint|capDrag))

	dev := b.hub.live(id)
	updateTerm(t, dev, func(d *terminalSession) {
		d.busy = true
		b.hub.render(d, time.Now(), false)
	})
	d.until(t, func(f protocolFrame) bool {
		if !isDisplay(f) {
			return false
		}
		r, _, _ := decodeDisplay(t, f)
		return image.Pt(180, 290).In(r)
	})

	d.tap(t, 180, 290, idleRev)
	d.barrier(t)
	st := dev.state()
	taps, thinkingRev := st.stopTaps, st.nativeRev
	if taps != 0 {
		t.Fatal("a stale Stop tap incremented stopTaps")
	}
	d.tap(t, 180, 290, thinkingRev)
	d.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgChime, "")(f) && len(p) >= 1 && p[0] == chimeTap
	})
}

func TestAcceptPointerRevisionAllowsAnimationFrames(t *testing.T) {
	t.Parallel()
	d := terminalSession{
		caps:         terminalCaps{PointerRev: true},
		nativeRev:    13,
		actionRev:    10,
		hasActionRev: true,
	}
	inFlight := uint16(12)
	if !d.acceptPointerRevision("stop", &inFlight) {
		t.Fatal("expected in-flight animation frame revision to be accepted for Stop")
	}
	stale := uint16(9)
	if d.acceptPointerRevision("stop", &stale) {
		t.Fatal("expected pre-action revision to be rejected for Stop")
	}
}

// The reply box keeps its rectangle when the card retargets to a different
// peer. The revision must move with the action value, not just the geometry.
func TestPointerRevisionTracksActionValues(t *testing.T) {
	t.Parallel()
	hits := []pixel.HitBox{{ID: "close", X1: 0, Y1: 0, X2: 99, Y2: 49}, {ID: "reply", X1: 100, Y1: 0, X2: 199, Y2: 49}}
	first := pointerKey(hits, map[string]pixel.Action{
		"close": {Kind: "close-card", Value: ""}, "reply": {Kind: "reply", Value: "mint"},
	})
	second := pointerKey(hits, map[string]pixel.Action{
		"close": {Kind: "close-card", Value: ""}, "reply": {Kind: "reply", Value: "cake"},
	})
	if first == second {
		t.Fatal("same rectangles with a different reply target share a pointer revision")
	}
	d := terminalSession{caps: terminalCaps{PointerRev: true}, nativeRev: 10, actionRev: 10, hasActionRev: true, actionHitsKey: first}
	// A render at nativeRev 11 sees the new action map.
	d.nativeRev = 11
	if second != d.actionHitsKey {
		d.actionHitsKey, d.actionRev = second, d.nativeRev
	}
	stale := uint16(10)
	if d.acceptPointerRevision("reply", &stale) {
		t.Fatal("a tap from the old card revision ran the retargeted reply")
	}
	current := uint16(11)
	if !d.acceptPointerRevision("reply", &current) {
		t.Fatal("the current reply revision was rejected")
	}
}
