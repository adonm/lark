package main

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

func menuFrame(t *testing.T, c *testWS, want ...string) string {
	t.Helper()
	frame := c.until(t, hasWS("svg", want[0]))
	svg := str(testObject(t, frame.data)["svg"])
	for _, text := range want {
		if !strings.Contains(svg, text) {
			t.Errorf("missing %q on screen", text)
		}
	}
	return svg
}

func tapMenu(t *testing.T, c *testWS, id string, want ...string) string {
	t.Helper()
	c.json(t, object{"ev": "tap", "id": id})
	return menuFrame(t, c, want...)
}

func TestWSMainMenuPages(t *testing.T) {
	b, base := wsHarness(t, nil)
	c := callClient(t, b, base, callCake, "Cake")
	page1 := tapMenu(t, c, "menu", "menu-intercom", "Talk to…", "menu-call", "menu-games", "menu-history", "menu-next")
	page2 := tapMenu(t, c, "menu-next", "menu-prev", "menu-memories", "menu-voice", "menu-volume", "menu-stats")
	for _, check := range []struct {
		svg  string
		omit []string
	}{
		{page1, []string{"menu-prev", "menu-memories", "menu-voice", "menu-volume", "menu-stats"}},
		{page2, []string{"menu-next", "menu-call", "menu-intercom", "menu-games", "menu-history"}},
	} {
		for _, id := range check.omit {
			if strings.Contains(check.svg, id) {
				t.Errorf("unexpected %s on page", id)
			}
		}
	}
	tapMenu(t, c, "menu-prev", "menu-call", "menu-next")
	tapMenu(t, c, "menu-back", "companion-ring")
}

func TestWSGamesMenuLaunchesOnBoard(t *testing.T) {
	b, base := wsHarness(t, nil)
	const id = "lark-games"
	board := testConnectWS(t, base, "unit", false)
	board.hello(t, id, "Cake")
	web := testConnectWS(t, base, "unit", true)
	web.watch(t, id)
	tapMenu(t, web, "menu", "menu-games")
	tapMenu(t, web, "menu-games", "No saved games yet", "menu-back")
	tapMenu(t, web, "menu-back", "menu-games")

	save := func(i int) string {
		t.Helper()
		name := fmt.Sprintf("game-%d", i)
		err := b.saveApp(t.Context(), savedApp{Name: name, Title: fmt.Sprintf("Arcade %d", i), Kind: "app", Code: `
			function init() { return "Ready to play"; }
			function render(state) {
				return '<svg><text x="180" y="180" text-anchor="middle" fill="#ffffff">' + state + '</text></svg>';
			}
			function onTap() { return "Playing on board"; }`})
		if err != nil {
			t.Fatal(err)
		}
		return name
	}
	for i := range 5 {
		save(i)
	}
	tapMenu(t, web, "menu-games", "Arcade 0", "Arcade 1", "Arcade 2", "Arcade 3", "menu-next")
	name := save(5) // Paging refreshes the library.
	tapMenu(t, web, "menu-next", "Arcade 4", "Arcade 5", "menu-prev")
	if deleted, err := b.deleteSavedApp(t.Context(), name); !deleted || err != nil {
		t.Fatal("delete saved game:", err)
	}
	tapMenu(t, web, "game-5", "Game unavailable", "menu-prev")
	save(5)
	tapMenu(t, web, "menu-prev", "Arcade 0", "menu-next")
	tapMenu(t, web, "menu-next", "Arcade 5", "menu-prev")

	// A native coordinate, not a web control ID, launches the game.
	board.json(t, object{"ev": "tap", "x": 246, "y": 125})
	ready := web.until(t, hasWS("svg", "Ready to play"))
	wantRev, err := strconv.Atoi(fmt.Sprint(testObject(t, ready.data)["rev"]))
	if err != nil {
		t.Fatal(err)
	}
	board.until(t, func(f wsFrame) bool {
		if !isDisplay(f) {
			return false
		}
		_, rev, _ := decodeDisplay(t, f)
		return rev == uint16(wantRev)
	})
	board.json(t, object{"ev": "tap", "x": 180, "y": 180})
	menuFrame(t, web, "Playing on board", exitNode)
}

