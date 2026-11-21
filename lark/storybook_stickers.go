package main

import (
	"fmt"
	"maps"
	"slices"
)

type stickerArt struct{ colorway, shape, accents string }

// Sticker paths are laid out in a common 100×100 box with room for the stroke.
var storybookStickers = map[string]stickerArt{
	"cat": {"peach", "", ""}, "fox": {"amber", "", ""}, "owl": {"lavender", "", ""},
	"dragon": {"mint", "", ""}, "bunny": {"berry", "", ""}, "bird": {"sky", "", ""},
	"star": {"amber", "M50 8Q53 8 56 16L66 35 89 39Q96 40 91 46L74 63 77 87Q78 95 71 91L50 80 29 91Q22 95 23 87L26 63 9 46Q4 40 11 39L34 35 44 16Q47 8 50 8Z",
		`<path d="M30 49l11-4 9-18M52 69l16 9-1-12" fill="none" stroke="@light" stroke-width="4" stroke-linecap="round"/>`},
	"heart": {"berry", "M50 89C42 83 10 61 8 40 6 13 36 8 50 29 64 8 94 13 92 40 90 61 58 83 50 89Z",
		`<path d="M17 41Q17 23 34 27M52 78Q71 65 80 49" fill="none" stroke="@light" stroke-width="5" stroke-linecap="round"/>`},
	"gem": {"lavender", "M11 35L30 15H70L89 35 50 88Z",
		`<path d="M11 35h78M30 15l7 20 13 53 13-53 7-20" fill="none" stroke="@ink" stroke-width="2.6"/><path d="M37 35l13-17 13 17Z" fill="@light"/><path d="M63 36h22L54 79Z" fill="@shade"/>`},
	"cloud": {"sky", "M27 79C5 79 3 46 25 41 25 14 62 8 72 37 96 34 101 79 74 79Z",
		`<path d="M20 62Q21 50 32 51Q32 29 52 28" fill="none" stroke="@light" stroke-width="7" stroke-linecap="round"/>`},
	"sun": {"amber", "M50 20Q62 20 70 31Q80 40 80 50Q80 64 70 73Q61 81 50 81Q35 81 27 72Q20 62 20 50Q20 36 30 28Q38 20 50 20Z",
		`<path d="M50 3v8m0 78v8M3 50h8m78 0h8M17 17l6 6m54 54 6 6m0-66-6 6M23 77l-6 6" fill="none" stroke="@ink" stroke-width="4" stroke-linecap="round"/><path d="M32 45q1-9 11-12" fill="none" stroke="@light" stroke-width="5" stroke-linecap="round"/>`},
	"moon": {"amber", "M62 10C40 32 52 67 83 63 77 87 52 96 31 84 10 72 6 47 18 28 27 14 42 8 62 10Z",
		`<path d="M25 53q0-18 13-27" fill="none" stroke="@light" stroke-width="5" stroke-linecap="round"/><path d="M76 14q2 10 12 12-10 2-12 12-2-10-12-12 10-2 12-12Z" fill="@light" stroke="@ink" stroke-width="2"/>`},
	"rocket": {"sky", "M35 53C36 32 55 10 87 10 90 38 73 61 50 68L45 83 36 85 35 70 20 66 21 57Z",
		`<path d="M59 18Q80 19 83 35M33 75Q16 76 15 91Q29 89 33 75Z" fill="#F6B575" stroke="@ink" stroke-width="3"/><circle cx="63" cy="34" r="10" fill="@light" stroke="@ink" stroke-width="3"/><path d="M43 52l10-10" stroke="@light" stroke-width="4" stroke-linecap="round"/>`},
	"flower": {"berry", "M50 25C48 4 74 7 75 24 92 21 97 44 81 53 97 65 84 82 67 76 64 97 39 96 34 76 16 84 5 65 20 53 3 44 11 21 27 25 25 7 51 5 50 25Z",
		`<circle cx="50" cy="51" r="14" fill="#F5C974" stroke="@ink" stroke-width="3"/><path d="M44 49q2-5 7-5M23 37l7 5" fill="none" stroke="@light" stroke-width="4" stroke-linecap="round"/>`},
	"leaf": {"sage", "M23 78C5 43 37 15 87 12 90 60 65 92 23 78Z",
		`<path d="M17 91Q44 53 73 28M27 72l32-3M43 55l-1-16M53 46l16-2" fill="none" stroke="@ink" stroke-width="3.5" stroke-linecap="round"/><path d="M28 54q6-18 23-25" fill="none" stroke="@light" stroke-width="4" stroke-linecap="round"/>`},
	"crown": {"amber", "M18 78 11 25Q11 20 16 23L35 44 46 17Q50 9 54 17L65 44 84 23Q89 20 89 25L82 78Q50 87 18 78Z",
		`<path d="M19 69q31 8 62 0" fill="none" stroke="@light" stroke-width="5" stroke-linecap="round"/><path d="M50 43l8 10-8 10-8-10Z" fill="#DF92AE" stroke="@ink" stroke-width="2.5"/>`},
	"book": {"lavender", "M50 23Q33 10 10 20L8 75Q30 69 50 85Q70 69 92 75L90 20Q67 10 50 23Z",
		`<path d="M50 25Q29 14 15 24L14 68Q32 63 50 79Q68 63 86 68L85 24Q71 14 50 25Z" fill="@light"/><path d="M50 25v53M23 34q9-1 17 4m-17 8q9-1 17 4m20-12q8-5 17-4m-17 16q8-5 17-4" fill="none" stroke="@ink" stroke-width="2.5" stroke-linecap="round"/>`},
	"music": {"mint", "M34 66V24L82 12v51C82 79 55 84 54 69 53 58 66 56 72 58V35L44 42v29C44 88 17 92 16 77 15 68 27 64 34 66Z",
		`<path d="M40 29l34-9M22 76q2-5 7-5" fill="none" stroke="@light" stroke-width="4" stroke-linecap="round"/>`},
	"apple": {"berry", "M50 36C80 15 96 42 82 68 72 88 61 93 50 84 39 93 28 88 18 68 4 42 20 15 50 36Z",
		`<path d="M50 36q5-11-3-23M53 27q-1-16 22-17-2 17-22 17Z" fill="#ACC280" stroke="@ink" stroke-width="3" stroke-linecap="round"/><path d="M22 50q2-10 10-11" fill="none" stroke="@light" stroke-width="5" stroke-linecap="round"/>`},
	"fish": {"sky", "M64 42Q80 25 91 30L87 50 92 70Q79 76 64 59C43 85 20 76 10 54 8 49 10 46 12 43 28 23 49 24 64 42Z",
		`<path d="M35 35q19 15 1 35M42 44l12 6-11 8" fill="none" stroke="@ink" stroke-width="3" stroke-linecap="round"/><circle cx="25" cy="47" r="4.5" fill="@eye"/><circle cx="24" cy="45.5" r="1.4" fill="#FFFDF7"/>`},
	"cake": {"peach", "M13 48Q50 39 87 48V82Q50 91 13 82Z",
		`<path d="M13 56q10 12 20 0 9 12 17 0 10 12 19 0 10 12 18 0" fill="none" stroke="@cream" stroke-width="5" stroke-linecap="round"/><path d="M45 47V30h10v17M50 26q-10 0 0-15 10 15 0 15Z" fill="#F5C974" stroke="@ink" stroke-width="3"/>`},
	"gift": {"mint", "M18 41H82V86H18Z",
		`<path d="M48 39Q16 37 25 22 32 10 48 39M52 39Q84 37 75 22 68 10 52 39" fill="@light" stroke="@ink" stroke-width="4"/><rect x="13" y="37" width="74" height="17" rx="4" ry="4" fill="@light" stroke="@ink" stroke-width="4"/><path d="M44 39h12v46H44Z" fill="#DF92AE" stroke="@ink" stroke-width="2.5"/>`},
	"wand": {"lavender", "M22 87Q17 93 12 88T13 78L56 36 65 45Z",
		`<path d="M64 9l6 14 16 1-12 11 4 15-14-8-14 8 3-15-12-11 16-1Z" fill="#F5C974" stroke="@ink" stroke-width="3" stroke-linejoin="round"/><path d="M28 27v12m-6-6h12m55 24v12m-6-6h12" stroke="@ink" stroke-width="3" stroke-linecap="round"/>`},
	"spark": {"amber", "M50 9Q54 43 89 50Q54 57 50 91Q46 57 11 50Q46 43 50 9Z",
		`<path d="M50 38l3 10 10 2-10 3-3 10-3-10-10-3 10-2Z" fill="@light"/><circle cx="77" cy="23" r="4" fill="@ink"/><circle cx="23" cy="77" r="3" fill="@ink"/>`},
}

