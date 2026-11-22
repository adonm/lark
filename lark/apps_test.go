package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"image"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestAppVMLeavesHubUnlockedAndCannotPublishAfterExit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := &larkServer{root: t.Context()}
		c := &wsClient{kind: "hardware", ctx: t.Context(), out: make(chan wsFrame, 8)}
		d := &wsTerminal{client: c, key: "vocat-a100", id: "vocat-a100", kind: "hardware", displayFormats: dispRGB565, companionID: "cake", screen: roundScreen, companion: blankCompanion()}
		h := &wsHub{b: b, terminals: map[string]*wsTerminal{d.key: d}, voiceOwners: map[string]string{}, companionStatuses: map[string]companionStatusState{}, resumes: map[[resumeTokenLen]byte]*resumeEntry{}}
		const code = `function init() { return {color: "red"}; }
			function render(s) { return '<svg><rect width="360" height="360" fill="' + s.color + '"/></svg>'; }
			function onTap(s) { waitForTest(); s.color = "lime"; chime("tap"); }`
		a, views, err := initApp(savedApp{Code: code}, []appPlayer{{id: d.companionID}}, nil, []*wsTerminal{d}, nil, buildScreen)
		if err != nil {
			t.Fatal(err)
		}
		entered, proceed := make(chan struct{}), make(chan struct{})
		release := syncOnceClose(proceed)
		defer release()
		if err := a.vm.Set("waitForTest", func() {
			close(entered)
			<-proceed
		}); err != nil {
			t.Fatal(err)
		}
		a.ctx, a.cancel = context.WithCancel(t.Context())
		h.mu.Lock()
		d.app, d.appView = a, views[0]
		h.render(d, time.Now(), false)
		h.mu.Unlock()
		synctest.Wait()
		h.mu.Lock()
		h.updateApp(a, appEvent{kind: "tap", x: 180, y: 180, now: time.Now()})
		h.mu.Unlock()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("app tap did not run")
		}
		if !h.mu.TryLock() {
			t.Fatal("VM held the global hub lock")
		}
		h.closeApp(a, time.Now())
		h.mu.Unlock()
		release()
		synctest.Wait()
		if d.app != nil || d.displayApp != nil || !strings.Contains(d.svg.source, "companion-ring") {
			t.Fatal("exited app published its superseded render")
		}
		for len(c.out) > 0 {
			f := <-c.out
			if hasProto(msgChime, "")(f) {
				t.Fatal("exited app's effects escaped", string(f.data))
			}
		}
	})
}

func (s *testWS) helloV2(t *testing.T, id, cat string) wsFrame {
	t.Helper()
	return s.helloHardware(t, id, cat, capBattery|capDrag)
}

func TestAppSandboxAndErrors(t *testing.T) {
	players := []appPlayer{{"lark-test", "Cake", 0}}
	for _, tc := range []struct{ code, want string }{
		{`while (true) {}`, "timeout"},
		{`function init(){for(;;){}} function render(){return "<svg/>"}`, "timeout"},
		{`app={get render(){for(;;){}}}`, "timeout"},
		{`function render(){ throw new Error("broken paint") }`, "broken paint"},
		{`function render(){ throw {toString(){for(;;){}}} }`, "JS exception"},
		{"\nfunction render(){ \n throw new Error('broken line');\n}", "app.js:3:"},
		{`function render(){return require("fs")}`, "require is not defined"},
		{`function init(){storeSet("score",1)} function render(){return "<svg/>"}`, "only available"},
		{`function render(){storeSet("score",1); return "<svg/>"}`, "only available"},
	} {
		start := time.Now()
		_, _, err := initApp(savedApp{Code: tc.code}, players, nil, nil, nil, buildScreen)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("want %q, got %v", tc.want, err)
		}
		if tc.want == "timeout" && (time.Since(start) < 40*time.Millisecond || time.Since(start) > time.Second) {
			t.Fatalf("timeout took %v", time.Since(start))
		}
	}
	code := `function render() {
		if ([typeof require,typeof process,typeof fetch,typeof setTimeout,typeof readFile].some(x=>x!=="undefined")) throw "host leak";
		return '<svg/>';
	} function onTap(){while(true){}}`
	app, _, err := initApp(savedApp{Code: code}, players, nil, nil, nil, buildScreen)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.update(appEvent{kind: "tap", id: "spot", x: 180, y: 180}, nil); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatal(err)
	}
	if _, err := app.screens(nil); err != nil {
		t.Fatalf("interruption leaked into next call: %v", err)
	}
}

