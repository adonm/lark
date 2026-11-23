package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestHardwareVolumeMenu(t *testing.T) {
	t.Parallel()
	b, base := wsHarness(t, nil)
	const id = "vocat-33d8"
	board := testConnectWS(t, base, "unit", false)
	board.helloHardware(t, id, "", capBattery|capDrag)
	level := func(want byte, persist bool) {
		t.Helper()
		f := board.until(t, func(f protocolFrame) bool {
			p := framePayload(f)
			return hasProto(msgVolume, "")(f) && len(p) >= 3 && p[0] == want
		})
		p := framePayload(f)
		if (p[1] != 0) != persist || p[2] != 0 {
			t.Fatalf("unexpected volume frame: level=%d persist=%d auto=%d", p[0], p[1], p[2])
		}
	}
	tap := func(x, y int) { board.tap(t, x, y, 0) }
	board.tapMenu(t)
	svg := waitSVG(t, b.hub, id, "menu-next")
	nx, ny, _ := hitCenter(svg, "menu-next")
	tap(nx, ny)
	svg = waitSVG(t, b.hub, id, "menu-more")
	mx2, my2, _ := hitCenter(svg, "menu-more")
	tap(mx2, my2)
	svg = waitSVG(t, b.hub, id, "menu-volume")
	vx, vy, _ := hitCenter(svg, "menu-volume")
	tap(vx, vy)
	drawer := waitSVG(t, b.hub, id, "vol-down")
	if strings.Contains(drawer, "vol-mode") {
		t.Fatal("auto mode is still in the volume menu")
	}
	for _, control := range []string{"vol-down", "vol-up", "menu-back"} {
		if !strings.Contains(drawer, control) {
			t.Fatal("missing volume submenu control", control)
		}
	}
	dx, dy, _ := hitCenter(drawer, "vol-down")
	tap(dx, dy)
	level(45, true)
	ux, uy, _ := hitCenter(waitSVG(t, b.hub, id, "vol-up"), "vol-up")
	tap(ux, uy)
	level(55, true)
	var st [16]byte
	board.sendProto(t, msgTelemetry, st[:])
	bx, by, _ := hitCenter(waitSVG(t, b.hub, id, "vol-up"), "menu-back")
	tap(bx, by)
	waitSVG(t, b.hub, id, "menu-volume")
	entry := apiObjects(t, base+"/api/terminals", "terminals")[0].(object)
	if vol := b.volumePref(id); vol != 55 || entry["volume"] != json.Number("55") {
		t.Fatalf("volume not persisted: %d, device %v", vol, entry)
	}
	if _, ok := entry["auto_volume"]; ok {
		t.Fatalf("auto_volume still reported: %v", entry)
	}
}

func TestVolumePersistence(t *testing.T) {
	t.Parallel()
	_, base := wsHarness(t, nil)
	v := testObject(t, testRequest(t, http.MethodPost, base+"/api/companions", "unit", object{}, 201))
	id := str(v["id"])
	const termID = "vocat-0001"
	ws := testConnectWS(t, base, "unit", false)
	ws.hello(t, termID, id)
	manual := patchControl(t, base, id, object{"volume": 70})
	if manual["volume"] != json.Number("70") {
		t.Fatal(manual)
	}
	if _, ok := manual["auto_volume"]; ok {
		t.Fatalf("auto_volume still returned: %v", manual)
	}
	held := patchControl(t, base, id, object{"sleep": true})
	if held["volume"] != json.Number("70") || held["sleeping"] != true {
		t.Fatal(held)
	}
}
