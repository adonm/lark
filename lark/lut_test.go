package main

import (
	"testing"
	"time"

	"lark/internal/pixel"
)

func TestUnconfiguredHubKeepsIdentityGrade(t *testing.T) {
	t.Parallel()
	var hub terminalHub
	at := func(h, m int) time.Time { return time.Date(2026, 1, 1, h, m, 0, 0, time.UTC) }
	if g := hub.frameGrade(&terminalSession{}, at(23, 0)); g != pixel.GradeDay {
		t.Fatalf("unconfigured hub did not keep the identity grade: %d", g)
	}
}
