package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// lark doctor prints one line per subsystem check. Failures exit non-zero;
// warnings describe genuine but non-blocking drift (stale firmware, a pack
// still building, a board that is offline).

type doctorStatus int

const (
	doctorPass doctorStatus = iota
	doctorWarn
	doctorFail
)

type doctorResult struct {
	status doctorStatus
	detail string
}

type doctorCheck struct {
	name string
	run  func(context.Context) doctorResult
}

type doctorFetch struct {
	done bool
	body object
	err  error
}

type doctor struct {
	c            *cliClient
	repo         string
	envFile      string
	configPath   string
	relay        string
	zone         string
	dbPath       string
	deviceStatus func(ip string) (object, error)
	relayProbe   func(host string) error

	health     doctorFetch
	companions doctorFetch
	terminals  doctorFetch
	library    doctorFetch
	tunnels    doctorFetch
	diag       doctorFetch
}

func (d *doctor) fetch(ctx context.Context, path string, f *doctorFetch) (object, error) {
	if !f.done {
		f.done = true
		f.body, f.err = d.c.do(ctx, http.MethodGet, path, nil)
	}
	return f.body, f.err
}

func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}

func result(status doctorStatus, format string, args ...any) doctorResult {
	return doctorResult{status: status, detail: fmt.Sprintf(format, args...)}
}

func pass(format string, args ...any) doctorResult { return result(doctorPass, format, args...) }
func warn(format string, args ...any) doctorResult { return result(doctorWarn, format, args...) }
func fail(format string, args ...any) doctorResult { return result(doctorFail, format, args...) }

func runDoctor(ctx context.Context, args []string, out, errOut io.Writer) error {
	if len(args) != 0 {
		fmt.Fprintln(errOut, "Usage: lark doctor")
		return errors.New("unexpected arguments")
	}
	c, err := newCLIClient()
	if err != nil {
		return err
	}
	repo, _ := repoRoot()
	return runDoctorWithClient(ctx, c, out, errOut, doctor{
		repo:       repo,
		envFile:    larkEnvFile(),
		configPath: openCodeConfigPath(),
		relay:      chatmailRelay(larkEnvFile()),
		dbPath:     larkDBPath(),
	})
}

func runDoctorWithClient(ctx context.Context, c *cliClient, out, errOut io.Writer, d doctor) error {
	d.c = c
	if d.deviceStatus == nil {
		d.deviceStatus = deviceStatusHTTP
	}
	if d.relayProbe == nil {
		d.relayProbe = probeRelay
	}
	checks := d.checks(ctx)
	failures := 0
	for _, check := range checks {
		status, detail := doctorPass, ""
		func() {
			defer func() {
				if r := recover(); r != nil {
					status, detail = doctorFail, fmt.Sprintf("check crashed: %v", r)
				}
			}()
			res := check.run(ctx)
			status, detail = res.status, res.detail
		}()
		label := "ok"
		switch status {
		case doctorWarn:
			label = "warn"
		case doctorFail:
			label = "fail"
			failures++
		}
		fmt.Fprintf(out, "%-4s  %-18s  %s\n", label, check.name, detail)
	}
	if failures > 0 {
		fmt.Fprintf(errOut, "%d check(s) failed\n", failures)
		return errors.New("doctor found failing checks")
	}
	return nil
}

