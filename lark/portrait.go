package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	imagecolor "image/color"
	"image/draw"
	_ "image/jpeg"
	"image/png"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	xdraw "golang.org/x/image/draw"

	"lark/internal/pixel"
)

const portraitModelID = "gemini-nano-banana-2.1"
const portraitEndpoint = "https://opencode.ai/inference/google/v1beta/models/" + portraitModelID + ":generateContent"
const openCodeImagePath = "/api/experimental/generate/image"
const portraitPixelSide = 64
const portraitPixelStyle = "pixel"
const portraitStorybookStyle = "storybook"
const portraitMaxBytes = 8 << 20
const portraitTimeout = 65 * time.Second
const portraitCenterY = 162

var portraitIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
var portraitSchema = closedObject(object{
	"prompt":  object{"type": "string", "maxLength": 600},
	"form":    object{"type": "string", "maxLength": 160},
	"recolor": object{"type": "string", "maxLength": 200},
	"edit":    object{"type": "boolean"},
}, "prompt")

type portraitSpec struct {
	Prompt, Form, Recolor, BG, Style string
	Edit                             bool
}

func decodePortraitSheet(raw []byte) (image.Image, error) {
	if len(raw) > portraitMaxBytes {
		return nil, errors.New("portrait sheet exceeds 8 MiB")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || (format != "png" && format != "jpeg") || cfg.Width != pixel.SheetSide || cfg.Height != pixel.SheetSide {
		return nil, errors.New("expected a 1024×1024 PNG or JPEG portrait sheet")
	}
	sheet, _, err := image.Decode(bytes.NewReader(raw))
	return sheet, err
}

func parsePortraitRef(raw string) (id, pose string, ok bool) {
	ref, found := strings.CutPrefix(raw, "portrait:")
	if !found {
		return "", "", false
	}
	id, pose, found = strings.Cut(ref, ":")
	if !found || !portraitIDPattern.MatchString(id) || !slices.Contains(pixel.Poses[:], pose) {
		return "", "", false
	}
	return id, pose, true
}

type portraitGenerator func(ctx context.Context, prompt string, source []byte) ([]byte, error)

type portraitBGQueue struct {
	running    bool
	dirty      bool
	form, vibe string
}

type portraitStore struct {
	dir      string
	generate portraitGenerator
}

func portraitPrompt(spec portraitSpec) string {
	style := "Lark Storybook Illustration"
	art := `Cozy hand-painted gouache meets soft storybook vector: crisp hue-shifted cocoa, pine, plum or navy outlines (not black), slightly warm top highlights, softly colored base shadows, simple matte pastel shapes, glossy eyes with cream catchlights.`
	if spec.Style == portraitPixelStyle {
		style = "indie-game pixel art"
		art = `Use crisp 64×64-style pixel clusters with hue-shifted colored outlines, warm highlights and stepped shadows for a cozy Stardew Valley or GBA look. Keep a consistent square pixel grid at the same scale in all four poses, with no antialiasing or dithering.`
	}
	prompt := `Create one square 1024×1024 ` + style + ` expression sheet, exactly four equal quadrants, no text, labels, borders or gridlines.
Top-left idle: gentle closed-mouth smile. Top-right blink: both eyes closed, same smile. Bottom-left talk: open mouth, friendly eyes. Bottom-right think: eyes glance up, thoughtful mouth.
Draw the very same front-facing character, head and rounded shoulders, at the exact same scale, silhouette, position and line weight in every quadrant. Only the expression changes. Keep each silhouette well within its quadrant; maintain generous circular margin with nothing touching an edge.
` + art + ` No extra props except requested accessories, no scene, no cast shadow, no texture or gradient on the background. Use the same solid flat ` + spec.BG + ` background across the entire sheet.`
	if spec.Edit {
		prompt += "\nUse the attached sheet as a reference. Keep character identity, silhouette, colors, expression layout and placement unchanged except for the requested change."
	}
	if spec.Form != "" {
		prompt += "\nCharacter form: " + spec.Form
	}
	if spec.Recolor != "" {
		prompt += "\nRecolor instruction: " + spec.Recolor
	}
	return prompt + "\nCharacter art direction: " + spec.Prompt
}

func configuredPortraitGenerator(oc upstream, dedicated bool, key string) portraitGenerator {
	if key != "" {
		client := &http.Client{
			Timeout:       portraitTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
		return func(ctx context.Context, prompt string, source []byte) ([]byte, error) {
			return requestPortrait(ctx, client, portraitEndpoint, key, prompt, source)
		}
	}
	if dedicated {
		return func(ctx context.Context, prompt string, source []byte) ([]byte, error) {
			return requestOpenCodePortrait(ctx, oc, prompt, source)
		}
	}
	return nil
}

type openCodeImage struct {
	MIME string `json:"mime"`
	Data []byte `json:"data"`
}

func requestOpenCodePortrait(ctx context.Context, oc upstream, prompt string, source []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, portraitTimeout)
	defer cancel()
	input := object{"model": ocModel{Provider: "opencode", ID: portraitModelID}, "prompt": prompt,
		"n": 1, "aspect": "1:1", "resolution": "1K"}
	if len(source) > 0 {
		input["images"] = []openCodeImage{{MIME: "image/png", Data: source}}
	}
	body, err := json.Marshal(input)
	if err != nil {
		return nil, oc.invalid()
	}
	res, err := oc.request(ctx, http.MethodPost, openCodeImagePath, jsonType, jsonType, int64(len(body)),
		func() io.Reader { return bytes.NewReader(body) })
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	const limit = 2 * portraitMaxBytes
	raw, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	var output struct {
		Data struct{ Images []openCodeImage }
	}
	if err != nil || len(raw) > limit || json.Unmarshal(raw, &output) != nil {
		return nil, oc.invalid()
	}
	for _, img := range output.Data.Images {
		if isPortraitImage(img.MIME, img.Data) {
			return img.Data, nil
		}
	}
	return nil, problem(502, "OpenCode response did not contain a portrait sheet")
}

func isPortraitImage(mime string, data []byte) bool {
	return (mime == "image/png" || mime == "image/jpeg") && len(data) > 0 && len(data) <= portraitMaxBytes
}

func requestPortrait(ctx context.Context, client *http.Client, endpoint, key, prompt string, source []byte) ([]byte, error) {
	type inline struct {
		MIME string `json:"mimeType"`
		Data []byte `json:"data"`
	}
	type part struct {
		Text   string  `json:"text,omitempty"`
		Inline *inline `json:"inlineData,omitempty"`
	}
	type content struct {
		Parts []part `json:"parts"`
	}
	input := content{Parts: []part{{Text: prompt}}}
	if len(source) > 0 {
		input.Parts = append(input.Parts, part{Inline: &inline{"image/png", source}})
	}
	body, err := json.Marshal(object{"contents": []content{input}, "generationConfig": object{
		"candidateCount": 1, "responseModalities": []string{"IMAGE"},
		"imageConfig": object{"aspectRatio": "1:1", "imageSize": "1K"},
	}})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := client.Do(req)
	if err != nil {
		return nil, problem(502, "Cannot reach the Console image service")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, problem(502, fmt.Sprintf("Console image service returned HTTP %d", res.StatusCode))
	}
	var output struct{ Candidates []struct{ Content content } }
	if err := json.NewDecoder(io.LimitReader(res.Body, 2*portraitMaxBytes)).Decode(&output); err != nil {
		return nil, problem(502, "Invalid Console image response")
	}
	for _, candidate := range output.Candidates {
		for _, p := range candidate.Content.Parts {
			if p.Inline != nil && isPortraitImage(p.Inline.MIME, p.Inline.Data) {
				return p.Inline.Data, nil
			}
		}
	}
	return nil, problem(502, "Console response did not contain a portrait sheet")
}

// Use the median of the four inset corner patches, not a single noisy pixel.

// Use the dominant connected silhouette for the bounds, ignoring small islands
// of compression or stray illustration at the edges.

func backgroundPrompt(form, vibe string) string {
	scene := strings.TrimSpace(form)
	if scene == "" {
		scene = "a cozy sunlit window sill with potted plants"
	}
	p := `Create a cozy indie-game pixel art background scene (Stardew Valley or GBA style) that fits the habitat and vibe of: ` + scene + `.`
	if v := strings.TrimSpace(vibe); v != "" {
		p += ` Vibe: ` + v + `.`
	}
	return p + ` Use crisp pixel clusters, soft warm lighting, and a harmonious limited palette. No characters, no creatures, no text, no UI, no borders, no frames. Keep the center-top area slightly calmer so a foreground character stands out clearly.`
}

func (p *portraitStore) generateBackground(ctx context.Context, id, form, vibe string) error {
	if p == nil || p.generate == nil || !portraitIDPattern.MatchString(id) {
		return nil
	}
	raw, err := p.generate(ctx, backgroundPrompt(form, vibe), nil)
	if err != nil {
		return err
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	norm, err := pixel.NormalizeBackground(img, pixel.SpecBox3.LogicalW, pixel.SpecBox3.LogicalH)
	if err != nil {
		return err
	}
	return pixel.SaveBackgroundPNG(filepath.Join(p.dir, id), norm)
}

func (p *portraitStore) create(ctx context.Context, spec portraitSpec, currentID string) (string, error) {
	if p == nil || p.generate == nil {
		return "", problem(503, "Portrait generation unavailable; configure a dedicated OpenCode service or OPENCODE_API_KEY, or use companion.preset")
	}
	var source []byte
	if spec.Edit {
		var err error
		source, err = p.read(currentID, "sheet")
		if err != nil {
			return "", fieldError("companion.portrait.edit", "current companion has no local portrait sheet")
		}
	}
	raw, err := p.generate(ctx, portraitPrompt(spec), source)
	if err != nil {
		return "", err
	}
	sheet, err := decodePortraitSheet(raw)
	if err != nil {
		return "", problem(502, err.Error())
	}
	bg := imagecolor.RGBA{250, 240, 230, 255}
	poses, err := pixel.Slice(sheet, bg)
	if err != nil {
		return "", problem(502, err.Error())
	}
	if spec.Style == portraitPixelStyle {
		pixels := image.NewRGBA(image.Rect(0, 0, portraitPixelSide, portraitPixelSide))
		for _, pose := range poses {
			xdraw.NearestNeighbor.Scale(pixels, pixels.Bounds(), pose, pose.Bounds(), draw.Src, nil)
			xdraw.NearestNeighbor.Scale(pose, pose.Bounds(), pixels, pixels.Bounds(), draw.Src, nil)
		}
	}
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		return "", problem(502, "Cannot create portrait storage")
	}
	id, err := randomHex(16)
	if err != nil {
		return "", problem(500, "Cannot create a portrait identifier")
	}
	dir := filepath.Join(p.dir, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", problem(502, "Cannot create portrait")
	}
	if err := func() error {
		if err := pixel.WritePNG(dir, "sheet", sheet); err != nil {
			return err
		}
		for i, pose := range pixel.Poses {
			if err := pixel.WritePNG(dir, pose, poses[i]); err != nil {
				return err
			}
		}
		if normPoses, err := pixel.NormalizePortraitSheet(sheet, pixel.PortraitHeight); err == nil {
			_ = pixel.SaveNormalizedPoses(dir, normPoses)
		}
		return ctx.Err()
	}(); err != nil {
		p.discard(id)
		return "", problem(502, "Cannot save portrait")
	}
	if spec.Form != "" && !spec.Edit {
		_ = p.generateBackground(ctx, id, spec.Form, spec.Prompt)
	} else if spec.Edit && currentID != "" {
		if bg, err := pixel.LoadBackgroundPNG(filepath.Join(p.dir, currentID)); err == nil {
			_ = pixel.SaveBackgroundPNG(dir, bg)
		}
	}
	return id, nil
}

func (p *portraitStore) discard(id string) {
	if p == nil || !portraitIDPattern.MatchString(id) {
		return
	}
	dir := filepath.Join(p.dir, id)
	for _, pose := range pixel.Poses {
		_ = os.Remove(filepath.Join(dir, pose+".png"))
		_ = os.Remove(filepath.Join(dir, pixel.PoseFilename(pose)))
	}
	_ = os.Remove(filepath.Join(dir, "bg.png"))
	_ = os.Remove(filepath.Join(dir, "sheet.png"))
	_ = os.Remove(dir)
}

func (p *portraitStore) read(id, pose string) ([]byte, error) {
	if p == nil || !portraitIDPattern.MatchString(id) || pose != "sheet" && !slices.Contains(pixel.Poses[:], pose) {
		return nil, os.ErrNotExist
	}
	return os.ReadFile(filepath.Join(p.dir, id, pose+".png"))
}

func (p *portraitStore) image(ref string) (image.Image, error) {
	id, pose, ok := parsePortraitRef(ref)
	if !ok {
		return nil, os.ErrNotExist
	}
	data, err := p.read(id, pose)
	if err != nil {
		return nil, err
	}
	return png.Decode(bytes.NewReader(data))
}

func (p *portraitStore) has(id string) bool {
	if p == nil || !portraitIDPattern.MatchString(id) {
		return false
	}
	_, err := os.Stat(filepath.Join(p.dir, id, "sheet.png"))
	return err == nil
}

// preparePortraitRequest validates a portrait edit without generating it.
// Generation runs in the single-flight worker after the text fields persist.
func (b *larkServer) preparePortraitRequest(companionID string, args object, raw any) (portraitSpec, error) {
	var spec portraitSpec
	current, ok := b.companionByID(companionID)
	if !ok {
		return spec, problem(404, "Companion not found")
	}
	if _, err := updateCompanion(current, args, b.voices); err != nil {
		return spec, err
	}
	if b.portraits == nil || b.portraits.generate == nil {
		return spec, problem(503, "Portrait generation unavailable; configure a dedicated OpenCode service or OPENCODE_API_KEY, or use companion.preset")
	}
	spec, err := readPortraitSpec(raw)
	if err != nil {
		return spec, err
	}
	if spec.Prompt = strings.TrimSpace(spec.Prompt); spec.Prompt == "" {
		return spec, fieldError("companion.portrait.prompt", "describe the character or edit")
	}
	if spec.Form = strings.TrimSpace(spec.Form); spec.Form == "" {
		spec.Form = current.Form
	}
	if _, err := updateCompanion(current, object{"form": spec.Form}, b.voices); err != nil {
		return spec, err
	}
	spec.Style = portraitPixelStyle
	spec.BG = "#FAF0E6"
	if spec.Edit && !b.portraits.has(current.PortraitID) {
		return spec, fieldError("companion.portrait.edit", "current companion has no local portrait sheet")
	}
	return spec, nil
}

// portraitReadyNotice confirms a background look change without a full turn.
const portraitReadyNotice = "Ta-da! My new look is ready!"

type portraitJob struct {
	running bool
	dirty   bool
	spec    portraitSpec
	done    chan struct{}
}

// requestPortrait starts or updates the one portrait worker for a companion.
func (b *larkServer) requestPortrait(id string, spec portraitSpec) {
	b.portraitJobsMu.Lock()
	if b.portraitJobs == nil {
		b.portraitJobs = map[string]*portraitJob{}
	}
	q := b.portraitJobs[id]
	if q == nil {
		q = &portraitJob{done: make(chan struct{})}
		b.portraitJobs[id] = q
	}
	q.spec, q.dirty = spec, true
	if q.running {
		b.portraitJobsMu.Unlock()
		return
	}
	q.running = true
	b.portraitJobsMu.Unlock()
	go b.runPortraitJob(id, q)
}

// waitPortrait blocks until the current portrait worker for a companion has
// no queued edit left. Tests use it instead of sleeping on the async result.
func (b *larkServer) waitPortrait(ctx context.Context, id string) error {
	b.portraitJobsMu.Lock()
	q := b.portraitJobs[id]
	if q == nil {
		b.portraitJobsMu.Unlock()
		return nil
	}
	done := q.done
	b.portraitJobsMu.Unlock()
	select {
	case <-done:
		b.portraitJobsMu.Lock()
		left := b.portraitJobs[id]
		b.portraitJobsMu.Unlock()
		if left != nil {
			return b.waitPortrait(ctx, id)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *larkServer) runPortraitJob(id string, q *portraitJob) {
	for {
		b.portraitJobsMu.Lock()
		spec := q.spec
		q.dirty = false
		b.portraitJobsMu.Unlock()
		ctx := b.root
		if ctx == nil {
			ctx = context.Background()
		}
		if err := b.applyPortrait(ctx, id, spec); err != nil {
			log.Printf("Lark could not create the portrait for %s: %v", id, err)
		}
		b.portraitJobsMu.Lock()
		if !q.dirty {
			delete(b.portraitJobs, id)
			q.running = false
			close(q.done)
			b.portraitJobsMu.Unlock()
			return
		}
		b.portraitJobsMu.Unlock()
	}
}

// applyPortrait generates the image, then atomically swaps portrait_id and
// pushes the new look everywhere the companion is attached.
func (b *larkServer) applyPortrait(ctx context.Context, id string, spec portraitSpec) error {
	current, ok := b.companionByID(id)
	if !ok {
		return problem(404, "Companion not found")
	}
	portraitID, err := b.portraits.create(ctx, spec, current.PortraitID)
	if err != nil {
		return err
	}
	b.companionMu.Lock()
	if latest, ok := b.companionByID(id); ok {
		current = latest
	}
	result, err := b.db.ExecContext(ctx, `UPDATE companions SET portrait_id=?, updated_at=? WHERE id=? AND portrait_id=?`,
		portraitID, time.Now().UnixMilli(), id, current.PortraitID)
	if err != nil {
		b.companionMu.Unlock()
		b.portraits.discard(portraitID)
		return problem(502, "Cannot save companion")
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		b.companionMu.Unlock()
		b.portraits.discard(portraitID)
		return problem(409, "Companion changed while the portrait was being drawn")
	}
	updated := b.companionFor(id)
	if b.hub != nil {
		b.hub.invalidateAssets(current.PortraitID)
		b.hub.companionChanged(id, updated)
	}
	b.companionMu.Unlock()
	if current.PortraitID != "" {
		b.portraits.discard(current.PortraitID)
	}
	b.chat(id, newChat("companion", updated.Name, portraitReadyNotice, "portrait"), true)
	if b.hub != nil {
		b.hub.deliverNotice(ctx, terminalNotice{
			recipientIDs: []string{id}, sender: updated, text: portraitReadyNotice,
			speakText: portraitReadyNotice, voice: b.voiceFor(id), chime: "celebrate", showCard: true,
		})
	}
	return nil
}

func parseHexRGBA(s string, fallback imagecolor.RGBA) imagecolor.RGBA {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "#") || len(s) != 7 {
		return fallback
	}
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return fallback
	}
	return imagecolor.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 255}
}

