package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

type companionSpecies struct {
	colorway, head, ears, details, muzzle, nose string
	eyeX, eyeY                                  int
}

const roundHead = "M180 84C235 84 282 126 287 180C293 229 248 262 180 263C112 262 67 229 73 180C78 126 125 84 180 84Z"
const catEars = `<path d="M80 164Q65 128 74 74Q79 54 99 65Q130 82 152 108M208 108Q230 82 261 65Q281 54 286 74Q295 128 280 164" fill="@fur" stroke="@ink" stroke-width="6" stroke-linejoin="round"/><path d="M87 132Q82 103 87 80Q106 88 127 112ZM273 132Q278 103 273 80Q254 88 233 112Z" fill="@ear"/>`
const buttonNose = "M170 197Q180 193 190 197Q191 204 180 209Q169 204 170 197Z"
const creamMuzzle = "M133 211C134 198 153 190 180 198C207 190 226 198 227 211C232 232 206 246 180 245C154 246 128 232 133 211Z"

var companionSpeciesArt = map[string]companionSpecies{
	"cat": {"peach", roundHead, catEars, "", creamMuzzle, buttonNose, 43, 171},
	"fox": {"amber",
		"M79 150Q88 107 135 89Q180 72 225 89Q272 107 281 150Q298 182 310 196Q291 207 281 226Q247 263 180 265Q113 263 79 226Q69 207 50 196Q62 182 79 150Z",
		`<path d="M82 157Q67 107 76 56Q81 44 96 50Q132 67 158 106M202 106Q228 67 264 50Q279 44 284 56Q293 107 278 157" fill="@fur" stroke="@ink" stroke-width="6" stroke-linejoin="round"/><path d="M87 125Q82 92 88 65Q113 75 133 104ZM273 125Q278 92 272 65Q247 75 227 104Z" fill="@ear"/>`, "",
		"M76 190Q104 182 134 197Q154 208 180 204Q206 208 226 197Q256 182 284 190Q271 248 180 259Q89 248 76 190Z", buttonNose, 43, 168},
	"owl": {"lavender",
		"M88 120Q75 94 79 76Q82 67 94 73Q123 87 135 89Q180 75 225 89Q237 87 266 73Q278 67 281 76Q285 94 272 120C307 172 300 216 268 244Q239 267 180 269Q121 267 92 244C60 216 53 172 88 120Z",
		"", `<path d="M180 147C138 93 91 129 93 179C93 218 132 246 180 232C228 246 267 218 267 179C269 129 222 93 180 147Z" fill="@cream"/><path d="M115 218l8 5m114 0 8-5" stroke="@shade" stroke-width="3" stroke-linecap="round"/>`,
		"", "M166 194Q180 186 194 194Q194 203 180 219Q166 203 166 194Z", 43, 171},
	"marten": {"amber",
		"M180 80C221 80 260 103 276 143Q301 196 267 232Q240 258 192 267Q180 272 168 267Q120 258 93 232Q59 196 84 143C100 103 139 80 180 80Z",
		`<ellipse cx="93" cy="108" rx="30" ry="34" fill="@fur" stroke="@ink" stroke-width="6"/><ellipse cx="267" cy="108" rx="30" ry="34" fill="@fur" stroke="@ink" stroke-width="6"/><ellipse cx="92" cy="104" rx="17" ry="20" fill="@ear"/><ellipse cx="268" cy="104" rx="17" ry="20" fill="@ear"/>`, "",
		"M105 206C112 181 146 185 180 204C214 185 248 181 255 206Q238 248 192 257Q180 263 168 257Q122 248 105 206Z", buttonNose, 43, 163},
	"dragon": {"mint",
		"M180 79C235 79 275 113 280 159C301 185 293 221 265 241Q233 264 180 264Q127 264 95 241C67 221 59 185 80 159C85 113 125 79 180 79Z",
		`<path d="M93 144Q64 112 46 121Q50 165 83 186M267 144Q296 112 314 121Q310 165 277 186" fill="@ear" stroke="@ink" stroke-width="5"/><path d="M102 119Q84 94 94 63Q97 55 102 65Q111 86 131 93M258 119Q276 94 266 63Q263 55 258 65Q249 86 229 93" fill="@cream" stroke="@ink" stroke-width="5" stroke-linejoin="round"/>`,
		`<path d="M158 91Q171 58 181 53Q185 77 199 86" fill="@shade" stroke="@ink" stroke-width="5" stroke-linecap="round" stroke-linejoin="round"/>`,
		"M112 215C112 196 146 185 180 193C214 185 248 196 248 215C248 240 218 249 180 249C142 249 112 240 112 215Z",
		"M148 199a4 4 0 1 0 8 0a4 4 0 1 0-8 0M204 199a4 4 0 1 0 8 0a4 4 0 1 0-8 0", 44, 165},
	"bunny": {"berry",
		"M180 109C236 109 280 137 285 185C290 232 239 263 180 263C121 263 70 232 75 185C80 137 124 109 180 109Z",
		`<path d="M109 156C92 134 86 50 109 37C137 20 157 83 158 124M202 124C203 83 223 20 251 37C274 50 268 134 251 156" fill="@fur" stroke="@ink" stroke-width="6" stroke-linecap="round"/><path d="M112 128C101 108 101 56 114 50C130 44 142 97 140 119ZM248 128C259 108 259 56 246 50C230 44 218 97 220 119Z" fill="@ear"/>`,
		"", creamMuzzle, buttonNose, 43, 175},
	"bear": {"amber", roundHead,
		`<ellipse cx="98" cy="104" rx="36" ry="35" fill="@fur" stroke="@ink" stroke-width="6"/><ellipse cx="262" cy="104" rx="36" ry="35" fill="@fur" stroke="@ink" stroke-width="6"/><ellipse cx="98" cy="104" rx="21" ry="20" fill="@ear"/><ellipse cx="262" cy="104" rx="21" ry="20" fill="@ear"/>`,
		"", "M128 213C128 189 153 179 180 179C207 179 232 189 232 213C232 237 208 247 180 247C152 247 128 237 128 213Z", buttonNose, 45, 165},
	"frog": {"sage",
		"M86 143C69 101 101 83 126 89Q147 91 159 116Q180 111 201 116Q213 91 234 89C259 83 291 101 274 143C301 161 309 201 286 227Q251 264 180 264Q109 264 74 227C51 201 59 161 86 143Z",
		"", `<ellipse cx="125" cy="143" rx="29" ry="33" fill="@light"/><ellipse cx="235" cy="143" rx="29" ry="33" fill="@light"/>`,
		"M94 215Q180 183 266 215Q242 257 180 257Q118 257 94 215Z", "", 55, 143},
	"bird": {"sky",
		"M180 83Q159 68 162 49Q191 53 202 81Q204 65 224 56Q231 72 220 93C257 107 284 137 285 184C287 228 248 266 180 264C112 266 73 228 75 184C76 130 117 91 180 83Z",
		`<path d="M101 198Q67 185 56 209Q62 239 110 240M259 198Q293 185 304 209Q298 239 250 240" fill="@shade" stroke="@ink" stroke-width="5"/>`, "",
		"M118 220Q130 189 180 193Q230 189 242 220Q227 253 180 258Q133 253 118 220Z",
		"M162 195Q179 178 198 195Q198 204 180 213Q162 204 162 195Z", 43, 164},
	"robot": {"midnight",
		"M107 98Q87 98 84 119L75 203Q74 247 116 254Q180 265 244 254Q286 247 285 203L276 119Q273 98 253 98Z",
		`<path d="M180 105L196 69M75 166Q55 166 55 185V201Q55 219 80 219M285 166Q305 166 305 185V201Q305 219 280 219" fill="@shade" stroke="@ink" stroke-width="6" stroke-linecap="round"/><circle cx="198" cy="62" r="12" fill="@cream" stroke="@ink" stroke-width="5"/>`,
		`<rect x="95" y="126" width="170" height="106" rx="27" ry="27" fill="@light" stroke="@ink" stroke-width="5"/>`, "", "", 43, 167},
}