func (d *doctor) checks(ctx context.Context) []doctorCheck {
	checks := []doctorCheck{
		{"server", d.checkServer},
		{"voice models", d.checkVoiceModels},
		{"opencode models", d.checkOpenCodeModels},
		{"firmware build", d.checkFirmwareBuild},
		{"schema", d.checkSchema},
		{"agent prompt", d.checkAgentPrompt},
		{"disk", d.checkDisk},
		{"captures", d.checkCaptures},
		{"companion voices", d.checkCompanionVoices},
		{"library", d.checkLibrary},
		{"deltachat", d.checkDeltaChat},
		{"deltachat accounts", d.checkDCAccounts},
		{"deltachat relay", d.checkDCRelay},
		{"qa leftovers", d.checkQALeftovers},
	}
	if body, err := d.fetch(ctx, "/api/terminals", &d.terminals); err == nil {
		list, _ := body["terminals"].([]any)
		for _, item := range list {
			term, _ := item.(object)
			id := str(term["id"])
			if id == "" {
				continue
			}
			checks = append(checks, doctorCheck{"terminal " + id, func(ctx context.Context) doctorResult {
				return d.checkTerminal(ctx, id)
			}})
		}
	} else {
		checks = append(checks, doctorCheck{"terminals", func(context.Context) doctorResult {
			return fail("GET /api/terminals: %v", err)
		}})
	}
	checks = append(checks,
		doctorCheck{"tunnels", d.checkTunnels},
		doctorCheck{"recent turns", d.checkTurns},
		doctorCheck{"clock", d.checkClock},
	)
	return checks
}

func (d *doctor) checkServer(ctx context.Context) doctorResult {
	body, err := d.fetch(ctx, "/health", &d.health)
	if err != nil {
		return fail("GET /health: %v", err)
	}
	if !boolOf(body["ok"]) {
		return fail("server reports ok=false")
	}
	var down []string
	for _, name := range []string{"opencode", "stt", "tts", "deltachat"} {
		if !boolOf(body[name]) {
			down = append(down, name)
		}
	}
	if len(down) > 0 {
		return fail("down: %s", strings.Join(down, ", "))
	}
	return pass("opencode, stt, tts, deltachat ready")
}

func (d *doctor) checkVoiceModels(ctx context.Context) doctorResult {
	body, err := d.fetch(ctx, "/health", &d.health)
	if err != nil {
		return fail("GET /health: %v", err)
	}
	var down []string
	if !boolOf(body["stt"]) {
		down = append(down, "STT")
	}
	if !boolOf(body["tts"]) {
		down = append(down, "TTS")
	}
	if !boolOf(body["smart_turn"]) {
		down = append(down, "Smart Turn")
	}
	if len(down) > 0 {
		return fail("not loaded: %s", strings.Join(down, ", "))
	}
	return pass("%s, %s, Smart Turn v3.2 loaded",
		str(body["stt_engine"]), str(body["tts_engine"]))
}

