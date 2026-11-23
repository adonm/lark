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

// pipe returns the channel notify closes on its next call, creating it on
// demand so a waiter that arrives before the event still wakes.
func (c *condSignal) pipe() chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ch == nil {
		c.ch = make(chan struct{})
	}
	return c.ch
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
	paths := modelPaths{
		parakeet:  filepath.Join(local, "stt-models", parakeetModel),
		vad:       filepath.Join(local, "stt-models", sileroModel),
		smartTurn: filepath.Join(local, "stt-models", smartTurnModel),
	}
	stub := stubSetup(local)
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
	models := newModelSet(started)
	endpointConfig := &endpointSettings{newVAD: func() endpointVAD { return newSileroVAD(paths.vad) }}
	openEngine := func() (speechEngine, error) {
		return openKokoro(modelDir, min(kokoroThreads, runtime.GOMAXPROCS(0)))
	}
	sttLabel, ttsLabel := sttEngine, kokoroEngine
	var transcribe func(context.Context, []byte) (string, error)
	if stub != nil {
		transcribe = stub.stt
		endpointConfig = stub.endpoint
		openEngine = stub.openEngine
		sttLabel, ttsLabel = "stub", "stub"
		// The e2e build has no model files; its scripted backends are ready
		// as soon as the server starts.
		models.stt.setReady()
		models.tts.setReady()
		models.smartTurn.setReady()
		models.vad.setReady()
	}
	worker := newSynthWorkerWithState(root, models.tts, openEngine)
	b := &larkServer{root: root, token: token, db: db, terminals: terminalStore(db),
		stt: transcribe, sttLabel: sttLabel, ttsLabel: ttsLabel,
		endpoint: endpointConfig, models: models, oc: oc, worker: worker,
		voices:  companionVoiceStore(db, fallbackVoice),
		history: &chatHistory{db: db},
		portraits: &portraitStore{dir: filepath.Join(local, "portraits"),
			generate: configuredPortraitGenerator(ocAPI, ocURL != "", os.Getenv("OPENCODE_API_KEY"))},
		library: env("LARK_LIBRARY_DIR", filepath.Join(local, "library")),
		ota:     otaFile{path: filepath.Join(env("LARK_OTA_DIR", filepath.Join(project, "firmware/build")), "lark.bin")},
		box3OTA: otaFile{path: filepath.Join(env("LARK_BOX3_OTA_DIR", filepath.Join(project, "firmware/build-box3")), "lark_box3.bin")}}
	b.hub = newTerminalHub(b, true)
	b.remote = newRemoteManager(b, findTunnelCLI(home))
	b.remote.restore(root)
	defer b.remote.close()
	if stub == nil {
		go b.provisionLibrary(root)
	}
	oc.flushHistory = b.history.flush
	b.dc = newDCService(b, findDCRPCBinary(local), filepath.Join(local, "deltachat"), os.Getenv("LARK_CHATMAIL_RELAY"))
	if stub != nil {
		stub.startDC(b)
	} else {
		go b.dc.start(root)
	}
	defer b.dc.shutdown()
	var termSrv *terminalServer
	if devicePort != 0 {
		termSrv, err = newTerminalServer(b.hub, host, devicePorts)
		if err != nil {
			b.hub.shutdown()
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
		b.hub.shutdown()
		finish()
		_ = listener.Close()
		<-worker.done
		return err
	}
	// Models load after the listener is up: terminals, /health and admin pages
	// work immediately, and turns wait for a model instead of failing.
	var modelsDone <-chan struct{}
	if stub != nil {
		ready := make(chan struct{})
		close(ready)
		modelsDone = ready
	} else {
		modelsDone = b.loadModels(root, paths)
	}
	go func() {
		// Warming needs Kokoro; wait for the background load instead of
		// spending the pre-synthesis budget on a not-yet-ready worker.
		ready, cancel := context.WithTimeout(root, modelWait)
		defer cancel()
		if err := models.tts.await(ready); err != nil {
			return
		}
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
	log.Printf("Lark serving in %s with models loading in the background",
		time.Since(started).Round(time.Millisecond))
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
		b.hub.shutdown()
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
	b.hub.shutdown()
	termSrv.shutdown()
	<-modelsDone
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
	fmt.Fprintln(w, "Commands: serve, status, doctor, companions, terminals, tunnels, sim, device, capture, stt-bench, ota, mcp, library")
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
	case "status", "companions", "terminals", "tunnels", "capture", "stt-bench":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		err = runCLI(ctx, os.Args[1], os.Args[2:], os.Stdout, os.Stderr)
		cancel()
	case "doctor":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		err = runDoctor(ctx, os.Args[2:], os.Stdout, os.Stderr)
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