var stickerNameSchema = object{"type": "string", "enum": slices.Sorted(maps.Keys(storybookStickers))}
var uiStickerOpts = closedObject(object{"x": uiInt(-1024, 1024), "y": uiInt(-1024, 1024),
	"size": uiInt(12, 240), "palette": storybookPaletteSchema, "id": uiIDSchema})

type stickerOptions struct {
	X, Y, Size int
	Palette    any
	ID         string
}

func compileSticker(name string, opts stickerOptions) (string, error) {
	def, ok := storybookStickers[name]
	if !ok {
		return "", fieldError("sticker.name", "unknown sticker %q", name)
	}
	if isExitNode(opts.ID) {
		return "", fieldError("sticker.id", "Home control ID is reserved")
	}
	p := storybookPalette(opts.Palette, def.colorway)
	if p.outline == "#000000" {
		return "", fieldError("sticker.palette.outline", "use a tinted outline")
	}
	art := storyStroke(def.shape, "@fur", "@ink", 4) + def.accents
	if def.shape == "" {
		s := companionSpeciesArt[name]
		nose := storyPath(s.nose, "@ink")
		if name == "bird" || name == "owl" {
			nose = storyStroke(s.nose, "#F5C974", "@ink", 3)
		}
		art = `<g transform="translate(-12 -5) scale(.345)">` + companionHead(name, s) +
			storyOval(113, 201, 13, 7, "@blush") + storyOval(247, 201, 13, 7, "@blush") +
			nose + companionEyes(s, "sparkle", "idle") + companionMouth(name, "idle") + `</g>`
	}
	art = fmt.Sprintf(`<g data-sticker="%s" transform="translate(%d %d) scale(%.3f)">%s</g>`, name, opts.X, opts.Y, float64(opts.Size)/100, p.paint(art))
	return screenHit(bounds{opts.X, opts.Y, opts.Size, opts.Size}, opts.ID, name, art), nil
}
