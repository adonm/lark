package edge

import (
	"image"
	"time"

	"lark/internal/audio"
	"lark/internal/pixel"
)

// Screen is the declarative JSON payload the brain sends to terminal.render.
// Unknown fields are ignored; an empty kind renders nothing.
type Screen struct {
	Kind          string         `json:"kind"` // home|caption|menu|list|reader|detail|pair_qr|volume|voice|stats
	PortraitDir   string         `json:"portrait_dir"`
	CompanionName string         `json:"companion_name"`
	CompanionForm string         `json:"companion_form"`
	Status        string         `json:"status"` // idle|listening|thinking|speaking
	Caption       string         `json:"caption"`
	ListenWindow  bool           `json:"listen_window"`
	Page          int            `json:"page"`
	Title         string         `json:"title"`
	Heading       string         `json:"heading"`
	Body          string         `json:"body"`
	BodyScale     int            `json:"body_scale"`
	Invite        string         `json:"invite"`
	Level         int            `json:"level"`
	Current       string         `json:"current"`
	Items         []ScreenItem   `json:"items"`
	Choices       []ScreenChoice `json:"choices"`
	Voices        []ScreenVoice  `json:"voices"`
	Rows          []ScreenRow    `json:"rows"`
}

type ScreenItem struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Detail string `json:"detail"`
	Icon   string `json:"icon"`
	Action string `json:"action"`
	Value  string `json:"value"`
}

type ScreenChoice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Value string `json:"value"`
}

type ScreenVoice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type ScreenRow struct {
	Title string `json:"title"`
	Text  string `json:"text"`
}

type renderJob struct {
	gen    uint64
	screen Screen
	mode   terminalMode
	full   bool
}

type frameBuilt struct {
	gen     uint64
	rev     uint64
	payload []byte
	hits    []pixel.HitBox
	actions map[string]pixel.Action
}

type assetEntry struct {
	dir   string
	bg    *image.RGBA
	poses [4]*image.RGBA
	theme pixel.Theme
}

// frameBuffers keeps the previous RGB565 frame and one scratch frame per
// terminal. renderWorker fills the scratch, diffs against the front, encodes,
// then swaps; resizing replaces both and forces a full frame.
type frameBuffers struct {
	front []uint16
	back  []uint16
	w, h  int
	valid bool
}

func (b *frameBuffers) scratch(w, h int) []uint16 {
	if b.back == nil || b.w != w || b.h != h {
		b.front = make([]uint16, w*h)
		b.back = make([]uint16, w*h)
		b.w, b.h = w, h
		b.valid = false
	}
	return b.back
}

func (b *frameBuffers) commit() {
	b.front, b.back = b.back, b.front
	b.valid = true
}

func (t *Terminal) renderWorker() {
	enc := new(pixel.FrameEncoder)
	var bufs frameBuffers
	var prevGrade pixel.GradeKey
	var rev uint64
	for {
		select {
		case <-t.life.done:
			return
		case job := <-t.life.jobs:
			img, hits, actions, err := t.draw(job.screen, job.mode)
			if err != nil || img == nil {
				if err != nil {
					t.logf("lark-edge: render %s: %v", t.info.id, err)
				}
				continue
			}
			b := img.Bounds()
			w, h := b.Dx(), b.Dy()
			fb := bufs.scratch(w, h)
			for y := 0; y < h; y++ {
				for x := 0; x < w; x++ {
					c := img.RGBAAt(b.Min.X+x, b.Min.Y+y)
					fb[y*w+x] = pixel.Pack565(c.R, c.G, c.B)
				}
			}
			grade := pixel.GradeAt(time.Now(), time.Local)
			rev++
			rect := pixel.DirtyRect(bufs.front, fb, w, h)
			if job.full || !bufs.valid {
				rect = image.Rect(0, 0, w, h)
			}
			if rect.Empty() && grade == prevGrade {
				pixel.ReleaseImage(img)
				continue
			}
			if rect.Empty() {
				rect = image.Rect(0, 0, 1, 1)
			}
			codecs := t.info.codecs
			if codecs == 0 {
				codecs = pixel.CodecBitAll
			}
			if !job.full && bufs.valid {
				codecs &^= 1 << 3 // JPEG is only worthwhile for whole scenes.
			}
			// Keep the rendered RGBA alive through encoding so JPEG can use it
			// directly instead of converting RGB565 back to RGBA.
			payload := enc.EncodeWithCodecs(fb, img, rect, rev, w, codecs)
			pixel.ReleaseImage(img)
			bufs.commit()
			prevGrade = grade
			select {
			case t.life.events <- termEvent{kind: evFrameBuilt, frame: &frameBuilt{
				gen: job.gen, rev: rev, payload: payload, hits: hits, actions: actions,
			}}:
			case <-t.life.done:
				return
			}
		}
	}
}

