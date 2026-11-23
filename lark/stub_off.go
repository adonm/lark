//go:build !e2e

package main

import "net/http"

func stubSetup(string) *stubConfig { return nil }

// stubHTTP serves the e2e-only control endpoints. It is compiled out of normal
// builds, which always report the request as unhandled.
func stubHTTP(*larkServer, *http.Request, string) (any, error, bool) {
	return nil, nil, false
}
