package main

import (
	"testing"
)

func TestMCPRejectsCompanionSprite(t *testing.T) {
	_, base := wsHarness(t, nil)
	const id = "new-companion"
	board := testConnectWS(t, base, "unit", false)
	board.helloV2(t, "vocat-0001", "")
	before := str(stateMe(t, base, id)["companion"].(object)["name"])
	rejected := toolTest(t, base, "patch", object{"companion_id": id, "companion": object{
		"sprite": object{"rows": []string{"kk", "k."}},
	}}, true)
	if got := str(rejected["error"]); got != "companion.sprite: unknown argument" {
		t.Fatalf("expected strict MCP rejection; got %q", got)
	}
	if got := str(stateMe(t, base, id)["companion"].(object)["name"]); got != before {
		t.Fatal("invalid patch changed the persisted companion")
	}
}

func TestJSPixelCanvasAPI(t *testing.T) {
	code := `function render(s, p, g) {
		g.clear("#1E1A2B");
		g.rect(g.safe.x, g.safe.y, g.safe.w, 12, "#2A2338", "#705F7C");
		g.text(g.safe.x + Math.floor(g.safe.w/2), g.safe.y + 2, "Hello", {color:"#FFC1A8", align:"center", bold:true});
		g.sprite(g.safe.x + 4, g.safe.y + 20, "apple", null, 1);
		g.button("start", g.safe.x, g.safe.y + 40, 40, 14, "Start");
		g.button("exit", g.safe.x, g.safe.y + g.safe.h - 14, 34, 14, "Back");
	}`
	for _, g := range []screenGeometry{roundScreen, {320, 240, "rect"}} {
		d := &wsTerminal{kind: "hardware", screen: g, client: &wsClient{kind: "hardware"}}
		spec := savedApp{Code: code}
		_, views, err := initApp(spec, []appPlayer{{id: "pixel"}}, nil, []*wsTerminal{d}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		v := views[0]
		if len(v.fb) != g.width*g.height || len(v.hits) != 2 {
			t.Fatalf("%v: fb=%d hits=%v", g, len(v.fb), v.hits)
		}
		start := v.hits[0]
		if hitID(v.hits, (start.x1+start.x2)/2, (start.y1+start.y2)/2) != "start" {
			t.Fatalf("start button hit mismatch: %v", start)
		}
	}
}
