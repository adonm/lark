//go:build sqlite_fts5

package library

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// benchLibrary builds a small wiki index and measures the search path the
// agent uses: FTS query shaping, bm25 ranking, and preview windows.
func benchLibrary(b *testing.B) *Library {
	b.Helper()
	dir := b.TempDir()
	pages := map[string]string{}
	for i := range 500 {
		pages["Article "+strconv.Itoa(i)] =
			"Air scatters short blue light, which is why the sky looks blue. " + strings.Repeat("More prose about light and weather. ", 20)
	}
	testWikiDB(b, filepath.Join(dir, "simple.db"), pages)
	return New(dir)
}

func BenchmarkLibrarySearch(b *testing.B) {
	lib := benchLibrary(b)
	ctx := context.Background()
	b.ResetTimer()
	for range b.N {
		if _, _, err := lib.Search(ctx, "why is the sky blue", "simple", 5); err != nil {
			b.Fatal(err)
		}
	}
}
