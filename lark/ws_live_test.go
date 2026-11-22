//go:build live

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"image"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestLiveWebSocket(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	conf := liveConfig(t, home)
	port := conf["LARK_PORT"]
	if port == "" {
		port = "8765"
	}
	base := env("LARK_TEST_URL", "http://"+net.JoinHostPort("127.0.0.1", port))
	token := conf["LARK_TOKEN"]
	client := pooledClient()
	client.Timeout = 115 * time.Second
	defer client.CloseIdleConnections()
	c := liveClient{client, base, token}
	html, res := c.read(t, "/app", nil, "", nil, 200)
	if !bytes.Contains(html, []byte(`<svg id="stage" viewBox="0 0 360 360"`)) ||
		!bytes.Contains(html, []byte("Join on Phone")) ||
		!bytes.Contains(html, []byte("+ New Companion")) ||
		!bytes.Contains(html, []byte("Reassign / Load Form")) ||
		!bytes.Contains(html, []byte("New Chat")) ||
		res.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatal("missing live app HTML or inline SVG")
	}
	for _, asset := range []string{"/app/style.css", "/app/client.js", "/app/mic.js"} {
		c.read(t, asset, nil, "", nil, 200)
	}
	c.read(t, "/api/diagnostics", nil, "", nil, 200)
	token = "lark-live-ws-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	stub, _ := liveModelStub(t, ocModel{Provider: "test", ID: "deterministic"})
	conf["LARK_OPENCODE_URL"], conf["LARK_OPENCODE_PASSWORD"] = stub, ""
	iso := liveIsolated(t, env("XDG_DATA_HOME", filepath.Join(home, ".local/share")), conf, token, client)
	base = iso.base

	var suffix [2]byte
	_, _ = rand.Read(suffix[:])
	phoneID := "phone-" + hex.EncodeToString(suffix[:])
	deviceID := "lark-ws-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	from := testConnectWS(t, base, token, false)
	first, _, _ := decodeDisplay(t, from.helloV2(t, deviceID, "Cake"))
	if first != image.Rect(0, 0, 360, 360) {
		t.Fatalf("live board did not receive full initial frame: %v", first)
	}
	viewer := testConnectWS(t, base, token, true)
	mirror := viewer.watch(t, deviceID)
	if rev, _ := decodeFrameSVG(mirror); rev == 0 {
		t.Fatal("missing initial display revisions")
	}
	var st [16]byte
	batt := int16(48)
	binary.LittleEndian.PutUint16(st[2:4], uint16(batt))
	from.sendProto(t, msgTelemetry, st[:])
	battery, _, _ := decodeDisplay(t, from.until(t, isDisplay))
	viewer.until(t, hasSVG(menuSuccess))
	if battery.Dx() > 50 || battery.Dy() > 25 {
		t.Fatalf("battery update did not use a dirty rectangle: %v", battery)
	}
	from.tap(t, 180, 300, 0)
	drawer := viewer.until(t, hasSVG("menu-games"))
	if bytes.Contains(drawer.data, []byte(`data-id="new"`)) {
		t.Fatal("hardware drawer offered reset")
	}
	from.tap(t, 180, 295, 0)
	viewer.until(t, hasSVG("companion-ring"))
	const code = `let text="Ready";
function render(){return '<svg viewBox="0 0 360 360"><rect data-id="flip" x="140" y="95" width="80" height="50" fill="gold"/><text x="180" y="195" fill="white" text-anchor="middle">'+text+'</text></svg>';}
function onTap(s,p,id){if(id==="flip")text="Live tap";}
function onTick(){if(text==="Live tap")text="Live reset";}`
	result := liveJSON[struct{ Result struct{ IsError bool } }](t, iso, "/mcp", object{"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": object{"name": "app", "arguments": object{"devices": deviceID, "title": "Live flip", "code": code, "tick_ms": 500, "max_ticks": 12}}}, nil, "", nil)
	if result.Result.IsError {
		t.Fatal("live MCP run_app rejected local interactivity")
	}
	viewer.until(t, hasSVG("Ready"))
	from.tap(t, 180, 120, 0)
	viewer.until(t, hasSVG("Live tap"))
	viewer.until(t, hasSVG("Live reset"))
	from.tap(t, 180, 316, 0)
	viewer.until(t, hasSVG("companion-ring"))
	t.Log("live cat drawer, raw tap, JS tick, dirty rectangle, SVG mirror, and exit verified")

	phone := testConnectWS(t, base, token, true)
	face := phone.helloWeb(t, phoneID, "", capDrag, false)
	if !hasSVG("#FFF2E7")(face) || !hasSVG("Say hi to meet me!")(face) {
		t.Fatal("live Phone Cat did not start blank")
	}
	viewer.until(t, hasProto(msgPeers, phoneID))
	pcm := bytes.Repeat([]byte{30, 0}, 320)
	from.recStart(t, phoneID)
	from.sendRTP(t, rtpPTRecPCM, 1, 320, 1, pcm)
	start := time.Now()
	from.recEnd(t, "", false)
	phone.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgChime, "")(f) && len(p) >= 1 && p[0] == chimeIntercom
	})
	phone.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgTTSCtrl, "")(f) && len(p) >= 1 && p[0] == ttsOpStart
	})
	phone.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgStatus, "")(f) && len(p) >= 5 && p[4] == statusSpeaking
	})
	var spoken []byte
	var audio uint64
	var elapsed time.Duration
	for range 128 {
		f := phone.read(t)
		mt, _, p, ok := unpackProtoPacket(f.data)
		if !ok {
			continue
		}
		if mt == msgTTSPCM {
			spoken = append(spoken, p...)
			elapsed = time.Since(start)
		} else if mt == msgTTSCtrl && len(p) >= 9 && p[0] == ttsOpEnd {
			audio = binary.LittleEndian.Uint64(p[1:9])
			break
		}
	}
	if !bytes.HasSuffix(spoken, pcm) {
		t.Fatal("live intercom changed capture PCM")
	}
	if audio == 0 {
		t.Fatal("missing live playback ID")
	}
	phone.sendProto(t, msgTTSAck, encodeTTSAck(false, audio))
	phone.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgStatus, "")(f) && len(p) >= 5 && (p[4] == statusDone || p[4] == statusIdle)
	})
	t.Logf("live raw intercom rec_end to PCM: %.2f ms; HTML/SVG and framebuffer mirror verified",
		float64(elapsed)/float64(time.Millisecond))
	for _, s := range []*testWS{from, phone, viewer} {
		s.write(t, maskedFrame(wsClose, true, wsCloseFrame(1000).data))
		s.until(t, func(f wsFrame) bool { return f.op == wsClose })
	}
	if len(spoken) == 0 {
		t.Fatal("no live intercom payload")
	}
	devices := liveJSON[struct {
		Devices []struct {
			ID        string `json:"device_id"`
			IP        string `json:"ip"`
			Connected bool   `json:"ws_connected"`
		}
	}](t, iso, "/api/devices", nil, nil, "", nil).Devices
	if len(devices) != 1 || devices[0].ID != deviceID || devices[0].IP != "127.0.0.1" || devices[0].Connected {
		t.Fatal("isolated registry did not remember only the disconnected physical board")
	}
}
