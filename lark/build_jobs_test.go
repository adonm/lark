package main

import (
	"context"
	"encoding/json"
	"net/http"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const quizProposal = `{"name":"dinosaur-quiz","title":"Dinosaur Quiz","template":"quiz","data":{"questions":[{"prompt":"Which could fly?","choices":["Pteranodon","Triceratops"],"answer":0}]}}`

type buildStub struct {
	allow    chan struct{}
	prompted chan string
	deleted  chan string
	reply    string
	delay    time.Duration
	model    ocModel
	override ocModel
}

func (s *buildStub) handler(t *testing.T) http.HandlerFunc {
	s.prompted, s.deleted = make(chan string, 8), make(chan string, 8)
	s.model = ocModel{"configured", "primary", "small"}
	var sessions sync.Map
	data := func(w http.ResponseWriter, data any) { sendJSON(w, 200, object{"data": data}) }
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch p {
		case "/api/agent/lark":
			data(w, object{"id": "lark", "model": s.model})
		case "/api/agent/" + builderAgent:
			data(w, object{"id": builderAgent, "model": s.override})
		case "/api/session":
			var in struct {
				ID, Title string
				ocSettings
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Error(err)
			}
			if in.ID == "" && in.Agent == "lark" {
				in.ID = "ses_foreground_stub"
			}
			if in.Agent == builderAgent {
				model := s.override
				if model == (ocModel{}) {
					model = s.model
				}
				if !strings.HasPrefix(in.ID, "ses_lark_build_") || in.Model != model ||
					len(in.Permissions) != 1 || in.Permissions[0] != (ocPermission{"*", "*", "deny"}) {
					t.Errorf("builder was not created with explicit model, ID and deny-all permissions")
				}
			}
			state := object{"id": in.ID, "title": in.Title, "agent": in.Agent, "model": in.Model, "permissions": in.Permissions}
			sessions.Store(in.ID, state)
			data(w, state)
		default:
			action, id := path.Base(p), path.Base(path.Dir(p))
			switch action {
			case "prompt":
				select {
				case s.prompted <- id:
				default:
					t.Error("too many prompts")
				}
				data(w, object{"id": "msg_prompt"})
			case "wait":
				if s.allow != nil {
					select {
					case <-s.allow:
					case <-r.Context().Done():
						return
					}
				}
				if s.delay != 0 {
					timer := time.NewTimer(s.delay)
					defer timer.Stop()
					select {
					case <-timer.C:
					case <-r.Context().Done():
						return
					}
				}
				w.WriteHeader(204)
			case "message":
				data(w, []object{
					{"id": "msg_idle", "type": "idle", "outcome": "succeeded"},
					{"id": "msg_answer", "type": "assistant", "finish": "stop", "content": []object{{"type": "text", "text": s.reply}}},
					{"id": "msg_prompt", "type": "user"},
				})
			case "interrupt":
				w.WriteHeader(204)
			default:
				state, ok := sessions.Load(action)
				switch {
				case !ok:
					http.NotFound(w, r)
				case r.Method == http.MethodDelete:
					sessions.Delete(action)
					s.deleted <- action
					w.WriteHeader(204)
				default:
					data(w, state)
				}
			}
		}
	}
}

func runBuildManager(t *testing.T, b *larkServer) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(b.root)
	done := make(chan struct{})
	go func() { defer close(done); b.builders.run(ctx) }()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	return stop
}

func stubBuildBridge(t *testing.T, stub *buildStub, enabled bool, tts *atomic.Int32) (*larkServer, string) {
	t.Helper()
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.oc = testOpenCode(t, b.root, dir, stub.handler(t))
		b.oc.settings = openCodeSettings("lark")
		if enabled {
			b.builders = newBuildManager(b)
		}
		if tts != nil {
			b.worker = fakeTTS(b.root, tts)
		}
	})
	if enabled {
		runBuildManager(t, b)
	}
	return b, base
}

