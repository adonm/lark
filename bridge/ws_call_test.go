package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const callCake = "phone-000a"
const callMint = "phone-000b"

func callClient(t *testing.T, b *bridge, base, id, name string) *testWS {
	return callClientCaps(t, b, base, id, name, true)
}

func callClientCaps(t *testing.T, b *bridge, base, id, name string, tagged bool) *testWS {
	t.Helper()
	tx, err := b.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	companion := blankCompanion()
	companion.Name, companion.Configured = name, true
	companion.FaceSVG = companion.defaultFace()
	if err = writeCompanion(t.Context(), tx, id, companion); err == nil {
		err = tx.Commit()
	}
	if err != nil {
		t.Fatal(err)
	}
	c := testConnectWS(t, base, "unit", false)
	c.json(t, object{"ev": "hello", "role": "web", "device_id": id, "character": name,
		"caps": object{"call": true, "call_pcm": tagged, "drag": true}})
	c.until(t, hasWS("svg", ""))
	return c
}

func tapCallControl(t *testing.T, c *testWS, id string) {
	t.Helper()
	c.json(t, object{"ev": "tap", "id": id, "x": 180, "y": 180})
}

func finishTestCall(t *testing.T, caller, peer *testWS) {
	t.Helper()
	caller.json(t, object{"ev": "head", "action": "pet"})
	caller.barrier(t)
	tapCallControl(t, caller, "call-hangup")
	for _, c := range []*testWS{caller, peer} {
		c.until(t, hasWS("call", `"reason":"hung up"`))
		c.until(t, func(f wsFrame) bool {
			return f.op == wsText && bytes.Contains(f.data, []byte("Voice call with "))
		})
	}
}

func checkCallStates(t *testing.T, h *wsHub, a, b callState) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, want := range map[string]callState{callCake: a, callMint: b} {
		d := h.devices[id]
		if d == nil {
			if want != callNone {
				t.Fatalf("%s disconnected; wanted %v", id, want)
			}
		} else if d.callState != want || want == callNone && (d.callID != 0 || d.callPeer != "" || d.callMuted || d.callCancel != nil) {
			t.Fatalf("%s has state=%v, id=%d, peer=%q; wanted %v", id, d.callState, d.callID, d.callPeer, want)
		}
	}
}

func TestWSCallDuplex(t *testing.T) {
	var stt atomic.Int32
	b, base := wsHarness(t, func(b *bridge, dir string) {
		b.stt = func(context.Context, []byte) (string, error) { stt.Add(1); return "", nil }
	})
	cake := callClient(t, b, base, callCake, "Cake")
	mint := callClient(t, b, base, callMint, "Mint")
	cake.json(t, object{"ev": "call", "target": callMint})
	cake.until(t, hasWS("chime", `"kind":"ring"`))
	cake.until(t, hasWS("svg", "Calling Mint"))
	mint.until(t, hasWS("chime", `"kind":"ring"`))
	mint.until(t, hasWS("svg", "call-answer"))
	checkCallStates(t, b.ws, callRingingOut, callRingingIn)

	tapCallControl(t, mint, "call-answer")
	for _, c := range []*testWS{cake, mint} {
		c.until(t, hasWS("call", `"state":"start"`))
		c.until(t, hasWS("svg", "call-mute"))
	}
	checkCallStates(t, b.ws, callActive, callActive)
	pcm := bytes.Repeat([]byte{100, 5}, callPCMBytes/2)
	sendPCM := func(c *testWS, sample []byte) {
		c.write(t, maskedFrame(wsBinary, true, append([]byte{callMicPrefix}, sample...)))
	}
	for _, pair := range [][2]*testWS{{cake, mint}, {mint, cake}} {
		sendPCM(pair[0], pcm)
		if got := pair[1].until(t, isPlayback); !bytes.Equal(got.data[1:], pcm) {
			t.Fatal("call changed the relayed PCM")
		}
	}

	tapCallControl(t, cake, "call-mute")
	cake.until(t, hasWS("call", `"muted":true`))
	cake.until(t, hasWS("svg", "Unmute"))
	sendPCM(cake, pcm)
	cake.barrier(t)
	// A mute blocks uplink only.
	sendPCM(mint, pcm)
	cake.until(t, isPlayback)
	tapCallControl(t, cake, "call-mute")
	cake.until(t, hasWS("call", `"muted":false`))
	probe := bytes.Repeat([]byte{5, 100}, callPCMBytes/2)
	sendPCM(cake, probe)
	if got := mint.until(t, isPlayback); !bytes.Equal(got.data[1:], probe) {
		t.Fatal("received PCM queued while caller was muted")
	}
	// A touch on the avatar must neither end the call nor start a recording.
	cake.json(t, object{"ev": "tap", "x": 180, "y": 113})
	cake.barrier(t)
	checkCallStates(t, b.ws, callActive, callActive)
	b.ws.mu.Lock()
	for _, d := range b.ws.devices {
		if d.recording.pcm != nil || d.recording.open() || d.busy {
			t.Error("call audio entered the assistant turn path")
		}
	}
	b.ws.mu.Unlock()

	tapCallControl(t, mint, "call-hangup")
	for i, c := range []*testWS{cake, mint} {
		c.until(t, hasWS("call", `"state":"end"`))
		c.until(t, hasWS("chime", `"kind":"cancel"`))
		peer := []string{"Mint", "Cake"}[i]
		var idle, noted bool
		c.until(t, func(f wsFrame) bool {
			idle = idle || hasWS("svg", "companion-ring")(f)
			noted = noted || f.op == wsText && bytes.Contains(f.data, []byte("Voice call with "+peer+" (0:"))
			return idle && noted
		})
		id := []string{callCake, callMint}[i]
		history, err := b.history.list(t.Context(), id)
		if err != nil || len(history) != 1 || history[0].Source != "call" {
			t.Fatalf("%s call history: %v %v", id, history, err)
		}
	}
	checkCallStates(t, b.ws, callNone, callNone)
	if stt.Load() != 0 {
		t.Fatal("duplex call invoked STT")
	}
}

