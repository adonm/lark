package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const callCake = "cake"
const callMint = "mint"

func callClient(t *testing.T, b *larkServer, base, id, name string) *testWS {
	return callClientCaps(t, b, base, id, name, true)
}

func callClientCaps(t *testing.T, b *larkServer, base, id, name string, tagged bool) *testWS {
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
	var caps uint32 = capCall | capDrag
	if tagged {
		caps |= capCallPCM
	}
	c.helloWeb(t, "web-"+randomHex(4), id, caps, false)
	return c
}

func callCtrlMatch(op uint8, substr string) func(wsFrame) bool {
	return func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgCallCtrl, substr)(f) && len(p) >= 1 && p[0] == op
	}
}

func sendCallStartReq(t *testing.T, c *testWS, target string) {
	t.Helper()
	b := append([]byte{callOpStart, 0, 0, 0, 0}, byte(len(target)))
	c.sendProto(t, msgCallReq, append(b, target...))
}

func sendCallEndReq(t *testing.T, c *testWS, callID uint32) {
	t.Helper()
	var b [6]byte
	b[0] = callOpEnd
	binary.LittleEndian.PutUint32(b[1:5], callID)
	c.sendProto(t, msgCallReq, b[:])
}

func tapCallControl(t *testing.T, c *testWS, id string) {
	t.Helper()
	switch id {
	case "call-answer", "call-mute":
		c.tap(t, 118, 272, 0)
	case "call-decline":
		c.tap(t, 242, 272, 0)
	case "call-hangup":
		c.tap(t, 200, 272, 0)
	default:
		c.tap(t, 180, 180, 0)
	}
}

func finishTestCall(t *testing.T, caller, peer *testWS) {
	t.Helper()
	caller.barrier(t)
	tapCallControl(t, caller, "call-hangup")
	for _, c := range []*testWS{caller, peer} {
		c.until(t, callCtrlMatch(callOpEnd, "hung up"))
		c.until(t, hasProto(msgEvent, "Voice call with "))
	}
}