func TestBuildIntent(t *testing.T) {
	for _, tc := range []struct {
		text, kind, target string
	}{
		{"Make me a dinosaur quiz.", "new", ""},
		{"Could you make a matching game?", "new", ""},
		{"Create some flashcards about rocks", "new", ""},
		{"Draw me a blue cat", "new", ""},
		{"Edit the app `dinosaur-quiz` to add a round", "edit", "dinosaur-quiz"},
		{"Change this game to use bigger numbers", "edit", ""},
		{"Change your eyes to stars.", "face", ""},
		{"Give yourself a bigger smile!", "face", ""},
		{"Cancel the build.", "cancel", ""},
		{"Stop the background job", "cancel", ""},
		{"Make it better", "clarify", ""},
		{"build something", "clarify", ""},
		{"make me an app", "clarify-app", ""},
		{"change your face", "clarify-face", ""},
		{"Stop", "", ""},
		{"Set a timer for three minutes", "", ""},
		{"Call Mint", "", ""},
		{"Tell Mint to make an app.", "", ""},
		{"Play a quiz about rocks", "", ""},
		{"Make the volume lower", "", ""},
		{"Make a joke about a game", "", ""},
		{"Change your voice", "", ""},
	} {
		if got := parseBuildIntent(tc.text); got != (buildIntent{tc.kind, tc.target}) {
			t.Errorf("%q → %+v, want %q/%q", tc.text, got, tc.kind, tc.target)
		}
	}
}

func TestBuildReadyWaitsForIdleAndSurvivesStop(t *testing.T) {
	const device = "phone-dead"
	var speech atomic.Int32
	stub := &buildStub{allow: make(chan struct{}), reply: quizProposal}
	b, base := stubBuildBridge(t, stub, true, &speech)
	s := testConnectWS(t, base, "unit", true)
	s.hello(t, device, "Cat")
	s.json(t, object{"ev": "text", "text": "Make me a dinosaur quiz."})
	cardAndSpeech := func(label string) {
		t.Helper()
		var seenCard bool
		var end wsFrame
		s.until(t, func(f wsFrame) bool {
			seenCard = seenCard || hasWS("svg", label)(f)
			if hasWS("tts", `"state":"end"`)(f) {
				end = f
			}
			return seenCard && end.data != nil
		})
		s.json(t, object{"ev": "tts_ack", "state": "done", "audio": testObject(t, end.data)["audio"]})
		s.barrier(t)
	}
	cardAndSpeech("Cancel build")
	var job string
	select {
	case job = <-stub.prompted:
	case <-time.After(2 * time.Second):
		t.Fatal("builder did not receive the prompt")
	}

	h := b.ws
	h.mu.Lock()
	d := h.devices[device]
	h.stopCompanionTurn(d) // Same foreground cancellation primitive; not the job gate.
	h.mu.Unlock()
	_, err := b.runApp(t.Context(), object{"devices": device, "title": "Not the build",
		"svg": `<svg viewBox="0 0 360 360"><circle cx="180" cy="180" r="30"/></svg>`})
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	running := d.app
	h.startRecording(d, "", time.Now(), false)
	h.mu.Unlock()
	close(stub.allow)
	expectValue(t, stub.deleted, job)
	waitBuildState(t, b, job, "ready")
	h.mu.Lock()
	if !d.recording.open() || d.card != nil {
		t.Error("Ready displaced an active recording")
	}
	d.recording.clear()
	h.mu.Unlock()
	b.builders.signal()
	cardAndSpeech("build-done")
	h.mu.Lock()
	if d.app != running || running == nil {
		t.Error("completed build replaced the running app")
	}
	h.mu.Unlock()
	if a, err := b.loadApp(t.Context(), "dinosaur-quiz"); err != nil || a.Template != "quiz" {
		t.Fatalf("build did not save valid template: %v", err)
	}
	b.ws.announceBuild(t.Context(), buildJob{id: job, device: device, state: "ready", result: "Dinosaur Quiz is ready in Games."})
	if got := speech.Load(); got != 2 { // Build acknowledgement and Ready.
		t.Errorf("wanted one acknowledgement and one announcement after warmup, got %d calls", got)
	}
	if sid, err := b.oc.get(device); err != nil || sid != "" {
		t.Fatalf("builder installed a foreground mapping")
	}
}