// modelIDs reads the dedicated OpenCode config. It never exposes credentials:
// only agent model references are returned.
func (d *doctor) modelIDs() (primary, deep, fallback string, err error) {
	raw, err := os.ReadFile(d.configPath)
	if err != nil {
		return "", "", "", err
	}
	var cfg struct {
		Agents map[string]struct {
			Model string `json:"model"`
		} `json:"agents"`
		Plugins []struct {
			Package string         `json:"package"`
			Options map[string]any `json:"options"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(stripJSONComments(string(raw))), &cfg); err != nil {
		return "", "", "", err
	}
	primary = cfg.Agents["lark"].Model
	deep = cfg.Agents["deep"].Model
	for _, plugin := range cfg.Plugins {
		if plugin.Package == "./lark-model-router" {
			fallback, _ = plugin.Options["fallback"].(string)
		}
	}
	return primary, deep, fallback, nil
}

func stripJSONComments(s string) string {
	var b strings.Builder
	inString, escaped := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inString:
			b.WriteByte(c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
		case c == '"':
			inString = true
			b.WriteByte(c)
		case c == '/' && i+1 < len(s) && s[i+1] == '/':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			i++
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func (d *doctor) checkOpenCodeModels(context.Context) doctorResult {
	primary, deep, fallback, err := d.modelIDs()
	if err != nil {
		return warn("cannot read %s: %v", d.configPath, err)
	}
	if primary == "" || deep == "" {
		return fail("primary=%q deep=%q in %s", primary, deep, d.configPath)
	}
	if fallback == "" {
		return warn("primary %s, deep %s; no fallback configured", primary, deep)
	}
	return pass("primary %s, deep %s, fallback %s", primary, deep, fallback)
}

func firmwareSourceVersion(repo string) string {
	if repo == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(repo, "firmware", "CMakeLists.txt"))
	if err != nil {
		return ""
	}
	m := regexp.MustCompile(`(?m)^\s*set\(PROJECT_VER\s+"([^"]+)"\)`).FindStringSubmatch(string(raw))
	if len(m) != 2 {
		return ""
	}
	return m[1]
}

func firmwareBuildVersion(repo, board string) string {
	if repo == "" {
		return ""
	}
	dir := "build"
	if board == "esp-box-3" {
		dir = "build-box3"
	}
	raw, err := os.ReadFile(filepath.Join(repo, "firmware", dir, "project_description.json"))
	if err != nil {
		return ""
	}
	var manifest struct {
		Version string `json:"project_version"`
	}
	if json.Unmarshal(raw, &manifest) != nil {
		return ""
	}
	return manifest.Version
}

func (d *doctor) checkFirmwareBuild(context.Context) doctorResult {
	source := firmwareSourceVersion(d.repo)
	if source == "" {
		return warn("firmware source not found (set LARK_REPO_DIR or run from the checkout)")
	}
	var stale []string
	found := false
	for _, board := range []string{"esp-vocat", "esp-box-3"} {
		if built := firmwareBuildVersion(d.repo, board); built != "" {
			found = true
			if built != source {
				stale = append(stale, fmt.Sprintf("%s build %s", board, built))
			}
		}
	}
	if len(stale) > 0 {
		return warn("source %s; stale %s", source, strings.Join(stale, ", "))
	}
	if !found {
		return warn("source %s; no local build output", source)
	}
	return pass("source %s, build outputs current", source)
}

func (d *doctor) checkDisk(ctx context.Context) doctorResult {
	body, err := d.fetch(ctx, "/api/library", &d.library)
	if err != nil {
		return fail("GET /api/library: %v", err)
	}
	free, reserve := jsonInt(body["free_bytes"]), jsonInt(body["reserve_bytes"])
	if jsonInt(body["total_bytes"]) == 0 {
		return fail("library volume unavailable: %s", str(body["error"]))
	}
	if free < reserve {
		return fail("free %s below 20%% reserve %s", humanBytes(free), humanBytes(reserve))
	}
	return pass("free %s, reserve %s (20%% of %s)", humanBytes(free), humanBytes(reserve),
		humanBytes(jsonInt(body["total_bytes"])))
}

func (d *doctor) checkAgentPrompt(context.Context) doctorResult {
	path := filepath.Join(d.repo, "agents/lark.md")
	info, err := os.Stat(path)
	if err != nil {
		return fail("agents/lark.md missing; install the prompt (lark-upgrade or setup-host)")
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fail("%s is not a non-empty prompt file", path)
	}
	return pass("%s, %s", path, humanBytes(info.Size()))
}

func (d *doctor) checkCaptures(context.Context) doctorResult {
	dir := debugCaptureDir()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return pass("no debug captures")
	}
	if err != nil {
		return fail("cannot read %s: %v", dir, err)
	}
	var count int
	var bytes int64
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			continue
		}
		if _, ok := captureCompanion(name); !ok && !strings.HasSuffix(name, ".txt") {
			continue
		}
		if info, err := entry.Info(); err == nil {
			count++
			bytes += info.Size()
		}
	}
	if bytes > captureMaxBytes {
		return warn("%s in %s exceeds the %s cap; old captures prune on the next recording",
			humanBytes(bytes), dir, humanBytes(captureMaxBytes))
	}
	return pass("%d files, %s (limit %s / %s)", count, humanBytes(bytes), humanBytes(captureMaxBytes), captureRetention)
}

func (d *doctor) checkSchema(ctx context.Context) doctorResult {
	path := d.dbPath
	if path == "" {
		path = larkDBPath()
	}
	db, err := sql.Open("lark-sqlite3", sqliteDSN(path, false))
	if err != nil {
		return fail("cannot open %s: %v", path, err)
	}
	defer db.Close()
	var missing []string
	for _, col := range requiredColumns {
		ok, err := columnExists(ctx, db, col.table, col.column)
		if err != nil {
			return fail("cannot inspect %s: %v", path, err)
		}
		if !ok {
			missing = append(missing, col.table+"."+col.column)
		}
	}
	if len(missing) > 0 {
		return fail("missing columns: %s; restart the server to migrate or run lark-upgrade",
			strings.Join(missing, ", "))
	}
	return pass("%d required columns present in %s", len(requiredColumns), path)
}

