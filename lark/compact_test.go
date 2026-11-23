package main

import (
	"encoding/hex"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestMCPSchemaHash(t *testing.T) {
	t.Parallel()
	original := hashMCPSchema(mcpTools, mcpInstructions, mcpRuntimeContract, "")
	if raw, err := hex.DecodeString(original); err != nil || len(raw) != 32 || original != hashMCPSchema(mcpTools, mcpInstructions, mcpRuntimeContract, "") {
		t.Fatal("schema hash is not a deterministic SHA-256 hex digest")
	}
	for _, key := range []string{"name", "description", "inputSchema"} {
		tools := slices.Clone(mcpTools)
		tools[0] = maps.Clone(tools[0])
		tools[0][key] = "changed"
		if hashMCPSchema(tools, mcpInstructions, mcpRuntimeContract, "") == original {
			t.Errorf("changing %s did not change the schema hash", key)
		}
	}
	for _, tc := range []struct{ name, instructions, runtime, prompt string }{
		{"instructions", "New instructions.", mcpRuntimeContract, ""},
		{"runtime", mcpInstructions, "new runtime", ""},
		{"agent prompt", mcpInstructions, mcpRuntimeContract, "New agent prompt."},
	} {
		if hashMCPSchema(mcpTools, tc.instructions, tc.runtime, tc.prompt) == original {
			t.Errorf("changing %s did not change the schema hash", tc.name)
		}
	}
}

// A missing prompt must fail startup instead of hashing an empty agent
// instruction.
func TestReadMCPContractRequiresAgentPrompt(t *testing.T) {
	t.Parallel()
	if _, err := readMCPContract(filepath.Join(t.TempDir(), "lark.md")); err == nil {
		t.Fatal("missing agents/lark.md hashed without error")
	}
	path := filepath.Join(t.TempDir(), "lark.md")
	if err := os.WriteFile(path, []byte("Use exact companion IDs."), 0o600); err != nil {
		t.Fatal(err)
	}
	hash, err := readMCPContract(path)
	if err != nil || hash == "" {
		t.Fatalf("readMCPContract = %q, %v", hash, err)
	}
}

func mcpServerStatus(name, status string) []object {
	return []object{{"name": name, "status": object{"status": status}}}
}
