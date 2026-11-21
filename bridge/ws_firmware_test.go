package main

import (
	"image"
	"testing"
	"time"
)

func TestNativeTapRequiresCurrentFrameRevision(t *testing.T) {
	b, base := wsHarness(t, nil)
	d := testConnectWS(t, base, "unit", false)
	const id = "lark-rev"
	d.json(t, object{"ev": "hello", "role": "device", "proto": 2, "device_id": id, "character": "Cake",
		"caps": object{"pointer_rev": true}})
	_, rev, _ := decodeDisplay(t, d.until(t, isDisplay))

	tap := object{"ev": "tap", "x": 180, "y": 150}
	d.json(t, tap) // A client that advertised pointer_rev may not omit it.
	tap["rev_lo"] = rev - 1
	d.json(t, tap)
	d.barrier(t)
	b.ws.mu.Lock()
	started := b.ws.devices[id].recording.active()
	rev = b.ws.devices[id].nativeRev // An idle blink may have produced another revision.
	b.ws.mu.Unlock()
	if started {
		t.Fatal("missing or stale revision started recording")
	}
	tap["rev_lo"] = rev
	d.json(t, tap)
	d.until(t, hasWS("rec", `"action":"start"`))
}

func TestDestructiveNativeRevisionWrapAndBrowser(t *testing.T) {
	d := wsDevice{caps: deviceCaps{PointerRev: true}}
	for _, action := range []string{"stop", "rec-cancel", "rec-send", "listen", "reply", "call-peer",
		"call-answer", "call-decline", "call-hangup", "call-mute"} {
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
	b, base := wsHarness(t, func(b *bridge, _ string) { b.endpoint = testEndpointSettings() })
	d := testConnectWS(t, base, "unit", false)
	const id = "lark-say-more"
	d.json(t, object{"ev": "hello", "role": "device", "proto": 2, "device_id": id, "character": "Cake",
		"caps": object{"pointer_rev": true, "bridge_endpoint": true}})
	d.until(t, hasWS("hello_ok", ""))
	_, idleRev, _ := decodeDisplay(t, d.until(t, isDisplay))

	b.ws.mu.Lock()
	cat := b.ws.devices[id]
	cat.busy = true
	b.ws.render(cat, time.Now(), false)
	b.ws.mu.Unlock()
	d.until(t, func(f wsFrame) bool {
		if !isDisplay(f) {
			return false
		}
		r, _, _ := decodeDisplay(t, f)
		return image.Pt(180, 265).In(r)
	})

	tap := object{"ev": "tap", "x": 180, "y": 265, "rev_lo": idleRev}
	d.json(t, tap)
	d.barrier(t)
	b.ws.mu.Lock()
	started, thinkingRev := cat.recording.endpoint != nil, cat.nativeRev
	b.ws.mu.Unlock()
	if started {
		t.Fatal("a stale Talk tap started a Say more capture")
	}
	tap["rev_lo"] = thinkingRev
	d.json(t, tap)
	start := testObject(t, d.until(t, hasWS("rec", `"action":"start"`)).data)
	if start["endpoint"] != "bridge" || start["target"] != "" {
		t.Fatal("Say more did not start an untargeted bridge capture")
	}
}

func TestNativeWakeWordUsesTalkFlowOnlyWhenEnabled(t *testing.T) {
	b, base := wsHarness(t, nil)
	d := testConnectWS(t, base, "unit", false)
	const id = "lark-wake"
	d.json(t, object{"ev": "hello", "role": "device", "proto": 2, "device_id": id, "character": "Cake",
		"caps": object{"wake_word": true, "bridge_endpoint": true}})
	d.until(t, hasWS("hello_ok", ""))
	d.until(t, isDisplay)
	d.json(t, object{"ev": "wake_word"})
	d.barrier(t)
	b.ws.mu.Lock()
	dev := b.ws.devices[id]
	started := dev.recording.active()
	endpoint := dev.caps.BridgeEndpoint
	b.ws.mu.Unlock()
	if started || !endpoint {
		t.Fatal("wake was not default-off or bridge endpoint capability was lost")
	}

	operator := testConnectWS(t, base, "unit", true)
	operator.json(t, object{"ev": "hello", "role": "web"})
	operator.json(t, object{"ev": "wake_word", "device_id": id, "enabled": true})
	operator.until(t, hasWS("wake_word", `"enabled":true`))
	d.until(t, hasWS("wake_word", `"enabled":true`))
	d.json(t, object{"ev": "wake_word"})
	d.until(t, hasWS("rec", `"action":"start"`))
	d.json(t, object{"ev": "rec_start"})
	d.barrier(t)
	b.ws.mu.Lock()
	open := dev.recording.open()
	b.ws.mu.Unlock()
	if !open {
		t.Fatal("recording did not wait for the normal rec_start acknowledgement")
	}
}
