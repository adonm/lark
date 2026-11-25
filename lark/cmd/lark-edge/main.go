// Command lark-edge runs the real-time media edge: the hardware terminal
// server and the Unix-socket IPC endpoint consumed by the TypeScript brain.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"lark/internal/edge"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if len(os.Args) > 1 && os.Args[1] == "ota" {
		if err := runOTA(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	fs := flag.NewFlagSet("lark-edge", flag.ExitOnError)
	addr := fs.String("addr", edge.DefaultAddr, "terminal TCP listen address")
	sock := fs.String("sock", "", "brain Unix socket path (default $LARK_EDGE_SOCK or ~/.local/share/lark/edge.sock)")
	data := fs.String("data", "", "data directory for models (default $LARK_DATA_DIR or ~/.local/share/lark)")
	noModels := fs.Bool("no-models", false, "start without loading speech models")
	fs.Parse(os.Args[1:])

	e, err := edge.New(edge.Config{
		Addr: *addr, Sock: *sock, DataDir: *data, DisableModels: *noModels,
	})
	if err != nil {
		return err
	}
	defer e.Close()
	e.Start()
	fmt.Printf("lark-edge terminal=%s udp=%s brain=%s\n", e.TCPAddr(), e.UDPAddr(), e.SockPath())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	return nil
}
