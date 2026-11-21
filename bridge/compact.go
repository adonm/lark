package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"os"
	"slices"
	"time"
)

const mcpSchemaMeta = "mcp_schema_hash"
const mcpRuntimeContract = "3" // Bump when runtime semantics change without a schema or prompt edit.

func hashMCPSchema(tools []object, instructions, runtime, prompt string) string {
	// json.Marshal sorts map keys, including nested schema properties.
	raw, err := json.Marshal(object{"tools": tools, "instructions": instructions, "runtime": runtime, "prompt": prompt})
	if err != nil {
		panic(err) // The built-in contract must be JSON-serializable.
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type compactResult struct {
	OK        bool     `json:"ok"`
	Compacted []string `json:"compacted"`
	Skipped   []string `json:"skipped"`
}

func (o *openCode) compactActiveSessions(ctx context.Context, device string) (compactResult, error) {
	result := compactResult{Compacted: []string{}, Skipped: []string{}}
	var sessions map[string]string
	if device == "" {
		all, err := o.store.allContext(ctx)
		if err != nil {
			return result, err
		}
		sessions = all
	} else {
		sid, _, err := o.store.getContext(ctx, device)
		if err != nil {
			return result, err
		}
		sessions = map[string]string{device: sid}
	}
	var failures []error
	for _, id := range slices.Sorted(maps.Keys(sessions)) {
		if ctx.Err() != nil {
			return result, errors.Join(append(failures, o.unavailable(ctx.Err()))...)
		}
		ok, err := o.CompactSession(ctx, id, sessions[id])
		switch {
		case err != nil:
			failures = append(failures, fmt.Errorf("%s: %w", id, err))
		case ok:
			result.Compacted = append(result.Compacted, id)
		default:
			result.Skipped = append(result.Skipped, id)
		}
	}
	result.OK = len(failures) == 0
	return result, errors.Join(failures...)
}

type diagSchemaRefresh struct {
	State    string     `json:"state"`
	Attempts int        `json:"attempts"`
	RetryAt  *time.Time `json:"next_retry_at,omitempty"`
}

func (b *bridge) runSchemaUpgrade(ctx context.Context, delay, maxDelay time.Duration) {
	for attempt := 1; ctx.Err() == nil; attempt++ {
		b.diag.mu.Lock()
		b.diag.refresh.State, b.diag.refresh.Attempts, b.diag.refresh.RetryAt = "running", attempt, nil
		b.diag.mu.Unlock()
		err := b.syncSchemaUpgrade(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			b.diag.mu.Lock()
			b.diag.refresh.State = "current"
			b.diag.mu.Unlock()
			return
		}
		log.Printf("EchoEar MCP schema upgrade pending: %v", err)
		next := time.Now().Add(delay).UTC()
		b.diag.mu.Lock()
		b.diag.refresh.State, b.diag.refresh.RetryAt = "waiting", &next
		b.diag.mu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		delay = min(2*delay, maxDelay)
	}
}

func readMCPContract(agentPath string) (string, error) {
	prompt, err := os.ReadFile(agentPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read agent prompt: %w", err)
	}
	return hashMCPSchema(mcpTools, mcpInstructions, mcpRuntimeContract, string(prompt)), nil
}

func (b *bridge) syncSchemaUpgrade(ctx context.Context) error {
	current := b.oc.contract
	previous, err := b.db.getMeta(ctx, mcpSchemaMeta)
	if err != nil || previous == current {
		return err
	}
	if err := b.oc.waitForStartup(ctx); err != nil {
		return err
	}
	refresh, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	if err := b.oc.ReconnectMCP(refresh, "echoear"); err != nil {
		return err
	}
	return b.db.setMeta(ctx, mcpSchemaMeta, current)
}

func (o *openCode) waitForStartup(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for !o.reachable(ctx, "/api/info") {
		select {
		case <-ctx.Done():
			return o.unavailable(ctx.Err())
		case <-ticker.C:
		}
	}
	return nil
}

func (b *bridge) compactAPI(w http.ResponseWriter, r *http.Request) (any, error) {
	var device string
	var reconnect bool
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		input, err := jsonBody(r)
		if err != nil {
			return nil, err
		}
		if _, present := input["device_id"]; present {
			device, err = deviceID(r, input, true)
			if err != nil {
				return nil, err
			}
		}
		if value, present := input["reconnect_mcp"]; present {
			var ok bool
			if reconnect, ok = value.(bool); !ok {
				return nil, problem(400, "reconnect_mcp must be a boolean")
			}
		}
	}
	if b.oc == nil {
		return nil, problem(503, "OpenCode unavailable")
	}
	ctx, cancel := context.WithTimeout(r.Context(), turnTimeout)
	defer cancel()
	var reconnectErr error
	if reconnect {
		refresh, stop := context.WithTimeout(ctx, 15*time.Second)
		reconnectErr = b.oc.ReconnectMCP(refresh, "echoear")
		stop()
	}
	result, err := b.oc.compactActiveSessions(ctx, device)
	if err := errors.Join(reconnectErr, err); err != nil {
		status := http.StatusBadGateway
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		sendJSON(w, status, object{"ok": false, "compacted": result.Compacted, "skipped": result.Skipped, "error": err.Error()})
		return nil, nil
	}
	return result, nil
}
