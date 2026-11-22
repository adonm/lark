package dogica

import "testing"

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
			g, ok := tc.font.Glyphs[r]
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
