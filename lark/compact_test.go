package main

import (
	"encoding/hex"
	"maps"
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

func mcpServerStatus(name, status string) []object {
	return []object{{"name": name, "status": object{"status": status}}}
}