func checkCallStates(t *testing.T, h *wsHub, a, b callState) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, want := range map[string]callState{callCake: a, callMint: b} {
		d := h.voiceOwner(id)
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
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.stt = func(context.Context, []byte) (string, error) { stt.Add(1); return "", nil }
	})
	cake := callClient(t, b, base, callCake, "Cake")
	mint := callClient(t, b, base, callMint, "Mint")
	sendCallStartReq(t, cake, callMint)
	cake.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgChime, "")(f) && len(p) >= 1 && p[0] == chimeRing
	})
	cake.until(t, hasSVG("Calling Mint"))
	mint.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgChime, "")(f) && len(p) >= 1 && p[0] == chimeRing
	})
	mint.until(t, hasSVG("call-answer"))
	checkCallStates(t, b.ws, callRingingOut, callRingingIn)

	tapCallControl(t, mint, "call-answer")
	for _, c := range []*testWS{cake, mint} {
		c.until(t, callCtrlMatch(callOpStart, ""))
		c.until(t, hasSVG("call-mute"))
	}
	checkCallStates(t, b.ws, callActive, callActive)
	pcm := bytes.Repeat([]byte{100, 5}, callPCMBytes/2)
	var rtpSeq uint16
	sendPCM := func(c *testWS, sample []byte) {
		for len(sample) > 0 {
			n := min(rtpPCMBytes, len(sample))
			rtpSeq++
			c.sendRTP(t, rtpPTCallUp, rtpSeq, uint32(rtpSeq)*320, 1, sample[:n])
			sample = sample[n:]
		}
	}
	readCallPCM := func(c *testWS, wantLen int) []byte {
		var out []byte
		c.until(t, func(f wsFrame) bool {
			if hasProto(msgMedia, "")(f) {
				p := framePayload(f)
				if pt, _, _, _, ok := decodeRTPHeader(p); ok && pt == rtpPTCallDown {
					out = append(out, p[rtpHeaderSize:]...)
				}
			}
			return len(out) >= wantLen
		})
		return out
	}
	for _, pair := range [][2]*testWS{{cake, mint}, {mint, cake}} {
		sendPCM(pair[0], pcm)
		if got := readCallPCM(pair[1], len(pcm)); !bytes.Equal(got, pcm) {
			t.Fatal("call changed the relayed PCM")
		}
	}

	tapCallControl(t, cake, "call-mute")
	cake.until(t, callCtrlMatch(callOpMute, ""))
	cake.until(t, hasSVG("Unmute"))
	sendPCM(cake, pcm)
	cake.barrier(t)
	// A mute blocks uplink only.
	sendPCM(mint, pcm)
	readCallPCM(cake, len(pcm))
	tapCallControl(t, cake, "call-mute")
	cake.until(t, callCtrlMatch(callOpUnmute, ""))
	probe := bytes.Repeat([]byte{5, 100}, callPCMBytes/2)
	sendPCM(cake, probe)
	if got := readCallPCM(mint, len(probe)); !bytes.Equal(got, probe) {
		t.Fatal("received PCM queued while caller was muted")
	}
	// A touch on the avatar must neither end the call nor start a recording.
	cake.tap(t, 180, 113, 0)
	cake.barrier(t)
	checkCallStates(t, b.ws, callActive, callActive)
	b.ws.mu.Lock()
	for _, d := range b.ws.terminals {
		if d.recording.pcm != nil || d.recording.open() || d.busy {
			t.Error("call audio entered the assistant turn path")
		}
	}
	b.ws.mu.Unlock()

	tapCallControl(t, mint, "call-hangup")
	for i, c := range []*testWS{cake, mint} {
		c.until(t, callCtrlMatch(callOpEnd, ""))
		c.until(t, func(f wsFrame) bool {
			p := framePayload(f)
			return hasProto(msgChime, "")(f) && len(p) >= 1 && p[0] == chimeCancel
		})
		peer := []string{"Mint", "Cake"}[i]
		var idle, noted bool
		c.until(t, func(f wsFrame) bool {
			idle = idle || hasSVG("companion-ring")(f)
			noted = noted || hasProto(msgEvent, "Voice call with "+peer+" (0:")(f)
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
	plum := callClient(t, b, base, "plum", "Plum")
	sendCallStartReq(t, cake, callMint)
	mint.until(t, hasSVG("call-decline"))
	sendCallStartReq(t, plum, callMint)
	plum.until(t, hasSVG("Mint is busy"))
	tapCallControl(t, mint, "call-decline")
	cake.until(t, callCtrlMatch(callOpEnd, "declined"))
	cake.until(t, hasSVG("Mint declined"))
	mint.until(t, callCtrlMatch(callOpEnd, "declined"))
	checkCallStates(t, b.ws, callNone, callNone)

	b.ws.mu.Lock()
	err := b.ws.ringCall(b.ws.voiceOwner(callCake), callMint, 40*time.Millisecond)
	b.ws.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	cake.until(t, callCtrlMatch(callOpEnd, "no answer"))
	cake.until(t, hasSVG("No answer from Mint"))
	mint.until(t, callCtrlMatch(callOpEnd, "no answer"))
	checkCallStates(t, b.ws, callNone, callNone)

	sendCallStartReq(t, cake, callMint)
	mint.until(t, hasSVG("call-answer"))
	tapCallControl(t, mint, "call-answer")
	cake.until(t, callCtrlMatch(callOpStart, ""))
	mint.conn.Close()
	cake.until(t, callCtrlMatch(callOpEnd, "disconnected"))
	checkCallStates(t, b.ws, callNone, callNone)

	old := testConnectWS(t, base, "unit", false)
	old.hello(t, "vocat-0099", "Legacy")
	sendCallStartReq(t, cake, "legacy")
	cake.until(t, hasSVG("firmware"))
	sendCallStartReq(t, cake, "offline")
	cake.until(t, hasSVG("offline is offline"))
}

func TestWSCallCardAndVoiceCommand(t *testing.T) {
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.stt = func(context.Context, []byte) (string, error) { return "Can you call Mint?", nil }
	})
	cake := callClient(t, b, base, callCake, "Cake")
	mint := callClient(t, b, base, callMint, "Mint")
	cake.recStart(t, "")
	cake.sendRTP(t, rtpPTRecPCM, 1, 320, 1, make([]byte, rtpPCMBytes))
	cake.recEnd(t, "", false)
	mint.until(t, hasSVG("call-answer"))
	tapCallControl(t, mint, "call-decline")
	mint.until(t, callCtrlMatch(callOpEnd, ""))
	checkCallStates(t, b.ws, callNone, callNone)

	toolTest(t, base, "emit", object{"to": callMint, "intercom": object{"from": callCake, "text": "Hello Mint"}}, false)
	msg := mint.until(t, hasSVG(`data-id="call"`))
	for _, control := range []string{"reply", "done"} {
		if !hasSVG(fmt.Sprintf(`data-id="%s"`, control))(msg) {
			t.Fatalf("card is missing %q", control)
		}
	}
	_, cardSVG := decodeFrameSVG(msg)
	cx, cy, ok := hitCenter(cardSVG, "call")
	if !ok {
		t.Fatal("missing call hit")
	}
	mint.tap(t, cx, cy, 0)
	cake.until(t, hasSVG("call-answer"))
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
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.stt = func(context.Context, []byte) (string, error) { return <-transcripts, nil }
	})
	cake := callClient(t, b, base, callCake, "Cake")
	mint := callClient(t, b, base, callMint, "Mint")
	greg := callClient(t, b, base, "greg", "Greg")
	voice := func(text string) {
		transcripts <- text
		cake.recStart(t, "")
		cake.until(t, hasSVG("rec-send"))
		cake.sendRTP(t, rtpPTRecPCM, 1, 320, 1, make([]byte, rtpPCMBytes))
		cake.recEnd(t, "", false)
	}
	for _, tc := range []struct {
		text string
		peer *testWS
	}{
		{"Call Mint.", mint},
		{"please ring greg", greg},
	} {
		voice(tc.text)
		tc.peer.until(t, hasSVG("call-answer"))
		tapCallControl(t, tc.peer, "call-decline")
		cake.until(t, callCtrlMatch(callOpEnd, "declined"))
	}
	for _, text := range []string{"what do you call a cat?", "call a taxi"} {
		voice(text)
		cake.until(t, hasSVG("Chat is not ready yet"))
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
	cake.until(t, hasSVG("Calling Mint"))
	mint.until(t, hasSVG("call-answer"))
	tapCallControl(t, mint, "call-answer")
	cake.until(t, callCtrlMatch(callOpStart, ""))
	mint.until(t, callCtrlMatch(callOpStart, ""))
	checkCallStates(t, b.ws, callActive, callActive)
	finishTestCall(t, cake, mint)
}

