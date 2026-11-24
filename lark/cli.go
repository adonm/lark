package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type cliClient struct {
	base  string
	token string
	http  *http.Client
}

func newCLIClient() (*cliClient, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	config := env("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	endpoint, token, err := mcpProxyConfig(filepath.Join(config, "lark", "lark.env"))
	if err != nil {
		return nil, err
	}
	base := os.Getenv("LARK_URL")
	if base == "" {
		base = strings.TrimSuffix(endpoint, "/mcp")
	}
	return &cliClient{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http:  &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func (c *cliClient) postRaw(ctx context.Context, path string, body []byte) (object, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Lark-Token", c.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var out object
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		_ = dec.Decode(&out)
	}
	if res.StatusCode >= 400 {
		if msg := str(out["error"]); msg != "" {
			return nil, errors.New(msg)
		}
		return nil, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return out, nil
}

func (c *cliClient) do(ctx context.Context, method, path string, body any) (object, error) {
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Lark-Token", c.token)
	if body != nil {
		req.Header.Set("Content-Type", jsonType)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	var out object
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&out); err != nil {
			if res.StatusCode >= 400 {
				return nil, fmt.Errorf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(raw)))
			}
			return nil, err
		}
	}
	if res.StatusCode >= 400 {
		if msg := str(out["error"]); msg != "" {
			return nil, errors.New(msg)
		}
		return nil, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return out, nil
}

func runCLI(ctx context.Context, cmd string, args []string, out, errOut io.Writer) error {
	c, err := newCLIClient()
	if err != nil {
		return err
	}
	return runCLIWithClient(ctx, c, cmd, args, out, errOut)
}

type cliHandler func(ctx context.Context, c *cliClient, args []string, out, errOut io.Writer) error

var cliCommands = map[string]cliHandler{
	"status":     cliStatus,
	"companions": cliCompanions,
	"terminals":  cliTerminals,
	"capture":    cliCapture,
	"stt-bench":  cliSTTBench,
	"tunnels":    cliTunnels,
}

func runCLIWithClient(ctx context.Context, c *cliClient, cmd string, args []string, out, errOut io.Writer) error {
	handler, ok := cliCommands[cmd]
	if !ok {
		return fmt.Errorf("unknown command %q", cmd)
	}
	return handler(ctx, c, args, out, errOut)
}

func jsonInt(value any) int64 {
	switch v := value.(type) {
	case json.Number:
		n, _ := v.Int64()
		return n
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	}
	return 0
}
