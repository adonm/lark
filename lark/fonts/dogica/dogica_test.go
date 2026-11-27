package dogica

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestRequiredRunesBaked(t *testing.T) {
	var required []rune
	for r := rune(0x20); r <= 0x7E; r++ {
		required = append(required, r)
	}
	for r := rune(0xA0); r <= 0xFF; r++ {
		required = append(required, r)
	}
	required = append(required, '\u2013', '\u2014', '\u2018', '\u2019', '\u201C', '\u201D', '\u2026')

	for _, tc := range []struct {
		name string
		font Font
	}{
		{"Regular", Regular},
		{"Bold", Bold},
	} {
		for _, r := range required {
			g, ok := tc.font.Lookup(r)
			if !ok {
				t.Errorf("%s missing glyph for %U (%q)", tc.name, r, r)
				continue
			}
			if r != ' ' && r != '\u00A0' && r != '\u00AD' && g.Width == 0 {
				t.Errorf("%s has empty bitmap for %U (%q)", tc.name, r, r)
			}
		}
	}
}

// TestGlyphsMatchGolden compares every baked glyph, including every row, with
// a dump captured from the map-based tables. The flat-array rewrite must not
// change a single pixel.
func TestGlyphsMatchGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/glyphs.txt")
	if err != nil {
		t.Fatal(err)
	}
	fonts := map[string]Font{"Regular": Regular, "Bold": Bold}
	seen := map[string]int{}
	for lineNo, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 7 {
			t.Fatalf("line %d: %d fields", lineNo+1, len(fields))
		}
		name := fields[0]
		font, ok := fonts[name]
		if !ok {
			t.Fatalf("line %d: unknown font %q", lineNo+1, name)
		}
		var r rune
		if n, err := strconv.ParseInt(strings.TrimPrefix(fields[1], "U+"), 16, 32); err == nil {
			r = rune(n)
		} else {
			t.Fatalf("line %d: rune %q: %v", lineNo+1, fields[1], err)
		}
		nums := make([]int, 0, len(fields)-2)
		for i, f := range fields[2:] {
			base := 10
			if i >= 5 {
				base = 16
			}
			n, err := strconv.ParseInt(f, base, 32)
			if err != nil {
				t.Fatalf("line %d: %q: %v", lineNo+1, f, err)
			}
			nums = append(nums, int(n))
		}
		g, ok := font.Lookup(r)
		if !ok {
			t.Fatalf("line %d: %s missing %U", lineNo+1, name, r)
		}
		got := []int{int(g.Advance), int(g.Width), int(g.Height), int(g.XOff), int(g.YOff)}
		for _, row := range g.Rows {
			got = append(got, int(row))
		}
		if len(got) != len(nums) {
			t.Fatalf("line %d: %s %U has %d fields, want %d", lineNo+1, name, r, len(got), len(nums))
		}
		for i := range got {
			if got[i] != nums[i] {
				t.Fatalf("line %d: %s %U field %d = %#x, want %#x", lineNo+1, name, r, i, got[i], nums[i])
			}
		}
		seen[name]++
	}
	for name, font := range fonts {
		if seen[name] != len(font.glyphs) {
			t.Fatalf("%s: golden has %d glyphs, table has %d", name, seen[name], len(font.glyphs))
		}
	}
}