func (t *Terminal) draw(s Screen, mode terminalMode) (*image.RGBA, []pixel.HitBox, map[string]pixel.Action, error) {
	spec := pixel.SpecForGeometry(t.info.width, t.info.height, t.info.shape)
	bg, poses, th := t.assets(s.PortraitDir)
	status := s.Status
	if status == "" {
		status = mode.String()
	}
	switch s.Kind {
	case "home", "caption", "":
		img := pixel.RenderHomeListen(spec, bg, poses[poseFor(status)], pixel.GradeAt(time.Now(), time.Local), statusIcon(status), s.Caption, s.ListenWindow)
		hits, actions := homeHits(spec, status, s.ListenWindow)
		return img, hits, actions, nil
	case "menu":
		img, hits, actions := pixel.RenderMenuWithHits(spec, s.Page, th)
		return img, hits, actions, nil
	case "list":
		items := make([]pixel.Tile, 0, len(s.Items))
		for _, it := range s.Items {
			act := pixel.Action{Kind: it.Action, Value: it.Value}
			if act.Kind == "" {
				act.Kind = it.ID
			}
			items = append(items, pixel.Tile{ID: it.ID, Label: it.Label, Detail: it.Detail, Icon: it.Icon, Action: act})
		}
		img, hits, actions := pixel.RenderListWithHits(spec, s.Title, items, "Nothing here", s.Page, th, th.Accent2)
		return img, hits, actions, nil
	case "reader":
		choices := make([]string, 0, len(s.Choices))
		for _, c := range s.Choices {
			choices = append(choices, c.Label)
		}
		img, hits, actions, _ := pixel.RenderStoryReaderWithHits(spec, s.Title, s.Body, choices, false, s.Page, bg, poses[poseFor(status)], th)
		for i, c := range s.Choices {
			id := c.ID
			if id == "" {
				id = "choice-" + string(rune('0'+i))
			}
			actions[id] = pixel.Action{Kind: "choice", Value: c.Value}
		}
		return img, hits, actions, nil
	case "detail":
		img, hits := pixel.RenderSceneCard(spec, pixel.Scene{Title: s.Title, Heading: s.Heading, Text: s.Body}, th)
		return img, hits, map[string]pixel.Action{}, nil
	case "pair_qr":
		img, hits, actions, err := pixel.RenderPairWithHits(spec, s.CompanionName, s.Invite)
		return img, hits, actions, err
	case "volume":
		img, hits, actions := pixel.RenderVolumeWithHits(spec, s.Level, s.Page, th)
		return img, hits, actions, nil
	case "voice":
		voices := make([]pixel.Tile, 0, len(s.Voices))
		for _, v := range s.Voices {
			voices = append(voices, pixel.Tile{ID: v.ID, Label: v.Label})
		}
		if len(voices) == 0 {
			for _, v := range audio.Catalog {
				voices = append(voices, pixel.Tile{ID: v.ID, Label: v.Name})
			}
		}
		img, hits, actions := pixel.RenderVoiceWithHits(spec, voices, s.Current, s.Page, th)
		return img, hits, actions, nil
	case "stats":
		rows := make([]pixel.Row, 0, len(s.Rows))
		for _, r := range s.Rows {
			rows = append(rows, pixel.Row{Title: r.Title, Text: r.Text})
		}
		img, hits, actions := pixel.RenderStatsWithHits(spec, rows, th)
		return img, hits, actions, nil
	}
	return pixel.RenderHomeListen(spec, bg, poses[poseFor(status)], pixel.GradeAt(time.Now(), time.Local), statusIcon(status), s.Caption, s.ListenWindow), nil, nil, nil
}

