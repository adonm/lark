package main

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var (
	lastMenuSVG = map[*testWS]string{}
	menuTargets = map[*testWS]struct {
		h      *terminalHub
		termID string
	}{}
)

func bindMenu(c *testWS, h *terminalHub, termID string) {
	menuTargets[c] = struct {
		h      *terminalHub
		termID string
	}{h, termID}
}

func hitCenter(svg, id string) (int, int, bool) {
	if id == "menu" && svg == "" {
		return 180, 316, true
	}
	re := regexp.MustCompile(`data-id="` + regexp.QuoteMeta(id) + `"[^>]*data-lark-hit="(\d+) (\d+) (\d+) (\d+)"`)
	m := re.FindStringSubmatch(svg)
	if m == nil {
		re2 := regexp.MustCompile(`data-lark-hit="(\d+) (\d+) (\d+) (\d+)"[^>]*data-id="` + regexp.QuoteMeta(id) + `"`)
		m = re2.FindStringSubmatch(svg)
	}
	if m == nil {
		if id == "menu" {
			return 180, 316, true
		}
		return 0, 0, false
	}
	x1, _ := strconv.Atoi(m[1])
	y1, _ := strconv.Atoi(m[2])
	x2, _ := strconv.Atoi(m[3])
	y2, _ := strconv.Atoi(m[4])
	return (x1 + x2) / 2, (y1 + y2) / 2, true
}

func menuFrame(t *testing.T, c *testWS, want ...string) string {
	t.Helper()
	tgt := menuTargets[c]
	svg := waitSVG(t, tgt.h, tgt.termID, want[0])
	lastMenuSVG[c] = svg
	for _, text := range want {
		if !strings.Contains(svg, text) {
			t.Errorf("missing %q on screen", text)
		}
	}
	return svg
}

func tapMenu(t *testing.T, c *testWS, id string, want ...string) string {
	t.Helper()
	if id == "menu" {
		c.tapMenu(t)
		return menuFrame(t, c, want...)
	}
	x, y, ok := hitCenter(lastMenuSVG[c], id)
	if !ok {
		t.Fatalf("control %q not found in SVG", id)
	}
	c.tap(t, x, y, 0)
	return menuFrame(t, c, want...)
}

func TestTerminalMainMenuPages(t *testing.T) {
	b, base := wsHarness(t, nil)
	_, _ = b.createCompanion(t.Context(), object{"id": "cake", "name": "Cake", "form": "a cat"})
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, "vocat-0001", "cake")
	bindMenu(c, b.hub, "vocat-0001")
	page1 := tapMenu(t, c, "menu", "menu-intercom", "Talk to…", "menu-stories", "menu-memories", "menu-next")
	page2 := tapMenu(t, c, "menu-next", "menu-voice", "menu-more", "menu-prev")
	for _, check := range []struct {
		svg  string
		omit []string
	}{
		{page1, []string{"menu-prev", "menu-voice", "menu-more", "menu-games"}},
		{page2, []string{"menu-next", "menu-intercom", "menu-stories", "menu-memories", "menu-games"}},
	} {
		for _, id := range check.omit {
			if strings.Contains(check.svg, id) {
				t.Errorf("unexpected %s on page", id)
			}
		}
	}
	tapMenu(t, c, "menu-prev", "menu-intercom", "menu-next")
	tapMenu(t, c, "menu-back", `data-id="listen"`)
}

