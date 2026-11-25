package main

import (
	"os"
	"path/filepath"
	"testing"

	"lark/internal/audio"
)

func TestParakeetRejectsWrongModel(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), parakeetModel)
	if err := audio.CheckParakeetModel(path); err == nil {
		t.Fatal("accepted missing model")
	}
	if err := os.WriteFile(path, []byte("not the pinned GGUF"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := audio.CheckParakeetModel(path); err == nil {
		t.Fatal("accepted unpinned model")
	}
}
