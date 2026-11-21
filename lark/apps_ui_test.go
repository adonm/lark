package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode"
)

func uiTestApp(t *testing.T, code string) (*appInstance, svgScreen) {
	t.Helper()
	a, views, err := initApp(savedApp{Code: code}, []appPlayer{{id: "reader"}}, nil, nil, nil, buildScreen)
	if err != nil {
		t.Fatal(err)
	}
	return a, views[0]
}

func checkUIScreens(t *testing.T, raw string, want []string) {
	t.Helper()
	for _, geom := range []screenGeometry{roundScreen, {320, 240, "rect"}} {
		view, err := buildScreen(raw, true, geom)
		if err != nil {
			t.Fatal(err)
		}
		if ids := appHitIDs(view); !slices.Equal(ids, want) {
			t.Fatalf("%v: want hits %v, got %v", geom, want, ids)
		}
		if warnings := layoutWarnings(view.hits, geom.shape == "round"); len(warnings) != 0 {
			t.Fatalf("%v: %v", geom, warnings)
		}
		for i, h := range view.hits {
			for _, x := range []int{h.x1, h.x2} {
				for _, y := range []int{h.y1, h.y2} {
					if (x-180)*(x-180)+(y-180)*(y-180) >= 180*180 {
						t.Errorf("%s clips at padded corner (%d,%d)", h.id, x, y)
					}
				}
			}
			if h.id != exitNode && h.y2 >= 290 {
				t.Errorf("%s crowds Home: %+v", h.id, h)
			}
			for _, other := range view.hits[:i] {
				if h.x1 <= other.x2 && other.x1 <= h.x2 && h.y1 <= other.y2 && other.y1 <= h.y2 {
					t.Errorf("overlapping padded hits: %s and %s", h.id, other.id)
				}
			}
			side := min(geom.width, geom.height)
			x := (geom.width-side)/2 + (h.x1+h.x2)*side/(2*screenSize)
			y := (geom.height-side)/2 + (h.y1+h.y2)*side/(2*screenSize)
			lx, ly := geom.inverseTap(x, y)
			if id := hitID(view.hits, lx, ly); id != h.id {
				t.Errorf("%v: center of %s hit %q", geom, h.id, id)
			}
		}
	}
}

func TestAppUIWrapAndEscape(t *testing.T) {
	_, view := uiTestApp(t, `
	const text = "&<>\"'";
	const thin = ui.wrap("iiiiiiii", 40, 16);
	const wide = ui.wrap("WWWWWWWW", 40, 16, true);
	const breaks = ui.wrap("first\n\nlast", 90);
	if (!Array.isArray(thin) || thin.length !== 1 || wide.length <= 1 ||
		wide.join("") !== "WWWWWWWW" || JSON.stringify(breaks) !== '["first","","last"]' ||
		ui.wrap("é猫", 1).join("|") !== "é|猫" || ui.wrap("one two", 58, 16)[0] !== "one two") {
		throw new Error("bad wrapping");
	}
	function render() { return '<svg><text x="90" y="180" aria-label="' + ui.esc(text) + '">' + ui.esc(text) + '</text></svg>'; }`)
	raw, err := parseSVG(view.raw)
	if err != nil {
		t.Fatal(err)
	}
	n := raw.children[0]
	if n.text != `&<>"'` || n.attr["aria-label"] != n.text {
		t.Fatalf("unexpected escaped SVG: %s", view.raw)
	}
}

func TestAppUIPagedTextPreservesContent(t *testing.T) {
	const title = `The & <Island> "Story"'`
	const opening = `A & <B> "C" 'D'.`
	text := opening + "\n\n" + strings.Repeat("Wé", 120) + "\n\n" + strings.Repeat("More words for the next page. ", 12)
	a, view := uiTestApp(t, fmt.Sprintf(`app = ui.pagedText({title:%q,text:%q,theme:"night"});`, title, text))
	var body strings.Builder
	for page := 0; ; page++ {
		if page > 30 {
			t.Fatal("reader never reached the end")
		}
		root, err := parseSVG(view.raw)
		if err != nil {
			t.Fatal(err)
		}
		var titleFound bool
		var baselines []string
		for _, node := range root.children {
			if node.tag == "text" {
				titleFound = titleFound || node.text == title
				if node.attr["text-anchor"] == "start" {
					body.WriteString(node.text)
					baselines = append(baselines, node.attr["y"])
					if b, _ := outlineText(node, svgTextStyle{"15", "", "", "start"}); b.x2 > 300 {
						t.Errorf("body line is too wide on page %d: %v", page+1, b)
					}
				}
			}
		}
		if !titleFound || page == 0 && (len(baselines) < 2 || baselines[0] != "143" || baselines[1] != "183") {
			t.Fatalf("missing title or blank paragraph line on page %d: %v", page+1, baselines)
		}
		ids := []string{exitNode}
		hasNext := hitID(view.hits, 236, 62) == "next"
		if hasNext {
			ids = append(ids, "next")
		}
		if page > 0 {
			ids = append(ids, "prev")
		}
		checkUIScreens(t, view.raw, ids)
		if !hasNext {
			if page < 2 {
				t.Fatal("long story was not paginated")
			}
			break
		}
		views, err := a.update(appEvent{kind: "tap", id: "next"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		view = views[0]
	}
	withoutSpace := func(s string) string {
		return strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return r
		}, s)
	}
	if withoutSpace(body.String()) != withoutSpace(text) {
		t.Fatalf("story text changed:\n%s", body.String())
	}
}

