package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"image"
	imagecolor "image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/pierrec/lz4/v4"
	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/f64"
	"golang.org/x/image/math/fixed"
)

const screenSize = 360
const maxSVG = 64 << 10
const screenCacheSize = 32

type screenGeometry struct {
	width, height int
	shape         string
}

var roundScreen = screenGeometry{screenSize, screenSize, "round"}

func (s screenGeometry) inverseTap(x, y int) (int, int) {
	side := min(s.width, s.height)
	// Offsets are (dimension - side) / 2. Keep half-pixel offsets exact.
	inverse := func(p, dimension int) int {
		return min(screenSize-1, max(0, (2*p-dimension+side)*screenSize/(2*side)))
	}
	return inverse(x, s.width), inverse(y, s.height)
}

func inScreenCircle(x, y int) bool {
	const r = screenSize / 2
	return (x-r)*(x-r)+(y-r)*(y-r) <= r*r
}

type hitBox struct {
	id             string
	x1, y1, x2, y2 int
	bounds         svgBounds // Transformed control bounds before touch padding and canvas clipping.
}

type svgScreen struct {
	raw, source string
	hits        []hitBox
	hitWarnings []string
	fb          []uint16
	render      time.Duration
	grade       gradeKey
}

// A prepared scene owns a normalized SVG tree and its topmost-first controls.
// Neither backend mutates it.
type preparedScene struct {
	root        *svgNode
	svg         string
	hits        []hitBox
	hitWarnings []string
	background  imagecolor.Color
}

func (s preparedScene) toBrowserSVG() string { return s.svg }

type screenCacheKey struct {
	hash     [sha256.Size]byte
	geometry screenGeometry
	exit     bool
	grade    gradeKey
}

// Cached screens and their slices are immutable after publication.
type screenCache struct {
	mu           sync.Mutex
	entries      map[screenCacheKey]svgScreen
	keys         [screenCacheSize]screenCacheKey
	next         int
	hits, misses uint64
}

var screens screenCache

func buildScreen(raw string, exit bool, geometry screenGeometry) (svgScreen, error) {
	return screens.build(raw, exit, geometry)
}

func (c *screenCache) build(raw string, exit bool, geometry screenGeometry) (svgScreen, error) {
	return c.buildGraded(raw, exit, geometry, gradeDay)
}

func (c *screenCache) buildGraded(raw string, exit bool, geometry screenGeometry, grade gradeKey) (svgScreen, error) {
	return c.buildGradedWithAssets(raw, exit, geometry, grade, nil)
}

func (c *screenCache) buildGradedWithAssets(raw string, exit bool, geometry screenGeometry, grade gradeKey, lookup photoLookup) (svgScreen, error) {
	start := time.Now()
	key := screenCacheKey{svgHash(raw), geometry, exit, grade}
	c.mu.Lock()
	screen, ok := c.entries[key]
	if ok {
		c.hits++
		c.mu.Unlock()
		screen.render = time.Since(start)
		return screen, nil
	}
	c.misses++
	c.mu.Unlock()

	screen, err := buildScreenPaint(raw, exit, geometry, lookup, grade)
	if err != nil {
		return svgScreen{}, err
	}
	c.mu.Lock()
	if cached, ok := c.entries[key]; ok {
		screen = cached
	} else {
		if c.entries == nil {
			c.entries = make(map[screenCacheKey]svgScreen, screenCacheSize)
		}
		if len(c.entries) == screenCacheSize {
			delete(c.entries, c.keys[c.next])
		}
		c.keys[c.next] = key
		c.next = (c.next + 1) % screenCacheSize
		c.entries[key] = screen
	}
	c.mu.Unlock()
	screen.render = time.Since(start)
	return screen, nil
}

func svgHash(raw string) [sha256.Size]byte {
	// Sum256 only reads the input; hashing the immutable string needs no copy.
	return sha256.Sum256(unsafe.Slice(unsafe.StringData(raw), len(raw)))
}

func buildScreenUncached(raw string, exit bool, geometry screenGeometry) (svgScreen, error) {
	return buildScreenPaint(raw, exit, geometry, nil, gradeDay)
}

func buildScreenPaint(raw string, exit bool, geometry screenGeometry, lookup photoLookup, grade gradeKey) (svgScreen, error) {
	scene, err := prepareScene(raw, exit)
	if err != nil {
		return svgScreen{}, err
	}
	if geometry.width == 0 {
		grade = gradeDay
	}
	screen := svgScreen{raw: raw, source: scene.toBrowserSVG(), hits: scene.hits, hitWarnings: scene.hitWarnings, grade: grade}
	if geometry.width != 0 {
		screen.fb, err = scene.toRGB565(geometry, lookup, grade)
	}
	return screen, err
}

func (c *screenCache) stats() (hits, misses uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}

func renderSVG(raw string) ([]uint16, []hitBox, error) {
	s, err := buildScreen(raw, false, roundScreen)
	return s.fb, s.hits, err
}

func hitID(hits []hitBox, x, y int) string {
	for _, h := range hits {
		if x >= h.x1 && x <= h.x2 && y >= h.y1 && y <= h.y2 {
			return h.id
		}
	}
	return ""
}

func isExitNode(id string) bool { return id == exitNode || id == "lark-exit" }

func layoutWarnings(hits []hitBox, round bool) []string {
	var exits []hitBox
	for _, h := range hits {
		if isExitNode(h.id) {
			exits = append(exits, h)
		}
	}
	const r = screenSize / 2
	outside := func(x, y, radius float64) bool {
		return (x-r)*(x-r)+(y-r)*(y-r) > radius*radius
	}
	var warnings []string
	for _, h := range hits {
		if isExitNode(h.id) {
			continue
		}
		if slices.ContainsFunc(exits, func(e hitBox) bool {
			return h.x1 <= e.x2 && e.x1 <= h.x2 && h.y1 <= e.y2 && e.y1 <= h.y2
		}) {
			warnings = append(warnings, fmt.Sprintf("control %q overlaps bottom Home button (keep controls above y=296)", h.id))
		}
		if round {
			b := h.bounds
			dx := max(math.Abs(b.x1-r), math.Abs(b.x2-r))
			dy := max(math.Abs(b.y1-r), math.Abs(b.y2-r))
			if dx*dx+dy*dy > r*r || outside((b.x1+b.x2)/2, (b.y1+b.y2)/2, r-6) {
				warnings = append(warnings, fmt.Sprintf("control %q clips outside the round display bezel", h.id))
			}
		}
	}
	return warnings
}

