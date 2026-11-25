//go:build tools

package dogica

// The font generator runs with `//go:build ignore` and is not part of the
// package graph. Keeping its imports here makes `go mod tidy` retain the
// modules that `go generate ./...` needs to rebuild dogica.go.
import (
	_ "golang.org/x/image/font/sfnt"
	_ "golang.org/x/image/vector"
)
