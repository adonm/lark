//go:build sqlite_fts5

package main

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// benchLibrary builds a small wiki index and measures the search path the
// agent uses: FTS query shaping, bm25 ranking, and preview windows.
func benchLibrary(b *testing.B) *larkServer {
	b.Helper()
	dir := b.TempDir()
	pages := map[string]string{}
	for i := range 500 {
		pages["Article "+strconv.Itoa(i)] =
			"Air scatters short blue light, which is why the sky looks blue. " + strings.Repeat("More prose about light and weather. ", 20)
	}
	testWikiDB(b, filepath.Join(dir, "simple.db"), pages)
	return &larkServer{library: dir}
}

func BenchmarkLibrarySearch(b *testing.B) {
	s := benchLibrary(b)
	ctx := context.Background()
	b.ResetTimer()
	for range b.N {
		if _, err := s.mcpLibrary(ctx, object{"op": "search", "query": "why is the sky blue", "source": "simple"}); err != nil {
			b.Fatal(err)
		}
	}
}
