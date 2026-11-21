package main

import "time"

type gradeKey uint8
type colorLUT [3][256]uint8

const (
	gradeDay gradeKey = iota
	gradeDawn
	gradeDusk
	gradeNight
	gradeBlend
)

var atmosphereEdges = [...]struct {
	minute        int
	before, after gradeKey
}{
	{5*60 + 30, gradeNight, gradeDawn},
	{8*60 + 30, gradeDawn, gradeDay},
	{17 * 60, gradeDay, gradeDusk},
	{20*60 + 30, gradeDusk, gradeNight},
}

var atmosphereLUTs, atmosphereKeys = makeAtmosphere()

func makeAtmosphere() ([gradeBlend + 48]colorLUT, [48]gradeKey) {
	var tables [gradeBlend + 48]colorLUT
	var keys [48]gradeKey
	// Dark and light endpoints per channel; the modest, monotonic
	// changes leave the palette recognizable and preserve contrast.
	shadow := [4][3]int{{0, 0, 0}, {6, 0, 1}, {5, 1, 0}, {1, 0, 9}}
	light := [4][3]int{{255, 255, 255}, {255, 244, 245}, {255, 248, 232}, {235, 232, 252}}
	for phase := range gradeBlend {
		for c := range 3 {
			for i := range 256 {
				tables[phase][c][i] = uint8((shadow[phase][c]*(255-i) + light[phase][c]*i + 127) / 255)
			}
		}
	}
	for slot := range keys {
		m, phase := slot*30+15, gradeNight
		for _, edge := range atmosphereEdges {
			if m >= edge.minute {
				phase = edge.after
			}
			if weight := m - edge.minute + 60; weight > 0 && weight < 120 {
				phase = gradeBlend + gradeKey(slot)
				for c := range 3 {
					for i := range 256 {
						a, b := int(tables[edge.before][c][i]), int(tables[edge.after][c][i])
						tables[phase][c][i] = uint8((a*(120-weight) + b*weight + 60) / 120)
					}
				}
				break
			}
		}
		keys[slot] = phase
	}
	return tables, keys
}

func gradeAt(now time.Time, loc *time.Location) gradeKey {
	if loc == nil {
		loc = time.Local
	}
	h, m, _ := now.In(loc).Clock()
	return atmosphereKeys[(h*60+m)/30]
}
