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

	d.tap(t, 180, 150, rev-1)
	d.barrier(t)
	b.ws.mu.Lock()
	started := b.ws.terminals[id].recording.active()
	rev = b.ws.terminals[id].nativeRev // An idle blink may have produced another revision.
	b.ws.mu.Unlock()
	if started {
		t.Fatal("stale revision started recording")
	}
	d.tap(t, 180, 150, rev)
	d.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpStart
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
		return image.Pt(180, 265).In(r)
	})

	d.tap(t, 180, 265, idleRev)
	d.barrier(t)
	b.ws.mu.Lock()
	started, thinkingRev := dev.recording.endpoint != nil, dev.nativeRev
	b.ws.mu.Unlock()
	if started {
		t.Fatal("a stale Talk tap started a Say more capture")
	}
	d.tap(t, 180, 265, thinkingRev)
	start := d.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 2 && p[0] == recOpStart
	})
	p := framePayload(start)
	if p[1]&recTargetBridgeE == 0 || p[4] != 0 {
		t.Fatalf("Say more did not start an untargeted bridge capture: %x", p)
	}
}

func TestNativeWakeWordUsesTalkFlowOnlyWhenEnabled(t *testing.T) {
	b, base := wsHarness(t, nil)
	d := testConnectWS(t, base, "unit", false)
	const id = "vocat-0003"
	d.helloHardware(t, id, "", capWakeWord|capBridgeEndpoint|capDrag)
	d.sendProto(t, msgWakeWord, nil)
	d.barrier(t)
	b.ws.mu.Lock()
	dev := b.ws.terminals[id]
	started := dev.recording.active()
	endpoint := dev.caps.BridgeEndpoint
	b.ws.mu.Unlock()
	if started || !endpoint {
		t.Fatal("wake was not default-off or bridge endpoint capability was lost")
	}

	b.ws.mu.Lock()
	dev.wakeWord = true
	b.ws.pushWakeWord(dev, true)
	b.ws.mu.Unlock()
	d.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgWakeWordCtrl, "")(f) && len(p) >= 1 && p[0] == 1
	})
	d.sendProto(t, msgWakeWord, nil)
	d.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpStart
	})
	d.recStart(t, "")
	d.barrier(t)
	b.ws.mu.Lock()
	open := dev.recording.open()
	b.ws.mu.Unlock()
	if !open {
		t.Fatal("recording did not wait for the normal rec_start acknowledgement")
	}
}
