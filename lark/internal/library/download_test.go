//go:build sqlite_fts5

package library

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPinnedGutenbergIDsResolveFromRepoRoot(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "scripts", "gutenberg-ids.txt"), []byte("11\n# a comment\n22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LARK_GUTENBERG_IDS", "")
	t.Setenv("LARK_REPO_DIR", repo)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	ids, err := pinnedGutenbergIDs()
	if err != nil || !slices.Equal(ids, []int64{11, 22}) {
		t.Fatalf("pinnedGutenbergIDs = %v, %v; want [11 22] from %s", ids, err, repo)
	}
}

func TestDownloadResumeAndChecksum(t *testing.T) {
	t.Parallel()
	payload := bytes.Repeat([]byte("offline library payload\n"), 4096)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "file", time.Time{}, bytes.NewReader(payload))
	}))
	defer server.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "file.bin")
	part := dest + ".part"
	if err := os.WriteFile(part, payload[:1000], 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum(payload)
	expect := hex.EncodeToString(sum[:])
	if err := downloadFile(t.Context(), server.Client(), server.URL, dest, "sha1", expect, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("resumed file mismatch: %d bytes", len(got))
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatalf("partial file survived: %v", err)
	}

	if err := downloadFile(t.Context(), server.Client(), server.URL, filepath.Join(dir, "bad.bin"), "sha1",
		strings.Repeat("0", 40), ioDiscard{}); err == nil {
		t.Fatal("expected checksum mismatch")
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

func TestChecksumAndManifestWritesReportErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := writeChecksums(filepath.Join(dir, "SHA256SUMS"), map[string]string{"a": "b"}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(file, "SHA256SUMS")
	if err := writeChecksums(blocked, map[string]string{"a": "b"}); err == nil {
		t.Fatal("writeChecksums into a directory path succeeded")
	}
	if err := appendLine(blocked, map[string]any{"id": 1}); err == nil {
		t.Fatal("appendLine into a directory path succeeded")
	}
}
