// Package library owns bulk content acquisition for the offline library: the
// raw dump and book downloads plus the SQLite FTS5 index builders. Runtime
// search, read, list, import, and disk planning live in the brain server, so
// this package keeps no query path and opens no read-only databases.
package library

import (
	"os"
	"path/filepath"
)

const (
	maxText       = 24 << 10 // Cap stored article text.
	gutenbergBase = "https://gutenberg.pglaf.org"
	wikiDumpsBase = "https://dumps.wikimedia.org"
	userAgent     = "LarkLibrary/1.0 (local companion reader)"
)

type source struct {
	name string
	db   string
}

var sources = map[string]source{
	"simple":    {name: "simple", db: "simple.db"},
	"enwiki":    {name: "enwiki", db: "enwiki.db"},
	"gutenberg": {name: "gutenberg", db: "gutenberg.db"},
}

// DefaultDir is $XDG_DATA_HOME/lark/library.
func DefaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data := env("XDG_DATA_HOME", filepath.Join(home, ".local/share"))
	return filepath.Join(data, "lark", "library")
}

func repoRoot() (string, error) {
	project, err := filepath.Abs(env("LARK_REPO_DIR", "."))
	if err != nil {
		return "", err
	}
	if os.Getenv("LARK_REPO_DIR") == "" && filepath.Base(project) == "lark" {
		if _, err := os.Stat(filepath.Join(project, "firmware")); err != nil {
			project = filepath.Dir(project)
		}
	}
	return project, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