type svgNode struct {
	tag, text string
	attr      map[string]string
	children  []*svgNode
	pixelRuns []image.Rectangle
}

func (n *svgNode) asGroup() *svgNode {
	n.tag = "g"
	delete(n.attr, "viewBox")
	delete(n.attr, "xmlns")
	return n
}

var (
	svgTags           = wordSet("svg g defs linearGradient radialGradient stop path rect circle ellipse line polygon polyline text image")
	svgAttrs          = wordSet("d x y x1 y1 x2 y2 width height rx ry r cx cy fx fy points fill fill-opacity fill-rule stroke stroke-opacity stroke-width stroke-linecap stroke-linejoin stroke-miterlimit stroke-dasharray stroke-dashoffset opacity transform gradientTransform gradientUnits spreadMethod offset stop-color stop-opacity font-size font-weight font-family text-anchor shape-rendering image-rendering aria-label data-id data-hit data-pose data-part data-pixel-runs data-sticker")
	svgNumber         = regexp.MustCompile(`[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?`)
	svgPath           = regexp.MustCompile(`^[MmZzLlHhVvCcSsQqTtAa0-9Ee.,+\s-]*$`)
	svgScalar         = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?(?:%|px)?$`)
	svgList           = regexp.MustCompile(`^[0-9Ee.,+\s-]+$`)
	svgPaint          = regexp.MustCompile(`^(?:[A-Za-z]+|#[0-9A-Fa-f]{3}|#[0-9A-Fa-f]{6}|rgb\([0-9.,% ]+\))$`)
	svgRef            = regexp.MustCompile(`^url\(#([A-Za-z0-9_.-]{1,64})\)$`)
	svgID             = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)
	svgTransform      = regexp.MustCompile(`^\s*(matrix|translate|scale|rotate|skewX|skewY)\s*\(([0-9Ee.,+\s-]+)\)\s*,?`)
	regularSVGFont, _ = sfnt.Parse(goregular.TTF)
	monoSVGFont, _    = sfnt.Parse(gomono.TTF)
	boldSVGFont, _    = sfnt.Parse(gobold.TTF)
)

func wordSet(words string) map[string]bool {
	m := map[string]bool{}
	for _, word := range strings.Fields(words) {
		m[word] = true
	}
	return m
}

func svgNums(s string) []float64 {
	var nums []float64
	for _, raw := range svgNumber.FindAllString(s, -1) {
		v, _ := strconv.ParseFloat(raw, 64)
		nums = append(nums, v)
	}
	return nums
}

func svgNum(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSuffix(s, "px"), 64)
	return v
}

func svgMatrix(raw string) (rasterx.Matrix2D, bool) {
	m := rasterx.Identity
	for len(strings.TrimSpace(raw)) != 0 {
		part := svgTransform.FindStringSubmatch(raw)
		if part == nil {
			return m, false
		}
		n := svgNums(part[2])
		raw = raw[len(part[0]):]
		switch part[1] {
		case "matrix":
			if len(n) != 6 {
				return m, false
			}
			m = m.Mult(rasterx.Matrix2D{A: n[0], B: n[1], C: n[2], D: n[3], E: n[4], F: n[5]})
		case "translate":
			if len(n) < 1 || len(n) > 2 {
				return m, false
			}
			m = m.Translate(n[0], append(n, 0)[1])
		case "scale":
			if len(n) < 1 || len(n) > 2 {
				return m, false
			}
			m = m.Scale(n[0], append(n, n[0])[1])
		case "rotate":
			if len(n) != 1 && len(n) != 3 {
				return m, false
			}
			if len(n) == 3 {
				m = m.Translate(n[1], n[2])
			}
			m = m.Rotate(n[0] * math.Pi / 180)
			if len(n) == 3 {
				m = m.Translate(-n[1], -n[2])
			}
		case "skewX", "skewY":
			if len(n) != 1 {
				return m, false
			}
			t := math.Tan(n[0] * math.Pi / 180)
			if part[1] == "skewX" {
				m = m.Mult(rasterx.Matrix2D{A: 1, C: t, D: 1})
			} else {
				m = m.Mult(rasterx.Matrix2D{A: 1, B: t, D: 1})
			}
		}
	}
	return m, boundedSVGMatrix(m)
}

func boundedSVGMatrix(m rasterx.Matrix2D) bool {
	for _, n := range []float64{m.A, m.B, m.C, m.D} {
		if !(math.Abs(n) <= 12) {
			return false
		}
	}
	return math.Abs(m.E) <= 4096 && math.Abs(m.F) <= 4096
}

func cleanSVGAttr(k, v string) (string, bool) {
	switch k {
	case "id":
		return v, svgID.MatchString(v)
	case "data-id", "data-part", "data-sticker":
		return v, svgID.MatchString(v)
	case "aria-label":
		return shortText(v, 128, false), v != ""
	case "data-pose":
		return v, true
	case "shape-rendering":
		return v, v == "crispEdges"
	case "image-rendering":
		return v, v == "pixelated"
	case "data-pixel-runs":
		return v, v == "1"
	case "fill", "stroke", "stop-color":
		if k != "stop-color" && strings.EqualFold(v, "transparent") {
			return "none", true
		}
		return v, svgPaint.MatchString(v) || k != "stop-color" && svgRef.MatchString(v)
	case "fill-rule", "stroke-linecap", "stroke-linejoin", "gradientUnits", "spreadMethod", "font-weight", "text-anchor":
		return v, svgPaint.MatchString(v) || k == "font-weight" && svgScalar.MatchString(v)
	case "font-family":
		return v, v == "monospace"
	case "transform", "gradientTransform":
		m, ok := svgMatrix(v)
		if !ok {
			return "", false
		}
		// oksvg collapses a one-argument scale on Y; share our SVG matrix with the rasterizer.
		return fmt.Sprintf("matrix(%g %g %g %g %g %g)", m.A, m.B, m.C, m.D, m.E, m.F), true
	case "d":
		if !svgPath.MatchString(v) {
			return "", false
		}
	case "stroke-dasharray":
		if v == "none" {
			return v, true
		}
		if !svgList.MatchString(v) {
			return "", false
		}
		for _, n := range svgNums(v) {
			if n < 0 || n > 360 {
				return "", false
			}
		}
		if !slices.ContainsFunc(svgNums(v), func(n float64) bool { return n >= 1 }) {
			return "", false
		}
	case "points", "data-hit":
		if !svgList.MatchString(v) {
			return "", false
		}
	default:
		// Canonical integer sprite geometry needs no regex or substring scan.
		if len(v) <= 5 && (k == "x" || k == "y" || k == "width" || k == "height") {
			if n, err := strconv.Atoi(v); err == nil {
				return v, n >= -4096 && n <= 4096
			}
		}
		if !svgScalar.MatchString(v) {
			return "", false
		}
	}
	for _, n := range svgNums(v) {
		if math.Abs(n) > 4096 {
			return "", false
		}
	}
	return v, true
}

