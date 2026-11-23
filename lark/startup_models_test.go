package main

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"
	"time"
)

// Readiness logging must name failed models instead of claiming everything is
// ready when one load failed.
func TestModelReadinessSummary(t *testing.T) {
	capture := func(b *larkServer) string {
		var buf bytes.Buffer
		old := log.Writer()
		log.SetOutput(&buf)
		defer log.SetOutput(old)
		b.logModelReadiness()
		return buf.String()
	}
	models := newModelSet(time.Now())
	models.stt.setReady()
	models.vad.setReady()
	models.smartTurn.setError(errors.New("weights missing"))
	b := &larkServer{models: models}
	out := capture(b)
	if !strings.Contains(out, "not ready") || !strings.Contains(out, "Smart Turn") || !strings.Contains(out, "weights missing") {
		t.Fatalf("failure summary = %q", out)
	}
	if strings.Contains(out, "models ready") {
		t.Fatalf("readiness was claimed despite a failure: %q", out)
	}
	models.smartTurn.setReady()
	out = capture(b)
	if !strings.Contains(out, "Lark models ready") {
		t.Fatalf("ready summary = %q", out)
	}
}