func TestWSCallDeclineBusyTimeoutAndDisconnect(t *testing.T) {
	b, base := wsHarness(t, nil)
	cake := callClient(t, b, base, callCake, "Cake")
	mint := callClient(t, b, base, callMint, "Mint")
	plum := callClient(t, b, base, "phone-000c", "Plum")
	cake.json(t, object{"ev": "call", "target": callMint})
	mint.until(t, hasWS("svg", "call-decline"))
	plum.json(t, object{"ev": "call", "target": callMint})
	plum.until(t, hasWS("svg", "Mint is busy"))
	tapCallControl(t, mint, "call-decline")
	cake.until(t, hasWS("call", `"reason":"declined"`))
	cake.until(t, hasWS("svg", "Mint declined"))
	mint.until(t, hasWS("call", `"reason":"declined"`))
	checkCallStates(t, b.ws, callNone, callNone)

	b.ws.mu.Lock()
	err := b.ws.ringCall(b.ws.devices[callCake], callMint, 40*time.Millisecond)
	b.ws.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	cake.until(t, hasWS("call", `"reason":"no answer"`))
	cake.until(t, hasWS("svg", "No answer from Mint"))
	mint.until(t, hasWS("call", `"reason":"no answer"`))
	checkCallStates(t, b.ws, callNone, callNone)

	cake.json(t, object{"ev": "call", "target": callMint})
	mint.until(t, hasWS("svg", "call-answer"))
	tapCallControl(t, mint, "call-answer")
	cake.until(t, hasWS("call", `"state":"start"`))
	mint.conn.Close()
	cake.until(t, hasWS("call", `"reason":"disconnected"`))
	checkCallStates(t, b.ws, callNone, callNone)

	old := testConnectWS(t, base, "unit", false)
	old.hello(t, "lark-legacy", "Legacy")
	cake.json(t, object{"ev": "call", "target": "lark-legacy"})
	cake.until(t, hasWS("svg", "firmware"))
	cake.json(t, object{"ev": "call", "target": "offline"})
	cake.until(t, hasWS("svg", "offline is offline"))
}