func parseSVG(raw string) (*svgNode, error) {
	if len(raw) > maxSVG {
		return nil, fmt.Errorf("SVG exceeds 64 KB")
	}
	dec := xml.NewDecoder(strings.NewReader(raw))
	var root *svgNode
	var stack []*svgNode
	nodes, photos := 0, 0
	for {
		token, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("SVG: %w", err)
		}
		switch t := token.(type) {
		case xml.StartElement:
			if len(stack) == 0 && (root != nil || t.Name.Local != "svg") {
				return nil, fmt.Errorf("SVG requires one <svg viewBox=\"0 0 360 360\"> root")
			}
			tag := t.Name.Local
			if len(stack) > 24 || nodes >= 512 {
				return nil, fmt.Errorf("SVG too complex")
			}
			if !svgTags[tag] || t.Name.Space != "" && t.Name.Space != "http://www.w3.org/2000/svg" ||
				len(stack) != 0 && (tag == "svg" || stack[len(stack)-1].tag == "text" || stack[len(stack)-1].tag == "image") {
				if len(stack) == 0 {
					return nil, fmt.Errorf("Invalid SVG namespace")
				}
				if err := dec.Skip(); err != nil {
					return nil, fmt.Errorf("SVG: %w", err)
				}
				continue
			}
			n := &svgNode{tag: tag, attr: map[string]string{}}
			pixelPath := tag == "path" && slices.ContainsFunc(t.Attr, func(a xml.Attr) bool {
				return a.Name.Space == "" && a.Name.Local == "data-pixel-runs" && strings.TrimSpace(a.Value) == "1"
			})
			for _, a := range t.Attr {
				k, v := a.Name.Local, strings.TrimSpace(a.Value)
				if a.Name.Space != "" {
					continue
				}
				if k == "d" && pixelPath {
					if runs, ok := parsePixelRuns(v); ok {
						n.attr[k], n.pixelRuns = v, runs
						continue
					}
					n.pixelRuns = nil
				}
				if tag == "image" && k == "href" {
					if _, _, isPortrait := parsePortraitRef(v); isPortrait {
						n.attr[k] = v
					}
				}
				if len(stack) == 0 && k == "viewBox" && !slices.Equal(svgNums(v), []float64{0, 0, 360, 360}) {
					return nil, fmt.Errorf(`SVG viewBox must be "0 0 360 360"`)
				}
				if svgAttrs[k] || k == "id" && (tag == "linearGradient" || tag == "radialGradient") {
					if k == "data-part" && tag != "g" {
						continue
					}
					if tag == "image" && strings.HasSuffix(v, "%") && (k == "x" || k == "y" || k == "width" || k == "height") {
						continue
					}
					if v, ok := cleanSVGAttr(k, v); ok {
						n.attr[k] = v
					}
				}
			}
			if tag == "image" {
				if n.attr["href"] == "" {
					if err := dec.Skip(); err != nil {
						return nil, fmt.Errorf("SVG: %w", err)
					}
					continue
				}
				photos++
				if photos > 8 {
					return nil, fmt.Errorf("SVG exceeds 8 image references")
				}
			}
			if len(stack) == 0 {
				root = n
				root.attr["viewBox"] = "0 0 360 360"
				root.attr["xmlns"] = "http://www.w3.org/2000/svg"
				delete(root.attr, "width")
				delete(root.attr, "height")
			} else {
				parent := stack[len(stack)-1]
				if parent.tag == "defs" && tag != "linearGradient" && tag != "radialGradient" ||
					(parent.tag == "linearGradient" || parent.tag == "radialGradient") && tag != "stop" {
					if err := dec.Skip(); err != nil {
						return nil, fmt.Errorf("SVG: %w", err)
					}
					continue
				}
				parent.children = append(parent.children, n)
			}
			stack = append(stack, n)
			nodes++
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 && stack[len(stack)-1].tag == "text" {
				n := stack[len(stack)-1]
				n.text += string(t)
				if runeLen(n.text) > 256 {
					return nil, fmt.Errorf("SVG text exceeds 256 characters")
				}
			} else if len(stack) == 0 && strings.TrimSpace(string(t)) != "" {
				return nil, fmt.Errorf("Unexpected text outside SVG")
			}
		}
	}
	if root == nil {
		return nil, fmt.Errorf("Missing SVG")
	}
	return root, nil
}

func (n *svgNode) encode(enc *xml.Encoder) {
	start := xml.StartElement{Name: xml.Name{Local: n.tag}}
	keys := make([]string, 0, len(n.attr))
	for k := range n.attr {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: k}, Value: n.attr[k]})
	}
	_ = enc.EncodeToken(start)
	if n.text != "" {
		_ = enc.EncodeToken(xml.CharData(n.text))
	}
	for _, child := range n.children {
		child.encode(enc)
	}
	_ = enc.EncodeToken(start.End())
}

func svgString(n *svgNode) string {
	return encodeSVG(n, "")
}

func encodeSVG(n *svgNode, indent string) string {
	var out strings.Builder
	enc := xml.NewEncoder(&out)
	enc.Indent("", indent)
	n.encode(enc)
	_ = enc.Flush()
	return out.String()
}

type svgBounds struct{ x1, y1, x2, y2 float64 }