var companionNames = slices.Sorted(maps.Keys(companionSpeciesArt))
var companionSchema = closedObject(object{
	"species":   object{"type": "string", "enum": companionNames},
	"palette":   storybookPaletteSchema,
	"eyes":      object{"type": "string", "enum": strings.Fields("sparkle happy sleepy curious star")},
	"markings":  object{"type": "string", "enum": strings.Fields("none tabby blush freckles star spots")},
	"accessory": object{"type": "string", "enum": strings.Fields("none scarf crown glasses bow beanie leaf")},
}, "species")

func facePart(name, art string) string {
	return `<g data-part="` + name + `">` + art + `</g>`
}

func companionHead(name string, s companionSpecies) string {
	art := s.ears + storyStroke(s.head, "@fur", "@ink", 6)
	switch name {
	case "robot":
		art += storyStroke("M108 111H234", "none", "@light", 6)
	case "frog":
		art += storyPath("M66 194Q69 233 119 247Q177 270 241 247Q289 233 294 194Q273 241 180 242Q87 241 66 194Z", "@shade")
		art += storyStroke("M103 109Q120 93 138 105M222 105Q240 93 257 109", "none", "@light", 5)
	default:
		art += storyPath("M78 190Q82 231 121 247Q179 273 239 247Q278 231 282 190Q268 236 180 239Q92 236 78 190Z", "@shade")
		top := 95
		if name == "bunny" {
			top = 119
		}
		art += fmt.Sprintf(`<path transform="translate(0 %d)" d="M105 35C131 2 186-9 226 9Q245 18 254 30C218 9 151 8 105 35Z" fill="@light"/>`, top)
	}
	art += s.details
	if s.muzzle != "" {
		art += storyPath(s.muzzle, "@cream")
	}
	return art
}

