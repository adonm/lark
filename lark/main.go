package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"
)

type condSignal struct {
	mu sync.Mutex
	ch chan struct{}
}

func (c *condSignal) notify() {
	c.mu.Lock()
	if c.ch != nil {
		close(c.ch)
		c.ch = nil
	}
	c.mu.Unlock()
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

var tokenRE = regexp.MustCompile(`^[!-~]+$`)

const larkListeningPrefix = "Lark listening on "

func openCodeSettings(agent string) ocSettings {
	s := ocSettings{
		Agent:       agent,
		Permissions: []ocPermission{{"*", "*", "deny"}},
	}
	if agent == "lark" {
		s.Permissions = append(s.Permissions, ocPermission{"websearch", "*", "allow"},
			ocPermission{"lark_*", "*", "allow"}, ocPermission{"execute", "*", "allow"},
			ocPermission{"subagent", "deep", "allow"})
	}
	return s
}

func run() error {
	started := time.Now()
	token := os.Getenv("LARK_TOKEN")
	if !tokenRE.MatchString(token) {
		return errors.New("set LARK_TOKEN to a nonempty ASCII token without whitespace")
	}
	settings := openCodeSettings(env("LARK_OPENCODE_AGENT", "lark"))
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	data := env("XDG_DATA_HOME", filepath.Join(home, ".local/share"))
	state := env("XDG_STATE_HOME", filepath.Join(home, ".local/state"))
	local := filepath.Join(data, "lark")
	path := filepath.Join(local, "stt-models", parakeetModel)
	if err := checkParakeetModel(path); err != nil {
		return err
	}
	vadPath := filepath.Join(local, "stt-models", sileroModel)
	if err := checkEndpointModel(vadPath); err != nil {
		return err
	}
	parakeet, err := openParakeet(path)
	if err != nil {
		return err
	}
	defer parakeet.close()
	if _, err := parakeet.transcribe(context.Background(), make([]byte, sampleRate*2)); err != nil {
		return err
	}
	vad := newSileroVAD(vadPath)
	if vad == nil {
		return errors.New("could not load the pinned Silero v5 model")
	}
	vad.close()
	log.Print("Lark endpoint VAD ready (in-process Silero v5)")
	endpointConfig := &endpointSettings{newVAD: func() endpointVAD { return newSileroVAD(vadPath) }}
	smartTurnPath := filepath.Join(local, "stt-models", smartTurnModel)
	if st, err := openSmartTurn(smartTurnPath, min(4, runtime.GOMAXPROCS(0))); err != nil {
		log.Printf("Smart Turn disabled: %v", err)
	} else {
		defer st.close()
		endpointConfig.smartTurn = st.predictPCM16
		log.Print("Lark semantic endpoint ready (Smart Turn v3.2)")
	}
	ocURL, ocPassword := os.Getenv("LARK_OPENCODE_URL"), os.Getenv("LARK_OPENCODE_PASSWORD")
	ocAPI, err := openCodeUpstream(pooledClient(), state, ocURL, ocPassword)
	if err != nil {
		return err
	}
	ocDir := env("LARK_OPENCODE_DIR", filepath.Join(local, "opencode/workspace"))
	syscall.Umask(0o077)
	if !filepath.IsAbs(ocDir) {
		return errors.New("LARK_OPENCODE_DIR must be an absolute path")
	}
	if err := os.MkdirAll(ocDir, 0o700); err != nil {
		return err
	}
	project, err := filepath.Abs(env("LARK_REPO_DIR", "."))
	if err != nil {
		return err
	}
	if os.Getenv("LARK_REPO_DIR") == "" && filepath.Base(project) == "lark" {
		if _, err := os.Stat(filepath.Join(project, "agents/lark.md")); err != nil {
			project = filepath.Dir(project)
		}
	}
	// Hash the prompt behind the installed lark agent symlink.
	contract, err := readMCPContract(filepath.Join(project, "agents/lark.md"))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(local, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(local, 0o700); err != nil {
		return err
	}
	fallbackVoice := env("LARK_TTS_VOICE", heartVoice)
	dbPath := env("LARK_DB_PATH", filepath.Join(local, "lark.db"))
	db, err := openLarkDB(context.Background(), dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	ports := env("LARK_PORT", "8765")
	if port, err := strconv.Atoi(ports); err != nil || port < 0 || port > 65535 {
		return errors.New("invalid LARK_PORT")
	}
	devicePorts := env("LARK_DEVICE_PORT", defaultDevPort)
	devicePort, err := strconv.Atoi(devicePorts)
	if err != nil || devicePort < 0 || devicePort > 65535 {
		return errors.New("invalid LARK_DEVICE_PORT")
	}
	host := env("LARK_HOST", "0.0.0.0")
	listener, err := net.Listen("tcp", net.JoinHostPort(host, ports))
	if err != nil {
		return err
	}
	root, finish := context.WithCancel(context.Background())
	defer finish()
	if productionDB(dbPath) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			db.backupPeriodically(root, filepath.Join(local, "backups"))
		}()
		defer func() { finish(); <-done }()
	}
	oc := newOpenCode(root, ocAPI, db, ocDir, settings)
	oc.contract = contract
	modelDir := ttsModelDir(local)
	worker := newSynthWorker(root, func() (speechEngine, error) {
		return openKokoro(modelDir, min(kokoroThreads, runtime.GOMAXPROCS(0)))
	})
	b := &larkServer{root: root, token: token, db: db, terminals: terminalStore(db),
		stt: parakeet.transcribe, endpoint: endpointConfig, oc: oc, worker: worker,
		voices:  companionVoiceStore(db, fallbackVoice),
		history: &chatHistory{db: db},
		portraits: &portraitStore{dir: filepath.Join(local, "portraits"),
			generate: configuredPortraitGenerator(ocAPI, ocURL != "", os.Getenv("OPENCODE_API_KEY"))},
		library: env("LARK_LIBRARY_DIR", filepath.Join(local, "library")),
		ota:     otaFile{path: filepath.Join(env("LARK_OTA_DIR", filepath.Join(project, "firmware/build")), "lark.bin")},
		box3OTA: otaFile{path: filepath.Join(env("LARK_BOX3_OTA_DIR", filepath.Join(project, "firmware/build-box3")), "lark_box3.bin")}}
	b.ws = newWSHub(b, true)
	b.remote = newRemoteManager(b, findTunnelCLI(home))
	b.remote.restore(root)
	defer b.remote.close()
	go b.provisionLibrary(root)
	oc.flushHistory = b.history.flush
	b.dc = newDCService(b, findDCRPCBinary(local), filepath.Join(local, "deltachat"), os.Getenv("LARK_CHATMAIL_RELAY"))
	go b.dc.start(root)
	defer b.dc.shutdown()
	var termSrv *terminalServer
	if devicePort != 0 {
		termSrv, err = newTerminalServer(b.ws, host, devicePorts)
		if err != nil {
			b.ws.shutdown()
			finish()
			_ = listener.Close()
			<-worker.done
			return err
		}
		log.Printf("Lark native device listener on %s", termSrv.tcpLn.Addr().String())
	}
	stopDiagnostics := b.sampleDiagnostics(started, dbPath)
	defer stopDiagnostics()
	b.timers = newTimerManager(b)
	if err := b.timers.restore(time.Now()); err != nil {
		b.timers.close()
		b.ws.shutdown()
		finish()
		_ = listener.Close()
		<-worker.done
		return err
	}
	go func() {
		ctx, cancel := context.WithTimeout(root, ttsWait)
		defer cancel()
		_, _ = b.clips.getWith(ctx, worker.warm, b.voices.fallback, "Sent to everyone!")
		b.clips.warmFillers(ctx, worker, b.voices.fallback)
		for _, v := range voiceCatalog {
			_, _ = b.clips.getWith(ctx, worker.warm, v.ID, voicePreviewText(v.ID))
		}
		if list, err := b.allCompanions(ctx); err == nil {
			seen := map[string]bool{b.voices.fallback: true}
			for _, c := range list {
				v := b.voiceFor(c.ID)
				if !seen[v] {
					seen[v] = true
					b.clips.warmFillers(ctx, worker, v)
				}
			}
		}
	}()
	server := &http.Server{Handler: b, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 105 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	log.Print(larkListeningPrefix + listener.Addr().String())
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	schemaDone := make(chan struct{})
	go func() {
		defer close(schemaDone)
		b.runSchemaUpgrade(root, 5*time.Second, 5*time.Minute)
	}()
	defer func() { finish(); <-schemaDone; b.history.close() }()
	select {
	case <-signals.Done():
		b.timers.close()
		b.ws.shutdown()
		termSrv.shutdown()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			_ = server.Close()
		}
		err = <-served
	case err = <-served:
	}
	finish()
	b.timers.close()
	b.ws.shutdown()
	termSrv.shutdown()
	<-worker.done
	oc.client.CloseIdleConnections()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func productionDB(path string) bool {
	if path == ":memory:" {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(os.TempDir(), abs)
	return err == nil && !filepath.IsLocal(rel)
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "Usage: lark <command> [args]")
	fmt.Fprintln(w, "Commands: serve, status, companions, terminals, tunnels, sim, device, ota, mcp, library")
}

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		if len(os.Args) > 2 {
			fmt.Fprintln(os.Stderr, "Usage: lark serve")
			os.Exit(2)
		}
		err = run()
	case "status", "companions", "terminals", "tunnels":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		err = runCLI(ctx, os.Args[1], os.Args[2:], os.Stdout, os.Stderr)
		cancel()
	case "ota":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		err = runOTA(ctx, os.Args[2:], os.Stdout, os.Stderr)
		cancel()
	case "sim":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		err = runSim(ctx, os.Args[2:], os.Stdin, os.Stdout, os.Stderr)
		cancel()
	case "device":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		err = runDevice(ctx, os.Args[2:], os.Stdout, os.Stderr)
		cancel()
	case "mcp":
		if len(os.Args) > 2 {
			fmt.Fprintln(os.Stderr, "Usage: lark mcp")
			os.Exit(2)
		}
		err = runMCPStdio()
	case "library":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		err = runLibrary(ctx, os.Args[2:], os.Stdout, os.Stderr)
		cancel()
	case "-h", "--help", "help":
		usage(os.Stdout)
		return
	default:
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}