func TestBuildDeliveryRespectsParentPolicy(t *testing.T) {
	const device = "phone-dead"
	const prompt = "Make me a dinosaur quiz."
	for _, reason := range []parentReason{parentQuiet, parentLimit, parentPause} {
		t.Run(string(reason), func(t *testing.T) {
			var speech atomic.Int32
			stub := &buildStub{allow: make(chan struct{}), reply: quizProposal}
			b, base := stubBuildBridge(t, stub, true, &speech)
			s := testConnectWS(t, base, "unit", true)
			s.hello(t, device, "Cat")
			finishSpeech := func(label, chat string) {
				t.Helper()
				seenCard := label == ""
				seenChat := chat == ""
				var end wsFrame
				s.until(t, func(f wsFrame) bool {
					seenCard = seenCard || hasWS("svg", label)(f)
					if f.op == wsText && strings.Contains(string(f.data), `"type":"chat"`) {
						seenChat = seenChat || strings.Contains(string(f.data), chat)
					}
					if hasWS("tts", `"state":"end"`)(f) {
						end = f
					}
					return seenCard && seenChat && end.data != nil
				})
				s.json(t, object{"ev": "tts_ack", "state": "done", "audio": testObject(t, end.data)["audio"]})
				s.barrier(t)
			}
			s.json(t, object{"ev": "text", "text": prompt})
			finishSpeech("Cancel build", "Building it now!")
			var job string
			select {
			case job = <-stub.prompted:
			case <-time.After(2 * time.Second):
				t.Fatal("builder did not receive the prompt")
			}

			h := b.ws
			h.mu.Lock()
			d := h.devices[device]
			turn := d.turn
			now := time.Now().UTC()
			settings := parentSettings{Timezone: "UTC"}
			switch reason {
			case parentQuiet:
				settings.QuietStart = now.Add(-time.Hour).Format("15:04")
				settings.QuietEnd = now.Add(time.Hour).Format("15:04")
			case parentLimit:
				settings.DailyMinutes = 1
				h.parents[device].used = time.Minute
			case parentPause:
				settings.PauseUntil = now.Add(time.Hour).UnixMilli()
			}
			err := h.parentSet(device, settings, time.UTC, now)
			restricted := d.parentReason
			h.mu.Unlock()
			if err != nil || restricted != reason {
				t.Fatal(restricted, err)
			}
			if reason == parentLimit {
				finishSpeech("", "") // Parent controls' own lockout announcement.
			}
			before := speech.Load()
			beforeChat, err := b.history.list(t.Context(), device)
			if err != nil || len(beforeChat) != 2 {
				t.Fatalf("permitted acknowledgement not in chat: %v, %v", beforeChat, err)
			}

			// Complete the running job concurrently with a late acknowledgement
			// and a new request from the now restricted device.
			close(stub.allow)
			h.buildAcknowledgement(t.Context(), d, turn, prompt, "typed", "Building it now!", job)
			if err := b.deliverOfflineBuildReply(t.Context(), d.who, prompt, "Building it now!", "typed"); err == nil {
				t.Error("restricted offline acknowledgement delivered")
			}
			testRequest(t, "POST", base+"/api/chat", "unit", object{"device_id": device, "text": prompt}, 423)
			s.json(t, object{"ev": "text", "text": prompt})
			s.barrier(t)
			if id, err := b.builders.enqueue(t.Context(), d.who, buildIntent{kind: "new"}, prompt); err == nil || id != "" {
				t.Fatalf("restricted enqueue: %q, %v", id, err)
			}
			expectValue(t, stub.deleted, job)
			result := waitBuildState(t, b, job, "ready")
			h.announceBuild(t.Context(), buildJob{id: job, device: device, state: "ready", result: result})
			s.write(t, maskedFrame(wsPing, true, nil))
			s.until(t, func(f wsFrame) bool {
				if f.op == wsText && strings.Contains(string(f.data), `"type":"chat"`) {
					t.Error("chat frame reached the restricted builder device")
				}
				return f.op == wsPong
			})

			var announced bool
			if err := b.db.QueryRow(`SELECT announced FROM build_jobs WHERE id=?`, job).Scan(&announced); err != nil || announced {
				t.Fatal("Ready was consumed under restriction", announced, err)
			}
			h.mu.Lock()
			if d.card != nil || d.stopAudio != nil {
				t.Error("a build card or speech reached the restricted device")
			}
			h.mu.Unlock()
			if speech.Load() != before {
				t.Error("builder synthesized speech under restriction")
			}
			duringChat, err := b.history.list(t.Context(), device)
			if err != nil || len(duringChat) != len(beforeChat) {
				t.Fatalf("builder delivered restricted chat: %v, %v", duringChat, err)
			}

			// A queued job accepted before a restriction must not be claimed
			// even if the worker already read it from the database.
			const queued = "synthetic-queued-race"
			if _, err := b.db.Exec(`INSERT INTO build_jobs (id,device_id,state,spec,created_at) VALUES (?,?,'queued','{}',?)`,
				queued, device, now.UnixMilli()); err != nil {
				t.Fatal(err)
			}
			if b.builders.claim(t.Context(), buildJob{id: queued, device: device}) {
				t.Fatal("worker claimed a build under restriction")
			}
			if _, err := b.builders.cancel(t.Context(), device, queued); err != nil {
				t.Fatal(err)
			}

			h.mu.Lock()
			err = h.parentSet(device, parentSettings{Timezone: "UTC"}, time.UTC, time.Now())
			h.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			b.builders.signal()
			finishSpeech("build-done", `"card_title":"Ready"`)
			if err := b.db.QueryRow(`SELECT announced FROM build_jobs WHERE id=?`, job).Scan(&announced); err != nil || !announced {
				t.Fatal("Ready did not deliver when allowed", announced, err)
			}
			if speech.Load() != before+1 {
				t.Error("Ready did not speak exactly once")
			}
			afterChat, err := b.history.list(t.Context(), device)
			if err != nil || len(afterChat) != len(beforeChat)+1 || afterChat[len(afterChat)-1].SceneTitle != "Ready" {
				t.Fatalf("Ready chat did not deliver exactly once: %v, %v", afterChat, err)
			}
		})
	}
}