func TestWSCallPreemptsGameAndStaysAwake(t *testing.T) {
	b, base := wsHarness(t, nil)
	cake := callClient(t, b, base, callCake, "Cake")
	mint := callClient(t, b, base, callMint, "Mint")
	game := object{"companions": callMint, "tick_ms": 250, "code": `
		function init(){return {x:180}}
		function render(s){return '<svg><circle data-id="game-target" cx="'+s.x+'" cy="180" r="30"/></svg>'}
		function onTick(s){s.x++; return s}
	`}
	toolTest(t, base, "app", game, false)
	mint.until(t, hasSVG("game-target"))
	sendCallStartReq(t, cake, callMint)
	mint.until(t, hasSVG("call-answer"))
	b.ws.mu.Lock()
	d := b.ws.voiceOwner(callMint)
	app := d.app
	// Even a stale app marker must not claim a tap on the committed call scene.
	d.displayApp = app
	b.ws.mu.Unlock()
	if app == nil {
		t.Fatal("ringing closed the game before Answer")
	}
	toolTest(t, base, "app", game, true)
	tapCallControl(t, mint, "call-answer")
	mint.until(t, callCtrlMatch(callOpStart, ""))
	mint.until(t, hasSVG("call-mute"))
	toolTest(t, base, "app", game, true)

	b.ws.mu.Lock()
	closed := d.app == nil && app.ctx.Err() != nil
	backdated := time.Now().Add(-idleSleep - time.Second)
	for _, side := range b.ws.terminals {
		side.callStarted, side.last = backdated, backdated
	}
	b.ws.mu.Unlock()
	if !closed {
		t.Fatal("Answer did not release the game")
	}
	for _, side := range []*testWS{cake, mint} {
		side.until(t, hasSVG(`aria-label="2:`))
	}
	b.ws.mu.Lock()
	for _, side := range b.ws.terminals {
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
	cake.recStart(t, "")
	cake.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgStatus, "")(f) && len(p) >= 5 && p[4] == statusListening
	})
	stream := newAudioStream()
	old := bytes.Repeat([]byte{5, 1}, rtpPCMBytes/2)
	stream.append(old)
	b.ws.mu.Lock()
	playback := b.ws.play(b.ws.voiceOwner(callMint), stream)
	b.ws.mu.Unlock()
	mint.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgTTSCtrl, "")(f) && len(p) >= 1 && p[0] == ttsOpStart
	})
	mint.until(t, isPlayback)

	sendCallStartReq(t, cake, callMint)
	cake.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpCancel
	})
	mint.until(t, hasSVG("call-answer"))
	if playback.Err() != nil {
		t.Fatal("callee playback stopped before Answer")
	}
	cake.sendRTP(t, rtpPTCallUp, 1, 320, 1, old)
	cake.barrier(t)
	tapCallControl(t, mint, "call-answer")
	mint.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgTTSCtrl, "")(f) && len(p) >= 1 && p[0] == ttsOpStop
	})
	for _, c := range []*testWS{cake, mint} {
		c.until(t, callCtrlMatch(callOpStart, ""))
	}
	mint.until(t, hasSVG("call-hangup"))
	if playback.Err() != context.Canceled {
		t.Fatal("Answer did not cancel the playback producer")
	}
	stream.append(old)
	stream.finish(nil)
	live := bytes.Repeat([]byte{15, 2}, rtpPCMBytes/2)
	cake.sendRTP(t, rtpPTRecPCM, 2, 640, 1, old)
	cake.sendRTP(t, rtpPTCallUp, 2, 640, 1, live)
	f := mint.until(t, hasProto(msgMedia, ""))
	if !bytes.Equal(framePayload(f)[rtpHeaderSize:], live) {
		t.Fatal("callee received stale note or TTS PCM")
	}
	b.ws.mu.Lock()
	d := b.ws.voiceOwner(callCake)
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
	sendCallStartReq(t, cake, callMint)
	sendCallStartReq(t, mint, callCake)
	c := cake.until(t, callCtrlMatch(callOpStart, ""))
	m := mint.until(t, callCtrlMatch(callOpStart, ""))
	if !bytes.Equal(framePayload(c), framePayload(m)) {
		t.Fatalf("glare created mismatched calls: %x / %x", framePayload(c), framePayload(m))
	}
	checkCallStates(t, b.ws, callActive, callActive)
	b.ws.mu.Lock()
	id := b.ws.voiceOwner(callMint).callID
	b.ws.mu.Unlock()
	sendCallEndReq(t, mint, id+1)
	mint.barrier(t)
	checkCallStates(t, b.ws, callActive, callActive)
	sendCallEndReq(t, mint, id)
	for _, c := range []*testWS{cake, mint} {
		c.until(t, callCtrlMatch(callOpEnd, "audio unavailable"))
	}
	checkCallStates(t, b.ws, callNone, callNone)
}