func TestWhackAMonsterOnV2Board(t *testing.T) {
	b, base := wsHarness(t, nil)
	const id = "cake"
	device := testConnectWS(t, base, "unit", false)
	first := device.helloV2(t, "vocat-0001", "Cake")
	r, _, _ := decodeDisplay(t, first)
	if r != image.Rect(0, 0, 360, 360) {
		t.Fatalf("first frame not full: %v", r)
	}
	const code = `app={
	init(ctx){ return {n:storeGet("score") ?? 0,x:100}; },
	render(s,p){ return '<svg viewBox="0 0 360 360"><rect width="360" height="360" fill="black"/><circle data-id="monster" cx="'+s.x+'" cy="180" r="30" fill="lime"/><text x="180" y="100" fill="white">Score '+s.n+' / saved '+(storeGet("score") ?? 0)+'</text></svg>'; },
	onTap(s,p,id){ if(id==="monster"){ s.n++; storeSet("score",s.n); if(storeGet("score")!==s.n)throw "store did not read its write"; play("C5:40 E5:40"); } },
	onTick(s){ s.x+=50; return s; }
}`
	out := toolTest(t, base, "app", object{"companions": id, "title": "Whack a Monster", "code": code, "save_as": "monster", "tick_ms": 250, "max_ticks": 1}, false)
	if out["saved_as"] != "monster" || !slices.Contains(out["hit_ids"].([]any), "monster") {
		t.Fatal(out)
	}
	f := device.until(t, isDisplay)
	decodeDisplay(t, f)
	waitSVG(t, b.ws, "vocat-0001", "Score 0")
	waitSVG(t, b.ws, "vocat-0001", `cx="150"`)
	// Trust hardware coordinates, not an ID claimed by a client.
	device.tap(t, 150, 180, 0)
	device.until(t, isPlayback)
	waitSVG(t, b.ws, "vocat-0001", "Score 1")
	a := stateMe(t, base, id)["active_app"].(object)
	if a["ticks_left"] != json.Number("0") || a["state"].(object)["n"] != json.Number("1") {
		t.Fatal(a)
	}
	mem := memoryScope(t, base, "app:monster")["score"].(object)
	if mem["value"] != json.Number("1") {
		t.Fatal(mem)
	}
	device.tap(t, 180, 318, 0)
	waitSVG(t, b.ws, "vocat-0001", "companion-ring")
	if a := stateMe(t, base, id)["active_app"]; a != nil {
		t.Fatal(a)
	}
	if app, err := b.loadApp(t.Context(), "monster"); err != nil || app.Code != code || app.TickMS != 250 || app.MaxTicks != 1 {
		t.Fatalf("saved %+v (%v)", app, err)
	}
	toolTest(t, base, "app", object{"companions": id, "load": "monster"}, false)
	waitSVG(t, b.ws, "vocat-0001", "Score 1 / saved 1")
	if got := toolTest(t, base, "app", object{"delete_saved": "monster"}, true); got["error"] != "saved app is running: monster" {
		t.Fatal(got)
	}
	toolTest(t, base, "app", object{"companions": id, "exit": true}, false)
	toolTest(t, base, "app", object{"delete_saved": "monster"}, false)
	saved := toolTest(t, base, "state", object{"include": []string{"apps"}}, false)
	if len(saved["apps"].(object)) != 0 {
		t.Fatal(saved)
	}
}

func TestAppDragAndUp(t *testing.T) {
	b, base := wsHarness(t, nil)
	const id = "cake"
	board := testConnectWS(t, base, "unit", false)
	board.helloV2(t, "vocat-0002", "Cake")
	const code = `
	function init(){ return {text:"Ready"}; }
	function render(s){ return '<svg><circle data-id="thumb" cx="180" cy="180" r="20"/><text x="50" y="90">'+s.text+'</text></svg>'; }
	function onDrag(s,p,id,x,y){ s.text="Drag "+p.index+" "+id+" "+x+","+y; }
	function onUp(s,p,id,x,y){ storeSet("end",x); return {text:s.text+" Up "+id+" "+storeGet("end")+","+y}; }`
	toolTest(t, base, "app", object{"companions": id, "code": code, "save_as": "slider"}, false)
	waitSVG(t, b.ws, "vocat-0002", "Ready")
	board.sendProto(t, msgPointer, encodePointer(ptrDrag, 180, 180, 0))
	waitSVG(t, b.ws, "vocat-0002", "Drag 0 thumb 180,180")
	board.sendProto(t, msgPointer, encodePointer(ptrUp, 190, 181, 0))
	waitSVG(t, b.ws, "vocat-0002", "Drag 0 thumb 180,180 Up thumb 190,181")
	mem := memoryScope(t, base, "app:slider")["end"].(object)
	if mem["value"] != json.Number("190") {
		t.Fatal(mem)
	}
}

func TestAppDragBurstPreservesCriticalEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &wsHub{b: &larkServer{}}
		app, _, err := initApp(savedApp{Code: `
			function init() { return {}; }
			function render() { return '<svg/>'; }
			function onDrag() {}
			function onUp() {}
			function onTap() {}
			function onTick() {}`}, nil, nil, nil, nil, buildScreen)
		if err != nil {
			t.Fatal(err)
		}
		app.running = true // Model a busy JS callback with an undrained queue.
		for x := range 1000 {
			if !h.updateApp(app, appEvent{kind: "drag", i: 0, x: x}) {
				t.Fatal("pending drag should be replaceable")
			}
		}
		for _, ev := range []appEvent{{kind: "up", i: 0}, {kind: "tick", i: -1}, {kind: "tap", i: 0}} {
			if !h.updateApp(app, ev) {
				t.Fatalf("drag burst blocked %s", ev.kind)
			}
		}
		if q := app.events; len(q) != 4 || q[0].kind != "drag" || q[0].x != 999 || q[1].kind != "up" || q[2].kind != "tick" || q[3].kind != "tap" {
			t.Fatalf("unexpected queue: %+v", q)
		}
		for len(app.events) < 32 {
			h.updateApp(app, appEvent{kind: "tap", i: 0})
		}
		if !h.updateApp(app, appEvent{kind: "up", i: 0}) || app.events[0].kind != "up" || app.events[31].kind != "up" {
			t.Fatal("full queue did not make room for a terminal event by dropping a drag")
		}
	})
}

func TestNativeAppDragAndUp(t *testing.T) {
	for _, tc := range []struct {
		board string
		screenGeometry
		drag, up image.Point
	}{
		{"esp-vocat", roundScreen, image.Pt(150, 168), image.Pt(210, 192)},
		{"esp-box-3", screenGeometry{320, 240, "rect"}, image.Pt(140, 112), image.Pt(180, 128)},
	} {
		t.Run(tc.board, func(t *testing.T) {
			b, base := wsHarness(t, nil)
			const id = "cake"
			board := testConnectWS(t, base, "unit", false)
			shape := shapeRound
			if tc.shape == "rect" {
				shape = shapeRect
			}
			board.sendProto(t, msgHello, encodeTerminalHello(terminalHello{
				protoVersion:   protoVersion1,
				kind:           termKindHardware,
				capsMask:       capDrag,
				displayFormats: dispRGB565,
				frameCodecs:    codecBitAll,
				width:          uint16(tc.width),
				height:         uint16(tc.height),
				shape:          shape,
				terminalID:     "vocat-0003",
				companionID:    "Cake",
				board:          tc.board,
				firmware:       "0.12.0",
				token:          "unit",
			}))
			board.until(t, isDisplay)
			const code = `
			function init(){ return {text:"Ready"}; }
			function render(s){ return '<svg><rect x="100" y="120" width="140" height="120" data-id="track"/>'+
				'<text x="70" y="100">'+s.text+'</text></svg>'; }
			function onDrag(s,p,id,x,y){ s.text="Drag "+id+" "+x+","+y; }
			function onUp(s,p,id,x,y){ s.text+=" Up "+id+" "+x+","+y; }
			function onTap(s){ s.text="Tapped"; }`
			toolTest(t, base, "app", object{"companions": id, "code": code}, false)
			waitSVG(t, b.ws, "vocat-0003", "Ready")

			send := func(kind uint8, p image.Point) {
				board.sendProto(t, msgPointer, encodePointer(kind, uint16(p.X), uint16(p.Y), 0))
			}
			b.ws.mu.Lock()
			d := b.ws.voiceOwner(id)
			before := d.last
			b.ws.mu.Unlock()
			send(ptrDrag, tc.drag)
			waitSVG(t, b.ws, "vocat-0003", "Drag track 150,168")
			b.ws.mu.Lock()
			dragged := d.last
			b.ws.mu.Unlock()
			send(ptrUp, tc.up)
			waitSVG(t, b.ws, "vocat-0003", "Drag track 150,168 Up track 210,192")
			b.ws.mu.Lock()
			released := d.last
			b.ws.setSleeping(d, true)
			b.ws.render(d, time.Now(), false)
			b.ws.mu.Unlock()
			if !dragged.After(before) || !released.After(dragged) {
				t.Fatal("pointer gestures did not refresh activity")
			}

			send(ptrDrag, tc.drag)
			send(ptrUp, tc.up)
			board.until(t, func(f wsFrame) bool {
				p := framePayload(f)
				return hasProto(msgPower, "")(f) && len(p) >= 1 && p[0] == 12
			})
			board.barrier(t)
			b.ws.mu.Lock()
			asleep, last := d.asleep, d.last
			b.ws.mu.Unlock()
			if !asleep || last != released {
				t.Fatal("trailing gestures woke a sleeping device")
			}
			waitSVG(t, b.ws, "vocat-0003", "companion-ring")
			send(ptrTap, tc.up)
			board.until(t, func(f wsFrame) bool {
				p := framePayload(f)
				return hasProto(msgPower, "")(f) && len(p) >= 1 && p[0] == 85
			})
			waitSVG(t, b.ws, "vocat-0003", "Drag track 150,168 Up track 210,192")
			send(ptrTap, tc.up)
			waitSVG(t, b.ws, "vocat-0003", "Tapped")
		})
	}
}

