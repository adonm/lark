// Command lark-library provisions the offline content library: the Wikipedia
// and Gutenberg downloads plus the SQLite FTS5 index builds the brain reads.
// It is a separate binary so the real-time edge never links SQLite or the
// downloaders.
package main

import (
	"context"
	"log"
	"os"

	"lark/internal/library"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if err := library.RunCLI(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		log.Fatal(err)
	}
}