func (n *svgNode) bounds() (svgBounds, bool) {
	v := func(k string) float64 { return svgNum(n.attr[k]) }
	x, y := v("x"), v("y")
	if p := svgNums(n.attr["data-hit"]); len(p) == 4 && p[2] > 0 && p[3] > 0 {
		return svgBounds{p[0], p[1], p[0] + p[2], p[1] + p[3]}, true
	}
	switch n.tag {
	case "rect", "image":
		return svgBounds{x, y, x + v("width"), y + v("height")}, v("width") > 0 && v("height") > 0
	case "circle", "ellipse":
		x, y = v("cx"), v("cy")
		rx, ry := v("r"), v("r")
		if n.tag == "ellipse" {
			rx, ry = v("rx"), v("ry")
		}
		return svgBounds{x - rx, y - ry, x + rx, y + ry}, rx > 0 && ry > 0
	case "polygon", "polyline":
		p := svgNums(n.attr["points"])
		if len(p) < 4 || len(p)%2 != 0 {
			break
		}
		b := svgBounds{p[0], p[1], p[0], p[1]}
		for i := 2; i < len(p); i += 2 {
			b = svgBounds{min(b.x1, p[i]), min(b.y1, p[i+1]), max(b.x2, p[i]), max(b.y2, p[i+1])}
		}
		return b, true
	case "line":
		return svgBounds{min(v("x1"), v("x2")), min(v("y1"), v("y2")), max(v("x1"), v("x2")), max(v("y1"), v("y2"))}, true
	}
	return svgBounds{}, false
}

// Outlining text at its original z-order gives browser and board identical Go
// font metrics without requiring a font download or a separate text renderer.
type svgTextStyle [4]string

var svgTextAttrs = [...]string{"font-size", "font-weight", "font-family", "text-anchor"}

type svgGlyphMetrics struct {
	font *sfnt.Font
	size fixed.Int26_6
	buf  sfnt.Buffer
}

func (m *svgGlyphMetrics) glyph(r rune, previous sfnt.GlyphIndex, kern bool) (id sfnt.GlyphIndex, spacing, advance fixed.Int26_6) {
	id, _ = m.font.GlyphIndex(&m.buf, r)
	if kern {
		spacing, _ = m.font.Kern(&m.buf, previous, id, m.size, font.HintingNone)
	}
	advance, _ = m.font.GlyphAdvance(&m.buf, id, m.size, font.HintingNone)
	return
}

func labelWidth(text string, size int) int {
	var buf sfnt.Buffer
	ppem := fixed.I(size)
	var advance fixed.Int26_6
	for _, r := range text {
		glyph, _ := boldSVGFont.GlyphIndex(&buf, r)
		w, _ := boldSVGFont.GlyphAdvance(&buf, glyph, ppem, font.HintingNone)
		advance += w
	}
	return advance.Ceil()
}

func fitLabel(text string, size, width int) int {
	return min(size, size*width/max(1, labelWidth(text, size)))
}

func outlineText(n *svgNode, style svgTextStyle) (svgBounds, bool) {
	f := regularSVGFont
	size, weight, family, anchor := svgNum(style[0]), style[1], style[2], style[3]
	if family == "monospace" {
		f = monoSVGFont
	} else if weight == "bold" || svgNum(weight) >= 600 {
		f = boldSVGFont
	}
	if size == 0 {
		size = 16
	}
	size = min(120, max(1, size))
	ppem := fixed.Int26_6(size * 64)
	x, y := svgNum(n.attr["x"]), svgNum(n.attr["y"])
	b := svgBounds{math.Inf(1), y - size, math.Inf(-1), y + size*.22}
	metrics := svgGlyphMetrics{font: f, size: ppem}
	var d strings.Builder
	compact := func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }
	for _, line := range strings.Split(strings.TrimSpace(n.text), "\n") {
		chars := []rune(strings.Join(strings.Fields(line), " "))
		glyphs := make([]sfnt.GlyphIndex, len(chars))
		offsets := make([]fixed.Int26_6, len(chars))
		var width fixed.Int26_6
		var previous sfnt.GlyphIndex
		for i, r := range chars {
			id, spacing, advance := metrics.glyph(r, previous, i > 0)
			glyphs[i], offsets[i], previous = id, width+spacing, id
			width += spacing + advance
		}
		start := x
		w := float64(width) / 64
		switch anchor {
		case "middle":
			start -= w / 2
		case "end":
			start -= w
		}
		b.x1, b.x2, b.y2 = min(b.x1, start), max(b.x2, start+w), y+size*.22
		for i, glyph := range glyphs {
			segments, _ := f.LoadGlyph(&metrics.buf, glyph, ppem, nil)
			for j, s := range segments {
				if j > 0 && s.Op == sfnt.SegmentOpMoveTo {
					d.WriteByte('Z')
				}
				d.WriteByte("MLQC"[s.Op])
				count := 1
				if s.Op >= sfnt.SegmentOpQuadTo {
					count = int(s.Op)
				}
				for _, p := range s.Args[:count] {
					d.WriteString(compact(start + float64(offsets[i]+p.X)/64))
					d.WriteByte(' ')
					d.WriteString(compact(y + float64(p.Y)/64))
					d.WriteByte(' ')
				}
			}
			if len(segments) != 0 {
				d.WriteByte('Z')
			}
		}
		y += size * 1.2
	}
	n.attr["aria-label"] = strings.Join(strings.Fields(n.text), " ")
	n.tag, n.text, n.attr["d"] = "path", "", d.String()
	return b, b.x2 > b.x1
}

