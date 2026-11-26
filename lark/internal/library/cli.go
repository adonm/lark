package library

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
)

// ---- Downloader and index builder ----

func RunCLI(ctx context.Context, args []string, out, errOut io.Writer) error {
	flags := flag.NewFlagSet("library", flag.ContinueOnError)
	flags.SetOutput(errOut)
	download := flags.Bool("download", false, "download raw dumps and books")
	build := flags.Bool("build", false, "build SQLite indexes from raw files")
	force := flags.Bool("force", false, "rebuild indexes even when present")
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
	if !*download && !*build {
		return errors.New("expected --download or --build")
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
