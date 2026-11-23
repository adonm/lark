package main

import (
	"strings"
	"testing"
)

func TestTranscriptLoggingIsRedactedByDefault(t *testing.T) {
	old := debugTranscripts
	t.Cleanup(func() { debugTranscripts = old })

	debugTranscripts = false
	redacted := redactTranscript("the secret lantern phrase")
	if strings.Contains(redacted, "secret") || strings.Contains(redacted, "lantern") {
		t.Fatalf("redacted output leaked text: %q", redacted)
	}
	if !strings.Contains(redacted, "len=25") || !strings.Contains(redacted, "words=4") || !strings.Contains(redacted, "sha=") {
		t.Fatalf("redacted output lacks the shape: %q", redacted)
	}
	if redactTranscript("a") == redactTranscript("b") {
		t.Fatal("redacted output is not content-sensitive")
	}

	debugTranscripts = true
	if got := redactTranscript("the secret lantern phrase"); got != "the secret lantern phrase" {
		t.Fatalf("opt-in did not keep the text: %q", got)
	}
}
