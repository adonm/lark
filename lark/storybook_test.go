package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestStorybookSpeciesAndStickersRender(t *testing.T) {
	if len(storybookPalettes) != 8 || len(companionNames) != 10 || len(storybookStickers) != 24 {
		t.Fatal("incomplete Storybook catalog")
	}
	for i, name := range companionNames {
		t.Run(name, func(t *testing.T) {
			eyes := []string{"sparkle", "happy", "sleepy", "curious", "star"}
			marks := []string{"none", "tabby", "blush", "freckles", "star", "spots"}
			extras := []string{"none", "scarf", "crown", "glasses", "bow", "beanie", "leaf"}
			art, p, err := compileCompanion(object{"species": name, "eyes": eyes[i%len(eyes)], "markings": marks[i%len(marks)],
				"accessory": extras[i%len(extras)], "palette": object{"bg": "#FEF5E8"}})
			if err != nil || p.bg != "#FEF5E8" {
				t.Fatal(err)
			}
			art, err = sanitizeFaceSVG(art)
			if err != nil {
				t.Fatal(err)
			}
			for _, part := range faceParts {
				if !strings.Contains(art, `data-part="`+part+`"`) {
					t.Fatalf("missing part %s", part)
				}
			}
			for _, pose := range faceEditPoses[1:] {
				layer, err := faceLayers.layer(art, pose)
				if err != nil || strings.Count(layer, "data-pose=") != 1 || !strings.Contains(layer, `data-pose="`+pose+`"`) {
					t.Fatalf("missing pose %s: %v", pose, err)
				}
			}
			edited, err := editFaceParts(art, []any{object{"part": "eyes", "pose": "idle", "svg": `<circle cx="135" cy="166" r="7" fill="#849BCD"/>`}})
			if err != nil || strings.Count(edited, `fill="#849BCD"`) != 1 {
				t.Fatalf("pose edit failed: %v", err)
			}
		})
	}
	for _, name := range stickerNameSchema["enum"].([]string) {
		t.Run(name, func(t *testing.T) {
			art, err := compileSticker(name, stickerOptions{X: 124, Y: 105, Size: 112, ID: "pick"})
			if err != nil {
				t.Fatal(err)
			}
			raw := svgStart + `<rect width="360" height="360" fill="#FFF2E7"/>` + art + `</svg>`
			s, err := buildScreen(raw, true, roundScreen)
			if err != nil || !strings.Contains(s.source, fmt.Sprintf(`data-sticker="%s"`, name)) ||
				hitID(s.hits, 180, 161) != "pick" || len(appLayoutWarnings([]svgScreen{s}, true)) != 0 {
				t.Fatalf("invalid sticker: %v", err)
			}
		})
	}
}

func TestStorybookThroughMCP(t *testing.T) {
	_, base := wsHarness(t, nil)
	const id = "bean"
	board := testConnectWS(t, base, "unit", false)
	board.helloV2(t, "vocat-0001", "Bean")
	out := toolTest(t, base, "patch", object{"companion_id": id, "companion": object{"preset": object{
		"species": "fox", "palette": "berry", "eyes": "star", "accessory": "scarf",
	}}}, false)
	companion := out["companion"].(object)
	if companion["face_bg"] != "#FAEAF0" || !strings.Contains(str(companion["face_svg"]), `data-part="eyes"`) {
		t.Fatal(companion)
	}
	toolTest(t, base, "patch", object{"companion_id": id, "companion": object{"preset": object{"species": "fox", "extra": true}}}, true)
	out = toolTest(t, base, "app", object{"companions": id, "screen": object{"title": "Owl spot", "body": []object{
		{"type": "sticker", "name": "owl", "palette": object{"base": "mint", "fur": "#96C7AD"}, "id": "bird"},
	}}}, false)
	if out["layout_warnings"] != nil {
		t.Fatal(out)
	}
	toolTest(t, base, "app", object{"companions": id, "screen": object{"body": []object{
		{"type": "sticker", "name": "owl", "id": "lark-exit"},
	}}}, true)
}