func TestWSCallCardAndVoiceCommand(t *testing.T) {
	b, base := wsHarness(t, func(b *bridge, dir string) {
		b.stt = func(context.Context, []byte) (string, error) { return "Can you call Mint?", nil }
	})
	cake := callClient(t, b, base, callCake, "Cake")
	mint := callClient(t, b, base, callMint, "Mint")
	cake.json(t, object{"ev": "rec_start"})
	cake.write(t, maskedFrame(wsBinary, true, append([]byte{micPrefix}, make([]byte, callPCMBytes)...)))
	cake.json(t, object{"ev": "rec_end"})
	mint.until(t, hasWS("svg", "call-answer"))
	tapCallControl(t, mint, "call-decline")
	mint.until(t, hasWS("call", `"state":"end"`))
	checkCallStates(t, b.ws, callNone, callNone)

	cake.json(t, object{"ev": "intercom", "target": callMint, "text": "Hello Mint"})
	msg := mint.until(t, hasWS("svg", `data-id=\"call\"`))
	for _, control := range []string{"reply", "done"} {
		if !hasWS("svg", fmt.Sprintf(`data-id=\"%s\"`, control))(msg) {
			t.Fatalf("card is missing %q", control)
		}
	}
	tapCallControl(t, mint, "call")
	cake.until(t, hasWS("svg", "call-answer"))
	checkCallStates(t, b.ws, callRingingIn, callRingingOut)
}

func TestParseCallCommand(t *testing.T) {
	for text, want := range map[string]string{
		"call Mint": "Mint", "Call Mint.": "Mint", "Can you call Mint?": "Mint",
		"could you call Mint please": "Mint", "please call Mint": "Mint", "hey call Mint": "Mint",
		"ring Mint": "Mint", "give Mint a call": "Mint", "please ring greg": "greg",
		"Hey Cake, could you please give Blue Bell a call, please?": "Blue Bell",
		"what do you call a cat?":                                   "", "don't call Mint": "", "call all": "",
		"Call": "", "Let's talk to Mint on a call": "",
	} {
		if got := parseCallCommand(text); got != want {
			t.Errorf("parseCallCommand(%q) = %q; want %q", text, got, want)
		}
	}
}

func TestWSNaturalVoiceCallRequiresKnownPeer(t *testing.T) {
	transcripts := make(chan string, 1)
	b, base := wsHarness(t, func(b *bridge, dir string) {
		b.stt = func(context.Context, []byte) (string, error) { return <-transcripts, nil }
	})
	cake := callClient(t, b, base, callCake, "Cake")
	mint := callClient(t, b, base, callMint, "Mint")
	greg := callClient(t, b, base, "phone-000c", "Greg")
	voice := func(text string) {
		transcripts <- text
		cake.json(t, object{"ev": "rec_start"})
		cake.until(t, hasWS("svg", "rec-send"))
		cake.write(t, maskedFrame(wsBinary, true, append([]byte{micPrefix}, make([]byte, callPCMBytes)...)))
		cake.json(t, object{"ev": "rec_end"})
	}
	for _, tc := range []struct {
		text string
		peer *testWS
	}{
		{"Call Mint.", mint},
		{"please ring greg", greg},
	} {
		voice(tc.text)
		tc.peer.until(t, hasWS("svg", "call-answer"))
		tapCallControl(t, tc.peer, "call-decline")
		cake.until(t, hasWS("call", `"reason":"declined"`))
	}
	for _, text := range []string{"what do you call a cat?", "call a taxi"} {
		voice(text)
		cake.until(t, hasWS("svg", "Chat is not ready yet"))
		checkCallStates(t, b.ws, callNone, callNone)
	}
}

func TestMCPEmitCall(t *testing.T) {
	b, base := wsHarness(t, nil)
	cake := callClient(t, b, base, callCake, "Cake")
	mint := callClient(t, b, base, callMint, "Mint")
	for _, args := range []object{
		{"to": "all", "call": object{"from": callCake}},
		{"to": callMint, "call": object{}},
		{"to": callMint, "call": object{"from": callCake}, "chime": "tap"},
		{"to": callCake, "call": object{"from": callCake}},
	} {
		toolTest(t, base, "emit", args, true)
		checkCallStates(t, b.ws, callNone, callNone)
	}
	out := toolTest(t, base, "emit", object{"to": callMint, "call": object{"from": callCake}}, false)
	if result := out["call"].(object); result["state"] != "ringing" || result["id"] == nil {
		t.Fatal(out)
	}
	cake.until(t, hasWS("svg", "Calling Mint"))
	mint.until(t, hasWS("svg", "call-answer"))
	tapCallControl(t, mint, "call-answer")
	cake.until(t, hasWS("call", `"state":"start"`))
	mint.until(t, hasWS("call", `"state":"start"`))
	checkCallStates(t, b.ws, callActive, callActive)
	finishTestCall(t, cake, mint)
}