func companionEyes(s companionSpecies, style, pose string) string {
	var out strings.Builder
	for i, x := range []int{180 - s.eyeX, 180 + s.eyeX} {
		y := s.eyeY
		if pose == "blink" || pose == "sleep" || pose == "think" && i == 1 || style == "sleepy" && pose == "idle" {
			out.WriteString(storyStroke(fmt.Sprintf("M%d %dQ%d %d %d %d", x-13, y, x, y+15, x+13, y), "none", "@eye", 5))
			continue
		}
		if style == "happy" && (pose == "idle" || pose == "talk") {
			out.WriteString(storyStroke(fmt.Sprintf("M%d %dQ%d %d %d %d", x-13, y+4, x, y-16, x+13, y+4), "none", "@eye", 5))
			continue
		}
		rx, ry := 16, 21
		if pose == "listen" || style == "curious" && i == 0 {
			rx, ry = 19, 23
		}
		out.WriteString(storyOval(x, y, rx+2, ry+2, "@cream"))
		out.WriteString(storyOval(x, y, rx, ry, "@eye"))
		out.WriteString(storyOval(x+2, y+9, rx-6, ry-12, "@shade"))
		if style == "star" {
			out.WriteString(storySpark(x-3, y-6, 9, "#FFFDF7"))
		} else {
			out.WriteString(storyOval(x-5, y-7, 6, 7, "#FFFDF7"))
		}
		out.WriteString(storyOval(x+7, y+6, 3, 3, "#FFFDF7"))
	}
	return out.String()
}

func companionMouth(name, pose string) string {
	avian := name == "bird" || name == "owl"
	switch {
	case pose == "talk":
		d := "M157 216Q180 223 203 216C201 249 159 249 157 216Z"
		y := 233
		if avian {
			d, y = "M166 205Q180 212 194 205Q192 233 180 235Q168 233 166 205Z", 226
		}
		return storyPath(d, "@eye") + storyOval(180, y, 9, 4, "@blush")
	case pose == "listen":
		if !avian {
			return storyOval(180, 223, 9, 8, "@eye")
		}
	case pose == "think":
		if !avian {
			return storyStroke("M171 222Q181 218 191 222", "none", "@eye", 4)
		}
	}
	switch {
	case avian:
		return storyStroke("M172 198Q180 204 188 198", "none", "@ink", 3)
	case name == "frog" || name == "robot" || name == "dragon":
		return storyStroke("M156 215Q180 240 204 215", "none", "@eye", 5)
	default:
		return storyStroke("M180 207V216M157 215Q167 230 180 216Q193 230 203 215", "none", "@eye", 4)
	}
}

func companionMarkings(name string) (forehead, extraCheeks string) {
	switch name {
	case "tabby":
		forehead = storyStroke("M164 120l4 11M180 116v15M196 120l-4 11", "none", "@shade", 6)
		extraCheeks = storyStroke("M83 177l16 4M88 188l12 4M277 177l-16 4M272 188l-12 4", "none", "@shade", 5)
	case "star":
		forehead = storySpark(180, 125, 13, "@cream")
	case "spots":
		forehead = storyOval(160, 122, 8, 5, "@shade") + storyOval(182, 113, 7, 4, "@shade") + storyOval(200, 127, 6, 4, "@shade")
	case "freckles":
		for _, x := range []int{113, 122, 131, 229, 238, 247} {
			extraCheeks += storyOval(x, 200+(x%2)*5, 2, 2, "@ink")
		}
	case "blush":
		extraCheeks = storyStroke("M105 205l4-5m5 6 4-5m5 5 4-5m106 4 4-5m5 6 4-5m5 5 4-5", "none", "@ear", 2)
	}
	return
}

