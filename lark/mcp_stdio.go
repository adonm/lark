package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

func mcpProxyConfig(path string) (string, string, error) {
	token, port := os.Getenv("LARK_TOKEN"), os.Getenv("LARK_PORT")
	if token == "" || port == "" {
		f, err := os.Open(path)
		if err == nil {
			defer f.Close()
			s := bufio.NewScanner(f)
			for s.Scan() {
				key, value, ok := strings.Cut(strings.TrimSpace(s.Text()), "=")
				if !ok {
					continue
				}
				value = strings.TrimSpace(value)
				if len(value) >= 2 && (value[0] == '\'' || value[0] == '"') && value[len(value)-1] == value[0] {
					value = value[1 : len(value)-1]
				}
				switch strings.TrimSpace(key) {
				case "LARK_TOKEN":
					if token == "" {
						token = value
					}
				case "LARK_PORT":
					if port == "" {
						port = value
					}
				}
			}
			if s.Err() != nil {
				return "", "", errors.New("Cannot read Lark lark.env")
			}
		} else if !os.IsNotExist(err) {
			return "", "", errors.New("Cannot open Lark lark.env")
		}
	}
	if !tokenRE.MatchString(token) {
		return "", "", errors.New("Set LARK_TOKEN in the environment or Lark lark.env")
	}
	if port == "" {
		port = "8765"
	}
	if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 {
		return "", "", errors.New("Invalid LARK_PORT")
	}
	return "http://" + net.JoinHostPort("127.0.0.1", port) + "/mcp", token, nil
}

func runMCPStdio() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	config := env("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	endpoint, token, err := mcpProxyConfig(filepath.Join(config, "lark", "lark.env"))
	if err != nil {
		return err
	}
	client := pooledClient()
	defer client.CloseIdleConnections()
	return proxyMCP(context.Background(), client, endpoint, token, os.Stdin, os.Stdout)
}

func proxyMCP(ctx context.Context, client *http.Client, endpoint, token string, in io.Reader, out io.Writer) error {
	s := bufio.NewScanner(in)
	s.Buffer(make([]byte, 4096), mcpLimit)
	ctx, cancelAll := context.WithCancel(ctx)
	defer cancelAll()
	var wg sync.WaitGroup
	var output, running sync.Mutex
	active := make(map[string]context.CancelFunc)
	var writeErr error
	write := func(raw []byte) {
		output.Lock()
		defer output.Unlock()
		if writeErr == nil {
			_, writeErr = out.Write(append(raw, '\n'))
			if writeErr != nil {
				cancelAll()
			}
		}
	}
	failure := func(id json.RawMessage, message string) {
		raw, _ := json.Marshal(rpcError(id, -32000, message))
		write(raw)
	}
	slots := make(chan struct{}, 32)
	for s.Scan() {
		line := bytes.TrimSpace(s.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg rpcRequest
		_ = json.Unmarshal(line, &msg)
		id := string(msg.ID)
		if msg.Method == "notifications/cancelled" {
			var params struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			if json.Unmarshal(msg.Params, &params) == nil {
				running.Lock()
				if cancel := active[string(params.RequestID)]; cancel != nil {
					cancel()
				}
				running.Unlock()
			}
		}
		select {
		case slots <- struct{}{}:
		default:
			if id != "" {
				failure(msg.ID, "Lark MCP proxy is busy")
			}
			continue
		}
		reqCtx, cancel := context.WithTimeout(ctx, 95*time.Second)
		if id != "" {
			running.Lock()
			if active[id] != nil {
				running.Unlock()
				cancel()
				<-slots
				failure(msg.ID, "Request ID already in use")
				continue
			}
			active[id] = cancel
			running.Unlock()
		}
		line = bytes.Clone(line)
		wg.Go(func() {
			defer func() {
				cancel()
				if id != "" {
					running.Lock()
					delete(active, id)
					running.Unlock()
				}
				<-slots
			}()
			req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(line))
			if err == nil {
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Content-Type", jsonType)
				req.Header.Set("Accept", "application/json, text/event-stream")
				var res *http.Response
				res, err = client.Do(req)
				if err == nil {
					defer res.Body.Close()
					if res.StatusCode == 202 || res.StatusCode == 204 {
						return
					}
					const responseLimit = 1 << 20
					raw, readErr := io.ReadAll(io.LimitReader(res.Body, responseLimit+1))
					var response struct {
						Version string `json:"jsonrpc"`
					}
					var compact bytes.Buffer
					if readErr == nil && len(raw) <= responseLimit && json.Unmarshal(raw, &response) == nil &&
						response.Version == "2.0" && json.Compact(&compact, raw) == nil {
						write(compact.Bytes())
						return
					}
				}
			}
			if id != "" && reqCtx.Err() != context.Canceled {
				failure(msg.ID, "Lark bridge did not return a JSON-RPC response")
			}
		})
	}
	wg.Wait()
	return errors.Join(s.Err(), writeErr)
}
