package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var otaHostRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]*(:[0-9]+)?$`)

type otaBoardSpec struct {
	buildDir  string
	imageName string
	boardName string
}

var otaBoards = map[string]otaBoardSpec{
	"vocat": {buildDir: "build", imageName: "lark.bin", boardName: "esp-vocat"},
	"box3":  {buildDir: "build-box3", imageName: "lark_box3.bin", boardName: "esp-box-3"},
}

type otaStatus struct {
	Board            string `json:"board"`
	Firmware         string `json:"firmware"`
	RunningPartition string `json:"running_partition"`
	Connected        bool   `json:"connected"`
	OTAPending       bool   `json:"ota_pending"`
}

func runOTA(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("ota", flag.ContinueOnError)
	fs.SetOutput(stderr)
	noBuild := fs.Bool("no-build", false, "skip firmware build")
	board := fs.String("board", env("BOARD", "vocat"), "target board (vocat or box3)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: lark ota [--no-build] [--board vocat|box3] <ip_or_host>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("expected <ip_or_host>")
	}
	host := fs.Arg(0)
	if !otaHostRE.MatchString(host) {
		return fmt.Errorf("invalid host %q", host)
	}
	spec, ok := otaBoards[*board]
	if !ok {
		return fmt.Errorf("unknown board %q (expected vocat or box3)", *board)
	}
	token, err := loadOTAToken()
	if err != nil {
		return err
	}
	baseURL := "http://" + host
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()

	before, _, err := fetchOTAStatus(ctx, client, baseURL+"/status", 5*time.Second)
	if err != nil {
		return err
	}
	if before.Board != spec.boardName {
		actual := before.Board
		if actual == "" {
			actual = "unknown board"
		}
		return fmt.Errorf("OTA refused: expected %s, got %s", spec.boardName, actual)
	}

	repo, err := repoRoot()
	if err != nil {
		return err
	}
	if !*noBuild {
		cmd := exec.CommandContext(ctx, filepath.Join(repo, "scripts/build.sh"), *board)
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("firmware build failed: %w", err)
		}
	}

	imagePath := filepath.Join(repo, "firmware", spec.buildDir, spec.imageName)
	version, size, err := espImageVersion(imagePath)
	if err != nil {
		return err
	}
	if err := uploadOTAImage(ctx, client, baseURL+"/ota/upload", token, imagePath, size); err != nil {
		return err
	}
	return waitOTAConfirm(ctx, client, baseURL+"/status", spec.boardName, before.RunningPartition, version, 60, time.Second, stdout)
}

func loadOTAToken() (string, error) {
	if token := os.Getenv("LARK_TOKEN"); token != "" {
		if !tokenRE.MatchString(token) {
			return "", errors.New("invalid LARK_TOKEN")
		}
		return token, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	envPath := filepath.Join(env("XDG_CONFIG_HOME", filepath.Join(home, ".config")), "lark", "lark.env")
	f, err := os.Open(envPath)
	if err != nil {
		return "", fmt.Errorf("set LARK_TOKEN or add it to %s: %w", envPath, err)
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		key, raw, ok := strings.Cut(strings.TrimSpace(s.Text()), "=")
		if !ok || strings.TrimSpace(key) != "LARK_TOKEN" {
			continue
		}
		val := strings.TrimSpace(raw)
		if len(val) >= 2 && (val[0] == '\'' || val[0] == '"') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if !tokenRE.MatchString(val) {
			return "", fmt.Errorf("invalid LARK_TOKEN in %s", envPath)
		}
		return val, nil
	}
	if err := s.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("set LARK_TOKEN or add it to %s", envPath)
}

func repoRoot() (string, error) {
	project, err := filepath.Abs(env("LARK_REPO_DIR", "."))
	if err != nil {
		return "", err
	}
	if os.Getenv("LARK_REPO_DIR") == "" && filepath.Base(project) == "lark" {
		project = filepath.Dir(project)
	}
	return project, nil
}

func espImageVersion(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("firmware image not found: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	var header [80]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return "", 0, errors.New("missing ESP-IDF app image descriptor")
	}
	// ESP-IDF image header (24), first segment header (8), app description magic (4).
	if header[0] != 0xe9 || !bytes.Equal(header[32:36], []byte{0x32, 0x54, 0xcd, 0xab}) {
		return "", 0, errors.New("missing ESP-IDF app image descriptor")
	}
	raw := header[48:80]
	if i := bytes.IndexByte(raw, 0); i >= 0 {
		raw = raw[:i]
	}
	version := string(raw)
	if version == "" {
		return "", 0, errors.New("invalid firmware version in app image")
	}
	for i := 0; i < len(version); i++ {
		if version[i] < 0x20 || version[i] > 0x7e {
			return "", 0, errors.New("invalid firmware version in app image")
		}
	}
	return version, info.Size(), nil
}

func fetchOTAStatus(ctx context.Context, client *http.Client, url string, timeout time.Duration) (otaStatus, []byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return otaStatus{}, nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return otaStatus{}, nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return otaStatus{}, nil, err
	}
	if res.StatusCode != http.StatusOK {
		return otaStatus{}, nil, fmt.Errorf("GET %s: HTTP %d: %s", url, res.StatusCode, strings.TrimSpace(string(body)))
	}
	var status otaStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return otaStatus{}, nil, fmt.Errorf("decode %s: %w", url, err)
	}
	return status, bytes.TrimSpace(body), nil
}

func uploadOTAImage(ctx context.Context, client *http.Client, url, token, imagePath string, size int64) error {
	f, err := os.Open(imagePath)
	if err != nil {
		return err
	}
	defer f.Close()
	reqCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, f)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("X-Lark-Token", token)
	req.Header.Set("Content-Type", "application/octet-stream")
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 16<<10))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("POST %s: HTTP %d: %s", url, res.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func waitOTAConfirm(ctx context.Context, client *http.Client, url, boardName, beforeSlot, version string, attempts int, interval time.Duration, stdout io.Writer) error {
	for range attempts {
		status, raw, err := fetchOTAStatus(ctx, client, url, 2*time.Second)
		if err == nil &&
			status.Board == boardName &&
			status.Firmware == version &&
			status.Connected &&
			!status.OTAPending &&
			status.RunningPartition != beforeSlot &&
			(status.RunningPartition == "ota_0" || status.RunningPartition == "ota_1") {
			fmt.Fprintln(stdout, string(raw))
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
	return fmt.Errorf("device did not confirm firmware %s on a different OTA partition with a daemon connection: %s", version, url)
}
