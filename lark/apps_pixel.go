package main

import (
	"fmt"
	"image"
	imagecolor "image/color"
	"strconv"
	"strings"

	"github.com/dop251/goja"
	"lark/fonts/dogica"
)

var pxAppSprites = map[string][]string{
	"apple": {
		".....gg...",
		"....g.....",
		"..rrrrrr..",
		".rrrrrrrr.",
		"rrrwwrrrrr",
		"rrrrrrrrrr",
		"rrrrrrrrrr",
		".rrrrrrrr.",
		"..rrr.rrr.",
		"..........",
	},
	"fish": {
		"..........",
		"....ssss..",
		"..sswwwws.",
		"s.swwkwwss",
		"ssssssssss",
		"s.ssssssss",
		"..sssssss.",
		"....ssss..",
		"..........",
		"..........",
	},
	"star": {
		"....yy....",
		"....yy....",
		".yyyyyyyy.",
		"..yyyyyy..",
		"...yyyy...",
		"..yyyyyy..",
		".yy....yy.",
		"y........y",
		"..........",
		"..........",
	},
	"heart": {
		"..........",
		".rrr..rrr.",
		"rrrrrrrrrr",
		"rrwrrrrrrr",
		"rrrrrrrrrr",
		".rrrrrrrr.",
		"..rrrrrr..",
		"...rrrr...",
		"....rr....",
		"..........",
	},
	"sun": {
		"..y....y..",
		"...yyyy...",
		".yyyyyyyy.",
		"yyyyyyyyyy",
		"yyyyyyyyyy",
		".yyyyyyyy.",
		"...yyyy...",
		"..y....y..",
		"..........",
		"..........",
	},
	"moon": {
		"...yyyy...",
		"..yyyy....",
		".yyyy.....",
		".yyyy.....",
		".yyyy.....",
		"..yyyy....",
		"...yyyy...",
		"....yyyy..",
		"..........",
		"..........",
	},
	"owl": {
		".bb....bb.",
		".bbbbbbbb.",
		"bbwwbbwwbb",
		"bbwkbbkwbb",
		"bbbbyybbbb",
		".bbbbbbbb.",
		".bwwwwwwb.",
		".bbbbbbbb.",
		"..bb..bb..",
		"..........",
	},
	"gift": {
		"..rr..rr..",
		"...rrrr...",
		".mmmmmmmm.",
		".mmmrrmmm.",
		".mmmmmmmm.",
		".rrrrrrrr.",
		".mmmrrmmm.",
		".mmmrrmmm.",
		".mmmmmmmm.",
		"..........",
	},
	"rocket": {
		"....rr....",
		"...rrrr...",
		"...wwww...",
		"..wwssww..",
		"..wwwwww..",
		".rwwwwwwr.",
		".rwwwwwwr.",
		"..rr..rr..",
		"..yy..yy..",
		"..........",
	},
	"fox": {
		".pp....pp.",
		".pppppppp.",
		"pppppppppp",
		"ppkppppkpp",
		"pwwwppwwwp",
		".ppkkkkpp.",
		"..pppppp..",
		"..pwwwwp..",
		"..pp..pp..",
		"..........",
	},
	"flower": {
		"...rrrr...",
		"..rrrrrr..",
		".rrryyrrr.",
		"..rrrrrr..",
		"...rrrr...",
		"....gg....",
		"..gggggg..",
		"....gg....",
		"....gg....",
		"..........",
	},
	"leaf": {
		"......ggg.",
		"....ggggg.",
		"..ggggggg.",
		".gggggggg.",
		".ggggggg..",
		"..ggggg...",
		"..gg......",
		".g........",
		"..........",
		"..........",
	},
	"spark": {
		"....yy....",
		"....yy....",
		"..yyyyyy..",
		"yyyywwyyyy",
		"..yyyyyy..",
		"....yy....",
		"....yy....",
		"..........",
		"..........",
		"..........",
	},
}

