package main

import "testing"

func BenchmarkParseTwee(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		if _, err := parseTwee(sampleBranchingTwee); err != nil {
			b.Fatal(err)
		}
	}
}