func (t *Terminal) assets(dir string) (*image.RGBA, [4]*image.RGBA, pixel.Theme) {
	if dir == "" {
		return nil, pixel.PlaceholderPoses(), pixel.PlaceholderTheme()
	}
	e := t.deps.e
	e.assetMu.Lock()
	if a := e.assets[dir]; a != nil && a.dir == dir {
		e.assetMu.Unlock()
		return a.bg, a.poses, a.theme
	}
	e.assetMu.Unlock()

	a := &assetEntry{dir: dir, poses: pixel.PlaceholderPoses()}
	if poses, err := pixel.LoadOrNormalizePoses(dir, pixel.PortraitHeight); err == nil {
		a.poses = poses
	}
	if bg, err := pixel.LoadBackgroundPNG(dir); err == nil {
		a.bg = bg
	}
	a.theme = pixel.DeriveTheme(a.bg, a.poses[:])
	e.assetMu.Lock()
	if e.assets == nil {
		e.assets = make(map[string]*assetEntry)
	}
	e.assets[dir] = a
	e.assetMu.Unlock()
	return a.bg, a.poses, a.theme
}

func poseFor(status string) int {
	switch status {
	case "thinking":
		return 3
	case "speaking":
		return 2
	}
	return 0
}

func statusIcon(status string) string {
	switch status {
	case "listening", "thinking", "speaking":
		return status
	}
	return ""
}

func homeHits(spec pixel.Spec, status string, listenWindow bool) ([]pixel.HitBox, map[string]pixel.Action) {
	actions := map[string]pixel.Action{}
	var hits []pixel.HitBox
	switch {
	case status == "listening" && listenWindow:
		hits = []pixel.HitBox{{ID: "listen-stop", Label: "Stop", X1: 0, Y1: 0,
			X2: spec.LogicalW*spec.ArtScale - 1, Y2: spec.LogicalH*spec.ArtScale - 1}}
		actions["listen-stop"] = pixel.Action{Kind: "listen-stop"}
	case status == "listening":
		btnH := pixel.NavHeight(spec)
		y := spec.Safe.Max.Y - btnH
		gap := 4
		bw := (spec.Safe.Dx() - gap) / 2
		hits = pixel.AddLabeledHit(hits, spec, "rec-cancel", "Cancel", image.Rect(spec.Safe.Min.X, y, spec.Safe.Min.X+bw, y+btnH))
		hits = pixel.AddLabeledHit(hits, spec, "rec-send", "Send", image.Rect(spec.Safe.Min.X+bw+gap, y, spec.Safe.Max.X, y+btnH))
		actions["rec-cancel"] = pixel.Action{Kind: "rec-cancel"}
		actions["rec-send"] = pixel.Action{Kind: "rec-send"}
	case status == "thinking" || status == "speaking":
		btnW, btnH := 42, pixel.NavHeight(spec)
		if !spec.Round {
			btnW = 48
		}
		btn := image.Rect((spec.LogicalW-btnW)/2, spec.Safe.Max.Y-btnH, (spec.LogicalW+btnW)/2, spec.Safe.Max.Y)
		hits = pixel.AddLabeledHit(hits, spec, "stop", "Stop", btn)
		actions["stop"] = pixel.Action{Kind: "stop"}
	default:
		hits = []pixel.HitBox{{ID: "listen", Label: "Listen", X1: 0, Y1: 0,
			X2: spec.LogicalW*spec.ArtScale - 1, Y2: spec.LogicalH*spec.ArtScale - 1}}
		hits = pixel.AddLabeledHit(hits, spec, "menu", "Menu", pixel.HomeMenuButtonRect(spec))
		actions["listen"] = pixel.Action{Kind: "listen"}
		actions["menu"] = pixel.Action{Kind: "menu"}
	}
	return hits, actions
}