func prepareScene(raw string, exit bool) (preparedScene, error) {
	root, err := parseSVG(raw)
	if err != nil {
		return preparedScene{}, err
	}
	var hits []hitBox
	var hitWarnings []string
	hasExit := false
	var walk func(*svgNode, rasterx.Matrix2D, svgTextStyle, bool, int) error
	walk = func(n *svgNode, m rasterx.Matrix2D, style svgTextStyle, defs bool, pad int) error {
		defs = defs || n.tag == "defs" || n.tag == "linearGradient" || n.tag == "radialGradient" || n.tag == "stop"
		if raw := n.attr["transform"]; raw != "" {
			local, _ := svgMatrix(raw)
			m = m.Mult(local)
			if !boundedSVGMatrix(m) {
				return fmt.Errorf("SVG combined transform is too large")
			}
		}
		if n.attr["id"] != "" && !strings.HasPrefix(n.attr["id"], "lark-paint-") {
			n.attr["id"] = "lark-paint-" + n.attr["id"]
		}
		for _, k := range []string{"fill", "stroke"} {
			if ref := n.attr[k]; strings.HasPrefix(ref, "url(#") && !strings.HasPrefix(ref, "url(#lark-paint-") {
				n.attr[k] = "url(#lark-paint-" + ref[5:]
			}
		}
		for i, k := range svgTextAttrs {
			if v := n.attr[k]; v != "" {
				style[i] = v
				delete(n.attr, k)
			}
		}
		id := n.attr["data-id"]
		var b svgBounds
		var ok bool
		if id != "" && !defs {
			b, ok = n.bounds()
		}
		if n.tag == "text" {
			metrics, measured := outlineText(n, style)
			if !ok {
				b, ok = metrics, measured
			}
			if len(n.attr["d"]) > 64<<10 {
				return fmt.Errorf("SVG text outlines too large")
			}
		}
		if !defs && id != "" && ok {
			out := svgBounds{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
			for _, p := range [][2]float64{{b.x1, b.y1}, {b.x2, b.y1}, {b.x1, b.y2}, {b.x2, b.y2}} {
				x, y := m.Transform(p[0], p[1])
				out = svgBounds{min(x, out.x1), min(y, out.y1), max(x, out.x2), max(y, out.y2)}
			}
			const last = screenSize - 1
			h := hitBox{id, max(0, int(math.Floor(out.x1))-pad), max(0, int(math.Floor(out.y1))-pad),
				min(last, int(math.Ceil(out.x2))+pad), min(last, int(math.Ceil(out.y2))+pad), out}
			if h.x1 <= h.x2 && h.y1 <= h.y2 {
				hits = append(hits, h)
				hasExit = hasExit || isExitNode(id)
				// The browser uses these logical bounds, not DOM geometry, for pointer and keyboard input.
				n.attr["data-lark-hit"] = fmt.Sprintf("%d %d %d %d", h.x1, h.y1, h.x2, h.y2)
			}
		}
		if n.attr["data-lark-hit"] == "" {
			if id != "" && !defs && !ok {
				hitWarnings = append(hitWarnings, fmt.Sprintf(`control %q has data-id but no measurable bounds — add data-hit="x,y,w,h" or put data-id on a rect/circle/text`, id))
			}
			delete(n.attr, "data-id")
		}
		for _, child := range n.children {
			if err := walk(child, m, style, defs, pad); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root, rasterx.Identity, svgTextStyle{}, false, 8); err != nil {
		return preparedScene{}, err
	}
	background := root.background()
	if exit && !hasExit {
		pill, _ := parseSVG(`<svg><g data-id="lark-exit" data-hit="137,304,86,32"><rect x="137" y="304" width="86" height="32" rx="16" ry="16" fill="#FFF2E7" stroke="#76463F" stroke-width="3"/><text x="180" y="326" text-anchor="middle" font-size="19" font-weight="bold" fill="#4C3036">Home</text></g></svg>`)
		_ = walk(pill, rasterx.Identity, svgTextStyle{}, false, 4)
		if root.attr["transform"] != "" || root.attr["opacity"] != "" {
			// Keep the bridge's escape hatch outside the author's transform/opacity.
			pill.children = append([]*svgNode{root.asGroup()}, pill.children...)
			root = pill
		} else {
			root.children = append(root.children, pill.children...)
		}
	}
	slices.Reverse(hits)
	return preparedScene{root: root, svg: svgString(root), hits: hits, hitWarnings: hitWarnings, background: background}, nil
}

// A leading, solid full-viewBox rectangle is the SVG canvas background.
func (root *svgNode) background() imagecolor.Color {
	for _, n := range root.children {
		if n.tag == "defs" {
			continue
		}
		b, _ := n.bounds()
		if n.tag == "rect" && b == (svgBounds{0, 0, screenSize, screenSize}) &&
			n.attr["transform"] == "" && root.attr["transform"] == "" {
			fill := n.attr["fill"]
			if fill == "" {
				fill = root.attr["fill"]
			}
			if c, err := oksvg.ParseSVGColor(fill); c != nil && err == nil {
				opacity := 1.0
				for _, attrs := range []map[string]string{root.attr, n.attr} {
					for _, key := range []string{"opacity", "fill-opacity"} {
						if v := attrs[key]; v != "" {
							opacity *= min(1, max(0, svgNum(v)))
						}
					}
				}
				r, g, b, _ := c.RGBA()
				return imagecolor.RGBA{uint8(float64(r>>8) * opacity), uint8(float64(g>>8) * opacity), uint8(float64(b>>8) * opacity), 255}
			}
		}
		break
	}
	return imagecolor.Black
}

type screenScanner struct {
	*rasterx.ScannerGV
	dst *image.RGBA
}

func (s screenScanner) Draw() {
	// ScannerGV composites the whole destination each time. Shrink the
	// destination without moving its origin: the coverage mask and gradient
	// coordinates still belong to the original canvas.
	extent := s.GetPathExtent()
	dst := *s.dst
	dst.Rect.Max = image.Pt(min(dst.Rect.Max.X, extent.Max.X.Ceil()+1), min(dst.Rect.Max.Y, extent.Max.Y.Ceil()+1))
	if !dst.Rect.Empty() {
		s.Dest = &dst
		s.ScannerGV.Draw()
	}
}

func (s preparedScene) raster(w, h int, x, y float64, side int, fill imagecolor.Color, lookup photoLookup) (img *image.RGBA, err error) {
	// oksvg and rasterx are also fed untrusted geometry.
	defer func() {
		if recover() != nil {
			img, err = nil, fmt.Errorf("Invalid SVG geometry")
		}
	}()
	img = image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(fill), image.Point{}, draw.Src)
	var dasher *rasterx.Dasher
	drawSVG := func(svg string) error {
		icon, err := oksvg.ReadIconStream(strings.NewReader(svg), oksvg.StrictErrorMode)
		if err != nil {
			return fmt.Errorf("SVG: %w", err)
		}
		icon.SetTarget(x, y, float64(side), float64(side))
		if dasher == nil {
			scan := screenScanner{rasterx.NewScannerGV(w, h, img, img.Bounds()), img}
			dasher = rasterx.NewDasher(w, h, scan)
		}
		icon.Draw(dasher, 1)
		return nil
	}
	photo := strings.Contains(s.svg, "<image ")
	if !photo && strings.Contains(s.svg, "Gradient") && !canSplitPaintServers(s.root) {
		// Nonstandard/nested paint servers need the original document order.
		return img, drawSVG(s.svg)
	}
	scale := float64(side) / screenSize
	err = rasterLayers(s, img, rasterx.Identity.Translate(x, y).Scale(scale, scale), photo, lookup, drawSVG)
	return img, err
}

func (s preparedScene) toRGB565(screen screenGeometry, lookup photoLookup, grade gradeKey) ([]uint16, error) {
	w, h := screen.width, screen.height
	side := min(w, h)
	x, y := float64(w-side)/2, float64(h-side)/2
	img, err := s.raster(w, h, x, y, side, imagecolor.Black, lookup)
	if err != nil {
		return nil, err
	}
	fb := make([]uint16, w*h)
	br, bgc, bb, _ := s.background.RGBA()
	lut := &atmosphereLUTs[grade]
	bar := pack565(lut[0][br>>8], lut[1][bgc>>8], lut[2][bb>>8])
	pix := img.Pix
	stride := img.Stride
	if screen.shape == "rect" {
		clipX0, clipY0 := int(x), int(y)
		clipX1, clipY1 := int(math.Ceil(x+float64(side))), int(math.Ceil(y+float64(side)))
		for py := range h {
			rowFB := fb[py*w : (py+1)*w]
			if py < clipY0 || py >= clipY1 {
				for px := range w {
					rowFB[px] = bar
				}
				continue
			}
			rowPix := pix[py*stride:]
			for px := range clipX0 {
				rowFB[px] = bar
			}
			if grade == gradeDay {
				for px := clipX0; px < clipX1; px++ {
					i := px * 4
					rowFB[px] = pack565(rowPix[i], rowPix[i+1], rowPix[i+2])
				}
			} else {
				for px := clipX0; px < clipX1; px++ {
					i := px * 4
					rowFB[px] = pack565(lut[0][rowPix[i]], lut[1][rowPix[i+1]], lut[2][rowPix[i+2]])
				}
			}
			for px := clipX1; px < w; px++ {
				rowFB[px] = bar
			}
		}
		return fb, nil
	}
	sideSq := side * side
	for py := range h {
		dy := 2*py - h
		dySq := dy * dy
		rowFB := fb[py*w : (py+1)*w]
		rowPix := pix[py*stride:]
		if grade == gradeDay {
			for px := range w {
				dx := 2*px - w
				if dx*dx+dySq <= sideSq {
					i := px * 4
					rowFB[px] = pack565(rowPix[i], rowPix[i+1], rowPix[i+2])
				}
			}
		} else {
			for px := range w {
				dx := 2*px - w
				if dx*dx+dySq <= sideSq {
					i := px * 4
					rowFB[px] = pack565(lut[0][rowPix[i]], lut[1][rowPix[i+1]], lut[2][rowPix[i+2]])
				}
			}
		}
	}
	return fb, nil
}

func pack565(r, g, b uint8) uint16 {
	return uint16(r&0xf8)<<8 | uint16(g&0xfc)<<3 | uint16(b)>>3
}

type photoLookup func(string) (image.Image, error)

func directStyle(n *svgNode) bool {
	for k, v := range n.attr {
		switch k {
		case "x", "y", "width", "height", "viewBox", "xmlns", "transform", "shape-rendering",
			"aria-label", "data-id", "data-hit", "data-lark-hit", "data-pose", "data-part", "data-sticker":
		case "d", "data-pixel-runs":
			if len(n.pixelRuns) == 0 {
				return false
			}
		case "fill":
			if strings.HasPrefix(v, "url(") {
				return false
			}
			if v != "none" && !strings.HasPrefix(v, "#") {
				if c, err := oksvg.ParseSVGColor(v); err != nil || c == nil {
					return false
				}
			}
		case "stroke":
			if v != "none" {
				return false
			}
		case "opacity", "fill-opacity":
			if v != "1" {
				return false
			}
		case "stroke-width", "rx", "ry":
			if v != "0" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func canSplitPaintServers(root *svgNode) bool {
	// Only hoist top-level defs preceded by simple rectangles with no
	// paint-server references. Let oksvg resolve other cases in place.
	var nested func(*svgNode) bool
	nested = func(n *svgNode) bool {
		return n.tag == "defs" || n.tag == "linearGradient" || n.tag == "radialGradient" ||
			slices.ContainsFunc(n.children, nested)
	}
	precededByVector := false
	for _, n := range root.children {
		if n.tag == "defs" {
			if precededByVector {
				return false
			}
		} else {
			if nested(n) {
				return false
			}
			precededByVector = precededByVector || n.tag != "rect" || len(n.children) != 0 || !directStyle(n)
		}
	}
	return true
}

func directPaint(fill string) (imagecolor.RGBA, bool) {
	var c imagecolor.RGBA
	if len(fill) == 7 && fill[0] == '#' {
		rgb, err := strconv.ParseUint(fill[1:], 16, 24)
		if err != nil {
			return c, false
		}
		c = imagecolor.RGBA{uint8(rgb >> 16), uint8(rgb >> 8), uint8(rgb), 255}
	} else {
		paint, err := oksvg.ParseSVGColor(fill)
		if err != nil || paint == nil {
			return c, false
		}
		r, g, b, a := paint.RGBA()
		if a != 0xffff {
			return c, false
		}
		c = imagecolor.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), 255}
	}
	return c, true
}

func directRect(n *svgNode, m rasterx.Matrix2D, crisp bool) (image.Rectangle, bool) {
	var xywh [4]float64
	for i, name := range [...]string{"x", "y", "width", "height"} {
		raw := n.attr[name]
		if strings.HasSuffix(raw, "%") {
			return image.Rectangle{}, false
		}
		xywh[i] = svgNum(raw)
	}
	x, y, w, h := xywh[0], xywh[1], xywh[2], xywh[3]
	if w <= 0 || h <= 0 {
		return image.Rectangle{}, false
	}
	x1, y1 := m.Transform(x, y)
	x2, y2 := m.Transform(x+w, y+h)
	if !crisp {
		for _, v := range [...]float64{x1, y1, x2, y2} {
			if math.Abs(v-math.Round(v)) > 1e-9 {
				return image.Rectangle{}, false
			}
		}
	}
	// Opt-in pixel sprites snap both sides to the same shared grid at
	// fractional display scales. Legacy SVG retains its antialiasing.
	return image.Rect(int(math.Round(x1)), int(math.Round(y1)), int(math.Round(x2)), int(math.Round(y2))), true
}

func fillRect(dst *image.RGBA, r image.Rectangle, c imagecolor.RGBA) {
	r = r.Intersect(dst.Bounds())
	if r.Empty() {
		return
	}
	offset, bytes := dst.PixOffset(r.Min.X, r.Min.Y), r.Dx()*4
	row := dst.Pix[offset : offset+bytes]
	for i := 0; i < bytes; i += 4 {
		p := row[i : i+4]
		p[0], p[1], p[2], p[3] = c.R, c.G, c.B, 255
	}
	for y := r.Min.Y + 1; y < r.Max.Y; y++ {
		offset += dst.Stride
		copy(dst.Pix[offset:offset+bytes], row)
	}
}

// Flush each consecutive vector run before painting a bitmap or simple
// rectangle directly. Recreate the run's ancestors so it inherits SVG styles.
func rasterLayers(s preparedScene, dst *image.RGBA, target rasterx.Matrix2D, photo bool, lookup photoLookup, drawSVG func(string) error) error {
	root := s.root
	var defs []*svgNode
	var collectDefs func(*svgNode)
	collectDefs = func(n *svgNode) {
		for _, child := range n.children {
			if child.tag == "defs" {
				defs = append(defs, child)
			} else {
				collectDefs(child)
			}
		}
	}
	if photo || strings.Contains(s.svg, "<defs") {
		collectDefs(root)
	}
	layer := &svgNode{tag: root.tag, attr: root.attr, children: slices.Clone(defs)}
	groups := map[*svgNode]*svgNode{root: layer}
	split, drewVector := photo, false
	flush := func() error {
		if len(layer.children) == len(defs) {
			return nil
		}
		err := drawSVG(svgString(layer))
		layer.children = layer.children[:len(defs)]
		clear(groups)
		groups[root] = layer
		drewVector = true
		return err
	}
	images := make(map[string]image.Image)
	var walk func(*svgNode, []*svgNode, rasterx.Matrix2D, float64, bool, bool) error
	walk = func(n *svgNode, parents []*svgNode, m rasterx.Matrix2D, opacity float64, simple, crisp bool) error {
		if raw := n.attr["transform"]; raw != "" {
			local, _ := svgMatrix(raw)
			m = m.Mult(local)
		}
		if s := n.attr["opacity"]; s != "" {
			opacity *= min(1, max(0, svgNum(s)))
		}
		crisp = crisp || n.attr["shape-rendering"] == "crispEdges"
		switch n.tag {
		case "defs":
		case "svg", "g":
			simple = simple && directStyle(n)
			for _, child := range n.children {
				if err := walk(child, append(parents, n), m, opacity, simple, crisp); err != nil {
					return err
				}
			}
		case "image":
			if lookup == nil || opacity <= 0 {
				return nil
			}
			width, height := svgNum(n.attr["width"]), svgNum(n.attr["height"])
			if width <= 0 || height <= 0 {
				return nil
			}
			id := n.attr["href"]
			src, ok := images[id]
			if !ok {
				var err error
				src, err = lookup(id)
				if err != nil {
					return err
				}
				images[id] = src
			}
			if src == nil {
				return nil
			}
			if err := flush(); err != nil {
				return err
			}
			b := src.Bounds()
			scale := min(width/float64(b.Dx()), height/float64(b.Dy()))
			x := svgNum(n.attr["x"]) + (width-scale*float64(b.Dx()))/2
			y := svgNum(n.attr["y"]) + (height-scale*float64(b.Dy()))/2
			m = m.Translate(x, y).Scale(scale, scale).Translate(-float64(b.Min.X), -float64(b.Min.Y))
			if m.A*m.D-m.B*m.C != 0 {
				var opts *xdraw.Options
				if opacity < 1 {
					opts = &xdraw.Options{SrcMask: image.NewUniform(imagecolor.Alpha{A: uint8(255 * opacity)})}
				}
				var interpolate xdraw.Interpolator = xdraw.BiLinear
				if n.attr["image-rendering"] == "pixelated" {
					interpolate = xdraw.NearestNeighbor
				}
				interpolate.Transform(dst, f64.Aff3{m.A, m.C, m.E, m.B, m.D, m.F}, src, b, draw.Over, opts)
			}
		default:
			if (n.tag == "rect" || len(n.pixelRuns) > 0) && simple && m.B == 0 && m.C == 0 && len(n.children) == 0 && directStyle(n) {
				if c, ok := directPaint(n.attr["fill"]); ok {
					if r, ok := directRect(n, m, crisp); n.tag == "rect" && ok {
						if err := flush(); err != nil {
							return err
						}
						split = true
						fillRect(dst, r, c)
						return nil
					} else if crisp && len(n.pixelRuns) > 0 {
						if err := flush(); err != nil {
							return err
						}
						split = true
						var xs, ys [maxSpriteSide + 1]int
						for i := range xs {
							xs[i] = int(math.Round(m.A*float64(i) + m.E))
							ys[i] = int(math.Round(m.D*float64(i) + m.F))
						}
						for _, r := range n.pixelRuns {
							fillRect(dst, image.Rect(xs[r.Min.X], ys[r.Min.Y], xs[r.Max.X], ys[r.Max.Y]), c)
						}
						return nil
					}
				}
			}
			parent := layer
			for _, ancestor := range parents {
				group := groups[ancestor]
				if group == nil {
					group = &svgNode{tag: ancestor.tag, attr: ancestor.attr}
					parent.children = append(parent.children, group)
					groups[ancestor] = group
				}
				parent = group
			}
			parent.children = append(parent.children, n)
		}
		return nil
	}
	if err := walk(root, nil, target, 1, true, false); err != nil {
		return err
	}
	if !split {
		return drawSVG(s.svg)
	}
	if len(layer.children) == len(defs) && len(defs) > 0 && !drewVector {
		return drawSVG(svgString(layer))
	}
	return flush()
}

const (
	frameCodecRaw    byte = 0
	frameCodecRLE565 byte = 1
	frameCodecJPEG   byte = 2
	frameCodecLZ4    byte = 3
)

func dirtyRect(prev, next []uint16, screen screenGeometry) image.Rectangle {
	w, h := screen.width, screen.height
	if prev == nil {
		return image.Rect(0, 0, w, h)
	}
	minX, minY, maxX, maxY := w, h, 0, 0
	for y := range h {
		off := y * w
		prevRow := prev[off : off+w]
		nextRow := next[off : off+w]
		for x := range w {
			if nextRow[x] != prevRow[x] {
				minX = min(minX, x)
				minY = min(minY, y)
				maxX = max(maxX, x+1)
				maxY = max(maxY, y+1)
			}
		}
	}
	if minX >= maxX {
		return image.Rectangle{}
	}
	return image.Rect(minX, minY, maxX, maxY)
}

func encodeRLE565FromBE(out []byte, rawBE []byte, pixels int) []byte {
	for i := 0; i < pixels; {
		start := i
		i++
		sOff := start * 2
		hi, lo := rawBE[sOff], rawBE[sOff+1]
		if i < pixels && rawBE[i*2] == hi && rawBE[i*2+1] == lo {
			for i < pixels && i-start < 128 && rawBE[i*2] == hi && rawBE[i*2+1] == lo {
				i++
			}
			out = append(out, 0x80|byte(i-start-1), hi, lo)
		} else {
			for i < pixels && i-start < 128 && (i+1 == pixels || rawBE[i*2] != rawBE[(i+1)*2] || rawBE[i*2+1] != rawBE[(i+1)*2+1]) {
				i++
			}
			out = append(out, byte(i-start-1))
			out = append(out, rawBE[sOff:i*2]...)
		}
	}
	return out
}

type frameEncoder struct {
	rawBE   []byte
	rleBuf  []byte
	lz4Buf  []byte
	lz4Comp lz4.Compressor
	jpegBuf bytes.Buffer
	jpegImg *image.RGBA
}

func (e *frameEncoder) rgbaFromFB(fb []uint16, r image.Rectangle, width int) *image.RGBA {
	rw, rh := r.Dx(), r.Dy()
	need := rw * rh * 4
	if e.jpegImg == nil || cap(e.jpegImg.Pix) < need {
		e.jpegImg = image.NewRGBA(image.Rect(0, 0, rw, rh))
	} else {
		e.jpegImg.Rect = image.Rect(0, 0, rw, rh)
		e.jpegImg.Stride = rw * 4
		e.jpegImg.Pix = e.jpegImg.Pix[:need]
	}
	pix := e.jpegImg.Pix
	off := 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for _, v := range fb[y*width+r.Min.X : y*width+r.Max.X] {
			r5 := uint8(v >> 11)
			g6 := uint8((v >> 5) & 0x3f)
			b5 := uint8(v & 0x1f)
			pix[off] = (r5 << 3) | (r5 >> 2)
			pix[off+1] = (g6 << 2) | (g6 >> 4)
			pix[off+2] = (b5 << 3) | (b5 >> 2)
			pix[off+3] = 255
			off += 4
		}
	}
	return e.jpegImg
}

func (e *frameEncoder) encode(fb []uint16, rgba *image.RGBA, r image.Rectangle, rev uint64, width int) wsFrame {
	rw, rh := r.Dx(), r.Dy()
	pixels := rw * rh
	rawLen := pixels * 2
	if cap(e.rawBE) < rawLen {
		e.rawBE = make([]byte, rawLen)
	}
	rawBE := e.rawBE[:rawLen]
	off := 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for _, v := range fb[y*width+r.Min.X : y*width+r.Max.X] {
			rawBE[off] = byte(v >> 8)
			rawBE[off+1] = byte(v)
			off += 2
		}
	}

	e.rleBuf = encodeRLE565FromBE(e.rleBuf[:0], rawBE, pixels)
	bestCodec := frameCodecRaw
	bestPayload := rawBE
	if len(e.rleBuf) <= len(bestPayload) {
		bestCodec = frameCodecRLE565
		bestPayload = e.rleBuf
	}

	bound := lz4.CompressBlockBound(rawLen)
	if cap(e.lz4Buf) < bound {
		e.lz4Buf = make([]byte, bound)
	}
	lz4Dst := e.lz4Buf[:bound]
	if n, err := e.lz4Comp.CompressBlock(rawBE, lz4Dst); err == nil && n > 0 && n < len(bestPayload) {
		bestCodec = frameCodecLZ4
		bestPayload = lz4Dst[:n]
	}

	if rw >= 64 && rh >= 64 && len(bestPayload) > rawLen/3 {
		var src image.Image
		if rgba != nil && r.In(rgba.Bounds()) {
			src = rgba.SubImage(r)
		} else {
			src = e.rgbaFromFB(fb, r, width)
		}
		e.jpegBuf.Reset()
		if err := jpeg.Encode(&e.jpegBuf, src, &jpeg.Options{Quality: 82}); err == nil {
			if e.jpegBuf.Len() < len(bestPayload)*7/10 {
				bestCodec = frameCodecJPEG
				bestPayload = e.jpegBuf.Bytes()
			}
		}
	}

	out := make([]byte, 12+len(bestPayload))
	out[0] = 0x03
	binary.LittleEndian.PutUint16(out[1:], uint16(r.Min.X))
	binary.LittleEndian.PutUint16(out[3:], uint16(r.Min.Y))
	binary.LittleEndian.PutUint16(out[5:], uint16(rw))
	binary.LittleEndian.PutUint16(out[7:], uint16(rh))
	out[9] = bestCodec
	binary.LittleEndian.PutUint16(out[10:], uint16(rev))
	copy(out[12:], bestPayload)
	return wsFrame{wsBinary, out}
}

func displayFrame(fb []uint16, r image.Rectangle, rev uint64, width int) wsFrame {
	var enc frameEncoder
	return enc.encode(fb, nil, r, rev, width)
}