var pxAppDefaultPalette = map[string]imagecolor.RGBA{
	".": {},
	"k": {36, 28, 44, 255},
	"w": {250, 240, 246, 255},
	"p": {255, 193, 168, 255},
	"m": {154, 229, 195, 255},
	"l": {191, 168, 229, 255},
	"s": {156, 191, 232, 255},
	"y": {255, 228, 155, 255},
	"r": {255, 150, 175, 255},
	"g": {172, 194, 128, 255},
	"b": {140, 96, 68, 255},
	"d": {67, 53, 78, 255},
}

func parseHexRGBA(s string, fallback imagecolor.RGBA) imagecolor.RGBA {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "#") || len(s) != 7 {
		return fallback
	}
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return fallback
	}
	return imagecolor.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 255}
}

type pixelAppContext struct {
	spec   pixelDisplaySpec
	canvas *pixelCanvas
	texts  []pixelTextOp
	hits   []hitBox
	assets map[string]*image.RGBA
}

func newPixelAppContext(spec pixelDisplaySpec, assets map[string]*image.RGBA) *pixelAppContext {
	c := newPixelCanvas(spec.logicalW, spec.logicalH)
	c.fill(pxPanelBG)
	return &pixelAppContext{
		spec:   spec,
		canvas: c,
		assets: assets,
	}
}

func (p *pixelAppContext) summary() string {
	var parts []string
	for _, t := range p.texts {
		parts = append(parts, t.text)
	}
	for _, h := range p.hits {
		parts = append(parts, h.id)
	}
	return strings.Join(parts, " ")
}

func (p *pixelAppContext) finish() (*image.RGBA, []hitBox) {
	return finishPixelScreen(p.canvas, p.spec, p.texts), p.hits
}

func asString(v goja.Value) (string, bool) {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return "", false
	}
	s, ok := v.(goja.String)
	if !ok || s.Length() > 16384 {
		return "", false
	}
	return s.String(), true
}

func (p *pixelAppContext) addHit(id string, x, y, w, h int) {
	if id == "" || w <= 0 || h <= 0 {
		return
	}
	s := p.spec.artScale
	p.hits = append(p.hits, hitBox{
		id: id,
		x1: x * s,
		y1: y * s,
		x2: (x+w)*s - 1,
		y2: (y+h)*s - 1,
	})
}

