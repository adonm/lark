package main

import (
	"time"

	"lark/internal/pixel"
)

const screenSize = pixel.ScreenSide

type screenGeometry struct {
	width, height int
	shape         string
}

var roundScreen = screenGeometry{screenSize, screenSize, "round"}

func inScreenCircle(x, y int) bool {
	const r = screenSize / 2
	return (x-r)*(x-r)+(y-r)*(y-r) <= r*r
}

type svgScreen struct {
	source string
	hits   []pixel.HitBox
	fb     []uint16
	render time.Duration
	grade  pixel.GradeKey
}

func hitID(hits []pixel.HitBox, x, y int) string {
	return pixel.HitID(hits, x, y)
}
