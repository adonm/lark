package main

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"
)

func ambientSample(blocks ...int16) []byte {
	const block = sampleRate / 100 * 2
	pcm := make([]byte, 20*block)
	for i := 0; i < 20; i++ {
		level := blocks[min(i, len(blocks)-1)]
		for j := i * block; j < (i+1)*block; j += 2 {
			le.PutUint16(pcm[j:], uint16(level))
		}
	}
	return pcm
}

func TestAmbientRMSAndVolume(t *testing.T) {
	if _, ok := ambientRMS(make([]byte, 6399)); ok {
		t.Fatal("accepted less than 200 ms")
	}
	pcm := append(ambientSample(-10, 20, -30, 40, 20000), ambientSample(0)...)
	if rms, ok := ambientRMS(pcm); !ok || rms != math.Sqrt(750) {
		t.Fatalf("expected RMS of four quietest blocks in first 200 ms, got %.3f (%t)", rms, ok)
	}
	for _, tc := range []struct {
		rms int16
		vol int
	}{{0, 40}, {80, 40}, {300, 58}, {800, 78}, {1300, 89}, {1800, 100}, {9000, 100}} {
		rms, ok := ambientRMS(ambientSample(tc.rms))
		if !ok || rms != float64(tc.rms) || ambientVolume(rms) != tc.vol {
			t.Errorf("ambient %d gave rms=%g volume=%d, want %d", tc.rms, rms, ambientVolume(rms), tc.vol)
		}
	}
	d := wsTerminal{autoVol: true}
	d.measureAmbient(ambientSample(80))
	d.measureAmbient(ambientSample(1800))
	if !d.hasAmbient || d.ambientEMA != 854 {
		t.Fatalf("expected alpha=.45 from 80 to 1800, got %g", d.ambientEMA)
	}
}

func TestAmbientVolumePrecedesPlaybackWithoutPersisting(t *testing.T) {
	b, base := wsHarness(t, nil)
	const id = "vocat-beef"
	web := testConnectWS(t, base, "unit", false)
	web.hello(t, id, "")
	volume := func(want byte) func(wsFrame) bool {
		return func(f wsFrame) bool {
			p := framePayload(f)
			return hasProto(msgVolume, "")(f) && len(p) >= 3 && p[0] == want
		}
	}
	b.ws.mu.Lock()
	d := b.ws.terminals[id]
	d.measureAmbient(ambientSample(1800))
	s := newAudioStream()
	s.append(make([]byte, 1600))
	s.finish(nil)
	b.ws.play(d, s)
	b.ws.mu.Unlock()
	f := web.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return volume(100)(f) || (hasProto(msgTTSCtrl, "")(f) && len(p) >= 1 && p[0] == ttsOpStart)
	})
	p := framePayload(f)
	if !volume(100)(f) || p[1] != 0 || p[2] != 1 {
		t.Fatalf("expected the 100%% non-persisted volume before TTS start: %x", p)
	}
	web.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgTTSCtrl, "")(f) && len(p) >= 1 && p[0] == ttsOpStart
	})
	if auto, level := b.volumePrefs(id); !auto || level != 55 {
		t.Fatalf("ambient level leaked into device preferences: %t %d", auto, level)
	}
}

func TestHardwareVolumeMenuAndLegacyHeadTouch(t *testing.T) {
	b, base := wsHarness(t, nil)
	const id = "vocat-33d8"
	board := testConnectWS(t, base, "unit", false)
	board.helloHardware(t, id, "", capBattery|capDrag)
	level := func(want byte, auto, persist bool) {
		t.Helper()
		f := board.until(t, func(f wsFrame) bool {
			p := framePayload(f)
			return hasProto(msgVolume, "")(f) && len(p) >= 3 && p[0] == want
		})
		p := framePayload(f)
		if (p[2] != 0) != auto || (p[1] != 0) != persist {
			t.Fatalf("unexpected volume frame: level=%d persist=%d auto=%d", p[0], p[1], p[2])
		}
	}
	tap := func(x, y int) { board.tap(t, x, y, 0) }
	tap(180, 180)
	svg := waitSVG(t, b.ws, id, "menu-next")
	nx, ny, _ := hitCenter(svg, "menu-next")
	tap(nx, ny)
	svg = waitSVG(t, b.ws, id, "menu-voice")
	nx, ny, _ = hitCenter(svg, "menu-next")
	tap(nx, ny)
	svg = waitSVG(t, b.ws, id, "menu-volume")
	vx, vy, _ := hitCenter(svg, "menu-volume")
	tap(vx, vy)
	drawer := waitSVG(t, b.ws, id, "vol-mode")
	for _, control := range []string{"vol-down", "menu-next", "menu-back"} {
		if !strings.Contains(drawer, control) {
			t.Fatal("missing volume submenu control", control)
		}
	}
	dx, dy, _ := hitCenter(drawer, "vol-down")
	tap(dx, dy)
	level(45, false, true)
	nx, ny, _ = hitCenter(drawer, "menu-next")
	tap(nx, ny)
	p1 := waitSVG(t, b.ws, id, "vol-up")
	ux, uy, _ := hitCenter(p1, "vol-up")
	tap(ux, uy)
	level(55, false, true)
	var st [16]byte
	board.sendProto(t, msgTelemetry, st[:])
	bx, by, _ := hitCenter(p1, "menu-back")
	tap(bx, by)
	waitSVG(t, b.ws, id, "menu-volume")
	entry := apiObjects(t, base+"/api/terminals", "terminals")[0].(object)
	if auto, vol := b.volumePrefs(id); auto || vol != 55 || entry["auto_volume"] != false || entry["volume"] != json.Number("55") {
		t.Fatalf("unintended head-touch volume change: persisted %t %d, device %v", auto, vol, entry)
	}
	vx, vy, _ = hitCenter(waitSVG(t, b.ws, id, "menu-volume"), "menu-volume")
	tap(vx, vy)
	drawer = waitSVG(t, b.ws, id, "vol-mode")
	mx, my, _ := hitCenter(drawer, "vol-mode")
	tap(mx, my)
	level(55, true, false)
	tap(mx, my)
	level(55, false, true)
	tap(mx, my)
	level(55, true, false)
	if auto, _ := b.volumePrefs(id); !auto {
		t.Fatal("Auto was not persisted")
	}
}

func TestVolumePersistence(t *testing.T) {
	_, base := wsHarness(t, nil)
	v := testObject(t, testRequest(t, http.MethodPost, base+"/api/companions", "unit", object{}, 201))
	id := str(v["id"])
	const termID = "vocat-0001"
	ws := testConnectWS(t, base, "unit", false)
	ws.hello(t, termID, id)
	manual := patchControl(t, base, id, object{"volume": 70})
	if manual["auto_volume"] != false || manual["volume"] != json.Number("70") {
		t.Fatal(manual)
	}
	auto := patchControl(t, base, id, object{"auto_volume": true})
	if auto["auto_volume"] != true || auto["volume"] != json.Number("55") {
		t.Fatal(auto)
	}
}