func (p *pixelAppContext) installJS(vm *goja.Runtime) *goja.Object {
	g := vm.NewObject()
	_ = g.Set("w", p.spec.logicalW)
	_ = g.Set("h", p.spec.logicalH)
	_ = g.Set("round", p.spec.round)
	_ = g.Set("safe", map[string]int{
		"x": p.spec.safe.Min.X,
		"y": p.spec.safe.Min.Y,
		"w": p.spec.safe.Dx(),
		"h": p.spec.safe.Dy(),
	})

	_ = g.Set("clear", func(c goja.FunctionCall) goja.Value {
		col := pxPanelBG
		if len(c.Arguments) > 0 {
			if s, ok := asString(c.Argument(0)); ok {
				col = parseHexRGBA(s, pxPanelBG)
			}
		}
		p.canvas.fill(col)
		return g
	})

	_ = g.Set("rect", func(c goja.FunctionCall) goja.Value {
		x := int(c.Argument(0).ToInteger())
		y := int(c.Argument(1).ToInteger())
		w := int(c.Argument(2).ToInteger())
		h := int(c.Argument(3).ToInteger())
		col := pxTileBG
		if s, ok := asString(c.Argument(4)); ok {
			col = parseHexRGBA(s, pxTileBG)
		}
		if len(c.Arguments) > 5 && !goja.IsUndefined(c.Argument(5)) && !goja.IsNull(c.Argument(5)) {
			if bs, ok := asString(c.Argument(5)); ok && bs != "" {
				p.canvas.panel(x, y, w, h, col, parseHexRGBA(bs, pxTileBorder))
				return g
			}
		}
		p.canvas.rect(x, y, w, h, col)
		return g
	})

	_ = g.Set("line", func(c goja.FunctionCall) goja.Value {
		x0 := int(c.Argument(0).ToInteger())
		y0 := int(c.Argument(1).ToInteger())
		x1 := int(c.Argument(2).ToInteger())
		y1 := int(c.Argument(3).ToInteger())
		col := pxInk
		if s, ok := asString(c.Argument(4)); ok {
			col = parseHexRGBA(s, pxInk)
		}
		p.canvas.line(x0, y0, x1, y1, col)
		return g
	})

	_ = g.Set("sprite", func(c goja.FunctionCall) goja.Value {
		x := int(c.Argument(0).ToInteger())
		y := int(c.Argument(1).ToInteger())
		arg2 := c.Argument(2)
		var rows []string
		if name, ok := asString(arg2); ok {
			if built, found := pxAppSprites[name]; found {
				rows = built
			} else if built, found := pxMenuSprites[name]; found {
				rows = built
			} else if built, found := pxStatusSprites[name]; found {
				rows = built
			}
		} else if !goja.IsUndefined(arg2) && !goja.IsNull(arg2) {
			if err := vm.ExportTo(arg2, &rows); err != nil {
				rows = nil
			}
		}
		if len(rows) == 0 || len(rows) > 64 {
			return g
		}
		pal := pxAppDefaultPalette
		if len(c.Arguments) > 3 && !goja.IsUndefined(c.Argument(3)) && !goja.IsNull(c.Argument(3)) {
			var overrides map[string]string
			if err := vm.ExportTo(c.Argument(3), &overrides); err == nil && len(overrides) > 0 {
				cloned := make(map[string]imagecolor.RGBA, len(pxAppDefaultPalette)+len(overrides))
				for k, v := range pxAppDefaultPalette {
					cloned[k] = v
				}
				for k, v := range overrides {
					if len(k) == 1 && k != "." {
						cloned[k] = parseHexRGBA(v, pxInk)
					}
				}
				pal = cloned
			}
		}
		scale := 1
		if len(c.Arguments) > 4 && !goja.IsUndefined(c.Argument(4)) && !goja.IsNull(c.Argument(4)) {
			scale = max(1, min(8, int(c.Argument(4).ToInteger())))
		}
		p.canvas.blitSprite(x, y, rows, pal, scale)
		return g
	})

	_ = g.Set("text", func(c goja.FunctionCall) goja.Value {
		x := int(c.Argument(0).ToInteger())
		y := int(c.Argument(1).ToInteger())
		strVal, _ := asString(c.Argument(2))
		if strVal == "" {
			return g
		}
		col := pxInk
		scale := p.spec.bodyTextScale
		f := dogica.Regular
		align := "left"
		if len(c.Arguments) > 3 && !goja.IsUndefined(c.Argument(3)) && !goja.IsNull(c.Argument(3)) {
			arg3 := c.Argument(3)
			if s, ok := asString(arg3); ok {
				if s == "title" {
					scale = p.spec.titleTextScale
					f = dogica.Bold
				} else if s == "body" {
					scale = p.spec.bodyTextScale
				} else {
					col = parseHexRGBA(s, pxInk)
				}
			} else if obj := arg3.ToObject(vm); obj != nil {
				if v, ok := asString(obj.Get("color")); ok {
					col = parseHexRGBA(v, pxInk)
				}
				if v, ok := asString(obj.Get("align")); ok {
					align = v
				}
				if v := obj.Get("bold"); v != nil && v.ToBoolean() {
					f = dogica.Bold
				}
				if v, ok := asString(obj.Get("size")); ok {
					if v == "title" {
						scale = p.spec.titleTextScale
						f = dogica.Bold
					} else if v == "small" {
						scale = 1
					}
				}
				if v := obj.Get("scale"); v != nil && !goja.IsUndefined(v) && !goja.IsNull(v) {
					if sc := int(v.ToInteger()); sc >= 1 && sc <= 4 {
						scale = sc
					}
				}
			}
		}
		px := x * p.spec.artScale
		py := y * p.spec.artScale
		tw := measureTextPx(strVal, f, scale)
		if align == "center" {
			px -= tw / 2
		} else if align == "right" {
			px -= tw
		}
		p.texts = append(p.texts, pixelTextOp{
			x: px, y: py, text: strVal, font: f, scale: scale, color: col,
		})
		return g
	})

	_ = g.Set("image", func(c goja.FunctionCall) goja.Value {
		x := int(c.Argument(0).ToInteger())
		y := int(c.Argument(1).ToInteger())
		name, _ := asString(c.Argument(2))
		if img := p.assets[name]; img != nil {
			p.canvas.blit(x, y, img)
		}
		return g
	})

	_ = g.Set("button", func(c goja.FunctionCall) goja.Value {
		id, _ := asString(c.Argument(0))
		x := int(c.Argument(1).ToInteger())
		y := int(c.Argument(2).ToInteger())
		w := int(c.Argument(3).ToInteger())
		h := int(c.Argument(4).ToInteger())
		label, _ := asString(c.Argument(5))
		bgCol := pxTileBG
		borderCol := pxTileBorder
		textCol := pxInk
		if len(c.Arguments) > 6 && !goja.IsUndefined(c.Argument(6)) && !goja.IsNull(c.Argument(6)) {
			if s, ok := asString(c.Argument(6)); ok && s != "" {
				bgCol = parseHexRGBA(s, pxTileBG)
			}
		}
		if len(c.Arguments) > 7 && !goja.IsUndefined(c.Argument(7)) && !goja.IsNull(c.Argument(7)) {
			if s, ok := asString(c.Argument(7)); ok && s != "" {
				borderCol = parseHexRGBA(s, pxTileBorder)
			}
		}
		if len(c.Arguments) > 8 && !goja.IsUndefined(c.Argument(8)) && !goja.IsNull(c.Argument(8)) {
			if s, ok := asString(c.Argument(8)); ok && s != "" {
				textCol = parseHexRGBA(s, pxInk)
			}
		}
		p.canvas.panel(x, y, w, h, bgCol, borderCol)
		if label != "" {
			physBox := physicalRect(image.Rect(x, y, x+w, y+h), p.spec.artScale)
			clipped := truncateEllipsisPx(label, dogica.Regular, p.spec.bodyTextScale, physBox.Dx()-4)
			p.texts = addCenteredText(p.texts, physBox, clipped, dogica.Regular, p.spec.bodyTextScale, textCol)
		}
		p.addHit(id, x, y, w, h)
		return g
	})

	_ = g.Set("hit", func(c goja.FunctionCall) goja.Value {
		id, _ := asString(c.Argument(0))
		x := int(c.Argument(1).ToInteger())
		y := int(c.Argument(2).ToInteger())
		w := int(c.Argument(3).ToInteger())
		h := int(c.Argument(4).ToInteger())
		p.addHit(id, x, y, w, h)
		return g
	})

	return g
}

func validatePixelAppSpriteNames(name string) bool {
	if _, ok := pxAppSprites[name]; ok {
		return true
	}
	_, ok := pxMenuSprites[name]
	return ok
}

func pixelAppSpriteList() []string {
	out := make([]string, 0, len(pxAppSprites))
	for k := range pxAppSprites {
		out = append(out, k)
	}
	return out
}

func rgbaToSVGScreenCompat(img *image.RGBA, hits []hitBox) (svgScreen, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	fb := make([]uint16, w*h)
	for y := range h {
		for x := range w {
			c := img.RGBAAt(b.Min.X+x, b.Min.Y+y)
			fb[y*w+x] = pack565(c.R, c.G, c.B)
		}
	}
	return svgScreen{
		fb:   fb,
		hits: hits,
	}, nil
}

func fmtColorHex(c imagecolor.RGBA) string {
	return fmt.Sprintf("#%02X%02X%02X", c.R, c.G, c.B)
}