func TestAppStatePatchAcrossPlayers(t *testing.T) {
	b, base := wsHarness(t, nil)
	const boardID, webID = "cake", "mint"
	if _, err := b.createCompanion(t.Context(), object{"id": webID, "name": "Mint"}); err != nil {
		t.Fatal(err)
	}
	board, web := testConnectWS(t, base, "unit", false), testConnectWS(t, base, "unit", false)
	board.helloV2(t, "vocat-0004", "Cake")
	web.helloV2(t, "vocat-0005", "Mint")
	ids := []string{boardID, webID}
	toolTest(t, base, "app", object{"companions": ids, "state_patch": object{"color": "red"}}, true)

	const code = `
	let taps = 0;
	function init(ctx) { return {...ctx.initial_state, visits:1}; }
	function onTap(s) { s.taps = ++taps; }
	function render(s,p) {
		if (s.color === "bad") throw Error("bad color");
		return '<svg><circle data-id="p'+p.index+'" cx="180" cy="180" r="40" fill="'+s.color+'"/><text x="150" y="100" fill="white">Player '+p.index+' taps '+taps+'</text></svg>';
	}`
	start := toolTest(t, base, "app", object{"companions": ids, "code": code, "title": "Players", "save_as": "players",
		"initial_state": object{"color": "#FF0000", "prefs": object{"a": true}}}, false)
	if start["state"].(object)["visits"] != json.Number("1") {
		t.Fatal(start)
	}
	decodeDisplay(t, board.until(t, isDisplay))
	waitSVG(t, b.ws, "vocat-0005", "Player 1 taps 0")
	web.tap(t, 180, 180, 0)
	waitSVG(t, b.ws, "vocat-0005", "Player 1 taps 1")
	decodeDisplay(t, board.until(t, isDisplay))

	bad := toolTest(t, base, "app", object{"companions": ids, "state_patch": object{"color": "bad"}}, true)
	if !strings.Contains(str(bad["error"]), "bad color") {
		t.Fatal(bad)
	}
	// Targeting either shared player updates both views without a new launch.
	out := toolTest(t, base, "app", object{"companions": webID, "state_patch": object{"color": "#00FF00", "prefs": object{"b": true}}}, false)
	state := out["state"].(object)
	if !slices.Equal(out["companions"].([]any), []any{boardID, webID}) || out["title"] != "Players" ||
		!slices.Contains(out["hit_ids"].([]any), "p0") || !slices.Contains(out["hit_ids"].([]any), "p1") ||
		state["taps"] != json.Number("1") || state["visits"] != json.Number("1") || state["prefs"].(object)["a"] != nil {
		t.Fatal(out)
	}
	_, _, pixels := decodeDisplay(t, board.until(t, isDisplay))
	if !slices.Contains(pixels, uint16(0x07e0)) {
		t.Fatal("board frame did not update to green")
	}
	svg := waitSVG(t, b.ws, "vocat-0005", "#00FF00")
	if !strings.Contains(svg, "Player 1 taps 1") {
		t.Fatal("patch restarted player VM")
	}
	if a := stateMe(t, base, boardID)["active_app"].(object); a["state"].(object)["color"] != "#00FF00" {
		t.Fatal(a)
	}
	for _, args := range []object{
		{"rename": []object{{"from": "players", "to": "new-players"}}},
		{"delete": []string{"players"}},
	} {
		if got := toolTest(t, base, "patch", object{"apps": args}, true); got["error"] != "saved app is running: players" {
			t.Fatal(got)
		}
	}
	if got := toolTest(t, base, "app", object{"delete_saved": "players"}, true); got["error"] != "saved app is running: players" {
		t.Fatal(got)
	}
	toolTest(t, base, "app", object{"companions": webID, "exit": true}, false)
	waitSVG(t, b.ws, "vocat-0005", "companion-ring")
	toolTest(t, base, "app", object{"delete_saved": "players"}, false)
	saved := toolTest(t, base, "state", object{"include": []string{"apps"}}, false)
	if len(saved["apps"].(object)) != 0 {
		t.Fatal(saved)
	}
	if stateMe(t, base, boardID)["active_app"] != nil {
		t.Fatal("exit did not close the shared app")
	}
}