func TestAppUIPagedTextRawTaps(t *testing.T) {
	_, base := wsHarness(t, nil)
	const id = "lark-reader"
	board := testConnectWS(t, base, "unit", false)
	board.helloV2(t, id, "Reader")
	const code = `app = ui.pagedText({title:"Story",pages:["First",{title:"Second & <Last>",text:"Last"}],page:-3,fontSize:99});
		chime = () => { throw new Error("broken user sound"); };`
	out := toolTest(t, base, "app", object{"devices": id, "code": code}, false)
	if _, warned := out["layout_warnings"]; warned || !slices.Equal(out["hit_ids"].([]any), []any{exitNode, "next"}) {
		t.Fatal(out)
	}
	board.until(t, isDisplay)
	checkPage := func(want string) {
		t.Helper()
		active, _ := stateMe(t, base, id)["active_app"].(object)
		state, _ := active["state"].(object)
		if state["page"] != json.Number(want) {
			t.Fatalf("wanted page %s, got %v", want, active)
		}
	}
	tap := func(x, y int, page string) {
		t.Helper()
		board.json(t, object{"ev": "tap", "x": x, "y": y})
		board.until(t, hasWS("chime", `"kind":"tap"`))
		board.until(t, isDisplay)
		checkPage(page)
	}
	checkPage("0")
	for _, xy := range [][2]int{{236, 62}, {195, 62}, {277, 35}} {
		tap(xy[0], xy[1], "1") // Center, inner padded edge, outer padded corner.
		tap(124, 62, "0")
	}
	board.json(t, object{"ev": "tap", "x": 180, "y": 320})
	board.until(t, isDisplay)
	if active := stateMe(t, base, id)["active_app"]; active != nil {
		t.Fatalf("Home did not exit the reader: %v", active)
	}

	_, last := uiTestApp(t, `app = ui.pagedText({title:"Volume",pages:["First",{title:"Last & <end>",text:"Final"}],page:999,fontSize:99})`)
	if !strings.Contains(last.raw, `>2/2</text>`) || !strings.Contains(last.raw, `>Last &amp; &lt;end&gt;</text>`) ||
		!strings.Contains(last.raw, `font-size="18" font-weight="normal">Final</text>`) {
		t.Fatalf("explicit page options not applied: %s", last.raw)
	}
	checkUIScreens(t, last.raw, []string{exitNode, "prev"})
}

func TestAppUIChoices(t *testing.T) {
	for n := range 5 {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			_, view := uiTestApp(t, fmt.Sprintf(`function render() {
				return ui.choices({title:"Pick & <one>", prompt:"A & <B>?\nWhich is correct?", badge:"\"Round\"",
					footer:"It's OK", theme:"mint", choices:Array.from({length:%d}, (_,i) => ({
						id:"pick-"+i, label:"&<>\"' "+ "W".repeat(300), sub:"<sub>&", accent:"#123456"
					}))});
			}`, n))
			ids := []string{exitNode}
			for i := n - 1; i >= 0; i-- {
				ids = append(ids, fmt.Sprintf("pick-%d", i))
			}
			checkUIScreens(t, view.raw, ids)
			for _, escaped := range []string{`Pick &amp; &lt;one&gt;`, `A &amp; &lt;B&gt;?`, `Which is correct?`, `&#34;Round&#34;`, `It&#39;s OK`} {
				if !strings.Contains(view.raw, escaped) {
					t.Errorf("missing escaped %q in %s", escaped, view.raw)
				}
			}
			if n > 0 {
				for _, escaped := range []string{`&amp;&lt;&gt;&#34;&#39;`, `&lt;sub&gt;&amp;`, `stroke="#123456"`, "…</text>"} {
					if !strings.Contains(view.raw, escaped) {
						t.Errorf("missing fitted/escaped choice %q", escaped)
					}
				}
			}
		})
	}
}
