package main

import (
	"encoding/xml"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/dop251/goja"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

// Bound native work: goja's timeout cannot interrupt a Go loop.
const maxUIText = 64 << 10
const uiLineRunes = 128

func uiEsc(text string) string {
	var out strings.Builder
	_ = xml.EscapeText(&out, []byte(text))
	return out.String()
}

func uiMetrics(size int, bold bool) svgGlyphMetrics {
	f := regularSVGFont
	if bold {
		f = boldSVGFont
	}
	return svgGlyphMetrics{font: f, size: fixed.I(size)}
}

// uiPrefix returns the last fitting byte offset and the latest word boundary.
func uiPrefix(m *svgGlyphMetrics, text string, width float64, limit int) (end, space int) {
	var previous sfnt.GlyphIndex
	var used fixed.Int26_6
	for i, r := range text {
		if r == ' ' {
			space = i
		}
		id, spacing, advance := m.glyph(r, previous, i > 0)
		used += spacing + advance
		if limit == 0 || float64(used) > width*64 {
			return i, space
		}
		limit--
		previous = id
	}
	return len(text), space
}

func uiWrap(text string, width float64, size int, bold bool, limit int) []string {
	m := uiMetrics(size, bold)
	var lines []string
	for p := range strings.SplitSeq(text, "\n") {
		p = strings.Join(strings.Fields(p), " ")
		if p == "" {
			lines = append(lines, "")
		}
		for p != "" {
			end, space := uiPrefix(&m, p, width, limit)
			if end < len(p) && space > 0 {
				end = space
			}
			if end == 0 {
				_, end = utf8.DecodeRuneInString(p)
			}
			lines = append(lines, p[:end])
			p = strings.TrimPrefix(p[end:], " ")
		}
	}
	return lines
}

type uiPalette struct{ bg, card, ink, accent, border string }

func uiColors(theme string) uiPalette {
	switch theme {
	case "night":
		return uiPalette{menuBG, menuCard, menuInk, menuPrimary, menuBorder}
	case "mint":
		return uiPalette{"#16342C", "#F0FAF5", "#1E312A", "#226850", "#88A69A"}
	case "sky":
		return uiPalette{"#23344C", "#F3F7FD", "#1D2F47", "#355F99", "#92A0B6"}
	default: // warm
		return uiPalette{menuBG, "#FFFAF5", "#292130", "#813F35", "#978391"}
	}
}

func uiText(x, y, size int, text, ink, anchor string, bold bool) string {
	if text == "" {
		return ""
	}
	weight := "normal"
	if bold {
		weight = "bold"
	}
	return fmt.Sprintf(`<text x="%d" y="%d" fill="%s" text-anchor="%s" font-size="%d" font-weight="%s">%s</text>`,
		x, y, ink, anchor, size, weight, uiEsc(text))
}

func uiEllipsis(m *svgGlyphMetrics, text string, width int) string {
	_, _, advance := m.glyph('…', 0, false)
	end, _ := uiPrefix(m, text, float64(width)-float64(advance)/64-1, uiLineRunes-1)
	return strings.TrimSpace(text[:end]) + "…"
}

func uiLabel(x, y, size, width int, text, ink string, bold bool) string {
	text = strings.Join(strings.Fields(text), " ")
	for {
		m := uiMetrics(size, bold)
		if end, _ := uiPrefix(&m, text, float64(width), uiLineRunes); end == len(text) {
			return uiText(x, y, size, text, ink, "middle", bold)
		}
		if size <= 12 {
			return uiText(x, y, size, uiEllipsis(&m, text, width), ink, "middle", bold)
		}
		size--
	}
}

func uiButton(b bounds, id, label, sub string, p uiPalette) string {
	x, y, w, h := b.x, b.y, b.w, b.h
	art := uiRoundRect(x, y, w, h, uiButtonRadius, p.card, p.accent, 2)
	if sub == "" {
		art += uiLabel(x+w/2, y+h/2+6, 17, w-16, label, p.ink, true)
	} else {
		art += uiLabel(x+w/2, y+16, 16, w-16, label, p.ink, true)
		art += uiLabel(x+w/2, y+30, 12, w-18, sub, p.ink, false)
	}
	return fmt.Sprintf(`<g data-id="%s" data-hit="%d,%d,%d,%d" aria-label="%s">%s</g>`,
		id, x, y, w, h, uiEsc(truncate(label, 128)), art)
}

func uiRoundRect(x, y, w, h, radius int, fill, stroke string, sw int) string {
	// oksvg requires both radii; browsers infer ry when it is omitted.
	return fmt.Sprintf(`<rect x="%d" y="%d" width="%d" height="%d" rx="%d" ry="%d" fill="%s" stroke="%s" stroke-width="%d"/>`,
		x, y, w, h, radius, radius, fill, stroke, sw)
}

type uiPage struct {
	title string
	lines []string
}

func (p uiPage) svg(index, count, size int, colors uiPalette) string {
	s := sceneView{bg: colors.bg, art: uiRoundRect(42, 88, 276, 202, uiCardRadius, colors.card, colors.border, 2)}
	counter := fmt.Sprintf("%d/%d", index+1, count)
	s.art += uiText(180, 68, fitLabel(counter, 16, 40), counter, menuInk, "middle", true)
	// All four corners still fit the round screen after 8 px of touch padding.
	if index > 0 {
		s.art += uiButton(bounds{90, 42, 68, 40}, "prev", "Prev", "", colors)
	}
	if index+1 < count {
		s.art += uiButton(bounds{202, 42, 68, 40}, "next", "Next", "", colors)
	}
	s.art += uiLabel(180, 117, 22, 228, p.title, colors.ink, true)
	for i, line := range p.lines {
		s.art += uiText(60, 143+i*(size+5), size, line, colors.ink, "start", false)
	}
	return s.svg("")
}

func uiGet(v goja.Value, name string) goja.Value {
	if obj, ok := v.(*goja.Object); ok {
		return obj.Get(name)
	}
	return nil
}

func uiNumber(v goja.Value, fallback, lo, hi float64) float64 {
	if v != nil && !goja.IsUndefined(v) && !goja.IsNull(v) {
		if n := v.ToFloat(); !math.IsNaN(n) {
			return min(hi, max(lo, n))
		}
	}
	return fallback
}

func installAppUI(vm *goja.Runtime, tapChime func()) {
	ui := vm.NewObject()
	palette := vm.NewObject()
	for key, tint := range larkPalette {
		_ = palette.DefineDataProperty(key, vm.ToValue(tint), goja.FLAG_FALSE, goja.FLAG_FALSE, goja.FLAG_TRUE)
	}
	freeze, _ := goja.AssertFunction(vm.Get("Object").ToObject(vm).Get("freeze"))
	_, _ = freeze(goja.Undefined(), palette)
	_ = ui.DefineDataProperty("palette", palette, goja.FLAG_FALSE, goja.FLAG_FALSE, goja.FLAG_TRUE)
	stringify, _ := goja.AssertFunction(vm.Get("JSON").ToObject(vm).Get("stringify"))
	decode := func(v goja.Value, field string) any {
		raw, err := stringify(goja.Undefined(), v)
		if err != nil {
			panic(vm.NewTypeError("ui.%s: %s", field, err))
		}
		if s, ok := raw.(goja.String); ok && s.Length() <= maxUIText {
			data := s.String()
			if len(data) <= maxUIText {
				val, err := decodeUIJSON(data)
				if err == nil {
					return val
				}
			}
		}
		panic(vm.NewTypeError("ui.%s expects JSON of at most 64 KB", field))
	}
	_ = ui.Set("sprite", func(c goja.FunctionCall) goja.Value {
		rows, err := readUIValue[[]string](decode(c.Argument(0), "sprite rows"), pixelRowsSchema, "sprite rows")
		if err != nil {
			panic(vm.NewTypeError("%s", err))
		}
		opts := uiSpriteOptions{Scale: 1}
		if value := c.Argument(1); !goja.IsUndefined(value) {
			opts, err = readUIValue[uiSpriteOptions](decode(value, "sprite options"), uiSpriteOpts, "sprite options")
			if err != nil {
				panic(vm.NewTypeError("%s", err))
			}
			opts.Scale = max(1, opts.Scale)
		}
		if isExitNode(opts.ID) {
			panic(vm.NewTypeError("ui.sprite control ID is reserved"))
		}
		art, err := compileSprite(rows, opts.Palette, opts.X, opts.Y, opts.Scale)
		if err != nil {
			panic(vm.NewTypeError("%s", err))
		}
		width := len(rows[0]) * opts.Scale
		return vm.ToValue(screenHit(bounds{opts.X, opts.Y, width, len(rows) * opts.Scale}, opts.ID, opts.ID, art))
	})
	_ = ui.Set("sticker", func(c goja.FunctionCall) goja.Value {
		name, err := readUIValue[string](decode(c.Argument(0), "sticker name"), stickerNameSchema, "sticker.name")
		if err != nil {
			panic(vm.NewTypeError("%s", err))
		}
		opts := stickerOptions{}
		if arg := c.Argument(1); !goja.IsUndefined(arg) {
			opts, err = readUIValue[stickerOptions](decode(arg, "sticker options"), uiStickerOpts, "sticker options")
			if err != nil {
				panic(vm.NewTypeError("%s", err))
			}
		}
		if opts.Size == 0 {
			opts.Size = 64
		}
		art, err := compileSticker(name, opts)
		if err != nil {
			panic(vm.NewTypeError("%s", err))
		}
		return vm.ToValue(art)
	})
	_ = ui.Set("screen", func(c goja.FunctionCall) goja.Value {
		spec, err := readUIValue[declarativeScreen](decode(c.Argument(0), "screen"), declarativeScreenSchema, "screen")
		if err != nil {
			panic(vm.NewTypeError("%s", err))
		}
		art, err := spec.svg()
		if err != nil {
			panic(vm.NewTypeError("%s", err))
		}
		return vm.ToValue(art)
	})
	text := func(v goja.Value) string {
		if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
			return ""
		}
		s := v.ToString().(goja.String)
		if s.Length() <= maxUIText {
			if raw := s.String(); len(raw) <= maxUIText {
				return raw
			}
		}
		panic(vm.NewTypeError("ui text exceeds 64 KB"))
	}
	field := func(v goja.Value, key string) string { return text(uiGet(v, key)) }
	arrayLen := func(v goja.Value, key string, limit int) int {
		if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
			return 0
		}
		if obj, ok := v.(*goja.Object); ok && obj.ClassName() == "Array" {
			if n := obj.Get("length").ToInteger(); n >= 0 && n <= int64(limit) {
				return int(n)
			}
		}
		panic(vm.NewTypeError("ui.%s must be an array of at most %d items", key, limit))
	}
	_ = ui.Set("esc", func(c goja.FunctionCall) goja.Value {
		return vm.ToValue(uiEsc(text(c.Argument(0))))
	})
	_ = ui.Set("wrap", func(c goja.FunctionCall) goja.Value {
		width := uiNumber(c.Argument(1), 240, 0, math.Inf(1))
		size := int(uiNumber(c.Argument(2), 16, 1, 120))
		lines := uiWrap(text(c.Argument(0)), width, size, c.Argument(3).ToBoolean(), maxUIText)
		items := make([]any, len(lines))
		for i, line := range lines {
			items[i] = line
		}
		return vm.NewArray(items...)
	})
	_ = ui.Set("pagedText", func(c goja.FunctionCall) goja.Value {
		opts := c.Argument(0)
		title, colors := field(opts, "title"), uiColors(field(opts, "theme"))
		size := int(uiNumber(uiGet(opts, "fontSize"), 15, 14, 18))
		total := len(title)
		var pages []uiPage
		add := func(heading, body string) {
			total += len(body)
			if total > maxUIText {
				panic(vm.NewTypeError("ui.pagedText story exceeds 64 KB"))
			}
			lines := uiWrap(body, 240, size, false, uiLineRunes)
			for chunk := range slices.Chunk(lines, 1+(270-143)/(size+5)) {
				pages = append(pages, uiPage{heading, chunk})
			}
		}
		input := uiGet(opts, "pages")
		if n := arrayLen(input, "pages", 512); n > 0 {
			for i := range n {
				page := uiGet(input, strconv.Itoa(i))
				if s, ok := page.(goja.String); ok {
					add(title, text(s))
					continue
				}
				heading := field(page, "title")
				total += len(heading)
				if heading == "" {
					heading = title
				}
				add(heading, field(page, "text"))
			}
		} else {
			add(title, field(opts, "text"))
		}
		index := func(v goja.Value) int { return int(uiNumber(v, 0, 0, float64(len(pages)-1))) }
		start := index(uiGet(opts, "page"))
		state := func(i int) *goja.Object {
			o := vm.NewObject()
			_ = o.DefineDataProperty("page", vm.ToValue(i), goja.FLAG_TRUE, goja.FLAG_TRUE, goja.FLAG_TRUE)
			return o
		}
		app := vm.NewObject()
		_ = app.Set("init", func(goja.FunctionCall) goja.Value { return state(start) })
		_ = app.Set("render", func(c goja.FunctionCall) goja.Value {
			i := index(uiGet(c.Argument(0), "page"))
			return vm.ToValue(pages[i].svg(i, len(pages), size, colors))
		})
		_ = app.Set("onTap", func(c goja.FunctionCall) goja.Value {
			i := index(uiGet(c.Argument(0), "page"))
			switch text(c.Argument(2)) {
			case "prev":
				i = max(0, i-1)
			case "next":
				i = min(len(pages)-1, i+1)
			default:
				return goja.Undefined()
			}
			tapChime()
			return state(i)
		})
		return app
	})
	_ = ui.Set("choices", func(c goja.FunctionCall) goja.Value {
		opts := c.Argument(0)
		p := uiColors(field(opts, "theme"))
		s := sceneView{bg: p.bg, art: uiRoundRect(48, 68, 264, 222, uiCardRadius, p.card, p.border, 2)}
		s.art += uiLabel(180, 88, 13, 176, field(opts, "badge"), p.ink, true)
		s.art += uiLabel(180, 111, 22, 232, field(opts, "title"), p.ink, true)
		s.art += uiLabel(180, 284, 13, 200, field(opts, "footer"), p.ink, false)
		choices := uiGet(opts, "choices")
		n := arrayLen(choices, "choices", 4)
		lines := uiWrap(field(opts, "prompt"), 232, 15, false, uiLineRunes)
		room := 9
		if n > 1 {
			room = 3
		} else if n == 1 {
			room = 6
		}
		if len(lines) > room {
			m := uiMetrics(15, false)
			lines[room-1] = uiEllipsis(&m, lines[room-1], 232)
			lines = lines[:room]
		}
		for i, line := range lines {
			s.art += uiText(64, 132+17*i, 15, line, p.ink, "start", false)
		}
		seen := map[string]bool{}
		for i := range n {
			choice := uiGet(choices, strconv.Itoa(i))
			id := field(choice, "id")
			if !svgID.MatchString(id) || isExitNode(id) || seen[id] {
				panic(vm.NewTypeError("ui.choices needs unique SVG control IDs (Home is reserved)"))
			}
			seen[id] = true
			b := bounds{64, 236, 232, 36}
			if n == 2 {
				b.y = 182 + 54*i
			} else if n > 2 {
				b = bounds{58 + 131*(i%2), 182 + 54*(i/2), 113, 36}
			}
			colors := p
			if accent := color(field(choice, "accent")); accent != "" {
				colors.accent = accent
			}
			s.art += uiButton(b, id, field(choice, "label"), field(choice, "sub"), colors)
		}
		return vm.ToValue(s.svg(""))
	})
	_ = vm.Set("ui", ui)
}