func TestWSCallPreemptsGameAndStaysAwake(t *testing.T) {
	b, base := wsHarness(t, nil)
	cake := callClient(t, b, base, callCake, "Cake")
	mint := callClient(t, b, base, callMint, "Mint")
	game := object{"devices": callMint, "tick_ms": 250, "code": `
		function init(){return {x:180}}
		function render(s){return '<svg><circle data-id="game-target" cx="'+s.x+'" cy="180" r="30"/></svg>'}
		function onTick(s){s.x++; return s}
	`}
	toolTest(t, base, "app", game, false)
	mint.until(t, hasWS("svg", "game-target"))
	cake.json(t, object{"ev": "call", "target": callMint})
	mint.until(t, hasWS("svg", "call-answer"))
	b.ws.mu.Lock()
	d := b.ws.devices[callMint]
	app := d.app
	// Even a stale app marker must not claim a tap on the committed call scene.
	d.displayApp = app
	b.ws.mu.Unlock()
	if app == nil {
		t.Fatal("ringing closed the game before Answer")
	}
	toolTest(t, base, "app", game, true)
	tapCallControl(t, mint, "call-answer")
	mint.until(t, hasWS("call", `"state":"start"`))
	mint.until(t, hasWS("svg", "call-mute"))
	toolTest(t, base, "app", game, true)

	b.ws.mu.Lock()
	closed := d.app == nil && app.ctx.Err() != nil
	backdated := time.Now().Add(-idleSleep - time.Second)
	for _, side := range b.ws.devices {
		side.callStarted, side.last = backdated, backdated
	}
	b.ws.mu.Unlock()
	if !closed {
		t.Fatal("Answer did not release the game")
	}
	for _, side := range []*testWS{cake, mint} {
		side.until(t, hasWS("svg", `aria-label=\"2:`))
	}
	b.ws.mu.Lock()
	for _, side := range b.ws.devices {
		if side.asleep || side.brightness() != 85 || side.callSecond < idleSleep {
			t.Errorf("%s dimmed or didn't animate past idle timeout", side.key)
		}
	}
	b.ws.mu.Unlock()
	checkCallStates(t, b.ws, callActive, callActive)
	finishTestCall(t, mint, cake)
}

func TestWSCallStopsPlaybackAndExcludesNoteTail(t *testing.T) {
	b, base := wsHarness(t, nil)
	cake := callClientCaps(t, b, base, callCake, "Cake", true)
	mint := callClientCaps(t, b, base, callMint, "Mint", true)
	cake.json(t, object{"ev": "rec_start"})
	cake.until(t, hasWS("mode", "listening"))
	stream := newAudioStream()
	old := bytes.Repeat([]byte{5, 1}, callPCMBytes/2)
	stream.append(old)
	b.ws.mu.Lock()
	playback := b.ws.play(b.ws.devices[callMint], stream)
	b.ws.mu.Unlock()
	mint.until(t, hasWS("tts", `"state":"start"`))
	mint.until(t, isPlayback)

	cake.json(t, object{"ev": "call", "target": callMint})
	cake.until(t, hasWS("rec", `"cancelled":true`))
	mint.until(t, hasWS("svg", "call-answer"))
	if playback.Err() != nil {
		t.Fatal("callee playback stopped before Answer")
	}
	cake.write(t, maskedFrame(wsBinary, true, append([]byte{callMicPrefix}, old...)))
	cake.barrier(t)
	tapCallControl(t, mint, "call-answer")
	mint.until(t, hasWS("tts", `"state":"abort"`))
	for _, c := range []*testWS{cake, mint} {
		c.until(t, hasWS("call", `"mic_prefix":4`))
	}
	if playback.Err() != context.Canceled {
		t.Fatal("Answer did not cancel the playback producer")
	}
	stream.append(old)
	stream.finish(nil)
	live := bytes.Repeat([]byte{15, 2}, callPCMBytes/2)
	cake.write(t, maskedFrame(wsBinary, true, append([]byte{micPrefix}, old...)))
	cake.write(t, maskedFrame(wsBinary, true, append([]byte{callMicPrefix}, live...)))
	if f := mint.until(t, isPlayback); !bytes.Equal(f.data[1:], live) {
		t.Fatal("callee received stale note or TTS PCM")
	}
	b.ws.mu.Lock()
	d := b.ws.devices[callCake]
	framesIn, framesOut := d.callFramesIn, d.callFramesOut
	b.ws.mu.Unlock()
	if framesIn != 1 || framesOut != 1 {
		t.Fatalf("call frame counters = %d in, %d out", framesIn, framesOut)
	}
	finishTestCall(t, mint, cake)
}

