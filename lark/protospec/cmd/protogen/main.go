// Command protogen renders the protocol header, Go codec, docs and fixtures.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"lark/protospec"
)

func main() {
	root := flag.String("root", "..", "repository root, relative to the lark package")
	flag.Parse()
	files, err := protospec.Render()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, f := range files {
		path := filepath.Join(*root, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := os.WriteFile(path, f.Content, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
