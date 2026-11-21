package main

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

const screenContentWidth = 220

var (
	uiColorSchema = object{"type": "string", "pattern": `^(?:#[0-9a-fA-F]{6}|[kwpmlsyrgabndco])$`}
	uiIDSchema    = object{"type": "string", "pattern": svgID.String()}
	uiVariant     = object{"type": "string", "enum": []string{"default", "primary", "secondary", "outline", "ghost", "success", "danger"}}
	uiSpriteOpts  = closedObject(object{"x": uiInt(-1024, 1024), "y": uiInt(-1024, 1024), "scale": uiInt(1, 128), "palette": pixelPaletteSchema, "id": uiIDSchema})
)

var declarativeScreenSchema = func() object {
	tag := func(name string) object { return object{"type": "string", "enum": []string{name}} }
	flags := object{"type": "boolean"}
	action := closedObject(object{"id": uiIDSchema, "label": uiString(80), "variant": uiVariant}, "id", "label")
	item := closedObject(object{"id": uiIDSchema, "label": uiString(80), "variant": uiVariant, "sprite": pixelRowsSchema, "sub": uiString(80), "active": flags}, "id", "label")
	return closedObject(object{
		"title": uiString(96), "badge": uiString(64), "bg": uiColorSchema, "accent": uiColorSchema,
		"actions": uiArray(action, 0, 3),
		"body": uiArray(object{"oneOf": []object{
			closedObject(object{"type": tag("sprite"), "rows": pixelRowsSchema, "scale": uiInt(1, 128), "palette": pixelPaletteSchema, "id": uiIDSchema}, "type", "rows"),
			closedObject(object{"type": tag("sticker"), "name": stickerNameSchema, "size": uiInt(12, 240), "palette": storybookPaletteSchema, "id": uiIDSchema}, "type", "name"),
			closedObject(object{"type": tag("text"), "text": uiString(512), "size": uiInt(10, 40), "bold": flags, "muted": flags}, "type", "text"),
			closedObject(object{"type": tag("meter"), "label": uiString(80), "value": object{"type": "number"},
				"max": object{"type": "number"}, "color": uiColorSchema}, "type", "label", "value", "max"),
			closedObject(object{"type": tag("grid"), "cols": uiInt(1, 3), "items": uiArray(item, 1, 9)}, "type", "items"),
		}}, 0, 8),
	}, "body")
}()

type uiSpriteOptions struct {
	X, Y, Scale int
	Palette     map[string]string
	ID          string
}

type declarativeScreen struct {
	Title, Badge, BG, Accent string
	Body                     []screenBlock
	Actions                  []screenAction
}

type screenBlock struct {
	Type, ID, Text, Label, Color, Name string
	Size, Scale, Cols                  int
	Bold, Muted                        bool
	Value, Max                         float64
	Rows                               []string
	Palette                            any
	Items                              []screenTile
}

type screenAction struct{ ID, Label, Variant string }

type screenTile struct {
	screenAction
	Sub    string
	Active bool
	Sprite []string
}

func paletteColor(val, fallback string) string {
	if c := larkPalette[val]; c != "" {
		return c
	}
	if val != "" {
		return val
	}
	return fallback
}

func screenHit(b bounds, id, label, art string) string {
	if id == "" {
		return art
	}
	return fmt.Sprintf(`<g data-id="%s" data-hit="%d,%d,%d,%d" aria-label="%s">%s</g>`,
		id, b.x, b.y, b.w, b.h, uiEsc(label), art)
}

func screenControl(b bounds, a screenAction, sub, art, accent string, active bool) string {
	card, ink, border := larkPalette["w"], larkPalette["k"], larkPalette["n"]
	switch a.Variant {
	case "primary":
		card = accent
		ink = uiContrastInk(accent)
	case "secondary":
		card = larkPalette["l"]
	case "success":
		card = larkPalette["m"]
	case "danger":
		card = larkPalette["r"]
	case "ghost":
		border = "none"
	case "outline":
		border = ink
	}
	stroke := 1
	if active {
		card, border, stroke = larkPalette["m"], ink, 2
	}
	if art == "" {
		if sub == "" {
			art = uiLabel(b.x+b.w/2, b.y+b.h/2+6, 16, b.w-10, a.Label, ink, true)
		} else {
			art = uiLabel(b.x+b.w/2, b.y+b.h/2, 15, b.w-10, a.Label, ink, true)
		}
	} else {
		y := b.y + b.h - 6
		if sub != "" {
			y -= 13
		}
		art += uiLabel(b.x+b.w/2, y, 14, b.w-10, a.Label, ink, true)
	}
	if sub != "" {
		art += uiLabel(b.x+b.w/2, b.y+b.h-5, 12, b.w-10, sub, ink, false)
	}
	return screenHit(b, a.ID, a.Label, uiRoundRect(b.x, b.y, b.w, b.h, uiButtonRadius, card, border, stroke)+art)
}

