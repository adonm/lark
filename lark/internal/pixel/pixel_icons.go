package pixel

import (
	imagecolor "image/color"
	"math"
)

// UI palette for pixel-art screens (applied after LUT grading).
var (
	pxPanelBG      = imagecolor.RGBA{30, 26, 43, 255}
	pxPanelBorder  = imagecolor.RGBA{141, 121, 148, 255}
	pxTileBG       = imagecolor.RGBA{42, 35, 56, 255}
	pxTileBorder   = imagecolor.RGBA{112, 95, 124, 255}
	pxTileActiveBG = imagecolor.RGBA{62, 49, 84, 255}
	pxInk          = imagecolor.RGBA{250, 240, 246, 255}
	pxMuted        = imagecolor.RGBA{197, 182, 204, 255}
	Peach          = imagecolor.RGBA{255, 193, 168, 255}
	Mint           = imagecolor.RGBA{154, 229, 195, 255}
	pxWarn         = imagecolor.RGBA{255, 208, 142, 255}
	Danger         = imagecolor.RGBA{255, 175, 194, 255}
	Sky            = imagecolor.RGBA{156, 191, 232, 255}
	pxQRDark       = imagecolor.RGBA{18, 16, 26, 255}
	pxQRWhite      = imagecolor.RGBA{255, 255, 255, 255}
)

var pxIconPalette = map[string]imagecolor.RGBA{
	".": {},
	"w": pxInk,
	"m": Mint,
	"p": Peach,
	"y": pxWarn,
	"r": Danger,
	"s": Sky,
	"d": pxPanelBorder,
	"k": pxPanelBG,
}

// 12x12 pixel sprites for Menu tiles and lists.
var pxMenuSprites = map[string][]string{
	"talk": {
		"..pppppppp..",
		".pwwwwwwwwp.",
		".pwkwwwwkwp.",
		".pwwwwwwwwp.",
		".pwkkkkkkwp.",
		".pwwwwwwwwp.",
		"..pppppppp..",
		"...pp.......",
		"..p.........",
		"............",
		"............",
		"............",
	},
	"books": {
		"............",
		".mmmmm.mmmm.",
		".mwwwm.mwwm.",
		".mwmwm.mwmm.",
		".mwwwm.mwwm.",
		".mwmwm.mwmm.",
		".mwwwm.mwwm.",
		".mwmwm.mwmm.",
		".mwwwm.mwwm.",
		".mmmmm.mmmm.",
		"..dddddddd..",
		"............",
	},
	"games": {
		"............",
		"............",
		"..ssssssss..",
		".swwwwwwwws.",
		".swwkwwkwws.",
		".swkkkwwrys.",
		".swwkwwkkys.",
		".swwwwwwwws.",
		".sswwwwwwss.",
		"............",
		"............",
		"............",
	},
	"memories": {
		"............",
		"..rrr..rrr..",
		".rrrrrrrrrr.",
		".rrwrrrrrrr.",
		".rrrrrrrrrr.",
		".rrrrrrrrrr.",
		"..rrrrrrrr..",
		"...rrrrrr...",
		"....rrrr....",
		".....rr.....",
		"............",
		"............",
	},
	"voice": {
		"............",
		".....mm.....",
		"....mmmm....",
		"....mwwm....",
		"....mwwm....",
		"..m.mmmm.m..",
		"..m..mm..m..",
		"...mmmmmm...",
		".....mm.....",
		"....mmmm....",
		"............",
		"............",
	},
	"volume": {
		"............",
		".....ss.....",
		"....sss..s..",
		".ssssss.s.s.",
		".swwwws...s.",
		".swwwws.s.s.",
		".ssssss..s..",
		"....sss.....",
		".....ss.....",
		"............",
		"............",
		"............",
	},
	"pair": {
		".pppp..pppp.",
		".pkkp..pkkp.",
		".pkkp..pkkp.",
		".pppp..pppp.",
		"............",
		".pppp..p..p.",
		".pkkp...pp..",
		".pkkp..p..p.",
		".pppp..p.pp.",
		"............",
		"............",
		"............",
	},
	"unpair": {
		".rrrr..rrrr.",
		".r..r..r..r.",
		".rrrr..rrrr.",
		"....r..r....",
		"...rr..rr...",
		"..r......r..",
		".rr......rr.",
		".r........r.",
		"............",
		"............",
		"............",
		"............",
	},
	"stats": {
		"............",
		".........mm.",
		"........mmm.",
		".....yy.mmm.",
		"....yyy.mmm.",
		".pp.yyy.mmm.",
		".pp.yyy.mmm.",
		".pp.yyy.mmm.",
		".dddddddddd.",
		"............",
		"............",
		"............",
	},
}