func TestAppAndEmitNamedSFX(t *testing.T) {
	_, base := wsHarness(t, nil)
	const id = "cake"
	dev := testConnectWS(t, base, "unit", false)
	dev.helloV2(t, "vocat-0005", "Cake")
	const code = `
	let index=0;
	function render(){ return '<svg><circle data-id="sound" cx="180" cy="180" r="40"/></svg>'; }
	function onTap(){ const sounds=["boing","pop","win","buzz","ding","whoosh"]; sfx(sounds[index++]); }`
	toolTest(t, base, "app", object{"companions": id, "code": code}, false)
	dev.until(t, isDisplay)
	expectAudio := func(t *testing.T, want []byte) {
		t.Helper()
		var got []byte
		frame := dev.until(t, func(f wsFrame) bool {
			if f.op != wsBinary {
				return false
			}
			mt, _, p, ok := unpackProtoPacket(f.data)
			if !ok {
				return false
			}
			if mt == msgTTSPCM {
				got = append(got, p...)
			}
			return mt == msgTTSCtrl && len(p) >= 9 && p[0] == ttsOpEnd
		})
		if !bytes.Equal(got, want) {
			t.Fatalf("sent %d PCM bytes, want %d matching bytes", len(got), len(want))
		}
		audioID := binary.LittleEndian.Uint64(framePayload(frame)[1:9])
		dev.sendProto(t, msgTTSAck, encodeTTSAck(false, audioID))
	}
	for _, name := range []string{"boing", "pop", "win", "buzz", "ding", "whoosh"} {
		t.Run(name, func(t *testing.T) {
			pcm, err := sfxPCM(name)
			if err != nil || len(pcm) == 0 || len(pcm)%2 != 0 || len(pcm) >= 2*16000*800/1000 {
				t.Fatalf("invalid 16 kHz PCM length: %d (%v)", len(pcm), err)
			}
			var peak int
			for offset := 0; offset < len(pcm); offset += 2 {
				v := int(int16(binary.LittleEndian.Uint16(pcm[offset:])))
				peak = max(peak, v, -v)
			}
			if peak < 1000 {
				t.Fatalf("near-silent PCM: peak %d", peak)
			}
			toolTest(t, base, "emit", object{"to": id, "sfx": name}, false)
			expectAudio(t, pcm)
			dev.tap(t, 180, 180, 0)
			expectAudio(t, pcm)
		})
	}
	toolTest(t, base, "emit", object{"to": id, "chime": "unknown"}, true)
	a, _, err := initApp(savedApp{Code: `function render(){return "<svg/>"} function onUp(){sfxAll("pop"); sfxAll("cancel");}`},
		[]appPlayer{{id: id}}, nil, nil, nil, buildScreen)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.update(appEvent{kind: "up"}, nil); err != nil || len(a.effects) != 2 ||
		a.effects[0].kind != "pcm" || !a.effects[0].all ||
		a.effects[1].kind != "chime" || a.effects[1].text != "cancel" || !a.effects[1].all {
		t.Fatalf("sfxAll did not broadcast a preset and a chime: %v", err)
	}
}

