package main

import (
	"context"
	"encoding/binary"
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

func TestBuildReadyWaitsForIdleAndSurvivesStop(t *testing.T) {
	const device = "cat"
	var speech atomic.Int32
	stub := &buildStub{allow: make(chan struct{}), reply: quizProposal}
	b, base := stubBuildBridge(t, stub, true, &speech)
	if _, err := b.createCompanion(t.Context(), object{"id": device, "name": "Cat", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	s := testConnectWS(t, base, "unit", false)
	s.hello(t, "vocat-0001", device)
	toolTest(t, base, "build", object{"companion_id": device, "request": "Make me a dinosaur quiz."}, false)
	cardAndSpeech := func(label string) {
		t.Helper()
		waitSVG(t, b.ws, "vocat-0001", label)
		var end wsFrame
		s.until(t, func(f wsFrame) bool {
			p := framePayload(f)
			if hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == ttsOpEnd {
				end = f
			}
			return end.data != nil
		})
		audioID := binary.LittleEndian.Uint64(framePayload(end)[1:9])
		s.sendProto(t, msgTTSAck, encodeTTSAck(false, audioID))
		s.barrier(t)
	}
	var job string
	select {
	case job = <-stub.prompted:
	case <-time.After(2 * time.Second):
		t.Fatal("builder did not receive the prompt")
	}

	h := b.ws
	h.mu.Lock()
	d := h.voiceOwner(device)
	h.stopCompanionTurn(d) // Same foreground cancellation primitive; not the job gate.
	h.mu.Unlock()
	_, err := b.runApp(t.Context(), object{"companions": device, "title": "Not the build",
		"code": `function render(s,p,g){g.clear("#1E1A2B")}`})
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
	cardAndSpeech("close")
	h.mu.Lock()
	if d.app != running || running == nil {
		t.Error("completed build replaced the running app")
	}
	h.mu.Unlock()
	if a, err := b.loadApp(t.Context(), "dinosaur-quiz"); err != nil || a.Template != "quiz" {
		t.Fatalf("build did not save valid template: %v", err)
	}
	b.ws.announceBuild(t.Context(), buildJob{id: job, device: device, state: "ready", result: "Dinosaur Quiz is ready in Games."})
	if got := speech.Load(); got != 1 { // Ready announcement.
		t.Errorf("wanted one announcement after warmup, got %d calls", got)
	}
	if sid, err := b.oc.get(device); err != nil || sid != "" {
		t.Fatalf("builder installed a foreground mapping")
	}
}

func TestBuildProposesBespokePortrait(t *testing.T) {
	stub := &buildStub{allow: make(chan struct{}), reply: `{"portrait":{"prompt":"a hedgehog in a space suit","form":"a hedgehog in a space suit"}}`}
	b, _ := stubBuildBridge(t, stub, true, nil)
	sheet := portraitFixture(t)
	b.portraits = &portraitStore{dir: t.TempDir(), generate: func(_ context.Context, prompt string, _ []byte) ([]byte, error) {
		if !strings.Contains(prompt, "indie-game pixel art") || !strings.Contains(prompt, "a hedgehog in a space suit") {
			t.Error("builder lost the portrait art direction")
		}
		return sheet, nil
	}}
	if _, err := b.createCompanion(t.Context(), object{"id": "portrait-build", "name": "Bean", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	job, err := b.builders.enqueue(t.Context(), "portrait-build", buildIntent{kind: "face"}, "Make your face a hedgehog in a space suit")
	if err != nil {
		t.Fatal(err)
	}
	expectValue(t, stub.prompted, job)
	close(stub.allow)
	expectValue(t, stub.deleted, job)
	waitBuildState(t, b, job, "ready")
	companion := b.companionFor("portrait-build")
	if !companion.Configured || companion.Name != "Bean" || !b.portraits.has(companion.PortraitID) {
		t.Fatal("did not apply the proposed portrait")
	}
}

func TestBuildRejectsNewerAppAndFace(t *testing.T) {
	for _, kind := range []string{"edit", "face", "new"} {
		t.Run(kind, func(t *testing.T) {
			const companion = "stale-build"
			edit := `{"title":"Late title"}`
			intent := buildIntent{kind: kind}
			switch kind {
			case "edit":
				intent.target = "dinosaur-quiz"
			case "face":
				edit = `{"portrait":{"prompt":"a hedgehog","form":"a hedgehog"}}`
			case "new":
				edit = quizProposal
			}
			stub := &buildStub{allow: make(chan struct{}), reply: edit}
			b, _ := stubBuildBridge(t, stub, true, nil)
			if _, err := b.createCompanion(t.Context(), object{"id": companion, "name": "Cat", "form": "a cat"}); err != nil {
				t.Fatal(err)
			}
			write := func(name string) {
				t.Helper()
				_, err := b.writeApp(t.Context(), savedApp{Name: "dinosaur-quiz", Title: name, Kind: "game",
					Code: `function render(s,p,g){g.clear('#1E1A2B')}`})
				if err != nil {
					t.Fatal(err)
				}
			}
			if kind == "edit" {
				write("Base title")
			}
			job, err := b.builders.enqueue(t.Context(), companion, intent, "Change this")
			if err != nil || job == "" {
				t.Fatal(job, err)
			}
			expectValue(t, stub.prompted, job)
			if kind == "face" {
				_, err := b.changeCompanion(t.Context(), companion, "update", object{"form": "a fox"})
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
				if got := b.companionFor(companion); got.Form != "a fox" || got.PortraitID != "" {
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
			const companion = "build-cancel"
			if _, err := b.createCompanion(t.Context(), object{"id": companion, "name": "Cat", "form": "a cat"}); err != nil {
				t.Fatal(err)
			}
			job, err := b.builders.enqueue(t.Context(), companion, buildIntent{kind: "new"}, "Make me a quiz")
			if job == "" || err != nil {
				t.Fatal(job, err)
			}
			expectValue(t, stub.prompted, job)
			if second, err := b.builders.enqueue(t.Context(), companion, buildIntent{kind: "new"}, "Make me another quiz"); second != "" || err != nil {
				t.Fatal("allowed two builds for one companion")
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
				if cancelled, err := b.builders.cancel(t.Context(), companion, job); !cancelled || err != nil {
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
		t.Fatalf("build state %q (result=%q), want %q: %v", state, result, expected, err)
	}
	return result
}

func TestMCPBuildToolVoiceAndDeltaChat(t *testing.T) {
	stub := &buildStub{allow: make(chan struct{}), reply: quizProposal}
	close(stub.allow)
	var speech atomic.Int32
	b, base := stubBuildBridge(t, stub, true, &speech)
	dcStub := newStubDCRPC()
	b.dc = newDCService(b, "", t.TempDir(), "nine.testrun.org")
	b.dc.rpc = dcStub
	b.dc.ready.Store(true)

	_, err := b.createCompanion(t.Context(), object{"id": "greg", "name": "Greg", "form": "a cat"})
	if err != nil {
		t.Fatal(err)
	}
	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, "box3-bld1", "greg")

	// 1. Delta Chat turn calls build -> completes -> sends DC message AND announces on terminal.
	b.setDCBuildTarget("greg", 1, 200)
	res := toolTest(t, base, "build", object{
		"companion_id": "greg",
		"request":      "Make a dinosaur quiz.",
	}, false)
	b.setDCBuildTarget("greg", 0, 0)
	jobID := str(res["job_id"])
	if jobID == "" {
		t.Fatalf("expected job_id, got %v", res)
	}
	expectValue(t, stub.deleted, jobID)
	waitBuildState(t, b, jobID, "ready")
	b.builders.signal()
	waitSVG(t, b.ws, "box3-bld1", "Dinosaur Quiz is ready in Games.")
	dcStub.mu.Lock()
	dcCount := len(dcStub.sent)
	dcStub.mu.Unlock()
	if dcCount != 1 || dcStub.sent[0].chatID != 200 || !strings.Contains(*dcStub.sent[0].data.Text, "Dinosaur Quiz is ready in Games.") {
		t.Fatalf("expected DC completion message, got %+v", dcStub.sent)
	}

	// 2. Voice turn calls build to edit the saved app -> completes -> announces on terminal without extra DC message.
	stub.reply = `{"title":"Dinosaur Quiz 2"}`
	editRes := toolTest(t, base, "build", object{
		"companion_id": "greg",
		"app":          "dinosaur-quiz",
		"request":      "Edit dinosaur-quiz to add a timer.",
	}, false)
	editJob := str(editRes["job_id"])
	if editJob == "" {
		t.Fatalf("expected edit job_id, got %v", editRes)
	}
	expectValue(t, stub.deleted, editJob)
	waitBuildState(t, b, editJob, "ready")
	b.builders.signal()
	if speech.Load() < 1 {
		t.Fatalf("expected terminal speech announcement, got %d", speech.Load())
	}
	dcStub.mu.Lock()
	dcCountAfter := len(dcStub.sent)
	dcStub.mu.Unlock()
	if dcCountAfter != 1 {
		t.Fatalf("voice build should not send DC message, got %d", dcCountAfter)
	}
}
