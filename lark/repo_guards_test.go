package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDocsAvoidRetiredTerms keeps deleted features out of the documentation.
// Historical records (docs/qa-report.md) are deliberately not scanned: they
// explain what was removed and must keep the old names to do so.
func TestDocsAvoidRetiredTerms(t *testing.T) {
	t.Parallel()
	retired := []struct {
		name, pattern string
	}{
		{"webxdc panel", `(?i)webxdc`},
		{"panel route", `/panel`},
		{"panel UI flag", `--panel-ui`},
		{"weather", `(?i)\bweather\b`},
		{"location env", `LARK_LOCATION`},
		{"goja runtime", `(?i)\bgoja\b`},
		{"eval command", `\blark eval\b`},
		{"listen_seconds", `listen_seconds`},
		{"auto_volume", `auto_volume`},
		{"wake word", `Wake word|WAKE_WORD`},
		{"call protocol", `\blive Call\b|CALL_`},
		{"EchoEar header", `X-EchoEar`},
		{"bridge module", `\bbridge/`},
		{"bridge cwd", `\bcd bridge\b`},
		{"library flag", `--library`},
		{"insecure websocket", `ws://`},
		{"apps table", `(?m)^\|.*\bApps?\b.*\|`},
	}
	files := []string{
		filepath.Join("..", "README.md"),
		filepath.Join("..", "ARCHITECTURE.md"),
		filepath.Join("..", "docs", "testing.md"),
	}
	agents, err := filepath.Glob(filepath.Join("..", "agents", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, agents...)
	if len(agents) == 0 {
		t.Fatal("no agent skill file found under agents/")
	}
	for _, name := range files {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, term := range retired {
			if re := regexp.MustCompile(term.pattern); re.MatchString(text) {
				t.Errorf("%s mentions retired %s (%s); historical notes belong in docs/qa-report.md",
					name, term.name, re.FindString(text))
			}
		}
	}
}

// TestCIWorkflowRules keeps the workflow honest about the checks it claims to
// run: never the --fast subset, and no schedule-gated job without a trigger.
func TestCIWorkflowRules(t *testing.T) {
	t.Parallel()
	workflows, err := filepath.Glob(filepath.Join("..", ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(workflows) == 0 {
		t.Fatal("no CI workflows")
	}
	cronRE := regexp.MustCompile(`(?m)^\s+schedule:\s*$`)
	for _, name := range workflows {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		if strings.Contains(text, "--fast") {
			t.Errorf("%s runs the --fast subset; CI must run the full gate", name)
		}
		if strings.Contains(text, "event_name == 'schedule'") && !cronRE.MatchString(text) {
			t.Errorf("%s gates a job on schedule but has no schedule: trigger", name)
		}
	}
}
