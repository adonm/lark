package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"time"
)

const fixtureName = "sessions.json"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "lark-eval:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	usage := func() error { return errors.New("usage: lark-eval {export|score|replay} [flags]") }
	if len(args) == 0 {
		return usage()
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	var input, label, turns, replayURL, location string
	var deadline time.Duration
	switch args[0] {
	case "export":
	case "score", "replay":
		fs.StringVar(&input, "fixture", "", "fixture path (default: private export)")
		if args[0] == "replay" {
			fs.StringVar(&label, "session", "", "session label from score")
			fs.StringVar(&turns, "turns", "", "comma-separated 1-based turn numbers (max 3)")
			fs.StringVar(&replayURL, "live", "", "explicit loopback URL of a disposable OpenCode + MCP pair; omit for deterministic stub")
			fs.StringVar(&location, "workspace", "", "absolute disposable OpenCode workspace")
			fs.DurationVar(&deadline, "timeout", 45*time.Second, "max duration per live turn (max 90s)")
		}
	default:
		return usage()
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return usage()
	}
	var f fixture
	if args[0] == "export" {
		root, err := storePath()
		if err != nil {
			return err
		}
		api, err := productionClient()
		if err != nil {
			return err
		}
		defer api.client.CloseIdleConnections()
		ctx, stop := context.WithTimeout(ctx, 2*time.Minute)
		defer stop()
		f, err = collect(ctx, api)
		if err != nil {
			return err
		}
		if err := saveFixture(root, f); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "Saved %d private session histories to the Lark eval store.\n", len(f.Sessions))
	} else {
		if input == "" {
			root, err := storePath()
			if err != nil {
				return err
			}
			input = filepath.Join(root, fixtureName)
		}
		var err error
		f, err = readFixture(input)
		if err != nil {
			return err
		}
	}
	scores := make([]sessionScore, 0, len(f.Sessions))
	source := "history"
	if args[0] == "replay" {
		results, mode, err := replay(ctx, f, label, turns, replayURL, location, deadline)
		if err != nil {
			return err
		}
		scores, source = results, mode
	} else {
		for _, h := range f.Sessions {
			scores = append(scores, score(h))
		}
	}
	table(stderr, scores)
	out := struct {
		Source   string         `json:"source"`
		Sessions []sessionScore `json:"sessions"`
	}{source, scores}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(out)
}

func storePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("cannot resolve home directory")
	}
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		data = filepath.Join(home, ".local", "share")
	}
	if !filepath.IsAbs(data) {
		return "", errors.New("XDG_DATA_HOME must be absolute")
	}
	return filepath.Join(data, "lark", "evals"), nil
}

func resolvedPath(abs string) (string, error) {
	p, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return p, nil
	}
	// A dangling symlink isn't an absent directory that we can safely create.
	if _, statErr := os.Lstat(abs); !os.IsNotExist(statErr) || filepath.Dir(abs) == abs {
		return "", errors.New("cannot resolve private eval path")
	}
	root, err := resolvedPath(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(root, filepath.Base(abs)), nil
}

func saveFixture(root string, f fixture) error {
	root, err := resolvedPath(root)
	if err != nil {
		return err
	}
	for dir := root; ; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
			return errors.New("refusing to export private histories into a Git worktree")
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return errors.New("cannot create private eval directory")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("private eval directory must have mode 0700")
	}
	tmp, err := os.CreateTemp(root, ".sessions-")
	if err != nil {
		return errors.New("cannot create private eval fixture")
	}
	defer os.Remove(tmp.Name())
	writeErr := json.NewEncoder(tmp).Encode(f)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil || os.Rename(tmp.Name(), filepath.Join(root, fixtureName)) != nil {
		return errors.New("cannot save private eval fixture")
	}
	return nil
}

func readFixture(path string) (fixture, error) {
	var f fixture
	const max = 64 << 20
	file, err := os.Open(path)
	if err != nil {
		return f, errors.New("cannot open eval fixture (run export first or pass -fixture)")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil || len(raw) > max || json.Unmarshal(raw, &f) != nil || f.Version != 1 || len(f.Sessions) == 0 {
		return f, errors.New("invalid or oversized eval fixture")
	}
	for _, h := range f.Sessions {
		if !publicLabel.MatchString(h.Label) {
			return f, errors.New("invalid eval session label")
		}
	}
	return f, nil
}
