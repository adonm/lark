package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

type storybookColors struct {
	bg, fur, furLight, furShadow, outline, innerEar, blush, eye, accent string
}

var storybookPalettes = map[string]storybookColors{
	"peach":    {"#FFF2E7", "#F5AD7E", "#FFE2B8", "#CD806F", "#76463F", "#E88E83", "#ED8F87", "#4C3036", "#FFF4DF"},
	"mint":     {"#E8F7EE", "#94D4BA", "#DDF2C8", "#6DA7A5", "#366B63", "#F5BFB5", "#ECAAAC", "#253F46", "#FFF0BD"},
	"lavender": {"#F3ECFA", "#BFA8E5", "#EBDCF9", "#9885C3", "#604B7A", "#F0BCD5", "#E6A2C4", "#3D3057", "#FDF0DB"},
	"sage":     {"#F0F3E1", "#ACC280", "#E3EAC0", "#809F7B", "#4E6547", "#F1C5AD", "#E7A894", "#374637", "#FFEDBD"},
	"berry":    {"#FAEAF0", "#DF92AE", "#FFD0D6", "#B66F9B", "#7D415E", "#F5B4BF", "#F48F9E", "#502842", "#FFF0DE"},
	"amber":    {"#FFF4DA", "#EAC071", "#FFECB4", "#C08D63", "#7C583D", "#EFAD91", "#EB9C81", "#48322C", "#FFF8DB"},
	"sky":      {"#EAF3FC", "#9CBFE8", "#DDECFD", "#7499C5", "#455F87", "#EFBBD1", "#E7A8BC", "#2B405E", "#FFF2CC"},
	"midnight": {"#252D48", "#8B9ABC", "#CDD8E9", "#637799", "#364461", "#CB9DB6", "#D693AE", "#27334F", "#FFE6A9"},
}

var hexTintSchema = object{"type": "string", "pattern": `^#[0-9a-fA-F]{6}$`}
var storybookColorwaySchema = object{"type": "string", "enum": slices.Sorted(maps.Keys(storybookPalettes))}
var storybookPaletteSchema = object{"anyOf": []object{
	storybookColorwaySchema,
	closedObject(object{"base": storybookColorwaySchema, "bg": hexTintSchema, "fur": hexTintSchema,
		"furLight": hexTintSchema, "furShadow": hexTintSchema, "outline": hexTintSchema,
		"innerEar": hexTintSchema, "blush": hexTintSchema, "eye": hexTintSchema, "accent": hexTintSchema}),
}}

func storybookPalette(raw any, fallback string) storybookColors {
	switch v := raw.(type) {
	case string:
		return storybookPalettes[v]
	case object:
		if base := str(v["base"]); base != "" {
			fallback = base
		}
		p := storybookPalettes[fallback]
		for key, dst := range map[string]*string{
			"bg": &p.bg, "fur": &p.fur, "furLight": &p.furLight, "furShadow": &p.furShadow,
			"outline": &p.outline, "innerEar": &p.innerEar, "blush": &p.blush, "eye": &p.eye, "accent": &p.accent,
		} {
			if tint := color(v[key]); tint != "" {
				*dst = tint
			}
		}
		return p
	default:
		return storybookPalettes[fallback]
	}
}

func (p storybookColors) paint(art string) string {
	return strings.NewReplacer("@light", p.furLight, "@shade", p.furShadow, "@fur", p.fur,
		"@ink", p.outline, "@ear", p.innerEar, "@blush", p.blush, "@eye", p.eye, "@cream", p.accent).Replace(art)
}

func storyPath(d, fill string) string {
	return `<path d="` + d + `" fill="` + fill + `"/>`
}

func storyStroke(d, fill, ink string, width int) string {
	return fmt.Sprintf(`<path d="%s" fill="%s" stroke="%s" stroke-width="%d" stroke-linecap="round" stroke-linejoin="round"/>`, d, fill, ink, width)
}

func storyOval(x, y, rx, ry int, fill string) string {
	return fmt.Sprintf(`<ellipse cx="%d" cy="%d" rx="%d" ry="%d" fill="%s"/>`, x, y, rx, ry, fill)
}

func storySpark(x, y, n int, fill string) string {
	return fmt.Sprintf(`<path transform="translate(%d %d) scale(%.3f)" d="M0-12Q2-2 12 0Q2 2 0 12Q-2 2-12 0Q-2-2 0-12Z" fill="%s"/>`, x, y, float64(n)/12, fill)
}