func readPortraitSpec(raw any) (portraitSpec, error) {
	var spec portraitSpec
	obj, ok := raw.(object)
	if !ok {
		return spec, fieldError("companion.portrait", "expected an object")
	}
	for k := range obj {
		switch k {
		case "prompt", "form", "recolor", "edit":
		default:
			return spec, fieldError("companion.portrait."+k, "unsupported field")
		}
	}
	data, err := json.Marshal(obj)
	if err == nil {
		err = json.Unmarshal(data, &spec)
	}
	return spec, err
}

func (b *larkServer) servePortrait(w http.ResponseWriter, r *http.Request, path, scope string) error {
	id, pose, found := strings.Cut(strings.TrimPrefix(path, "/api/portraits/"), "/")
	pose, pngFile := strings.CutSuffix(pose, ".png")
	if !found || !pngFile || !portraitIDPattern.MatchString(id) || !slices.Contains(pixel.Poses[:], pose) {
		return problem(404, "Portrait not found")
	}
	if scope != "" {
		var portraitID string
		if b.db == nil || b.db.readDB().QueryRowContext(r.Context(), `SELECT portrait_id FROM companions WHERE id=?`, strings.ToLower(scope)).Scan(&portraitID) != nil ||
			portraitID != id {
			return problem(404, "Portrait not found")
		}
	}
	data, err := b.portraits.read(id, pose)
	if err != nil {
		return problem(404, "Portrait not found")
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	sendBytes(w, 200, "image/png", data)
	return nil
}