func (s declarativeScreen) svg() (string, error) {
	seen := map[string]bool{}
	id := func(id string) error {
		if isExitNode(id) || seen[id] {
			return fieldError("screen", "duplicate or reserved control ID %q", id)
		}
		seen[id] = true
		return nil
	}
	for _, a := range s.Actions {
		if err := id(a.ID); err != nil {
			return "", err
		}
	}
	for _, b := range s.Body {
		if b.ID != "" {
			if err := id(b.ID); err != nil {
				return "", err
			}
		}
		for _, item := range b.Items {
			if err := id(item.ID); err != nil {
				return "", err
			}
		}
		if b.Type == "meter" && b.Max <= 0 {
			return "", fieldError("screen meter max", "must be positive")
		}
	}
	// The card and all 8 px hit padding fit the inscribed bezel; the
	// last action stops above the system's Home target at y=300.
	const top, bottom = 83, 282
	accent := paletteColor(s.Accent, larkPalette["p"])
	view := sceneView{bg: paletteColor(s.BG, menuBG), art: uiRoundRect(55, 66, 250, 222, uiCardRadius, larkPalette["w"], menuBorder, 2)}
	for _, density := range []int{100, 85, 70, 55} {
		var content []func(y int) string
		height := 0
		gap := max(5, density/12)
		add := func(h int, draw func(y int) string) {
			if len(content) > 0 {
				height += gap
			}
			offset := height
			content = append(content, func(y int) string { return draw(y + offset) })
			height += h
		}
		if s.Title != "" || s.Badge != "" {
			add(22, func(y int) string {
				if s.Title == "" {
					return uiLabel(180, y+16, 14, screenContentWidth, s.Badge, menuPrimaryInk, true)
				}
				if s.Badge == "" {
					return uiLabel(180, y+18, 22, screenContentWidth, s.Title, larkPalette["k"], true)
				}
				return uiLabel(142, y+17, 20, 142, s.Title, larkPalette["k"], true) +
					uiRoundRect(218, y+1, 70, 20, 9, accent, "none", 0) +
					uiLabel(253, y+15, 12, 60, s.Badge, uiContrastInk(accent), true)
			})
		}
		for _, b := range s.Body {
			switch b.Type {
			case "sprite":
				scale := b.Scale
				cols := len(b.Rows[0])
				if scale == 0 {
					scale = max(1, 96/max(len(b.Rows), cols))
				}
				scale = min(max(1, scale*density/100), screenContentWidth/cols, (bottom-top)/len(b.Rows))
				if max(cols, len(b.Rows)) > 16 {
					scale = min(scale, 12)
				}
				w, h := cols*scale, len(b.Rows)*scale
				x := 180 - w/2
				var overrides map[string]string
				if raw, ok := b.Palette.(object); ok {
					overrides = make(map[string]string, len(raw))
					for key, value := range raw {
						overrides[key] = str(value)
					}
				}
				art, err := compileSprite(b.Rows, overrides, x, 0, scale)
				if err != nil {
					return "", err
				}
				add(h, func(y int) string {
					return screenHit(bounds{x, y, w, h}, b.ID, b.ID, fmt.Sprintf(`<g transform="translate(0 %d)">%s</g>`, y, art))
				})
			case "sticker":
				size := b.Size
				if size == 0 {
					size = 84
				}
				size = min(bottom-top, max(12, size*density/100))
				art, err := compileSticker(b.Name, stickerOptions{X: 180 - size/2, Size: size, Palette: b.Palette, ID: b.ID})
				if err != nil {
					return "", err
				}
				add(size, func(y int) string {
					return fmt.Sprintf(`<g transform="translate(0 %d)">%s</g>`, y, art)
				})
			case "text":
				size := b.Size
				if size == 0 {
					size = 17
				}
				size = max(10, size*density/100)
				lineHeight := size + 3
				lines := uiWrap(b.Text, screenContentWidth, size, b.Bold, uiLineRunes)
				add(len(lines)*lineHeight, func(y int) string {
					var out strings.Builder
					ink := larkPalette["k"]
					if b.Muted {
						ink = "#51485E"
					}
					for i, line := range lines {
						out.WriteString(uiText(180, y+i*lineHeight+size, size, line, ink, "middle", b.Bold))
					}
					return out.String()
				})
			case "meter":
				add(32, func(y int) string {
					w := int(math.Round(screenContentWidth * min(1, max(0, b.Value/b.Max))))
					art := uiLabel(180, y+14, 14, screenContentWidth, b.Label, larkPalette["k"], true) +
						uiRoundRect(70, y+20, screenContentWidth, 8, 4, larkPalette["d"], "none", 0)
					if w > 0 {
						art += uiRoundRect(70, y+20, w, 8, min(w/2, 4), paletteColor(b.Color, accent), "none", 0)
					}
					return art
				})
			case "grid":
				n := b.Cols
				if n == 0 {
					n = min(3, len(b.Items))
				}
				for _, item := range b.Items {
					if len(item.Sprite) != 0 {
						n = min(n, (screenContentWidth+16)/(len(item.Sprite[0])+24))
					}
				}
				n = min(n, len(b.Items))
				w := (screenContentWidth - 16*(n-1)) / n
				for row := range slices.Chunk(b.Items, n) {
					h := 37
					x := 180 - (len(row)*w+16*(len(row)-1))/2
					var arts []string
					for i, item := range row {
						art := ""
						if len(item.Sprite) > 0 {
							labelSpace := 21
							if item.Sub != "" {
								labelSpace += 13
							}
							room := max(len(item.Sprite), 66*density/100-labelSpace)
							scale := min(3, (w-8)/len(item.Sprite[0]), room/len(item.Sprite))
							h = max(h, scale*len(item.Sprite)+labelSpace)
							var err error
							art, err = compileSprite(item.Sprite, nil, x+i*(w+16)+(w-scale*len(item.Sprite[0]))/2, 3, scale)
							if err != nil {
								return "", err
							}
						}
						arts = append(arts, art)
					}
					add(h, func(y int) string {
						var out strings.Builder
						for i, item := range row {
							art := arts[i]
							if art != "" {
								art = fmt.Sprintf(`<g transform="translate(0 %d)">%s</g>`, y, art)
							}
							out.WriteString(screenControl(bounds{x + i*(w+16), y, w, h}, item.screenAction, item.Sub, art, accent, item.Active))
						}
						return out.String()
					})
				}
			}
		}
		if n := len(s.Actions); n > 0 {
			add(34, func(y int) string {
				w := min(138, (screenContentWidth-16*(n-1))/n)
				x := 180 - (n*w+16*(n-1))/2
				var out strings.Builder
				for i, a := range s.Actions {
					out.WriteString(screenControl(bounds{x + i*(w+16), y, w, 34}, a, "", "", accent, false))
				}
				return out.String()
			})
		}
		if height <= bottom-top {
			for _, draw := range content {
				view.art += draw(top + (bottom-top-height)/2)
			}
			raw := view.svg("")
			if _, err := parseSVG(raw); err != nil {
				return "", fieldError("screen", "%s", err)
			}
			return raw, nil
		}
	}
	return "", fieldError("screen", "content does not fit; shorten the text or split the screen")
}

func uiContrastInk(bg string) string {
	rgb, _ := strconv.ParseUint(strings.TrimPrefix(bg, "#"), 16, 24)
	channel := func(x uint64) float64 {
		c := float64(x&255) / 255
		if c <= 0.04045 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	if 0.2126*channel(rgb>>16)+0.7152*channel(rgb>>8)+0.0722*channel(rgb) < 0.18 {
		return larkPalette["w"]
	}
	return larkPalette["k"]
}