func companionAccessory(name string, s companionSpecies, accessory string) string {
	top := 89
	if name == "bunny" || name == "frog" || name == "robot" {
		top = 120
	}
	switch accessory {
	case "scarf":
		return storyStroke("M125 250Q180 270 235 250L230 270Q214 278 206 278L214 286Q200 293 186 287L180 276Q143 277 130 269Z", "@blush", "@ink", 5) +
			storyStroke("M139 261Q180 276 219 261M193 277l7 8", "none", "@cream", 3)
	case "glasses":
		x, y := s.eyeX, s.eyeY
		return fmt.Sprintf(`<g fill="none" stroke="@ink" stroke-width="5"><ellipse cx="%d" cy="%d" rx="27" ry="29"/><ellipse cx="%d" cy="%d" rx="27" ry="29"/><path d="M%d %dQ180 %d %d %d"/></g>`, 180-x, y, 180+x, y, 207-x, y, y-10, 153+x, y)
	case "crown", "bow", "beanie", "leaf":
		var art string
		switch accessory {
		case "crown":
			art = storyStroke("M155 3l-9-31 23 12 11-25 13 25 21-12-9 31Q180 11 155 3Z", "#F5C974", "@ink", 4) + storySpark(180, -13, 7, "#FFFDF7")
		case "bow":
			art = storyStroke("M175-4Q139-27 143-3Q141 20 176 6H184Q219 20 217-3Q221-27 185-4Z", "@blush", "@ink", 4) + storyOval(180, 1, 8, 8, "@light")
		case "beanie":
			art = storyOval(182, -36, 10, 10, "@blush") + storyStroke("M131 2Q136-31 180-33Q224-31 229 2Z", "@blush", "@ink", 4) +
				storyStroke("M132 0Q180-10 228 0L227 13Q180 6 133 13Z", "@ear", "@ink", 4)
		case "leaf":
			art = storyStroke("M185 9Q173-14 150-25Q190-36 194-4Q194-26 218-31Q218-3 193 2", "@light", "@ink", 4)
		}
		return fmt.Sprintf(`<g transform="translate(0 %d)">%s</g>`, top, art)
	}
	return ""
}

func compileCompanion(raw any) (string, storybookColors, error) {
	spec, err := readUIValue[object](raw, companionSchema, "companion.preset")
	if err != nil {
		return "", storybookColors{}, err
	}
	name := str(spec["species"])
	s := companionSpeciesArt[name]
	p := storybookPalette(spec["palette"], s.colorway)
	if p.outline == "#000000" {
		return "", p, fieldError("companion.preset.palette.outline", "use a tinted outline")
	}
	forehead, cheeks := companionMarkings(str(spec["markings"]))
	cheeks = storyOval(113, 201, 13, 7, "@blush") + storyOval(247, 201, 13, 7, "@blush") + cheeks
	nose := ""
	if s.nose != "" {
		nose = storyPath(s.nose, "@ink")
		if name == "bird" || name == "owl" {
			nose = storyStroke(s.nose, "#F5C974", "@ink", 3)
		}
	}
	whiskers := ""
	if name == "cat" || name == "marten" {
		whiskers = storyStroke("M91 200l-16-4m18 13-16 5m192-14 16-4m-18 13 16 5", "none", "@ink", 3)
	}
	var out strings.Builder
	out.WriteString(storyOval(180, 281, 71, 7, "@shade"))
	out.WriteString(storyStroke("M117 272Q117 243 154 235H206Q243 243 243 272Q221 288 180 286Q139 288 117 272Z", "@fur", "@ink", 6))
	out.WriteString(storyPath("M149 281Q148 270 157 260Q180 246 203 260Q212 270 211 281Q180 286 149 281Z", "@light"))
	out.WriteString(companionHead(name, s))
	out.WriteString(facePart("forehead", forehead) + facePart("cheeks", cheeks) + facePart("whiskers", whiskers) + facePart("nose", nose))
	for _, pose := range []string{"idle", "blink", "listen", "think", "talk", "sleep"} {
		out.WriteString(`<g data-pose="` + pose + `">` +
			facePart("eyes", companionEyes(s, str(spec["eyes"]), pose)) + facePart("mouth", companionMouth(name, pose)))
		if pose == "sleep" {
			out.WriteString(storySpark(299, 118, 10, "@shade") + storySpark(311, 97, 6, "@ink"))
		} else if pose == "think" {
			out.WriteString(storyOval(297, 117, 4, 4, "@ink") + storyOval(311, 105, 6, 6, "@shade"))
		}
		out.WriteString(`</g>`)
	}
	out.WriteString(companionAccessory(name, s, str(spec["accessory"])))
	return svgStart + `<rect width="360" height="360" fill="` + p.bg + `"/>` + p.paint(out.String()) + `</svg>`, p, nil
}
