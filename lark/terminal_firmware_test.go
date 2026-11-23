package main

import (
	"image"
	"testing"
	"time"
)

func TestNativeTapRequiresCurrentFrameRevision(t *testing.T) {
	b, base := wsHarness(t, nil)
	d := testConnectWS(t, base, "unit", false)
	const id = "vocat-0001"
	_, rev, _ := decodeDisplay(t, d.helloHardware(t, id, "", capPointerRev|capDrag))

	d.recStart(t, "")
	waitSVG(t, b.hub, id, "rec-send")
	b.hub.mu.Lock()
	rev = b.hub.terminals[id].nativeRev
	b.hub.mu.Unlock()

	d.tap(t, 247, 290, rev-1)
	d.barrier(t)
	b.hub.mu.Lock()
	stillActive := b.hub.terminals[id].recording.active()
	rev = b.hub.terminals[id].nativeRev
	b.hub.mu.Unlock()
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
	b, base := wsHarness(t, func(b *larkServer, _ string) { b.endpoint = testEndpointSettings() })
	d := testConnectWS(t, base, "unit", false)
	const id = "vocat-0002"
	_, idleRev, _ := decodeDisplay(t, d.helloHardware(t, id, "", capPointerRev|capBridgeEndpoint|capDrag))

	b.hub.mu.Lock()
	dev := b.hub.terminals[id]
	dev.busy = true
	b.hub.render(dev, time.Now(), false)
	b.hub.mu.Unlock()
	d.until(t, func(f protocolFrame) bool {
		if !isDisplay(f) {
			return false
		}
		r, _, _ := decodeDisplay(t, f)
		return image.Pt(180, 290).In(r)
	})

	d.tap(t, 180, 290, idleRev)
	d.barrier(t)
	b.hub.mu.Lock()
	taps, thinkingRev := dev.stopTaps, dev.nativeRev
	b.hub.mu.Unlock()
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
