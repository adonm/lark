package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

var tokenRE = regexp.MustCompile(`^[!-~]+$`)

const bridgeListeningPrefix = "Lark bridge listening on "

func openCodeSettings(agent string) ocSettings {
	s := ocSettings{Agent: agent, Permissions: []ocPermission{{"*", "*", "deny"}}}
	if agent == "lark" {
		s.Permissions = append(s.Permissions, ocPermission{"websearch", "*", "allow"},
			ocPermission{"lark_*", "*", "allow"}, ocPermission{"execute", "*", "allow"})
	}
	return s
}

func run() error {
	started := time.Now()
	token := os.Getenv("LARK_BRIDGE_TOKEN")
	if !tokenRE.MatchString(token) {
		return errors.New("set LARK_BRIDGE_TOKEN to a nonempty ASCII token without whitespace")
	}
	settings := openCodeSettings(env("LARK_OPENCODE_AGENT", "lark"))
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	parentToken, err := loadParentToken(filepath.Join(env("XDG_CONFIG_HOME", filepath.Join(home, ".config")), "lark", "parent.token"), token)
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
	ocURL, ocPassword := os.Getenv("LARK_OPENCODE_URL"), os.Getenv("LARK_OPENCODE_PASSWORD")
	ocAPI, err := openCodeUpstream(pooledClient(), state, ocURL, ocPassword)
	if err != nil {
		return err
	}
	pairPort, err := strconv.Atoi(env("LARK_OPENCODE_PAIR_PORT", "0"))
	if err != nil || pairPort < 0 || pairPort > 65535 || pairPort != 0 && (ocURL == "" || ocPassword == "") {
		return errors.New("LARK_OPENCODE_PAIR_PORT requires a valid port and a dedicated OpenCode URL and password")
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
	if os.Getenv("LARK_REPO_DIR") == "" && filepath.Base(project) == "bridge" {
		project = filepath.Dir(project)
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
	db, err := openBridgeDB(context.Background(), dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	ports := env("LARK_BRIDGE_PORT", "8765")
	if port, err := strconv.Atoi(ports); err != nil || port < 0 || port > 65535 {
		return errors.New("invalid LARK_BRIDGE_PORT")
	}
	devicePorts := env("LARK_DEVICE_PORT", defaultDevPort)
	devicePort, err := strconv.Atoi(devicePorts)
	if err != nil || devicePort < 0 || devicePort > 65535 {
		return errors.New("invalid LARK_DEVICE_PORT")
	}
	host := env("LARK_BRIDGE_HOST", "0.0.0.0")
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
	b := &bridge{root: root, token: token, parentToken: parentToken, db: db, peers: peers{deviceStore(db)},
		stt: parakeet.transcribe, endpoint: endpointConfig, oc: oc, pairPort: pairPort, worker: worker,
		voices:  deviceVoiceStore(db, fallbackVoice),
		virtual: virtualStore(db), history: &chatHistory{db},
		portraits: &portraitStore{dir: filepath.Join(local, "portraits"),
			generate: configuredPortraitGenerator(ocAPI, ocURL != "", os.Getenv("OPENCODE_API_KEY"))},
		ota:     otaFile{path: filepath.Join(env("LARK_OTA_DIR", filepath.Join(project, "firmware/build")), "lark.bin")},
		box3OTA: otaFile{path: filepath.Join(env("LARK_BOX3_OTA_DIR", filepath.Join(project, "firmware/build-box3")), "lark_box3.bin")}}
	b.ws = newWSHub(b, true)
	var devSrv *deviceServer
	if devicePort != 0 {
		devSrv, err = newDeviceServer(b.ws, host, devicePorts)
		if err != nil {
			b.ws.shutdown()
			finish()
			_ = listener.Close()
			<-worker.done
			return err
		}
		log.Printf("Lark native device listener on %s", devSrv.tcpLn.Addr().String())
	}
	b.builders = newBuildManager(b)
	buildsDone := make(chan struct{})
	go func() {
		defer close(buildsDone)
		b.builders.run(root)
	}()
	defer func() { finish(); <-buildsDone }()
	// Serialize shutdown cleanly.
	bin := findTunnelCLI(home)
	var runner tunnelRunner
	if bin != "" {
		runner = tunnelCLI(bin)
	}
	b.tunnels = newTunnelManager(b, bin, runner)
	b.tunnels.restore()
	stopDiagnostics := b.sampleDiagnostics(started, dbPath)
	defer stopDiagnostics()
	b.timers = newTimerManager(b)
	if err := b.timers.restore(time.Now()); err != nil {
		b.timers.close()
		b.tunnels.shutdown()
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
	}()
	server := &http.Server{Handler: b, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 105 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	log.Print(bridgeListeningPrefix + listener.Addr().String())
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	schemaDone := make(chan struct{})
	go func() {
		defer close(schemaDone)
		b.runSchemaUpgrade(root, 5*time.Second, 5*time.Minute)
	}()
	defer func() { finish(); <-schemaDone }()
	select {
	case <-signals.Done():
		b.timers.close()
		b.tunnels.shutdown()
		b.ws.shutdown()
		devSrv.shutdown()
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
	b.tunnels.shutdown()
	b.ws.shutdown()
	devSrv.shutdown()
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

func main() {
	if len(os.Args) == 2 && os.Args[1] == "mcp" {
		if err := runMCPStdio(); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
