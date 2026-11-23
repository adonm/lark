package main

import "testing"

// serial marks a test that must not run alongside the parallel suite. Tests in
// this package call t.Parallel unless they share process state: t.Setenv (Go
// panics when combined with t.Parallel), a package-level setting such as
// listenWindow, or a measured wall-clock bound that parallel load would
// invalidate. The test runner runs every test without t.Parallel to
// completion before it starts the parallel batch, so serial is documentation
// rather than a lock: it makes the exception reviewable and grep-able.
func serial(t *testing.T) {
	t.Helper()
}