// 8x8 pixel status icons for Home corner.
var pxStatusSprites = map[string][]string{
	"listening": {
		"...rr...",
		"..rwwr..",
		"..rwwr..",
		"..rrrr..",
		".r.rr.r.",
		"..rrrr..",
		"...rr...",
		"..rrrr..",
	},
	"thinking": {
		"..yyyy..",
		".ywwwwy.",
		"ywywywyy",
		".ywwwwy.",
		"..yyyy..",
		"...yy...",
		"..y.....",
		"........",
	},
	"speaking": {
		"...m..m.",
		"..mm...m",
		"mmmm.m.m",
		"mwwm.m.m",
		"mmmm.m.m",
		"..mm...m",
		"...m..m.",
		"........",
	},
	"error": {
		"...rr...",
		"..rwwr..",
		"..rwwr..",
		".rrwwrr.",
		".rrwwrr.",
		"rrrrrrrr",
		"rrrwwrrr",
		"rrrrrrrr",
	},
}

// defaultPixelBackground generates a cozy procedural pixel scene (sky gradient,
// clouds, distant hills, and foreground meadow) at logical grid resolution.
func defaultPixelBackground(w, h int, tintHex string) *imagecolor.RGBA {
	_ = tintHex
	return nil
}

func drawDefaultPixelBackground(c *pixelCanvas) {
	w, h := c.w, c.h
	skyBands := []imagecolor.RGBA{
		{154, 198, 236, 255},
		{175, 214, 242, 255},
		{198, 228, 246, 255},
		{222, 239, 247, 255},
		{242, 242, 234, 255},
	}
	horizonY := h * 2 / 3
	for y := range horizonY {
		band := min(len(skyBands)-1, y*len(skyBands)/horizonY)
		c.rect(0, y, w, 1, skyBands[band])
	}

	// Soft pixel clouds.
	cloud := imagecolor.RGBA{252, 250, 246, 255}
	drawCloud := func(cx, cy, cw int) {
		c.rect(cx, cy, cw, 4, cloud)
		c.rect(cx+2, cy-2, cw-4, 2, cloud)
		c.rect(cx+4, cy-3, cw/2, 1, cloud)
	}
	drawCloud(w/6, h/6, 18)
	drawCloud(w*2/3, h/4, 22)

	// Distant hills.
	farHill := imagecolor.RGBA{144, 186, 168, 255}
	midHill := imagecolor.RGBA{118, 168, 134, 255}
	nearMeadow := imagecolor.RGBA{96, 148, 108, 255}
	grassTop := imagecolor.RGBA{132, 184, 132, 255}

	for x := range w {
		yFar := horizonY - 8 - int(math.Round(6*math.Sin(float64(x)*0.06)))
		c.rect(x, yFar, 1, h-yFar, farHill)

		yMid := horizonY - 2 - int(math.Round(5*math.Cos(float64(x)*0.08+1.1)))
		c.rect(x, yMid, 1, h-yMid, midHill)

		yNear := horizonY + 8 + int(math.Round(3*math.Sin(float64(x)*0.05+2.0)))
		c.rect(x, yNear, 1, 2, grassTop)
		c.rect(x, yNear+2, 1, h-yNear-2, nearMeadow)
	}
}
