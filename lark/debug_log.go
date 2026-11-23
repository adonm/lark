package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// debugTranscripts opts in to raw transcript text in debug logs. Endpoint
// logging is verbose but must default to lengths and hashes: a journal is read
// by more people than the speaker expects.
var debugTranscripts = os.Getenv("LARK_DEBUG_TRANSCRIPTS") != ""

// redactTranscript returns the text only when explicitly enabled; otherwise a
// stable, low-detail descriptor suitable for endpoint diagnostics.
func redactTranscript(text string) string {
	if debugTranscripts {
		return text
	}
	sum := sha256.Sum256([]byte(text))
	return fmt.Sprintf("<len=%d words=%d sha=%s>", len(text), len(strings.Fields(text)), hex.EncodeToString(sum[:6]))
}
