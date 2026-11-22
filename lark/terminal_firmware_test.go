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
	waitSVG(t, b.ws, id, "rec-send")
	b.ws.mu.Lock()
	rev = b.ws.terminals[id].nativeRev
	b.ws.mu.Unlock()

	d.tap(t, 247, 290, rev-1)
	d.barrier(t)
	b.ws.mu.Lock()
	stillActive := b.ws.terminals[id].recording.active()
	rev = b.ws.terminals[id].nativeRev
	b.ws.mu.Unlock()
	if !stillActive {
		t.Fatal("stale revision stopped recording")
	}
	d.tap(t, 247, 290, rev)
	d.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpStop
	})
}

func TestDestructiveNativeRevisionWrapAndBrowser(t *testing.T) {
	d := wsTerminal{caps: terminalCaps{PointerRev: true}}
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

	b.ws.mu.Lock()
	dev := b.ws.terminals[id]
	dev.busy = true
	b.ws.render(dev, time.Now(), false)
	b.ws.mu.Unlock()
	d.until(t, func(f wsFrame) bool {
		if !isDisplay(f) {
			return false
		}
		r, _, _ := decodeDisplay(t, f)
		return image.Pt(180, 290).In(r)
	})

	d.tap(t, 180, 290, idleRev)
	d.barrier(t)
	b.ws.mu.Lock()
	taps, thinkingRev := dev.stopTaps, dev.nativeRev
	b.ws.mu.Unlock()
	if taps != 0 {
		t.Fatal("a stale Stop tap incremented stopTaps")
	}
	d.tap(t, 180, 290, thinkingRev)
	d.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgChime, "")(f) && len(p) >= 1 && p[0] == chimeTap
	})
}
