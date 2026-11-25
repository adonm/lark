//go:build sqlite_fts5

package library

import (
	"strings"
	"testing"
)

func TestTweePassageBounds(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("A sentence with several words to fill the passage. ", 2000)
	twee, err := Twee("A Long Book", text)
	if err != nil {
		t.Fatal(err)
	}
	headers := strings.Count(twee, "\n:: ")
	if headers > maxPassages {
		t.Fatalf("too many passages: %d", headers)
	}
	if !strings.HasPrefix(twee, ":: StoryTitle\nA Long Book\n\n") {
		t.Fatalf("missing story title:\n%s", twee[:min(len(twee), 200)])
	}
	if !strings.Contains(twee, "{\"end\":true}") {
		t.Fatalf("last passage is not an ending:\n%s", twee)
	}
	if strings.Contains(twee, "[[[") || strings.Contains(twee, "]]]") {
		t.Fatalf("unsafe bracket sequence survived:\n%s", twee)
	}
}
