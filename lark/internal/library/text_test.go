//go:build sqlite_fts5

package library

import (
	"strings"
	"testing"

	"lark/internal/stories"
)

func TestTweePassageBounds(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("A sentence with several words to fill the passage. ", 2000)
	twee, err := Twee("A Long Book", text)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := stories.Parse(twee)
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled.Passages) > maxPassages {
		t.Fatalf("too many passages: %d", len(compiled.Passages))
	}
	for _, passage := range compiled.Passages {
		if !passage.End {
			if len(passage.Ops) == 0 {
				t.Fatalf("empty passage %q", passage.Name)
			}
		}
	}
	if strings.Contains(twee, "[[[") || strings.Contains(twee, "]]]") {
		t.Fatalf("unsafe bracket sequence survived:\n%s", twee)
	}
}