func TestBuildProposesStorybookCompanion(t *testing.T) {
	stub := &buildStub{allow: make(chan struct{}), reply: `{"preset":{"species":"owl","palette":"mint","accessory":"scarf"}}`}
	b, _ := stubBuildBridge(t, stub, true, nil)
	who := peer{DeviceID: "lark-storybook-build", Character: "Bean"}
	job, err := b.builders.enqueue(t.Context(), who, buildIntent{kind: "face"}, "Make me a mint owl wearing a scarf")
	if err != nil || job == "" {
		t.Fatal(job, err)
	}
	expectValue(t, stub.prompted, job)
	close(stub.allow)
	expectValue(t, stub.deleted, job)
	waitBuildState(t, b, job, "ready")
	companion := b.companionFor(who)
	if !companion.Configured || companion.FaceBG != "#E8F7EE" || !strings.Contains(companion.FaceSVG, `data-part="eyes"`) {
		t.Fatal("did not apply the proposed Storybook form")
	}
}

func TestBuildProposesBespokePortrait(t *testing.T) {
	stub := &buildStub{allow: make(chan struct{}), reply: `{"portrait":{"prompt":"a hedgehog in a space suit","style":"pixel"}}`}
	b, _ := stubBuildBridge(t, stub, true, nil)
	sheet := portraitFixture(t)
	b.portraits = &portraitStore{dir: t.TempDir(), generate: func(_ context.Context, prompt string, _ []byte) ([]byte, error) {
		if !strings.Contains(prompt, "indie-game pixel art") || !strings.Contains(prompt, "a hedgehog in a space suit") {
			t.Error("builder lost the portrait art direction")
		}
		return sheet, nil
	}}
	who := peer{DeviceID: "lark-portrait-build", Character: "Bean"}
	job, err := b.builders.enqueue(t.Context(), who, buildIntent{kind: "face"}, "Make your face a hedgehog in a space suit")
	if err != nil {
		t.Fatal(err)
	}
	expectValue(t, stub.prompted, job)
	close(stub.allow)
	expectValue(t, stub.deleted, job)
	waitBuildState(t, b, job, "ready")
	companion := b.companionFor(who)
	if !companion.Configured || companion.Name != "Bean" || !b.portraits.has(activePortrait(companion.FaceSVG)) {
		t.Fatal("did not apply the proposed portrait")
	}
}