func (d *doctor) checkCompanionVoices(ctx context.Context) doctorResult {
	body, err := d.fetch(ctx, "/api/companions", &d.companions)
	if err != nil {
		return fail("GET /api/companions: %v", err)
	}
	list, _ := body["companions"].([]any)
	var retired []string
	for _, item := range list {
		c, _ := item.(object)
		if voice := str(c["voice_invalid"]); voice != "" {
			retired = append(retired, str(c["id"])+" -> "+voice)
		}
	}
	if len(retired) > 0 {
		return warn("retired stored voices use the default: %s; patch companion voice to repair", strings.Join(retired, ", "))
	}
	return pass("%d companions use installed voices", len(list))
}

func (d *doctor) checkLibrary(ctx context.Context) doctorResult {
	body, err := d.fetch(ctx, "/api/library", &d.library)
	if err != nil {
		return fail("GET /api/library: %v", err)
	}
	packs, _ := body["packs"].([]any)
	if len(packs) == 0 {
		return warn("no packs planned")
	}
	var parts []string
	var notInstalled []string
	for _, item := range packs {
		pack, _ := item.(object)
		name, state := str(pack["name"]), str(pack["state"])
		part := name + " " + state
		if state == "installed" {
			part += " " + humanBytes(jsonInt(pack["installed_bytes"]))
		} else if reason := str(pack["reason"]); reason != "" {
			part += " (" + reason + ")"
		}
		if lastErr := str(pack["last_error"]); lastErr != "" {
			part += "; last error: " + lastErr
		}
		parts = append(parts, part)
		if state != "installed" {
			notInstalled = append(notInstalled, name)
		}
	}
	if len(notInstalled) > 0 {
		return warn("%s; pending: %s", strings.Join(parts, "; "), strings.Join(notInstalled, ", "))
	}
	return pass("%s", strings.Join(parts, "; "))
}

func (d *doctor) checkDeltaChat(ctx context.Context) doctorResult {
	body, err := d.fetch(ctx, "/health", &d.health)
	if err != nil {
		return fail("GET /health: %v", err)
	}
	if !boolOf(body["deltachat"]) {
		return fail("rpc-server not ready (relay %s)", d.relay)
	}
	return pass("rpc-server ready, relay %s", d.relay)
}

func (d *doctor) checkDCAccounts(ctx context.Context) doctorResult {
	body, err := d.fetch(ctx, "/api/companions", &d.companions)
	if err != nil {
		return fail("GET /api/companions: %v", err)
	}
	list, _ := body["companions"].([]any)
	withAccount := 0
	for _, item := range list {
		comp, _ := item.(object)
		if jsonInt(comp["dc_account_id"]) > 0 {
			withAccount++
		}
	}
	diag, err := d.fetch(ctx, "/api/diagnostics", &d.diag)
	if err == nil {
		accounts, _ := diag["deltachat_accounts"].(object)
		var orphans []string
		if raw, ok := accounts["orphans"].([]any); ok {
			for _, id := range raw {
				orphans = append(orphans, fmt.Sprint(id))
			}
		}
		if pending := jsonInt(accounts["pending_removal"]); pending > 0 {
			return warn("%d of %d companions linked; %d account removal(s) pending", withAccount, len(list), pending)
		}
		if len(orphans) > 0 {
			return warn("%d of %d companions linked; orphaned Delta Chat accounts: %s",
				withAccount, len(list), strings.Join(orphans, ", "))
		}
	}
	if withAccount < len(list) {
		return warn("%d of %d companions have accounts", withAccount, len(list))
	}
	return pass("%d of %d companions have accounts", withAccount, len(list))
}

