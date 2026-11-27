// Command lark-ops runs host-side device operations that do not belong in the
// real-time edge. Firmware OTA needs the HTTP client and the firmware checkout,
// so keeping it here leaves net/http out of lark-edge.
package main

import (
	"log"
	"os"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if len(os.Args) < 2 || os.Args[1] != "ota" {
		log.Fatal("usage: lark-ops ota [--no-build] [--board vocat|box3] <ip_or_host>")
	}
	if err := runOTA(os.Args[2:]); err != nil {
		log.Fatal(err)
	}
}
