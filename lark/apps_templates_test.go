package main

import (
	"fmt"
	"image"
	"testing"
)

func TestAppTemplatesPixelHitCoordinates(t *testing.T) {
	for _, geometry := range []screenGeometry{roundScreen, {320, 240, "rect"}} {
		spec := pixelSpecForGeometry(geometry)
		safePhys := physicalRect(spec.safe, spec.artScale)
		for _, tc := range []struct {
			template, data, id string
		}{
			{"quiz", `{}`, "answer-1"},
			{"spot-the-difference", `{}`, "t-2"},
			{"drawing", `{}`, "c-0"},
			{"pet", `{"name":"Sparky"}`, "feed"},
			{"timer", `{"seconds":10}`, "toggle"},
			{"picture-math", `{"problems":[{"left":2,"right":3}]}`, "answer-0"},
			{"memory-match", `{}`, "m-0"},
			{"fireworks", `{}`, "sky"},
		} {
			t.Run(fmt.Sprintf("%dx%d/%s", geometry.width, geometry.height, tc.template), func(t *testing.T) {
				data, err := decodeObject([]byte(tc.data))
				if err != nil {
					t.Fatal(err)
				}
				var appSpec savedApp
				if err := appSpec.setTemplate(tc.template, data); err != nil {
					t.Fatal(err)
				}
				d := &wsTerminal{kind: "hardware", screen: geometry, client: &wsClient{kind: "hardware"}}
				a, views, err := initApp(appSpec, []appPlayer{{id: "board"}}, nil, []*wsTerminal{d}, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				check := func(view svgScreen) {
					t.Helper()
					if len(view.fb) != geometry.width*geometry.height {
						t.Fatalf("fb len = %d, want %d", len(view.fb), geometry.width*geometry.height)
					}
					hasExit := false
					for i, h := range view.hits {
						if h.id == "exit" {
							hasExit = true
						}
						r := image.Rect(h.x1, h.y1, h.x2+1, h.y2+1)
						if !r.In(safePhys) {
							t.Errorf("%s hit %q %v outside safe area %v", tc.template, h.id, r, safePhys)
						}
						for _, other := range view.hits[:i] {
							or := image.Rect(other.x1, other.y1, other.x2+1, other.y2+1)
							if r.Overlaps(or) {
								t.Errorf("%s hits overlap: %q %v and %q %v", tc.template, h.id, r, other.id, or)
							}
						}
					}
					if !hasExit {
						t.Errorf("%s missing exit button", tc.template)
					}
				}
				view := views[0]
				check(view)
				var target hitBox
				found := false
				for _, h := range view.hits {
					if h.id == tc.id {
						target = h
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("missing hit target %q in %v", tc.id, view.hits)
				}
				tx, ty := (target.x1+target.x2)/2, (target.y1+target.y2)/2
				if got := hitID(view.hits, tx, ty); got != tc.id {
					t.Fatalf("hitID(%d,%d) = %q, want %q", tx, ty, got, tc.id)
				}
				views, err = a.update(appEvent{kind: "tap", i: 0, id: tc.id, x: tx, y: ty}, nil)
				if err != nil {
					t.Fatal(err)
				}
				check(views[0])
			})
		}
	}
}