func TestWSCallGlareAndAudioFailure(t *testing.T) {
	b, base := wsHarness(t, nil)
	cake := callClient(t, b, base, callCake, "Cake")
	mint := callClient(t, b, base, callMint, "Mint")
	cake.json(t, object{"ev": "call", "target": callMint})
	mint.json(t, object{"ev": "call", "target": callCake})
	c := cake.until(t, hasWS("call", `"state":"start"`))
	m := mint.until(t, hasWS("call", `"state":"start"`))
	if !bytes.Equal(c.data, m.data) {
		t.Fatalf("glare created mismatched calls: %s / %s", c.data, m.data)
	}
	checkCallStates(t, b.ws, callActive, callActive)
	b.ws.mu.Lock()
	id := b.ws.devices[callMint].callID
	b.ws.mu.Unlock()
	mint.json(t, object{"ev": "call_end", "call": id + 1})
	mint.barrier(t)
	checkCallStates(t, b.ws, callActive, callActive)
	mint.json(t, object{"ev": "call_end", "call": id})
	for _, c := range []*testWS{cake, mint} {
		c.until(t, hasWS("call", `"reason":"audio unavailable"`))
	}
	checkCallStates(t, b.ws, callNone, callNone)
}

func TestCallPCMBackpressure(t *testing.T) {
	h := &wsHub{devices: make(map[string]*wsDevice)}
	makeSide := func(id, peer string) *wsDevice {
		d := &wsDevice{key: id, callPeer: peer, callID: 1, callState: callActive,
			client: &wsClient{ctx: context.Background(), out: make(chan wsFrame, 8)}}
		h.devices[id] = d
		return d
	}
	caller := makeSide(callCake, callMint)
	callee := makeSide(callMint, callCake)
	for i := range 10 {
		h.relayCallPCM(caller, append([]byte{micPrefix, byte(i)}, make([]byte, callPCMBytes-1)...))
	}
	if len(callee.client.out) != 6 {
		t.Fatal("call relay did not enforce its queue ceiling")
	}
	for i := range 6 {
		if got := <-callee.client.out; got.data[0] != pcmPrefix || got.data[1] != byte(i) {
			t.Fatalf("unexpected PCM: %v", got)
		}
	}
}

func TestWSPagedVoiceMenu(t *testing.T) {
	b, base := wsHarness(t, nil)
	cake := callClient(t, b, base, callCake, "Cake")
	tapCallControl(t, cake, "menu")
	home := cake.until(t, hasWS("svg", "menu-call"))
	if !hasWS("svg", "menu-intercom")(home) {
		t.Fatal("main menu must offer both Call and Talk to…")
	}
	tapCallControl(t, cake, "menu-next")
	cake.until(t, hasWS("svg", "menu-voice"))
	tapCallControl(t, cake, "menu-voice")
	for i, voice := range voiceCatalog {
		if i%tilesPerPage != 0 {
			continue
		}
		page := cake.until(t, hasWS("svg", "voice-"+strings.ToLower(voice.Name)))
		for _, want := range voiceCatalog[i:min(i+tilesPerPage, len(voiceCatalog))] {
			if !hasWS("svg", "voice-"+strings.ToLower(want.Name))(page) {
				t.Fatalf("missing voice %s from page %d", want.Name, i/tilesPerPage)
			}
		}
		if i+tilesPerPage < len(voiceCatalog) {
			tapCallControl(t, cake, "menu-next")
		}
	}
	tapCallControl(t, cake, "menu-prev")
	cake.until(t, hasWS("svg", "voice-"+strings.ToLower(voiceCatalog[0].Name)))
}

func TestHelloCallCapabilities(t *testing.T) {
	for _, tc := range []struct {
		event wsEvent
		want  deviceCaps
	}{
		{wsEvent{Role: "device"}, deviceCaps{Drag: true}},
		{wsEvent{Role: "web", Caps: wsCaps{Call: new(true), Drag: new(true)}}, deviceCaps{Call: true, Drag: true}},
		{wsEvent{Role: "device", Caps: wsCaps{Battery: new(true), HeadPads: new(true), Call: new(true), Drag: new(false)}},
			deviceCaps{Battery: true, HeadPads: true, Call: true}},
	} {
		if got := helloCaps(tc.event); got != tc.want {
			t.Errorf("%s: got %+v; want %+v", tc.event.Role, got, tc.want)
		}
	}
}