func TestWSMenuListDetails(t *testing.T) {
	const id = "phone-20af"
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
				if _, err := b.db.setMemory(t.Context(), memoryWrite{id, fmt.Sprintf("memory-%d", i), strconv.Quote(value)}, nil); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := b.db.setMemory(t.Context(), memoryWrite{"phone-20ff", "private", `"Not this cat's memory"`}, nil); err != nil {
				t.Fatal(err)
			}
		}},
		{drawerHistory, "No messages yet", "Message 21", "From Mint", func(t *testing.T, b *larkServer) {
			for i := range 22 {
				e := newChat("user", "", fmt.Sprintf("Message %02d", i), "typed")
				e.Timestamp = time.Unix(int64(i), 0)
				switch i {
				case 18:
					e.Role, e.Sender, e.Text = "intercom", "Mint", body
				case 20:
					e.Role, e.Sender = "companion", "Cake"
				}
				b.history.add(id, e)
			}
			b.history.add("phone-20ff", newChat("user", "", "Not this cat's message", "typed"))
		}},
	} {
		t.Run(tc.drawer, func(t *testing.T) {
			b, base := wsHarness(t, nil)
			c := callClient(t, b, base, id, "Cake")
			tapMenu(t, c, "menu", "menu-history")
			if tc.drawer == drawerMemories {
				tapMenu(t, c, "menu-next", "menu-memories")
			}
			open := "menu-" + tc.drawer
			tapMenu(t, c, open, tc.empty, "menu-back")
			tapMenu(t, c, "menu-back", open)
			tc.seed(t, b)
			first := tapMenu(t, c, open, tc.first, "menu-row-0", "menu-row-1", "menu-row-2", "menu-next")
			if strings.Contains(first, "Not this cat") {
				t.Fatal("drawer leaked another cat's data")
			}
			if tc.drawer == drawerHistory {
				if !strings.Contains(first, `aria-label="You"`) || !strings.Contains(first, `aria-label="Cake"`) {
					t.Fatal("history does not distinguish the user and cat")
				}
				if a, b, c := strings.Index(first, "Message 21"), strings.Index(first, "Message 20"), strings.Index(first, "Message 19"); !(a >= 0 && a < b && b < c) {
					t.Fatal("history isn't newest first")
				}
			}
			list := tapMenu(t, c, "menu-next", "menu-row-3", tc.label, "A long note starts here and", "menu-prev")
			if strings.Contains(list, "blue kite") {
				t.Fatal("long row preview wasn't clipped")
			}
			detail := tapMenu(t, c, "menu-row-3", "blue kite.", tc.label, "menu-back", `data-id="companion"`)
			if strings.Contains(detail, "menu-row-") {
				t.Fatal("detail still has list hitboxes")
			}
			tapMenu(t, c, "menu-back", "menu-row-3", "menu-prev")
			if tc.drawer == drawerHistory {
				for start := 6; start < 20; start += 3 {
					tapMenu(t, c, "menu-next", fmt.Sprintf("menu-row-%d", start))
				}
				tail := tapMenu(t, c, "menu-row-19", "Message 02", "You", "menu-back")
				if strings.Contains(tail, "Message 01") || strings.Contains(tail, "menu-next") {
					t.Fatal("history exceeds the latest 20 messages")
				}
			} else {
				tapMenu(t, c, "menu-row-3", "blue kite.")
			}
			tapMenu(t, c, "companion", "companion-ring")
		})
	}
}

func TestWSMenuSQLLeavesHubUnlocked(t *testing.T) {
	b, base := wsHarness(t, nil)
	const id = "lark-game-sql"
	board := testConnectWS(t, base, "unit", false)
	board.hello(t, id, "Cake")
	web := testConnectWS(t, base, "unit", true)
	web.watch(t, id)
	tapMenu(t, web, "menu", "menu-games")
	if err := b.saveApp(t.Context(), savedApp{Name: "slow", Title: "Slow Game", Kind: "drawing",
		SVG: `<svg><rect width="360" height="360" fill="red"/></svg>`}); err != nil {
		t.Fatal(err)
	}

	for _, control := range []string{"menu-games", "game-0"} {
		b.db.readDB().SetMaxOpenConns(1)
		conn, err := b.db.readDB().Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		wait := b.db.readDB().Stats().WaitCount
		web.json(t, object{"ev": "tap", "id": control})
		waitPool(t, func() bool { return b.db.readDB().Stats().WaitCount > wait })
		if control == "game-0" {
			board.json(t, object{"ev": "tap", "x": 180, "y": 316}) // Leave Games while the app is still loading.
		} else {
			board.json(t, object{"ev": "status", "battery_pct": 80})
		}
		board.barrier(t) // The device event must be handled before the connection is returned.
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		b.db.readDB().SetMaxOpenConns(8)
		if control == "menu-games" {
			menuFrame(t, web, "Slow Game")
		} else {
			menuFrame(t, web, "menu-games")
			web.barrier(t)
			b.ws.mu.Lock()
			running := b.ws.devices[id].app != nil
			b.ws.mu.Unlock()
			if running {
				t.Fatal("leaving Games didn't cancel the stale launch")
			}
		}
	}
}