func TestAppSFXChimeAndUnknownSoundKeepGameRunning(t *testing.T) {
	_, base := wsHarness(t, nil)
	const id = "cake"
	board := testConnectWS(t, base, "unit", false)
	board.helloV2(t, "vocat-0006", "Cake")
	const code = `
	function init(){ return {level:1}; }
	function render(s){ return '<svg><rect data-id="next" x="120" y="160" width="120" height="40"/>'+
		'<text x="120" y="120" fill="white">Level '+s.level+'</text></svg>'; }
	function onTap(s,p,id){ if(id==="next"){ sfx(s.level===1 ? "tap" : "nope"); return {level:s.level+1}; } }`
	out := toolTest(t, base, "app", object{"companions": id, "code": code}, false)
	if _, warned := out["layout_warnings"]; warned {
		t.Fatal(out)
	}
	board.until(t, isDisplay)
	tap := func() { board.tap(t, 180, 180, 0) }
	checkLevel := func(want json.Number) {
		t.Helper()
		active, _ := stateMe(t, base, id)["active_app"].(object)
		state, _ := active["state"].(object)
		if state["level"] != want {
			t.Fatalf("wanted open app at level %s, got %v", want, active)
		}
	}
	tap()
	board.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgChime, "")(f) && len(p) >= 1 && p[0] == chimeTap
	})
	board.until(t, isDisplay)
	checkLevel("2")

	tap()
	board.until(t, func(f wsFrame) bool {
		if hasProto(msgChime, "")(f) || isPlayback(f) {
			t.Fatalf("unknown sound should be silent: %q", f.data)
		}
		return isDisplay(f)
	})
	checkLevel("3")
}

func TestAppExitAcceptsBothControlIDs(t *testing.T) {
	_, base := wsHarness(t, nil)
	const id = "cake"
	board := testConnectWS(t, base, "unit", false)
	board.helloV2(t, "vocat-0007", "Cake")
	for _, exit := range []string{exitNode, "lark-exit"} {
		code := `function render(){return '<svg><rect data-id="` + exit + `" x="140" y="290" width="80" height="25"/></svg>'}`
		out := toolTest(t, base, "app", object{"companions": id, "code": code}, false)
		if hits := out["hit_ids"].([]any); !slices.Equal(hits, []any{exit}) {
			t.Fatalf("custom exit should suppress the injected pill: %v", hits)
		}
		board.until(t, isDisplay)
		board.tap(t, 180, 302, 0)
		board.until(t, isDisplay)
		if stateMe(t, base, id)["active_app"] != nil {
			t.Fatal("app remained active after exit tap")
		}
	}
}

func TestAppLaunchReportsLayoutWarnings(t *testing.T) {
	_, base := wsHarness(t, nil)
	const roundID, rectID = "cake", "mint"
	for _, dev := range []struct{ id, name, board string }{{"vocat-0008", "Cake", "esp-vocat"}, {"box3-0008", "Mint", "esp-box-3"}} {
		c := testConnectWS(t, base, "unit", false)
		w, h, sh := uint16(360), uint16(360), shapeRound
		if dev.board == "esp-box-3" {
			w, h, sh = 320, 240, shapeRect
		}
		c.sendProto(t, msgHello, encodeTerminalHello(terminalHello{
			protoVersion:   protoVersion1,
			kind:           termKindHardware,
			displayFormats: dispRGB565,
			frameCodecs:    codecBitAll,
			width:          w,
			height:         h,
			shape:          sh,
			terminalID:     dev.id,
			companionID:    dev.name,
			board:          dev.board,
			firmware:       "0.12.0",
			token:          "unit",
		}))
		c.until(t, isDisplay)
	}
	const svg = `<svg><rect data-id="next" x="150" y="270" width="60" height="25"/>
		<rect data-id="wide" x="80" y="12" width="200" height="24"/></svg>`
	const overlap = `control "next" overlaps bottom Home button (keep controls above y=296)`
	const bezel = `control "wide" clips outside the round display bezel`
	for _, tc := range []struct {
		title   string
		devices any
		want    []any
	}{
		{"rect only", rectID, []any{overlap}},
		{"mixed", []string{rectID, roundID}, []any{bezel, overlap}},
	} {
		t.Run(tc.title, func(t *testing.T) {
			out := toolTest(t, base, "app", object{"companions": tc.devices, "svg": svg}, false)
			if warnings, _ := out["layout_warnings"].([]any); !slices.Equal(warnings, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, out)
			}
		})
	}
}

