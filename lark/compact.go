package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
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

type diagSchemaRefresh struct {
	State    string     `json:"state"`
	Attempts int        `json:"attempts"`
	RetryAt  *time.Time `json:"next_retry_at,omitempty"`
}

func (b *larkServer) runSchemaUpgrade(ctx context.Context, delay, maxDelay time.Duration) {
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
			go b.prewarmSessions(ctx)
			return
		}
		log.Printf("Lark MCP schema upgrade pending: %v", err)
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

func (b *larkServer) prewarmSessions(ctx context.Context) {
	if b.oc == nil {
		return
	}
	list, err := b.allCompanions(ctx)
	if err != nil {
		return
	}
	for _, c := range list {
		if ctx.Err() != nil {
			return
		}
		b.oc.prewarm(ctx, c.ID, c.Name)
	}
}

func (b *larkServer) syncSchemaUpgrade(ctx context.Context) error {
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
	if err := b.oc.ReconnectMCP(refresh, "lark"); err != nil {
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
