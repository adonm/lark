package main

import (
	"fmt"
	"image"
	"maps"
	"regexp"
	"strconv"
	"strings"
)

var larkPalette = map[string]string{
	".": "transparent", "k": "#4C3036", "w": "#FFF8ED", "p": "#F5AD7E",
	"m": "#94D4BA", "l": "#BFA8E5", "s": "#9CBFE8", "y": "#FFE49B",
	"r": "#DF92AE", "g": "#ACC280", "a": "#EAC071", "b": "#7C583D",
	"n": "#86798D", "d": "#43354E", "c": "#FFD0D6", "o": "#CD806F",
}

const maxSpriteSide = 64
const maxSpriteRuns = maxSpriteSide * maxSpriteSide

var pixelKey = regexp.MustCompile(`^[!-~]$`)
var pixelColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func spriteColors(overrides map[string]string) (map[string]string, error) {
	if len(overrides) == 0 {
		return larkPalette, nil
	}
	p := maps.Clone(larkPalette)
	for key, value := range overrides {
		if key == "." || !pixelKey.MatchString(key) || !pixelColor.MatchString(value) {
			return nil, fmt.Errorf("sprite palette: expected one printable ASCII key (not '.') and a #RRGGBB color, got %q", key)
		}
		p[key] = strings.ToUpper(value)
	}
	return p, nil
}

func spriteGrid(rows []string, p map[string]string) (int, error) {
	if len(rows) < 2 || len(rows) > maxSpriteSide || len(rows[0]) < 2 || len(rows[0]) > maxSpriteSide {
		return 0, fmt.Errorf("sprite grid must be 2–64 rows and columns")
	}
	w := len(rows[0])
	for i, row := range rows {
		if len(row) != w {
			return 0, fmt.Errorf("sprite row %d must have %d columns", i, w)
		}
		for j := range len(row) {
			if p[row[j:j+1]] == "" {
				return 0, fmt.Errorf("sprite row %d: unknown palette key %q", i, row[j:j+1])
			}
		}
	}
	return w, nil
}

func compileSprite(rows []string, palette map[string]string, x, y, scale int) (string, error) {
	p, err := spriteColors(palette)
	if err != nil {
		return "", err
	}
	if _, err := spriteGrid(rows, p); err != nil {
		return "", err
	}
	return spriteArtwork(rows, p, x, y, scale)
}

func spriteArtwork(rows []string, p map[string]string, x, y, scale int) (string, error) {
	if scale < 1 || scale > 128 || x < -1024 || y < -1024 || x > 1024 || y > 1024 {
		return "", fmt.Errorf("sprite position or scale out of range")
	}
	if len(rows) == 0 {
		return "", nil
	}
	dense := max(len(rows), len(rows[0])) > 16
	if dense && scale > 12 {
		return "", fmt.Errorf("sprites larger than 16 pixels need a scale of at most 12")
	}
	type colorPath struct {
		d    []byte
		x, y int
	}
	paths := map[string]colorPath{}
	var colors []string
	var out strings.Builder
	for j, row := range rows {
		for i := 0; i < len(row); {
			fill := p[row[i:i+1]]
			end := i + 1
			for end < len(row) && p[row[end:end+1]] == fill {
				end++
			}
			if fill != "transparent" {
				if dense {
					path := paths[fill]
					op, dx, dy := byte('m'), i-path.x, j-path.y
					if len(path.d) == 0 {
						colors = append(colors, fill)
						op, dx, dy = 'M', i, j
					}
					width := int64(end - i)
					path.d = strconv.AppendInt(append(path.d, op), int64(dx), 10)
					path.d = strconv.AppendInt(append(path.d, ' '), int64(dy), 10)
					path.d = strconv.AppendInt(append(path.d, 'h'), width, 10)
					path.d = strconv.AppendInt(append(path.d, 'v'), 1, 10)
					path.d = strconv.AppendInt(append(path.d, 'h'), -width, 10)
					path.d = append(path.d, 'Z')
					path.x, path.y = i, j
					paths[fill] = path
				} else {
					fmt.Fprintf(&out, `<rect x="%d" y="%d" width="%d" height="%d" fill="%s"/>`, x+i*scale, y+j*scale, (end-i)*scale, scale, fill)
				}
			}
			i = end
		}
	}
	if len(colors) > 0 {
		fmt.Fprintf(&out, `<g shape-rendering="crispEdges" transform="translate(%d %d) scale(%d)">`, x, y, scale)
		for _, fill := range colors {
			fmt.Fprintf(&out, `<path data-pixel-runs="1" fill="%s" d="%s"/>`, fill, paths[fill].d)
		}
		out.WriteString("</g>")
		return out.String(), nil
	}
	if out.Len() == 0 {
		return "", nil
	}
	return `<g shape-rendering="crispEdges">` + out.String() + `</g>`, nil
}

// The path is also ordinary browser SVG. Accept only our exact, disjoint-run
// grammar on the native fast path; everything else remains a vector path.
func parsePixelRuns(d string) ([]image.Rectangle, bool) {
	at := 0
	take := func(ch byte) bool {
		if at == len(d) || d[at] != ch {
			return false
		}
		at++
		return true
	}
	number := func() (int, bool) {
		neg := take('-')
		start, n := at, 0
		for at < len(d) && d[at] >= '0' && d[at] <= '9' {
			n = 10*n + int(d[at]-'0')
			at++
			if at-start > 2 || n > maxSpriteSide {
				return 0, false
			}
		}
		if neg {
			n = -n
		}
		return n, at > start
	}
	var runs []image.Rectangle
	x, y, end := 0, 0, 0
	for at < len(d) && len(runs) < maxSpriteRuns {
		op := byte('m')
		if len(runs) == 0 {
			op = 'M'
		}
		if !take(op) {
			break
		}
		dx, ok1 := number()
		if !take(' ') {
			break
		}
		dy, ok2 := number()
		if !ok1 || !ok2 || !take('h') {
			break
		}
		width, ok := number()
		x, y = x+dx, y+dy
		// Compiler emits left-to-right non-overlapping runs in each row.
		if !ok || width < 1 || x < 0 || x+width > maxSpriteSide || y < 0 || y >= maxSpriteSide ||
			dy < 0 || len(runs) > 0 && dy == 0 && x < end || !take('v') || !take('1') || !take('h') {
			break
		}
		back, ok := number()
		if !ok || back != -width || !take('Z') {
			break
		}
		runs = append(runs, image.Rect(x, y, x+width, y+1))
		end = x + width
		if at == len(d) {
			return runs, true
		}
	}
	return nil, false
}
