package main

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"lark/internal/library"
)

// mcpLibrary is the typed MCP entry point.
func (b *larkServer) mcpLibrary(ctx context.Context, args object) (object, error) {
	op := strings.TrimSpace(str(args["op"]))
	start := time.Now()
	var out object
	var err error
	switch op {
	case "search":
		out, err = b.librarySearch(ctx, args)
	case "read":
		out, err = b.libraryRead(ctx, args)
	case "list":
		out, err = b.libraryList(ctx, args)
	case "import":
		out, err = b.libraryImport(ctx, args)
	default:
		return nil, fieldError("op", "expected search, read, list, or import")
	}
	opened, _ := out["opened"].(bool)
	b.libraryStats.record(op, str(args["query"]), str(args["title"]), time.Since(start), err == nil, opened)
	return out, err
}

func (b *larkServer) librarySearch(ctx context.Context, args object) (object, error) {
	query := strings.TrimSpace(str(args["query"]))
	if query == "" {
		return nil, fieldError("query", "required")
	}
	source := strings.TrimSpace(str(args["source"]))
	if source == "" {
		source = "auto"
	}
	limit := 5
	if n, err := mcpUint(args, "limit"); err != nil {
		return nil, err
	} else if n != nil {
		limit = int(*n)
	}
	if limit < 1 || limit > 20 {
		return nil, fieldError("limit", "expected 1–20")
	}
	if source != "auto" && !library.IsSource(source) {
		return nil, fieldError("source", "expected auto, simple, enwiki, or gutenberg")
	}
	hits, available, err := b.lib.Search(ctx, query, source, limit)
	if err != nil {
		return nil, b.libraryError(err, "Cannot search the library")
	}
	return object{"ok": true, "results": hits, "available": available}, nil
}

func (b *larkServer) libraryRead(ctx context.Context, args object) (object, error) {
	source := strings.TrimSpace(str(args["source"]))
	ref := strings.TrimSpace(str(args["title"]))
	if ref == "" {
		ref = strings.TrimSpace(str(args["ref"]))
	}
	if ref == "" {
		return nil, fieldError("title", "required")
	}
	if source == "" {
		source = "auto"
	}
	if source != "auto" && !library.IsSource(source) {
		return nil, fieldError("source", "expected auto, simple, enwiki, or gutenberg")
	}
	section := 0
	if n, err := mcpUint(args, "section"); err != nil {
		return nil, err
	} else if n != nil {
		section = int(*n)
	}
	if section < 0 || section > 100000 {
		return nil, fieldError("section", "expected a non-negative integer")
	}
	out, err := b.lib.Read(ctx, source, ref, section)
	if err != nil {
		if errors.Is(err, library.ErrNotFound) {
			if source == "auto" {
				return nil, problem(404, "Library text not found")
			}
			return nil, err
		}
		return nil, b.libraryError(err, "Cannot read library text")
	}
	return out, nil
}

func (b *larkServer) libraryList(ctx context.Context, args object) (object, error) {
	source := strings.TrimSpace(str(args["source"]))
	if source == "" {
		source = "auto"
	}
	limit := 50
	if n, err := mcpUint(args, "limit"); err != nil {
		return nil, err
	} else if n != nil {
		limit = int(*n)
	}
	if limit < 1 || limit > 200 {
		return nil, fieldError("limit", "expected 1–200")
	}
	if source != "auto" && !library.IsSource(source) {
		return nil, fieldError("source", "expected auto, simple, enwiki, or gutenberg")
	}
	out, err := b.lib.List(ctx, source, limit)
	if err != nil {
		return nil, b.libraryError(err, "Cannot list the library")
	}
	return out, nil
}

func (b *larkServer) libraryImport(ctx context.Context, args object) (object, error) {
	source := strings.TrimSpace(str(args["source"]))
	ref := strings.TrimSpace(str(args["title"]))
	if ref == "" {
		ref = strings.TrimSpace(str(args["ref"]))
	}
	if ref == "" {
		return nil, fieldError("title", "required")
	}
	if source != "" && source != "auto" && !library.IsSource(source) {
		return nil, fieldError("source", "expected auto, simple, enwiki, or gutenberg")
	}
	title, author, text, used, err := b.lib.SourceText(ctx, source, ref)
	if err != nil {
		if errors.Is(err, library.ErrNotFound) {
			if source == "" || source == "auto" {
				return nil, problem(404, "Library text not found")
			}
			return nil, err
		}
		return nil, b.libraryError(err, "Cannot import library text")
	}
	twee, err := library.Twee(title, text)
	if err != nil {
		return nil, problem(400, err.Error())
	}
	if source == "" || source == "auto" {
		source = used
	}
	slug := b.nextAvailableStorySlug(ctx, title)
	saved, cs, err := b.saveStory(ctx, slug, twee, author, true)
	if err != nil {
		return nil, err
	}
	openOn := true
	if raw, ok := args["open"]; ok {
		openOn, _ = raw.(bool)
	}
	companionID := strings.ToLower(strings.TrimSpace(str(args["companion_id"])))
	opened := false
	if openOn && companionID != "" && b.hub != nil {
		for _, d := range b.hub.liveFor(companionID) {
			b.hub.openStory(d, saved, false)
			opened = true
		}
	}
	return object{"ok": true, "id": saved.ID, "title": saved.Title, "author": saved.Author,
		"passages": saved.Passages, "start": cs.Passages[cs.Start].Name, "source": source, "ref": ref,
		"opened": opened}, nil
}

// libraryError logs the underlying SQLite or filesystem detail and returns the
// server's stable problem shape to the agent.
func (b *larkServer) libraryError(err error, message string) error {
	if err == nil || errors.Is(err, library.ErrNotFound) {
		return err
	}
	log.Printf("library: %s: %v", message, err)
	return problem(502, message)
}

type libraryCall struct {
	Op    string  `json:"op"`
	Query string  `json:"query,omitempty"`
	MS    float64 `json:"ms"`
	OK    bool    `json:"ok"`
	At    string  `json:"at"`
}

type libraryStats struct {
	mu       sync.Mutex
	searches int64
	reads    int64
	imports  int64
	opens    int64
	lists    int64
	recent   []libraryCall
}

func (s *libraryStats) record(op, query, title string, elapsed time.Duration, ok, opened bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch op {
	case "search":
		s.searches++
		if query == "" {
			query = title
		}
	case "read":
		s.reads++
		if query == "" {
			query = title
		}
	case "import":
		s.imports++
		if opened {
			s.opens++
		}
		if query == "" {
			query = title
		}
	case "list":
		s.lists++
	}
	s.recent = append(s.recent, libraryCall{Op: op, Query: shortText(query, 96, false),
		MS: milliseconds(elapsed), OK: ok, At: time.Now().UTC().Format(time.RFC3339)})
	if len(s.recent) > 20 {
		s.recent = s.recent[len(s.recent)-20:]
	}
}

func (b *larkServer) libraryReport() object {
	b.libraryStats.mu.Lock()
	out := object{"searches": b.libraryStats.searches, "reads": b.libraryStats.reads,
		"imports": b.libraryStats.imports, "opens": b.libraryStats.opens, "lists": b.libraryStats.lists,
		"recent": append([]libraryCall(nil), b.libraryStats.recent...)}
	b.libraryStats.mu.Unlock()
	return out
}