func TestBuildRejectsNewerAppAndFace(t *testing.T) {
	for _, kind := range []string{"edit", "face", "new"} {
		t.Run(kind, func(t *testing.T) {
			const device = "lark-stale-build"
			who := peer{DeviceID: device, Character: "Cat"}
			edit := `{"title":"Late title"}`
			intent := buildIntent{kind: kind}
			switch kind {
			case "edit":
				intent.target = "dinosaur-quiz"
			case "face":
				edit = `{"face_edits":[{"part":"eyes","svg":"<circle cx=\"90\" cy=\"100\" r=\"2\"/>"}]}`
			case "new":
				edit = quizProposal
			}
			stub := &buildStub{allow: make(chan struct{}), reply: edit}
			b, _ := stubBuildBridge(t, stub, true, nil)
			write := func(name string) {
				t.Helper()
				_, err := b.writeApp(t.Context(), savedApp{Name: "dinosaur-quiz", Title: name, Kind: "drawing",
					SVG: `<svg viewBox="0 0 360 360"><circle cx="180" cy="180" r="40"/></svg>`})
				if err != nil {
					t.Fatal(err)
				}
			}
			if kind == "edit" {
				write("Base title")
			}
			job, err := b.builders.enqueue(t.Context(), who, intent, "Change this")
			if err != nil || job == "" {
				t.Fatal(job, err)
			}
			expectValue(t, stub.prompted, job)
			if kind == "face" {
				_, _, err := b.changeCompanion(t.Context(), who, "", "update", object{"face_bg": "#000000"})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				write("Newer title")
			}
			close(stub.allow)
			expectValue(t, stub.deleted, job)
			if message := waitBuildState(t, b, job, "failed"); !strings.Contains(message, "kept the newer") {
				t.Fatal(message)
			}
			if kind == "face" {
				if got := b.companionFor(who); got.FaceBG != "#000000" || strings.Contains(got.FaceSVG, `cx="90"`) {
					t.Fatal("late job overwrote the newer face")
				}
			} else if got, err := b.loadApp(t.Context(), "dinosaur-quiz"); err != nil || got.Title != "Newer title" {
				t.Fatal("late job overwrote the saved app")
			}
		})
	}
}

func TestBuildCancelAndRestartNeverPublish(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "restart"}[restart], func(t *testing.T) {
			stub := &buildStub{allow: make(chan struct{}), reply: quizProposal, override: ocModel{"configured", "builder", ""}}
			b, _ := stubBuildBridge(t, stub, false, nil)
			b.builders = newBuildManager(b)
			stop := runBuildManager(t, b)
			who := peer{DeviceID: "lark-build-cancel", Character: "Cat"}
			job, err := b.builders.enqueue(t.Context(), who, buildIntent{kind: "new"}, "Make me a quiz")
			if job == "" || err != nil {
				t.Fatal(job, err)
			}
			expectValue(t, stub.prompted, job)
			if second, err := b.builders.enqueue(t.Context(), who, buildIntent{kind: "new"}, "Make me another quiz"); second != "" || err != nil {
				t.Fatal("allowed two builds for one cat")
			}
			if restart {
				stop()
				b.builders = newBuildManager(b)
				runBuildManager(t, b)
				expectValue(t, stub.deleted, job)
				if result := waitBuildState(t, b, job, "failed"); !strings.Contains(result, "restarted") {
					t.Fatalf("unexpected recovery status: %q", result)
				}
			} else {
				if cancelled, err := b.builders.cancel(t.Context(), who.DeviceID, job); !cancelled || err != nil {
					t.Fatal("explicit cancellation failed", err)
				}
				expectValue(t, stub.deleted, job)
				waitBuildState(t, b, job, "cancelled")
			}
			if _, err := b.loadApp(t.Context(), "dinosaur-quiz"); err == nil {
				t.Fatal("cancelled job saved the app")
			}
		})
	}
}

func waitBuildState(t *testing.T, b *larkServer, id, expected string) string {
	t.Helper()
	var state, result string
	if err := b.db.QueryRow(`SELECT state,result FROM build_jobs WHERE id=?`, id).Scan(&state, &result); err != nil || state != expected {
		t.Fatalf("build state %q, want %q: %v", state, expected, err)
	}
	return result
}

func TestBuildForegroundLatencyStub(t *testing.T) {
	const prompt = "Make me a dinosaur quiz."
	const delay = 250 * time.Millisecond
	for _, background := range []bool{false, true} {
		t.Run(map[bool]string{false: "inline", true: "background"}[background], func(t *testing.T) {
			stub := &buildStub{delay: delay, reply: quizProposal}
			b, _ := stubBuildBridge(t, stub, background, nil)
			start := time.Now()
			result, err := b.turn(t.Context(), peer{DeviceID: "lark-build-eval", Character: "Synthetic"}, prompt)
			elapsed := time.Since(start)
			t.Logf("stub foreground acknowledgement: %s (fixed model delay %s)", elapsed, delay)
			if err != nil {
				t.Fatal(err)
			}
			if background && (result.Reply != "Building it now!" || elapsed >= delay) {
				t.Fatalf("foreground waited for the builder: %s", elapsed)
			}
			if !background && elapsed < delay {
				t.Fatal("baseline did not wait for the model")
			}
		})
	}
}
