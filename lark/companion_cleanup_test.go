package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Deleting a companion must remove the portrait directory and its debug
// captures, not orphan them on disk.
func TestDeleteCompanionRemovesPortraitAndCaptures(t *testing.T) {
	serial(t)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	b, _ := wsHarness(t, nil)
	b.portraits = &portraitStore{dir: filepath.Join(t.TempDir(), "portraits")}
	c, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "an old chameleon"})
	if err != nil {
		t.Fatal(err)
	}
	portraitID := "0123456789abcdef0123456789abcdef"
	if _, err := b.db.ExecContext(t.Context(), `UPDATE companions SET portrait_id=? WHERE id=?`, portraitID, c.ID); err != nil {
		t.Fatal(err)
	}
	poseDir := filepath.Join(b.portraits.dir, portraitID)
	if err := os.MkdirAll(poseDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(poseDir, "idle.png"), []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(debugCaptureDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(debugCaptureDir(), "capture-"+c.ID+"-20260101-000000.wav")
	if err := writeCaptureWAV(capture, []byte{1, 0, 2, 0}); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(debugCaptureDir(), "capture-other-20260101-000000.wav")
	if err := writeCaptureWAV(keep, []byte{1, 0, 2, 0}); err != nil {
		t.Fatal(err)
	}

	if err := b.deleteCompanion(t.Context(), c.ID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{poseDir, capture} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s survived companion delete: %v", path, err)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("another companion's capture was deleted: %v", err)
	}
}

func TestDeleteStoryRemovesBackgrounds(t *testing.T) {
	t.Parallel()
	b, _ := wsHarness(t, nil)
	b.portraits = &portraitStore{dir: filepath.Join(t.TempDir(), "portraits")}
	if _, _, err := b.saveStory(t.Context(), "lost_island", sampleBranchingTwee, "", true); err != nil {
		t.Fatal(err)
	}
	bgDir := filepath.Join(filepath.Dir(b.portraits.dir), "story-bg", "lost_island")
	if err := os.MkdirAll(bgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bgDir, "start.png"), []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := b.deleteStory(t.Context(), "lost_island"); err != nil || !ok {
		t.Fatalf("deleteStory = %v, %v", ok, err)
	}
	if _, err := os.Stat(bgDir); !os.IsNotExist(err) {
		t.Fatalf("story background survived delete: %v", err)
	}
}