func TestCallPCMBackpressure(t *testing.T) {
	h := &wsHub{terminals: make(map[string]*wsTerminal)}
	makeSide := func(id, peer string) *wsTerminal {
		d := &wsTerminal{key: id, id: id, callPeer: peer, callID: 1, callState: callActive,
			client: &wsClient{ctx: context.Background(), out: make(chan wsFrame, 8)}}
		h.terminals[id] = d
		return d
	}
	caller := makeSide(callCake, callMint)
	callee := makeSide(callMint, callCake)
	for i := range 10 {
		pcm := make([]byte, rtpPCMBytes)
		pcm[0] = byte(i)
		h.relayCallPCMBytes(caller, pcm)
	}
	if len(callee.client.out) != 6 {
		t.Fatal("call relay did not enforce its queue ceiling")
	}
	for i := range 6 {
		got := <-callee.client.out
		mt, _, p, ok := unpackProtoPacket(got.data)
		if !ok || mt != msgMedia || p[rtpHeaderSize] != byte(i) {
			t.Fatalf("unexpected PCM: ok=%t mt=%x", ok, mt)
		}
	}
}

func TestWSPagedVoiceMenu(t *testing.T) {
	b, base := wsHarness(t, nil)
	cake := callClient(t, b, base, callCake, "Cake")
	cake.tap(t, 180, 316, 0)
	home := cake.until(t, hasSVG("menu-call"))
	if !hasSVG("menu-intercom")(home) {
		t.Fatal("main menu must offer both Call and Talk to…")
	}
	cake.tap(t, 248, 294, 0)
	menuP2 := cake.until(t, hasSVG("menu-voice"))
	_, p2svg := decodeFrameSVG(menuP2)
	vx, vy, ok := hitCenter(p2svg, "menu-voice")
	if !ok {
		t.Fatal("missing menu-voice hit")
	}
	cake.tap(t, vx, vy, 0)
	for i, voice := range voiceCatalog {
		if i%tilesPerPage != 0 {
			continue
		}
		page := cake.until(t, hasSVG("voice-"+strings.ToLower(voice.Name)))
		for _, want := range voiceCatalog[i:min(i+tilesPerPage, len(voiceCatalog))] {
			if !hasSVG("voice-"+strings.ToLower(want.Name))(page) {
				t.Fatalf("missing voice %s from page %d", want.Name, i/tilesPerPage)
			}
		}
		if i+tilesPerPage < len(voiceCatalog) {
			cake.tap(t, 248, 294, 0)
		}
	}
	px, py, ok := hitCenter(p2svg, "menu-prev")
	if !ok {
		cake.tap(t, 180, 294, 0)
	} else {
		cake.tap(t, px, py, 0)
	}
	cake.until(t, hasSVG("voice-"+strings.ToLower(voiceCatalog[0].Name)))
}

func TestHelloCallCapabilities(t *testing.T) {
	for _, tc := range []struct {
		mask uint32
		want terminalCaps
	}{
		{capDrag, terminalCaps{Drag: true}},
		{capCall | capDrag, terminalCaps{Call: true, Drag: true}},
		{capBattery | capHeadPads | capCall, terminalCaps{Battery: true, HeadPads: true, Call: true}},
	} {
		if got := helloCapsFromMask(tc.mask); got != tc.want {
			t.Errorf("%#x: got %+v; want %+v", tc.mask, got, tc.want)
		}
	}
}
