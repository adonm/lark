package main

import "testing"

// testHex and mustHex mint identifiers for test fixtures. Production code
// handles randomHex errors explicitly; fixtures fail loudly instead.
func testHex(t *testing.T, n int) string {
	t.Helper()
	s, err := randomHex(n)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustHex(n int) string {
	s, err := randomHex(n)
	if err != nil {
		panic(err)
	}
	return s
}
