//go:build sqlite_fts5

package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// TestLibraryLatencyInstalled measures the real installed indexes. It is a
// skip when no library is present, so the offline suite still runs on a fresh
// checkout.
func TestLibraryLatencyInstalled(t *testing.T) {
	dir := env("LARK_LIBRARY_DIR", defaultLibraryDir())
	var sources []string
	for _, name := range librarySourceOrder {
		if _, err := os.Stat(filepath.Join(dir, librarySources[name].db)); err == nil {
			sources = append(sources, name)
		}
	}
	if len(sources) == 0 {
		t.Skip("no installed library indexes")
	}
	queries := []string{
		"why is the sky blue",
		"three little pigs",
		"photosynthesis",
		"Roman empire",
		"how do volcanoes erupt",
	}
	if slices.Contains(sources, "simple") {
		queries = append(queries, "what is a mammal")
	}
	b := &larkServer{library: dir}
	ctx := context.Background()

	search := []time.Duration{}
	for _, query := range queries {
		for range 2 {
			if _, err := b.mcpLibrary(ctx, object{"op": "search", "query": query, "limit": 5}); err != nil {
				t.Fatal(err)
			}
		}
		for range 10 {
			start := time.Now()
			if _, err := b.mcpLibrary(ctx, object{"op": "search", "query": query, "limit": 5}); err != nil {
				t.Fatal(err)
			}
			search = append(search, time.Since(start))
		}
	}
	slices.Sort(search)
	p50, p95 := percentile(search, 50), percentile(search, 95)
	t.Logf("library.search on %v: p50 %v, p95 %v, n=%d", sources, p50, p95, len(search))
	// bm25 ranks every matching row on the full 7.25M-page enwiki index.
	// Ranking through the FTS5 rank column keeps that scan on the index pages;
	// snippets come from the final rows only. Measured 2026-11-23: p50 30 ms,
	// p95 160 ms across all packs (was 90/170 with snippet() on every candidate).
	// The small packs stay well under 50 ms.
	searchLimit := 50 * time.Millisecond
	if slices.Contains(sources, "enwiki") {
		searchLimit = 250 * time.Millisecond
		if p50 > 50*time.Millisecond {
			t.Fatalf("library.search p50 on enwiki %v exceeds 50ms", p50)
		}
	}
	if p95 > searchLimit {
		t.Fatalf("library.search p95 %v exceeds %v", p95, searchLimit)
	}

	// Read the strongest hit from each source and time bounded reads.
	read := []time.Duration{}
	for _, source := range sources {
		out, err := b.mcpLibrary(ctx, object{"op": "search", "query": queries[0], "source": source, "limit": 1})
		if err != nil {
			t.Fatal(err)
		}
		hits, _ := out["results"].([]libraryHit)
		if len(hits) == 0 {
			continue
		}
		for range 10 {
			start := time.Now()
			if _, err := b.mcpLibrary(ctx, object{"op": "read", "source": source, "title": hits[0].Ref}); err != nil {
				t.Fatal(err)
			}
			read = append(read, time.Since(start))
		}
	}
	if len(read) > 0 {
		slices.Sort(read)
		p50, p95 = percentile(read, 50), percentile(read, 95)
		t.Logf("library.read: p50 %v, p95 %v, n=%d", p50, p95, len(read))
		if p95 > 50*time.Millisecond {
			t.Fatalf("library.read p95 %v exceeds 50ms", p95)
		}
	}
}

func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	index := (len(sorted) - 1) * p / 100
	return sorted[index]
}