func TestTerminalMenuListDetails(t *testing.T) {
	const id = "cake"
	const body = "A long note starts here and finishes with a tiny blue kite."
	for _, tc := range []struct {
		drawer, empty, first, label string
		seed                        func(*testing.T, *larkServer)
	}{
		{drawerMemories, "No memories yet", "Memory 0", "memory-3", func(t *testing.T, b *larkServer) {
			for i := range 5 {
				value := fmt.Sprintf("Memory %d", i)
				if i == 3 {
					value = body
				}
				if _, err := b.db.setMemory(t.Context(), memoryWrite{"companion:" + id, fmt.Sprintf("memory-%d", i), strconv.Quote(value)}, nil); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := b.db.setMemory(t.Context(), memoryWrite{"companion:other", "private", `"Not this cat's memory"`}, nil); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.drawer, func(t *testing.T) {
			b, base := wsHarness(t, nil)
			_, _ = b.createCompanion(t.Context(), object{"id": id, "name": "Cake", "form": "a cat"})
			c := testConnectWS(t, base, "unit", false)
			c.hello(t, "vocat-0001", id)
			bindMenu(c, b.hub, "vocat-0001")
			tapMenu(t, c, "menu", "menu-memories")
			open := "menu-" + tc.drawer
			tapMenu(t, c, open, tc.empty, "menu-back")
			tapMenu(t, c, "menu-back", open)
			tc.seed(t, b)
			prefix := "history-"
			if tc.drawer == drawerMemories {
				prefix = "memory-"
			}
			first := tapMenu(t, c, open, tc.first, prefix+"0", prefix+"1", "menu-next")
			if strings.Contains(first, "Not this cat") {
				t.Fatal("drawer leaked another cat's data")
			}
			list := tapMenu(t, c, "menu-next", "menu-prev", prefix+"2", prefix+"3")
			detail := tapMenu(t, c, prefix+"3", "menu-detail-view")
			if strings.Contains(detail, prefix) {
				t.Fatal("detail still has list hitboxes")
			}
			_ = list
			tapMenu(t, c, "menu-back", prefix+"2", "menu-prev")
			return
		})
	}
}

func TestTerminalMenuSQLLeavesHubUnlocked(t *testing.T) {
	b, base := wsHarness(t, nil)
	if _, err := b.createCompanion(t.Context(), object{"id": "mint", "name": "Mint", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	board := testConnectWS(t, base, "unit", false)
	board.hello(t, "vocat-0001", "mint")
	web := testConnectWS(t, base, "unit", false)
	web.hello(t, "vocat-0002", "Cake")
	bindMenu(web, b.hub, "vocat-0002")
	tapMenu(t, web, "menu", "menu-stories")
	if _, _, err := b.saveStory(t.Context(), "slow", ":: StoryTitle\nSlow Story\n\n:: Start {\"end\":true}\nOnce upon a time.\n", "", true); err != nil {
		t.Fatal(err)
	}

	b.db.readDB().SetMaxOpenConns(1)
	conn, err := b.db.readDB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	wait := b.db.readDB().Stats().WaitCount
	x, y, ok := hitCenter(lastMenuSVG[web], "menu-stories")
	if !ok {
		t.Fatalf("control %q not found", "menu-stories")
	}
	web.tap(t, x, y, 0)
	waitPool(t, func() bool { return b.db.readDB().Stats().WaitCount > wait })
	var st [16]byte
	board.sendProto(t, msgTelemetry, st[:])
	board.barrier(t) // The device event must be handled before the connection is returned.
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	b.db.readDB().SetMaxOpenConns(8)
	menuFrame(t, web, "Slow Story")
}

func TestHomeTapStartsRecordingAndMenuButtonOpensMenu(t *testing.T) {
	for _, tc := range []struct {
		name, termID, board string
		spec                pixelDisplaySpec
		shape               uint8
		w, h                uint16
		tapX, tapY          int
	}{
		{"vocat", "vocat-tap1", "esp-vocat", pixelSpecVoCat, shapeRound, 360, 360, 180, 180},
		{"box3", "box3-tap1", "esp-box-3", pixelSpecBox3, shapeRect, 320, 240, 160, 120},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, base := wsHarness(t, nil)
			_, _ = b.createCompanion(t.Context(), object{"id": "cake", "name": "Cake", "form": "a cat"})
			dev := testConnectWS(t, base, "unit", false)
			dev.sendProto(t, msgHello, encodeTerminalHello(terminalHello{
				protoVersion: protoVersion1, kind: termKindHardware, capsMask: capDrag | capBridgeEndpoint | capPointerRev,
				displayFormats: dispRGB565, frameCodecs: codecBitAll,
				width: tc.w, height: tc.h, shape: tc.shape, terminalID: tc.termID, board: tc.board,
				firmware: "0.16.0", token: "unit", companionID: "cake",
			}))
			dev.until(t, hasProto(msgWelcome, ""))
			first := dev.until(t, isDisplay)
			_, rev0, _ := decodeDisplay(t, first)

			// Tap anywhere outside Menu on Home starts recording.
			dev.tap(t, tc.tapX, tc.tapY, rev0)
			dev.until(t, func(f protocolFrame) bool {
				p := framePayload(f)
				return hasProto(msgRecCtrl, "")(f) && len(p) >= 2 && p[0] == recOpStart && p[1]&recTargetListen == 0
			})
			dev.until(t, isDisplay)
			b.hub.mu.Lock()
			d := b.hub.terminals[tc.termID]
			rec := d.recording.active()
			drawer := d.drawer
			b.hub.mu.Unlock()
			if !rec || drawer != "" {
				t.Fatalf("home tap: recording=%v drawer=%q, want true and empty", rec, drawer)
			}

			// Cancel recording to return to Home.
			b.hub.mu.Lock()
			b.hub.recordingControl(d, time.Now(), true)
			b.hub.mu.Unlock()
			dev.until(t, func(f protocolFrame) bool {
				p := framePayload(f)
				return hasProto(msgRecCtrl, "")(f) && len(p) >= 1 && p[0] == recOpCancel
			})
			fIdle := dev.until(t, isDisplay)
			_, revIdle, _ := decodeDisplay(t, fIdle)

			// Tap Menu button on Home opens Menu without starting recording.
			btn := physicalRect(pixelHomeMenuButtonRect(tc.spec), tc.spec.artScale)
			if btn.Dx() < 48 || btn.Dy() < 48 {
				t.Fatalf("menu button %v smaller than 48px physical", btn)
			}
			dev.tap(t, (btn.Min.X+btn.Max.X)/2, (btn.Min.Y+btn.Max.Y)/2, revIdle)
			waitSVG(t, b.hub, tc.termID, "menu-intercom")
			b.hub.mu.Lock()
			rec = d.recording.active()
			drawer = d.drawer
			b.hub.mu.Unlock()
			if rec || drawer != drawerMenu {
				t.Fatalf("menu tap: recording=%v drawer=%q, want false and %q", rec, drawer, drawerMenu)
			}
		})
	}
}

func TestIntercomMenuPagingRoundScreen(t *testing.T) {
	b, base := wsHarness(t, nil)
	dev := testConnectWS(t, base, "unit", false)
	dev.helloV2(t, "vocat-intercom-page", "Cake")
	p1 := testConnectWS(t, base, "unit", false)
	p1.helloV2(t, "vocat-peer1", "Mint")
	p2 := testConnectWS(t, base, "unit", false)
	p2.helloV2(t, "vocat-peer2", "George")

	b.hub.mu.Lock()
	d := b.hub.terminals["vocat-intercom-page"]
	d.drawer = drawerIntercom
	d.menuPage = 1
	b.hub.render(d, time.Now(), false)
	b.hub.mu.Unlock()

	svg := waitSVG(t, b.hub, "vocat-intercom-page", "peer-mint")
	if !strings.Contains(svg, "peer-mint") {
		t.Fatalf("expected peer-mint on page 1 of round intercom menu, got %s", svg)
	}
	b.hub.mu.Lock()
	page := d.menuPage
	b.hub.mu.Unlock()
	if page != 1 {
		t.Fatalf("d.menuPage = %d, want 1", page)
	}
}

func TestOpenStoryEntersStartPassage(t *testing.T) {
	b, base := wsHarness(t, nil)
	dev := testConnectWS(t, base, "unit", false)
	dev.helloV2(t, "vocat-story-open", "Cake")

	twee := ":: StoryTitle\nTest Owl\n\n:: Start\nOnce upon a time.\n[[Fly->End]]\n\n:: End {\"end\":true}\nThe end.\n"
	b.hub.mu.Lock()
	d := b.hub.terminals["vocat-story-open"]
	b.hub.openStory(d, savedStory{ID: "test-owl", Title: "Test Owl", Twee: twee}, false)
	sess := d.story
	b.hub.mu.Unlock()

	if sess == nil || sess.text != "Once upon a time." || len(sess.choices) != 1 {
		t.Fatalf("openStory did not enter start passage: %+v", sess)
	}
}

func storyTTSHits(t *testing.T, twee string, narrate bool) (*atomic.Int32, *terminalSession, *larkServer, *synthWorker) {
	t.Helper()
	b, base := wsHarness(t, nil)
	requests := new(atomic.Int32)
	b.worker = fakeTTS(b.root, requests)
	const termID = "vocat-story-voice"
	dev := testConnectWS(t, base, "unit", false)
	dev.helloV2(t, termID, "Cake")
	b.hub.mu.Lock()
	d := b.hub.terminals[termID]
	b.hub.openStory(d, savedStory{ID: "test-owl", Title: "Test Owl", Twee: twee}, narrate)
	b.hub.mu.Unlock()
	return requests, d, b, b.worker
}

func TestOpenStoryFromMenuIsSilent(t *testing.T) {
	twee := ":: StoryTitle\nTest Owl\n\n:: Start\nOnce upon a time.\n[[Fly->End]]\n\n:: End {\"end\":true}\nThe end.\n"
	requests, d, b, worker := storyTTSHits(t, twee, false)
	if n := requests.Load(); n != 0 {
		t.Fatalf("opening a story from the menu synthesized %d TTS clips, want silence", n)
	}
	b.hub.mu.Lock()
	target := d.story.choices[0].Target
	b.hub.advanceStoryPassage(d, target, false)
	b.hub.mu.Unlock()
	if n := requests.Load(); n != 0 {
		t.Fatalf("tapping a story choice synthesized %d TTS clips, want silence", n)
	}
	b.hub.mu.Lock()
	b.hub.advanceStoryPassage(d, target, true)
	b.hub.mu.Unlock()
	waitTTSRequest(t, worker)
	if requests.Load() == 0 {
		t.Fatal("a voice story choice did not narrate the next passage")
	}
}

func TestConversationalStoryOpenStillNarrates(t *testing.T) {
	twee := ":: StoryTitle\nTest Owl\n\n:: Start\nOnce upon a time.\n[[Fly->End]]\n\n:: End {\"end\":true}\nThe end.\n"
	requests, _, _, worker := storyTTSHits(t, twee, true)
	waitTTSRequest(t, worker)
	if requests.Load() == 0 {
		t.Fatal("a conversational story open did not narrate the passage")
	}
}

func TestVoiceMenuTapSelectsAndPreviewsCachedSample(t *testing.T) {
	b, base := wsHarness(t, func(b *larkServer, _ string) {
		b.voices = companionVoiceStore(b.db, heartVoice)
	})
	samplePCM := bytes.Repeat([]byte{0x12, 0x34}, 1600) // 100ms 16kHz mono PCM
	_, _ = b.clips.getWith(t.Context(), func(_ context.Context, _, _ string) ([]byte, error) {
		return samplePCM, nil
	}, "bm_fable", voicePreviewText("bm_fable"))

	const termID = "vocat-voice-prev"
	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, termID, "Cake")
	dev.tapMenu(t)
	waitSVG(t, b.hub, termID, "menu-next")
	dev.tap(t, 248, 294, 0)
	p2svg := waitSVG(t, b.hub, termID, "menu-voice")
	vx, vy, ok := hitCenter(p2svg, "menu-voice")
	if !ok {
		t.Fatal("missing menu-voice hit")
	}
	dev.tap(t, vx, vy, 0)
	waitSVG(t, b.hub, termID, "voice-bm_fable")

	b.hub.mu.Lock()
	var x, y int
	for _, h := range b.hub.terminals[termID].svg.hits {
		if h.id == "voice-bm_fable" {
			x, y = (h.x1+h.x2)/2, (h.y1+h.y2)/2
		}
	}
	b.hub.mu.Unlock()
	if x == 0 && y == 0 {
		t.Fatal("voice-bm_fable hit not found")
	}

	dev.tap(t, x, y, 0)
	dev.until(t, func(f protocolFrame) bool {
		if len(f.data) > 0 {
			p := framePayload(f)
			if hasProto(msgTTSCtrl, "")(f) && len(p) >= 1 && p[0] == ttsOpStart {
				return true
			}
		}
		return false
	})
	var gotPCM []byte
	dev.until(t, func(f protocolFrame) bool {
		if isPlayback(f) {
			gotPCM = append(gotPCM, framePayload(f)...)
			return len(gotPCM) >= len(samplePCM)
		}
		return false
	})
	if !bytes.Equal(gotPCM[:len(samplePCM)], samplePCM) {
		t.Fatalf("preview PCM mismatch: got %d bytes, want %d", len(gotPCM), len(samplePCM))
	}
	if got := b.voiceFor("cake"); got != "bm_fable" {
		t.Fatalf("voiceFor(cake) = %q, want bm_fable", got)
	}
	msgs, err := b.history.list(t.Context(), "cake")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("voice preview should not record messages, got %d", len(msgs))
	}
}
