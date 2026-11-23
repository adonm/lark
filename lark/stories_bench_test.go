package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// benchServer builds a companion with many stories and a saved reading
// position, which is the shape that made the 750 ms remote poll expensive.
func benchServer(tb testing.TB) *larkServer {
	tb.Helper()
	db, err := openLarkDB(context.Background(), filepath.Join(tb.TempDir(), "lark.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { db.Close() })
	b := &larkServer{db: db, root: context.Background(), terminals: terminalStore(db),
		history: &chatHistory{db: db}, voices: companionVoiceStore(db, heartVoice)}
	if _, err := b.createCompanion(context.Background(), object{"id": "greg", "name": "Greg", "form": "an old chameleon"}); err != nil {
		tb.Fatal(err)
	}
	body := strings.Repeat("A quiet passage with a little weather and a lantern. ", 7)
	for i := range 24 {
		var twee strings.Builder
		twee.WriteString(":: StoryTitle\nStory " + fmt.Sprint(i) + "\n\n")
		for p := range 12 {
			twee.WriteString(fmt.Sprintf(":: P%d\n%s\n\n[[On->P%d]]\n\n", p, body, (p+1)%12))
		}
		if _, _, err := b.saveStory(context.Background(), fmt.Sprintf("story-%02d", i), twee.String(), "", true); err != nil {
			tb.Fatal(err)
		}
	}
	if err := b.setStoryProgress(context.Background(), storyProgress{CompanionID: "greg", StoryID: "story-00", Passage: "P6"}); err != nil {
		tb.Fatal(err)
	}
	return b
}

func BenchmarkListStoriesShapes(b *testing.B) {
	s := benchServer(b)
	ctx := context.Background()
	b.Run("full-twee", func(b *testing.B) {
		for range b.N {
			if _, err := s.listStories(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("summaries", func(b *testing.B) {
		for range b.N {
			if _, err := s.listStorySummaries(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkRemoteSnapshotStories(b *testing.B) {
	s := benchServer(b)
	ctx := context.Background()
	b.Run("cold-cache", func(b *testing.B) {
		for range b.N {
			s.invalidateStoryCache("story-00")
			if snap := s.remoteSnapshot(ctx, "greg", false); snap == nil {
				b.Fatal("empty snapshot")
			}
		}
	})
	b.Run("warm-cache", func(b *testing.B) {
		s.remoteSnapshot(ctx, "greg", false)
		for range b.N {
			if snap := s.remoteSnapshot(ctx, "greg", false); snap == nil {
				b.Fatal("empty snapshot")
			}
		}
	})
}
