package main

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"
)

// benchSeedMessages fills one companion's history with mixed in-person and
// Delta Chat rows so paging and search exercise both filters.
func benchSeedMessages(tb testing.TB, s *larkServer, n int) {
	tb.Helper()
	for i := range n {
		source, sender, channel := "deltachat", "Mum", "deltachat"
		if i%3 == 0 {
			source, sender, channel = "voice", "You", "in_person"
		}
		if _, err := s.db.Exec(insertMessage, fmt.Sprintf("m%d", i), "greg", "user", sender,
			fmt.Sprintf("Message number %d about the weather", i), source, channel, "",
			time.Now().Add(-time.Duration(i)*time.Minute).UnixMilli()); err != nil {
			tb.Fatal(err)
		}
	}
}

// The remote page polls the snapshot every 750 ms, and the story list is the
// expensive part of that read.
func BenchmarkRemoteSnapshot(b *testing.B) {
	s := benchServer(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if snap := s.remoteSnapshot(context.Background(), "greg", false); snap["companion"] == nil {
			b.Fatal("empty snapshot")
		}
	}
}

func BenchmarkHistoryPage(b *testing.B) {
	s := benchServer(b)
	benchSeedMessages(b, s, 200)
	before := &historyCursor{TS: time.Now().UnixMilli(), ID: "m999"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		page, err := s.readHistoryPage(context.Background(), "greg", 20, before)
		if err != nil || len(page.Messages) == 0 {
			b.Fatalf("page = %+v, %v", page, err)
		}
	}
}

func BenchmarkRemoteHistory(b *testing.B) {
	s := benchServer(b)
	benchSeedMessages(b, s, 200)
	tok := remoteToken{CompanionID: "greg", Contact: "mum@example.org", ContactName: "Mum"}
	query := url.Values{"limit": []string{"20"}, "channel": []string{"all"}, "q": []string{"message"}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		page, err := s.remoteHistory(context.Background(), tok, query)
		if err != nil || len(page["items"].([]object)) == 0 {
			b.Fatalf("history = %+v, %v", page, err)
		}
	}
}