// checkQALeftovers catches the debris a crashed QA run leaves on the server:
// an offline sim terminal or a qa-/new-companion row. A live sim is expected
// while a test runs, so only offline terminal rows are flagged.
func (d *doctor) checkQALeftovers(ctx context.Context) doctorResult {
	terms, err := d.fetch(ctx, "/api/terminals", &d.terminals)
	if err != nil {
		return fail("GET /api/terminals: %v", err)
	}
	comps, err := d.fetch(ctx, "/api/companions", &d.companions)
	if err != nil {
		return fail("GET /api/companions: %v", err)
	}
	var leftovers []string
	if list, ok := terms["terminals"].([]any); ok {
		for _, item := range list {
			m, _ := item.(object)
			id := str(m["id"])
			if strings.HasPrefix(id, "sim-") && !boolOf(m["online"]) {
				leftovers = append(leftovers, fmt.Sprintf("offline terminal %s (lark terminals rm %s)", id, id))
			}
		}
	}
	if list, ok := comps["companions"].([]any); ok {
		for _, item := range list {
			m, _ := item.(object)
			id := str(m["id"])
			if strings.HasPrefix(id, "qa-") || strings.HasPrefix(id, "new-companion") {
				leftovers = append(leftovers, fmt.Sprintf("companion %s (lark companions rm %s)", id, id))
			}
		}
	}
	if len(leftovers) > 0 {
		return warn("%s", strings.Join(leftovers, "; "))
	}
	return pass("no QA leftovers")
}

func (d *doctor) checkDCRelay(context.Context) doctorResult {
	if d.relay == "" {
		return warn("no relay configured")
	}
	if err := d.relayProbe(d.relay); err != nil {
		return warn("%s unreachable: %v", d.relay, err)
	}
	return pass("%s reachable", d.relay)
}

func (d *doctor) checkTerminal(ctx context.Context, id string) doctorResult {
	body, err := d.fetch(ctx, "/api/terminals", &d.terminals)
	if err != nil {
		return fail("GET /api/terminals: %v", err)
	}
	list, _ := body["terminals"].([]any)
	var term object
	for _, item := range list {
		if m, _ := item.(object); str(m["id"]) == id {
			term = m
			break
		}
	}
	if term == nil {
		return fail("terminal disappeared")
	}
	online := boolOf(term["online"])
	firmware := str(term["firmware"])
	if firmware == "" {
		firmware = "unknown"
	}
	source := firmwareSourceVersion(d.repo)
	if !online {
		return warn("offline; firmware %s", firmware)
	}
	var notes []string
	firmwareDrift := source != "" && firmware != source
	if firmwareDrift {
		notes = append(notes, fmt.Sprintf("firmware %s, source %s", firmware, source))
	} else {
		notes = append(notes, "firmware "+firmware)
	}
	status, err := d.deviceStatus(str(term["ip"]))
	if err != nil {
		notes = append(notes, "device status: "+err.Error())
		return warn("online; %s", strings.Join(notes, "; "))
	}
	lowHeap := false
	if minHeap := jsonInt(status["min_free_heap"]); minHeap > 0 {
		if minHeap < 40<<10 {
			lowHeap = true
			notes = append(notes, fmt.Sprintf("min_free_heap %s below 40 KiB", humanBytes(minHeap)))
		} else {
			notes = append(notes, "min_free_heap "+humanBytes(minHeap))
		}
	}
	if lowHeap {
		return warn("online; %s", strings.Join(notes, "; "))
	}
	if firmwareDrift {
		notes = append(notes, "run lark ota")
		return warn("online; %s", strings.Join(notes, "; "))
	}
	return pass("online; %s", strings.Join(notes, "; "))
}

func (d *doctor) checkTunnels(ctx context.Context) doctorResult {
	body, err := d.fetch(ctx, "/api/remote/tunnels", &d.tunnels)
	if err != nil {
		return fail("GET /api/remote/tunnels: %v", err)
	}
	list, _ := body["tunnels"].([]any)
	running := 0
	var down []string
	for _, item := range list {
		tunnel, _ := item.(object)
		if boolOf(tunnel["active"]) {
			running++
		} else {
			down = append(down, str(tunnel["companion_id"]))
		}
	}
	if len(down) > 0 {
		return warn("%d configured, %d running; stopped: %s", len(list), running, strings.Join(down, ", "))
	}
	if len(list) == 0 {
		return pass("none configured")
	}
	return pass("%d configured, %d running", len(list), running)
}

