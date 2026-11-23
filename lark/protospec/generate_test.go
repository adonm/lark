package protospec

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestGeneratedFilesUpToDate fails when a generated file is edited by hand, or
// the spec changed without running go generate ./...
func TestGeneratedFilesUpToDate(t *testing.T) {
	root := filepath.Join("..", "..")
	files, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		want := f.Content
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Errorf("%s: %v (run go generate ./...)", f.Path, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is stale; run go generate ./...", f.Path)
		}
	}
}
