package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	deployServeWait = 60 * time.Second
	deployModelWait = 2 * time.Minute
	deployBoardWait = 60 * time.Second
)

// runDeploy builds the release binary with scripts/build-lark.sh (the same
// path setup-host.sh uses), swaps it in atomically, restarts lark.service and
// waits until the server and its models are usable again.
func runDeploy(ctx context.Context, args []string, out, errOut io.Writer) error {
	if len(args) != 0 {
		fmt.Fprintln(errOut, "Usage: lark deploy")
		return errors.New("unexpected arguments")
	}
	repo, err := repoRoot()
	if err != nil {
		return err
	}
	script := filepath.Join(repo, "scripts", "build-lark.sh")
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("lark deploy needs the repo checkout with %s: %w", script, err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	binary := filepath.Join(env("XDG_DATA_HOME", filepath.Join(home, ".local/share")), "lark", "lark")
	if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
		return err
	}
	c, err := newCLIClient()
	if err != nil {
		return err
	}
	before := connectedTerminals(ctx, c)
	started := time.Now()

	build := filepath.Join(filepath.Dir(binary), fmt.Sprintf(".lark.deploy-%d", os.Getpid()))
	defer os.Remove(build)
	deployStep(out, "building")
	built := time.Now()
	cmd := exec.CommandContext(ctx, script, build)
	cmd.Stdout, cmd.Stderr = out, errOut
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build failed: %w", err)
	}
	buildTime := time.Since(built)

	install := time.Now()
	if err := os.Rename(build, binary); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	installTime := time.Since(install)

	restart := time.Now()
	if err := restartService(ctx); err != nil {
		return err
	}
	restartTime := time.Since(restart)

	serving, modelsReady, err := waitDeployHealth(ctx, c, out, errOut)
	if err != nil {
		return err
	}
	if err := waitServices(ctx, c, out); err != nil {
		return err
	}
	boards, err := waitTerminals(ctx, c, before, out)
	if err != nil {
		return err
	}
	total := time.Since(started)
	fmt.Fprintf(out, "Deploy: build %s, install %s, restart %s, serving %s, models %s, terminals %s, total %s\n",
		roundMS(buildTime), roundMS(installTime), roundMS(restartTime),
		roundMS(serving), roundMS(modelsReady), roundMS(boards), roundMS(total))
	return nil
}

func roundMS(d time.Duration) time.Duration {
	return d.Round(10 * time.Millisecond)
}

func deployStep(out io.Writer, step string) {
	fmt.Fprintf(out, "Deploy: %s\n", step)
}

func restartService(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "systemctl", "--user", "restart", "lark.service")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemctl --user restart lark.service: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// waitDeployHealth waits for the listener (serving) and then for every model
// to reach ready. A model load error fails the deploy with its message.
func waitDeployHealth(ctx context.Context, c *cliClient, out, errOut io.Writer) (serving, models time.Duration, err error) {
	started := time.Now()
	serveDeadline := started.Add(deployServeWait)
	modelDeadline := started.Add(deployModelWait)
	reported := ""
	serveAt := time.Time{}
	for {
		health, err := c.do(ctx, http.MethodGet, "/health", nil)
		if err == nil {
			if serveAt.IsZero() {
				serveAt = time.Now()
				serving = serveAt.Sub(started)
				fmt.Fprintf(out, "Deploy: serving in %s\n", roundMS(serving))
			}
			states, ready, failure := deployModels(health)
			if states != reported {
				reported = states
				fmt.Fprintf(out, "Deploy: models %s\n", states)
			}
			switch {
			case failure != "":
				return serving, 0, fmt.Errorf("model failed to load: %s", failure)
			case ready:
				models = time.Since(started)
				return serving, models, nil
			}
		}
		now := time.Now()
		deadline := serveDeadline
		if !serveAt.IsZero() {
			deadline = modelDeadline
		}
		if now.After(deadline) {
			if serveAt.IsZero() {
				return 0, 0, errors.New("server did not answer /health after 60s; check journalctl --user -u lark.service")
			}
			return serving, 0, fmt.Errorf("models still loading after %s: %s", deployModelWait, reported)
		}
		select {
		case <-ctx.Done():
			return serving, 0, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// deployModels formats per-model states from /health and reports whether all
// are ready or one failed.
func deployModels(health object) (states string, ready bool, failure string) {
	models, _ := health["models"].(object)
	if models == nil {
		// Older server: fall back to the aggregate flags.
		stt, tts := boolOf(health["stt"]), boolOf(health["tts"])
		return fmt.Sprintf("stt=%v tts=%v", stt, tts), stt && tts, ""
	}
	var parts []string
	ready = true
	for _, name := range []string{"parakeet", "kokoro", "smart_turn", "silero"} {
		model, _ := models[name].(object)
		if model == nil {
			continue
		}
		state := str(model["state"])
		parts = append(parts, name+"="+state)
		switch state {
		case modelReady:
		case modelError:
			ready = false
			if failure == "" && str(model["error"]) != "" {
				failure = name + ": " + str(model["error"])
			}
		default:
			ready = false
		}
	}
	return strings.Join(parts, " "), ready, failure
}

func connectedTerminals(ctx context.Context, c *cliClient) map[string]bool {
	connected := map[string]bool{}
	res, err := c.do(ctx, http.MethodGet, "/api/terminals", nil)
	if err != nil {
		return connected
	}
	list, _ := res["terminals"].([]any)
	for _, item := range list {
		term, _ := item.(object)
		id := str(term["id"])
		if id != "" && boolOf(term["connected"]) {
			connected[id] = true
		}
	}
	return connected
}

// waitServices waits for the supervisor-managed services that doctor checks:
// OpenCode and Delta Chat should be back before the deploy is called done.
func waitServices(ctx context.Context, c *cliClient, out io.Writer) error {
	deadline := time.Now().Add(30 * time.Second)
	var last string
	for {
		health, err := c.do(ctx, http.MethodGet, "/health", nil)
		if err == nil {
			opencode, deltachat := boolOf(health["opencode"]), boolOf(health["deltachat"])
			if opencode && deltachat {
				fmt.Fprintln(out, "Deploy: opencode=ok deltachat=ok")
				return nil
			}
			last = fmt.Sprintf("opencode=%v deltachat=%v", opencode, deltachat)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("services not ready after 30s: %s", last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// waitTerminals waits for boards that were connected before the restart to
// reconnect, then prints one line per terminal.
func waitTerminals(ctx context.Context, c *cliClient, before map[string]bool, out io.Writer) (time.Duration, error) {
	started := time.Now()
	deadline := started.Add(deployBoardWait)
	for {
		res, err := c.do(ctx, http.MethodGet, "/api/terminals", nil)
		if err == nil {
			list, _ := res["terminals"].([]any)
			pending := 0
			online := 0
			for _, item := range list {
				term, _ := item.(object)
				id := str(term["id"])
				connected := boolOf(term["connected"])
				if connected {
					online++
				}
				if before[id] && !connected {
					pending++
				}
			}
			if pending == 0 {
				fmt.Fprintf(out, "Deploy: terminals %d/%d connected\n", online, len(list))
				return time.Since(started), nil
			}
		}
		if time.Now().After(deadline) {
			return time.Since(started), fmt.Errorf("terminals did not reconnect within %s", deployBoardWait)
		}
		select {
		case <-ctx.Done():
			return time.Since(started), ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