func TestAppLaunchWarnsOnDataIDWithoutBounds(t *testing.T) {
	_, base := wsHarness(t, nil)
	const id = "mint"
	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, "vocat-0009", "Mint")
	const warning = `control "m0" has data-id but no measurable bounds — add data-hit="x,y,w,h" or put data-id on a rect/circle/text`
	for _, hit := range []string{"", ` data-hit="150,150,60,60"`} {
		code := `function render(){return '<svg><g data-id="m0"` + hit + `><circle cx="180" cy="180" r="30"/></g></svg>';}`
		out := toolTest(t, base, "app", object{"companions": id, "code": code}, false)
		hitIDs := out["hit_ids"].([]any)
		if hit == "" {
			if got, _ := out["layout_warnings"].([]any); !slices.Equal(got, []any{warning}) || slices.Contains(hitIDs, "m0") {
				t.Fatal(out)
			}
		} else if out["layout_warnings"] != nil || !slices.Contains(hitIDs, "m0") {
			t.Fatal(out)
		}
	}
}

func TestAppSaveDoesNotCommitStaleLaunch(t *testing.T) {
	for _, change := range []string{"view changed", "disconnected"} {
		t.Run(change, func(t *testing.T) {
			b, base := wsHarness(t, nil)
			const id, name = "cake", "stale-app"
			board := testConnectWS(t, base, "unit", false)
			board.helloV2(t, "vocat-0010", "Cake")
			b.db.readDB().SetMaxOpenConns(1)
			conn, err := b.db.readDB().Conn(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			before := b.db.readDB().Stats().WaitCount
			done := make(chan error, 1)
			go func() {
				_, err := b.runApp(t.Context(), object{"companions": id, "save_as": name, "code": `function render(){ return "<svg/>"; }`})
				done <- err
			}()
			// Memory loads after the launch captures its device generation.
			waitPool(t, func() bool { return b.db.readDB().Stats().WaitCount > before })
			if change == "disconnected" {
				b.ws.mu.Lock()
				termID := b.ws.voiceOwner(id).id
				b.ws.mu.Unlock()
				b.ws.disconnectTerminal(termID)
			} else {
				b.ws.mu.Lock()
				b.ws.clearView(b.ws.voiceOwner(id))
				b.ws.mu.Unlock()
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if ae, ok := err.(*apiError); !ok || ae.status != 409 || !strings.Contains(ae.text, change) {
					t.Fatalf("launch error = %v, want 409 %s", err, change)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("stale launch did not return")
			}
			if apps, err := b.listApps(t.Context(), false); err != nil || len(apps) != 0 {
				t.Fatalf("stale launch persisted an app: %v (%v)", apps, err)
			}
		})
	}
}

func TestAppStoreEffectsRollBackOnWriteFailure(t *testing.T) {
	b, base := wsHarness(t, nil)
	const id = "cake"
	const code = `function render(){return "<svg/>"} function onTap(){storeSet("k1",true); storeSet("overflow",true);}`
	dev := testConnectWS(t, base, "unit", false)
	dev.helloV2(t, "vocat-0011", "Cake")
	if _, err := b.db.Exec(`WITH RECURSIVE keys(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM keys WHERE n<64)
		INSERT INTO memory (scope,key,value_json,updated_at) SELECT 'app:full', 'k'||n, '0', 0 FROM keys`); err != nil {
		t.Fatal(err)
	}
	toolTest(t, base, "app", object{"companions": id, "code": code, "save_as": "full"}, false)
	dev.until(t, isDisplay)
	dev.tap(t, 180, 180, 0)
	dev.until(t, hasProto(msgStatus, `Could not save app key "overflow"`))
	if a := stateMe(t, base, id)["active_app"]; a != nil {
		t.Fatal("app stayed active after a failed write")
	}
	mem := memoryScope(t, base, "app:full")
	if existing := mem["k1"].(object); existing["value"] != json.Number("0") || existing["version"] != json.Number("1") || mem["overflow"] != nil {
		t.Fatal("failed batch did not roll back the first write", existing, mem["overflow"])
	}
}

const ticTacToe = `
function init(ctx) { if (ctx.players.length!==2) throw new Error("need two players"); return { turn:0, cells:Array(9).fill(-1), winner:-1 }; }
function onTap(s,p,id) {
  if (!/^cell[0-8]$/.test(id) || p.index!==s.turn || s.winner!==-1) return;
  const i=+id.slice(4); if (s.cells[i]!==-1) return;
  s.cells[i]=p.index;
  for (const [a,b,c] of [[0,1,2],[3,4,5],[6,7,8],[0,3,6],[1,4,7],[2,5,8],[0,4,8],[2,4,6]]) {
    if (s.cells[a]===p.index && s.cells[b]===p.index && s.cells[c]===p.index) {
      s.winner=p.index; playAll(["G5:40"]); return s;
    }
  }
  s.turn=1-s.turn;
}
function render(s,p) {
  const label=s.winner===p.index?"You win!":s.winner!==-1?"Other player won":s.turn===p.index?"Your turn!":"Waiting...";
  let svg='<svg viewBox="0 0 360 360"><text x="180" y="85" fill="#fff" text-anchor="middle">'+label+'</text>';
  for (let i=0;i<9;i++) {
    const x=90+(i%3)*60, y=115+Math.floor(i/3)*60;
    const c=["#ff0000","#0000ff"][s.cells[i]] || "#cccccc";
    svg+='<rect data-id="cell'+i+'" x="'+x+'" y="'+y+'" width="52" height="52" rx="6" fill="'+c+'"/>';
  }
  return svg+'</svg>';
}`

func TestTicTacToePersonalizedAndAuthoritative(t *testing.T) {
	a, views, err := initApp(savedApp{Code: ticTacToe}, []appPlayer{{"a", "Cake", 0}, {"b", "Mint", 1}}, nil, nil, nil, buildScreen)
	if err != nil {
		t.Fatal(err)
	}
	requireView := func(index int, text string) {
		t.Helper()
		if !strings.Contains(views[index].source, text) {
			t.Fatal("missing personalized text:", text)
		}
	}
	requireView(0, "Your turn!")
	requireView(1, "Waiting...")
	apply := func(index int, id string) {
		t.Helper()
		views, err = a.update(appEvent{kind: "tap", i: index, id: id, x: 150, y: 150}, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	apply(1, "cell2")
	requireView(0, "Your turn!")
	for i, next := range []string{"cell0", "cell3", "cell1", "cell4", "cell2"} {
		apply(i%2, next)
	}
	requireView(0, "You win!")
	requireView(1, "Other player won")
	if len(a.effects) != 1 || !a.effects[0].all || len(a.effects[0].pcm) == 0 {
		t.Fatal("winner did not play all")
	}
	apply(1, "cell5")
	requireView(0, "You win!")
}

func TestSVGDirtyFrameMatchesBrowserAndExit(t *testing.T) {
	b, base := wsHarness(t, nil)
	const id = "cake"
	board := testConnectWS(t, base, "unit", false)
	first, _, initial := decodeDisplay(t, board.helloV2(t, "box3-0012", "Cake"))
	if first != image.Rect(0, 0, 360, 360) {
		t.Fatal(first)
	}
	const svg = `<svg viewBox="0 0 360 360"><rect width="360" height="360" fill="#122234"/><rect data-id="tile" x="179" y="179" width="2" height="2" fill="#123456"/></svg>`
	run := func(raw string) {
		t.Helper()
		toolTest(t, base, "app", object{"companions": id, "svg": raw}, false)
		svgSrc := waitSVG(t, b.ws, "box3-0012", "tile")
		rect, rev, pix := decodeDisplay(t, board.until(t, isDisplay))
		if rev != 2 && rev != 3 {
			t.Fatal(rev)
		}
		if raw != svg && rect != image.Rect(179, 179, 181, 181) {
			t.Fatalf("dirty bounds %v, rev=%d", rect, rev)
		}
		for y := rect.Min.Y; y < rect.Max.Y; y++ {
			copy(initial[y*360+rect.Min.X:], pix[(y-rect.Min.Y)*rect.Dx():(y-rect.Min.Y+1)*rect.Dx()])
		}
		fb, _, err := renderSVG(svgSrc)
		if err != nil || len(fb) != len(initial) {
			t.Fatalf("rendered SVG failed: %v", err)
		}
	}
	run(svg)
	run(strings.Replace(svg, "#123456", "#654321", 1))
	if out := toolTest(t, base, "app", object{"companions": id, "code": "function render(){ throw Error('wrong value') }"}, true); !strings.Contains(str(out["error"]), "app.js:1") {
		t.Fatal(out)
	}
	snap := stateMe(t, base, id)
	if snap["active_app"] == nil {
		t.Fatal("failed validation changed active app")
	}
}
