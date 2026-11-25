package library

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
)

func Status(ctx context.Context, dir string) map[string]any {
	out := map[string]any{"dir": dir, "sources": map[string]any{}}
	for _, name := range sourceOrder {
		pack, _ := libraryPackByName(name)
		path := filepath.Join(dir, sources[name].db)
		entry := map[string]any{"path": path}
		available, reason := libraryPackHealth(dir, pack)
		entry["available"] = available
		if reason != "" {
			entry["error"] = reason
		}
		if info, err := os.Stat(path); err == nil {
			entry["db_bytes"] = info.Size()
		} else if !errors.Is(err, os.ErrNotExist) {
			entry["error"] = err.Error()
		}
		if available {
			db, err := openDB(path, true)
			if err != nil {
				entry["available"], entry["error"] = false, err.Error()
			} else {
				table := "pages"
				if name == "gutenberg" {
					table = "books"
				}
				var total int
				if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&total); err != nil {
					entry["available"], entry["error"] = false, err.Error()
				} else {
					entry["items"] = total
				}
				var built string
				if db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='built_at'`).Scan(&built) == nil {
					entry["built_at"] = built
				}
				db.Close()
			}
		}
		if lastErr := readPackError(dir, name); lastErr != "" {
			entry["last_error"] = lastErr
		}
		if name == "gutenberg" {
			if books, err := os.ReadDir(filepath.Join(dir, "books")); err == nil {
				var bytes int64
				for _, book := range books {
					if info, err := book.Info(); err == nil {
						bytes += info.Size()
					}
				}
				entry["book_files"], entry["book_bytes"] = len(books), bytes
			}
		}
		out["sources"].(map[string]any)[name] = entry
	}
	return out
}

// ---- Downloader and index builder ----

func RunCLI(ctx context.Context, args []string, out, errOut io.Writer) error {
	flags := flag.NewFlagSet("library", flag.ContinueOnError)
	flags.SetOutput(errOut)
	download := flags.Bool("download", false, "download raw dumps and books")
	build := flags.Bool("build", false, "build SQLite indexes from raw files")
	force := flags.Bool("force", false, "rebuild indexes even when present")
	status := flags.Bool("status", false, "print library paths, sizes, and counts")
	plan := flags.Bool("plan", false, "print the disk-aware provisioning plan and exit")
	search := flags.String("search", "", "search the installed indexes and print JSON")
	read := flags.String("read", "", "read a bounded section and print JSON")
	source := flags.String("source", "all", "all, enwiki, simple, or gutenberg")
	limit := flags.Int("limit", 3000, "maximum Gutenberg titles")
	jobs := flags.Int("jobs", 8, "parallel Gutenberg downloads")
	if err := flags.Parse(args); err != nil {
		return err
	}
	dir := env("LARK_LIBRARY_DIR", DefaultDir())
	if dir == "" {
		return errors.New("cannot resolve the library directory")
	}
	if err := os.MkdirAll(filepath.Join(dir, "raw"), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "books"), 0o700); err != nil {
		return err
	}
	if *status {
		raw, _ := json.MarshalIndent(Status(ctx, dir), "", "  ")
		fmt.Fprintln(out, string(raw))
		return nil
	}
	if *plan {
		p, err := planLibrary(dir, nil)
		if err != nil {
			return err
		}
		raw, _ := json.MarshalIndent(p, "", "  ")
		fmt.Fprintln(out, string(raw))
		return nil
	}
	if *search != "" || *read != "" {
		lib := New(dir)
		name := *source
		if name == "all" {
			name = "auto"
		}
		var result any
		var err error
		if *read != "" {
			result, err = lib.Read(ctx, name, *read, 0)
		} else {
			hits, available, searchErr := lib.Search(ctx, *search, name, 5)
			err = searchErr
			result = map[string]any{"ok": true, "results": hits, "available": available}
		}
		if err != nil {
			return err
		}
		raw, _ := json.MarshalIndent(result, "", "  ")
		fmt.Fprintln(out, string(raw))
		return nil
	}
	if !*download && !*build {
		return errors.New("expected --plan, --download, --build, or --status")
	}
	if *download {
		if *source == "all" || *source == "enwiki" {
			if err := downloadWikipedia(ctx, dir, "enwiki", out); err != nil {
				return err
			}
		}
		if *source == "all" || *source == "simple" {
			if err := downloadWikipedia(ctx, dir, "simple", out); err != nil {
				return err
			}
		}
		if *source == "all" || *source == "gutenberg" {
			if err := downloadGutenberg(ctx, dir, *limit, *jobs, out); err != nil {
				return err
			}
		}
	}
	if *build {
		if *source == "all" || *source == "enwiki" {
			if err := buildWikipedia(ctx, dir, "enwiki", *force, out); err != nil {
				return err
			}
		}
		if *source == "all" || *source == "simple" {
			if err := buildWikipedia(ctx, dir, "simple", *force, out); err != nil {
				return err
			}
		}
		if *source == "all" || *source == "gutenberg" {
			if err := buildGutenberg(ctx, dir, *force, out); err != nil {
				return err
			}
		}
	}
	return nil
}

func libraryLog(out io.Writer, format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	fmt.Fprintln(out, line)
	log.Print("library: " + line)
}
