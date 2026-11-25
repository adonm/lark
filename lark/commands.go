package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// One command per capability sits here. Transports decode their own typed
// request, call the command, and only format the result; validation and
// side effects live once.

// errUnsupportedVoice is the single voice rejection shared by REST, MCP,
// remote, and companion updates.
var errUnsupportedVoice = problem(400, "Unsupported voice")

// terminalUpdate is the settled state after a volume or sleep change.
type terminalUpdate struct {
	TerminalID  string
	CompanionID string // Empty when only a stored preference changed.
	Volume      int
	Sleeping    bool
}

// object renders the shape existing clients read from the terminal volume
// API. Offline volume-only changes carry no companion because no device is
// connected.
func (t terminalUpdate) object() object {
	out := object{"ok": true, "terminal_id": t.TerminalID, "volume": t.Volume, "sleeping": t.Sleeping}
	if t.CompanionID != "" {
		out["companion_id"] = t.CompanionID
	}
	return out
}

// terminalSetting is one volume/sleep change. Nil fields were not requested.
type terminalSetting struct {
	Volume *int  `json:"volume,omitempty"`
	Sleep  *bool `json:"sleep,omitempty"`
}

func (b *larkServer) setVolume(ctx context.Context, terminalID string, level int) (terminalUpdate, error) {
	return b.applyTerminal(ctx, terminalID, terminalSetting{Volume: &level})
}

func (b *larkServer) setSleep(ctx context.Context, terminalID string, asleep bool) (terminalUpdate, error) {
	return b.applyTerminal(ctx, terminalID, terminalSetting{Sleep: &asleep})
}

// applyTerminal changes the live device when it is connected and otherwise
// stores the volume preference; sleep needs a live device.
func (b *larkServer) applyTerminal(ctx context.Context, terminalID string, set terminalSetting) (terminalUpdate, error) {
	if set.Volume == nil && set.Sleep == nil {
		return terminalUpdate{}, problem(400, "Expected volume or sleep")
	}
	if set.Volume != nil && (*set.Volume < 0 || *set.Volume > 100) {
		return terminalUpdate{}, problem(400, "Volume must be an integer from 0 to 100")
	}
	id := strings.ToLower(strings.TrimSpace(terminalID))
	if set.Sleep == nil {
		if _, ok := b.terminalByID(id); !ok {
			return terminalUpdate{}, problem(404, "Terminal not found")
		}
	}
	h := b.hub
	if h == nil {
		return terminalUpdate{}, problem(503, "Terminal hub unavailable")
	}
	update := terminalUpdate{TerminalID: id}
	d := h.live(id)
	if d == nil {
		if set.Sleep != nil {
			return terminalUpdate{}, problem(409, "Terminal is not connected")
		}
		level := h.b.volumePref(id)
		if set.Volume != nil {
			level = *set.Volume
		}
		h.settingsMu.Lock()
		err := h.b.saveVolumePref(ctx, id, level)
		h.settingsMu.Unlock()
		if err != nil {
			return terminalUpdate{}, err
		}
		update.Volume = level
		return update, nil
	}
	update.TerminalID, update.CompanionID = d.id, d.companionID.get()
	if set.Volume != nil {
		if err := h.setVolume(ctx, d, *set.Volume, ""); err != nil {
			return terminalUpdate{}, err
		}
	}
	var sleeping bool
	var volume int
	d.update(func(d *terminalSession) {
		if set.Sleep != nil {
			now := time.Now()
			d.last = now
			h.setSleeping(d, *set.Sleep)
			h.render(d, now, false)
		}
		sleeping, volume = d.asleep, d.vol
	})
	update.Volume, update.Sleeping = volume, sleeping
	return update, nil
}

func (b *larkServer) checkVoice(voice string) error {
	if !b.voices.allowed(voice) {
		return errUnsupportedVoice
	}
	return nil
}

// setVoice validates once, then persists through the transport's publish
// path: admin REST updates live tiles in place, while the agent and remote
// paths replay the companion so every open screen follows.
func (b *larkServer) setVoice(ctx context.Context, companionID, voice string, replay bool) error {
	if err := b.checkVoice(voice); err != nil {
		return err
	}
	if replay {
		_, err := b.patchCompanion(ctx, companionID, object{"voice": voice})
		return err
	}
	save := b.voices.save
	if b.hub != nil {
		save = b.hub.saveVoice
	}
	return save(companionID, voice)
}

func (b *larkServer) setListening(ctx context.Context, companionID string, on bool) error {
	_, err := b.patchCompanion(ctx, companionID, object{"listening": on})
	return err
}

// openStory shows an existing story on every live terminal for a companion.
func (b *larkServer) openStory(ctx context.Context, companionID, storyID string) (savedStory, error) {
	st, err := b.loadStory(ctx, storyID)
	if err != nil {
		return savedStory{}, problem(404, "Story not found")
	}
	b.showStory(companionID, st)
	return st, nil
}

func (b *larkServer) restartStory(ctx context.Context, companionID, storyID string) (savedStory, error) {
	st, err := b.loadStory(ctx, storyID)
	if err != nil {
		return savedStory{}, problem(404, "Story not found")
	}
	if err := b.clearStoryProgress(ctx, companionID, st.ID); err != nil {
		return savedStory{}, problem(502, "Could not reset progress")
	}
	b.showStory(companionID, st)
	return st, nil
}

func (b *larkServer) showStory(companionID string, st savedStory) {
	if b.hub == nil {
		return
	}
	for _, d := range b.hub.liveFor(companionID) {
		b.hub.openStory(d, st, false)
	}
}

// Capture length is one rule for the CLI, the admin API, and the remote page.
const (
	captureMinSeconds     = 1
	captureMaxSeconds     = 60
	captureDefaultSeconds = 20 // Interactive CLI default.
	capturePageSeconds    = 60 // Remote page default.
	// The CLI HTTP client allows this much overhead beyond the window.
	captureCLIRequestSlack = 20 * time.Second
)

// captureSeconds validates a requested length, or applies the transport's
// default when the request omitted it.
func captureSeconds(raw *int, def int) (int, error) {
	if raw == nil {
		return def, nil
	}
	if *raw < captureMinSeconds || *raw > captureMaxSeconds {
		return 0, problem(400, fmt.Sprintf("seconds must be between %d and %d", captureMinSeconds, captureMaxSeconds))
	}
	return *raw, nil
}

// parseCaptureSeconds validates the CLI's optional length argument.
func parseCaptureSeconds(arg string) (int, error) {
	n, err := strconv.Atoi(arg)
	if err != nil {
		return 0, problem(400, fmt.Sprintf("seconds must be between %d and %d", captureMinSeconds, captureMaxSeconds))
	}
	return captureSeconds(&n, captureDefaultSeconds)
}

// startCapture is the one capture entry point for the CLI and remote page.
func (b *larkServer) startCapture(companionID string, req captureRequest, def int) (*captureRun, error) {
	seconds, err := captureSeconds(req.Seconds, def)
	if err != nil {
		return nil, err
	}
	return b.beginCapture(companionID, seconds)
}
