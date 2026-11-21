package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
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
	d := wsDevice{autoVol: true}
	d.measureAmbient(ambientSample(80))
	d.measureAmbient(ambientSample(1800))
	if !d.hasAmbient || d.ambientEMA != 854 {
		t.Fatalf("expected alpha=.45 from 80 to 1800, got %g", d.ambientEMA)
	}
}

func TestAmbientVolumePrecedesPlaybackWithoutPersisting(t *testing.T) {
	b, base := wsHarness(t, nil)
	const id = "phone-beef"
	web := testConnectWS(t, base, "unit", false)
	web.hello(t, id, "Test")
	volume := func(level string) func(wsFrame) bool { return hasWS("volume", `"level":`+level) }
	f := testObject(t, web.until(t, volume("55")).data)
	if f["persist"] != false || f["auto_volume"] != true {
		t.Fatal(f)
	}
	b.ws.mu.Lock()
	d := b.ws.devices[id]
	d.measureAmbient(ambientSample(1800))
	s := newAudioStream()
	s.append(make([]byte, 1600))
	s.finish(nil)
	b.ws.play(d, s)
	b.ws.mu.Unlock()
	f = testObject(t, web.until(t, func(f wsFrame) bool {
		return volume("100")(f) || hasWS("tts", `"state":"start"`)(f)
	}).data)
	if f["op"] != "volume" || f["persist"] != false || f["auto_volume"] != true {
		t.Fatal("expected the 100% non-persisted volume before TTS start:", f)
	}
	web.until(t, hasWS("tts", `"state":"start"`))
	if auto, level := b.volumePrefs(id); !auto || level != 55 {
		t.Fatalf("ambient level leaked into device preferences: %t %d", auto, level)
	}
}

func TestHardwareVolumeMenuAndLegacyHeadTouch(t *testing.T) {
	b, base := wsHarness(t, nil)
	const id = "lark-33d8"
	board := testConnectWS(t, base, "unit", false)
	board.json(t, object{"ev": "hello", "role": "device", "device_id": id, "character": "Cake", "proto": 2, "volume": 95})
	level := func(want string, auto, persist bool) {
		t.Helper()
		frame := testObject(t, board.until(t, hasWS("volume", `"level":`+want)).data)
		if frame["auto_volume"] != auto || frame["persist"] != persist {
			t.Fatalf("unexpected volume frame: %v", frame)
		}
	}
	level("55", true, false)
	web := testConnectWS(t, base, "unit", true)
	web.json(t, object{"ev": "hello", "role": "web"})
	web.until(t, hasWS("peers", id))
	web.json(t, object{"ev": "watch", "device_id": id})
	web.until(t, hasWS("svg", "cat-ring"))
	tap := func(x, y int) { board.json(t, object{"ev": "tap", "x": x, "y": y}) }
	tap(180, 300)
	web.until(t, hasWS("svg", "menu-next"))
	tap(248, 294)
	web.until(t, hasWS("svg", "Auto · 55%"))
	tap(245, 130)
	drawer := string(web.until(t, hasWS("svg", "vol-auto")).data)
	for _, control := range []string{"vol-minus", "vol-plus", "menu-back"} {
		if !strings.Contains(drawer, control) {
			t.Fatal("missing volume submenu control", control)
		}
	}
	tap(120, 200)
	level("45", false, true)
	tap(240, 200)
	level("55", false, true)
	for _, action := range []string{"vol_up", "vol_down"} {
		board.json(t, object{"ev": "head", "action": action, "volume": 95})
	}
	board.json(t, object{"ev": "status", "volume": 95})
	tap(180, 315)
	web.until(t, hasWS("svg", "menu-volume"))
	entry := apiObjects(t, base+"/api/devices", "devices")[0].(object)
	if auto, vol := b.volumePrefs(id); auto || vol != 55 || entry["auto_volume"] != false || entry["volume"] != json.Number("55") {
		t.Fatalf("unintended head-touch volume change: persisted %t %d, device %v", auto, vol, entry)
	}
	tap(245, 130)
	web.until(t, hasWS("svg", "vol-auto"))
	tap(180, 262)
	level("55", true, false)
	tap(180, 262)
	level("55", false, true)
	tap(180, 262)
	level("55", true, false)
	if auto, _ := b.volumePrefs(id); !auto {
		t.Fatal("Auto was not persisted")
	}
}

func TestVolumeAPIAndScopedPersistence(t *testing.T) {
	b, base := wsHarness(t, func(b *bridge, _ string) { b.virtual = virtualStore(b.db) })
	v := testObject(t, testRequest(t, http.MethodPost, base+"/api/virtual-clients", "unit", object{}, 201))
	id := str(v["id"])
	client, _ := b.virtualByID(id)
	scoped := httptest.NewServer(b.scoped(client, "private"))
	defer scoped.Close()
	path := "/api/devices/" + id + "/volume"
	testRequest(t, http.MethodPost, scoped.URL+"/api/devices/lark-other/volume", "private", object{"auto": true}, 403)
	testRequest(t, http.MethodPost, base+path, "unit", object{"auto": false, "volume": 101}, 400)
	testRequest(t, http.MethodPost, scoped.URL+path, "private", object{"auto": false, "volume": 65}, 200)
	f := testObject(t, testRequest(t, http.MethodGet, scoped.URL+"/api/scope", "private", nil, 200))
	if f["auto_volume"] != false || f["volume"] != json.Number("65") {
		t.Fatal("unexpected scoped preferences", f)
	}
	ws := testConnectWS(t, base, "unit", false)
	ws.hello(t, id, "Cat")
	ws.until(t, hasWS("volume", `"level":65`))
	manual := patchControl(t, base, id, object{"volume": 70})
	if manual["auto_volume"] != false || manual["volume"] != json.Number("70") {
		t.Fatal(manual)
	}
	auto := patchControl(t, base, id, object{"auto_volume": true})
	if auto["auto_volume"] != true || auto["volume"] != json.Number("55") {
		t.Fatal(auto)
	}
}