func (d *doctor) checkTurns(ctx context.Context) doctorResult {
	body, err := d.fetch(ctx, "/api/diagnostics", &d.diag)
	if err != nil {
		return fail("GET /api/diagnostics: %v", err)
	}
	turns, _ := body["recent_turns"].([]any)
	if len(turns) == 0 {
		return pass("no recent turns")
	}
	failed := 0
	for _, item := range turns {
		turn, _ := item.(object)
		if boolOf(turn["error"]) {
			failed++
		}
	}
	rate := 100 * float64(failed) / float64(len(turns))
	switch {
	case rate > 20:
		return fail("%d of %d recent turns failed (%.0f%%)", failed, len(turns), rate)
	case failed > 0:
		return warn("%d of %d recent turns failed (%.0f%%)", failed, len(turns), rate)
	}
	return pass("0 of %d recent turns failed", len(turns))
}

func (d *doctor) checkClock(context.Context) doctorResult {
	now := time.Now()
	zone, offset := now.Zone()
	host := d.zone
	if host == "" {
		host = systemTimeZone()
	}
	if host == "" {
		return warn("system zone unknown; host reports %s (%+03d:%02d)", zone, offset/3600, offset%3600/60)
	}
	loc, err := time.LoadLocation(host)
	if err != nil {
		return warn("system zone %q cannot be loaded", host)
	}
	want, _ := now.In(loc).Zone()
	if want != zone {
		return fail("host reports %s but system zone %s is %s", zone, host, want)
	}
	return pass("%s (%s %+03d:%02d); agent prompt uses it", host, zone, offset/3600, offset%3600/60)
}

func systemTimeZone() string {
	if tz := os.Getenv("TZ"); tz != "" {
		return tz
	}
	if raw, err := os.ReadFile("/etc/timezone"); err == nil {
		if name := strings.TrimSpace(string(raw)); name != "" {
			return name
		}
	}
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(link, "zoneinfo/"); i >= 0 {
			return link[i+len("zoneinfo/"):]
		}
	}
	// Some immutable distributions copy the zone file instead of linking it.
	if zone := zoneForLocalFile(); zone != "" {
		return zone
	}
	return ""
}

func zoneForLocalFile() string {
	local, err := os.ReadFile("/etc/localtime")
	if err != nil || len(local) == 0 {
		return ""
	}
	root := "/usr/share/zoneinfo"
	var found string
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if entry.IsDir() {
			switch entry.Name() {
			case "posix", "right":
				if path != root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Size() != int64(len(local)) {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err == nil && string(raw) == string(local) {
			found, _ = filepath.Rel(root, path)
		}
		return nil
	})
	return found
}

func openCodeConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data := env("XDG_DATA_HOME", filepath.Join(home, ".local/share"))
	return filepath.Join(data, "lark", "opencode", "config", "opencode", "opencode.jsonc")
}

func larkEnvFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(env("XDG_CONFIG_HOME", filepath.Join(home, ".config")), "lark", "lark.env")
}

func envFileValue(path, key string) string {
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

func chatmailRelay(envFile string) string {
	if relay := os.Getenv("LARK_CHATMAIL_RELAY"); relay != "" {
		return relay
	}
	if relay := envFileValue(envFile, "LARK_CHATMAIL_RELAY"); relay != "" {
		return relay
	}
	return defaultChatmailRelay
}

func probeRelay(host string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, "443"))
	if err != nil {
		return err
	}
	return conn.Close()
}

func deviceStatusHTTP(ip string) (object, error) {
	if ip == "" {
		return nil, errors.New("no address")
	}
	client := &http.Client{Timeout: 3 * time.Second}
	res, err := client.Get("http://" + net.JoinHostPort(ip, "80") + "/status")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	var body object
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	return body, nil
}
