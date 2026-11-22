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
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	xdraw "golang.org/x/image/draw"
)

const portraitModelID = "gemini-nano-banana-2.1"
const portraitEndpoint = "https://opencode.ai/inference/google/v1beta/models/" + portraitModelID + ":generateContent"
const openCodeImagePath = "/api/experimental/generate/image"
const portraitSheetSide = 1024
const portraitPixelSide = 64
const portraitPixelStyle = "pixel"
const portraitStorybookStyle = "storybook"
const portraitMaxBytes = 8 << 20
const portraitTimeout = 65 * time.Second
const portraitCenterY = 162

var portraitPoses = [...]string{"idle", "blink", "talk", "think"}
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

type pendingPortrait struct {
	id, base, form string
}

func parsePortraitRef(raw string) (id, pose string, ok bool) {
	ref, found := strings.CutPrefix(raw, "portrait:")
	if !found {
		return "", "", false
	}
	id, pose, found = strings.Cut(ref, ":")
	if !found || !portraitIDPattern.MatchString(id) || !slices.Contains(portraitPoses[:], pose) {
		return "", "", false
	}
	return id, pose, true
}

type portraitGenerator func(ctx context.Context, prompt string, source []byte) ([]byte, error)

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

func decodePortraitSheet(raw []byte) (image.Image, error) {
	if len(raw) > portraitMaxBytes {
		return nil, errors.New("portrait sheet exceeds 8 MiB")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || (format != "png" && format != "jpeg") || cfg.Width != portraitSheetSide || cfg.Height != portraitSheetSide {
		return nil, errors.New("expected a 1024×1024 PNG or JPEG portrait sheet")
	}
	sheet, _, err := image.Decode(bytes.NewReader(raw))
	return sheet, err
}

// Use the median of the four inset corner patches, not a single noisy pixel.
func portraitCorner(img *image.RGBA, r image.Rectangle) imagecolor.RGBA {
	var channels [3][]uint8
	for _, x := range []int{r.Min.X + 8, r.Max.X - 13} {
		for _, y := range []int{r.Min.Y + 8, r.Max.Y - 13} {
			for j := range 5 {
				for i := range 5 {
					c := img.RGBAAt(x+i, y+j)
					channels[0] = append(channels[0], c.R)
					channels[1] = append(channels[1], c.G)
					channels[2] = append(channels[2], c.B)
				}
			}
		}
	}
	for _, values := range channels {
		slices.Sort(values)
	}
	return imagecolor.RGBA{channels[0][50], channels[1][50], channels[2][50], 255}
}

func portraitDiff(a, b imagecolor.RGBA) int {
	sq := func(n int) int { return n * n }
	return sq(int(a.R)-int(b.R)) + sq(int(a.G)-int(b.G)) + sq(int(a.B)-int(b.B))
}

// Use the dominant connected silhouette for the bounds, ignoring small islands
// of compression or stray illustration at the edges.
func portraitBounds(img *image.RGBA, r image.Rectangle, bg imagecolor.RGBA) image.Rectangle {
	w, h := r.Dx(), r.Dy()
	mask := make([]bool, w*h)
	for y := range h {
		for x := range w {
			mask[y*w+x] = portraitDiff(img.RGBAAt(r.Min.X+x, r.Min.Y+y), bg) > 25*25
		}
	}
	var best image.Rectangle
	var queue []int
	largest := 0
	for i := range mask {
		if !mask[i] {
			continue
		}
		mask[i] = false
		queue = append(queue[:0], i)
		bounds := image.Rectangle{Min: image.Pt(w, h)}
		for head := 0; head < len(queue); head++ {
			pos := queue[head]
			x, y := pos%w, pos/w
			bounds.Min = image.Pt(min(bounds.Min.X, x), min(bounds.Min.Y, y))
			bounds.Max = image.Pt(max(bounds.Max.X, x+1), max(bounds.Max.Y, y+1))
			push := func(p int) {
				if mask[p] {
					mask[p] = false
					queue = append(queue, p)
				}
			}
			if x > 0 {
				push(pos - 1)
			}
			if x+1 < w {
				push(pos + 1)
			}
			if y > 0 {
				push(pos - w)
			}
			if y+1 < h {
				push(pos + w)
			}
		}
		if len(queue) > largest {
			best, largest = bounds.Add(r.Min), len(queue)
		}
	}
	if largest < 1024 || best.Dx() < 70 || best.Dy() < 70 {
		return image.Rectangle{}
	}
	return best
}

func slicePortrait(sheet image.Image, bg imagecolor.RGBA) ([4]*image.RGBA, error) {
	var poses [4]*image.RGBA
	if sheet.Bounds().Size() != image.Pt(portraitSheetSide, portraitSheetSide) {
		return poses, errors.New("portrait sheet must be 1024×1024")
	}
	img := image.NewRGBA(image.Rect(0, 0, portraitSheetSide, portraitSheetSide))
	draw.Draw(img, img.Bounds(), image.NewUniform(bg), image.Point{}, draw.Src)
	draw.Draw(img, img.Bounds(), sheet, sheet.Bounds().Min, draw.Over)
	const side = portraitSheetSide / 2
	var crops [4]image.Rectangle
	var backgrounds [4]imagecolor.RGBA
	scale := 1.0
	for i, pose := range portraitPoses {
		cell := image.Rect(0, 0, side, side).Add(image.Pt(i%2*side, i/2*side))
		backgrounds[i] = portraitCorner(img, cell)
		inner := cell.Inset(10)
		b := portraitBounds(img, inner, backgrounds[i])
		if b.Empty() || b.Min.X == inner.Min.X || b.Min.Y == inner.Min.Y || b.Max.X == inner.Max.X || b.Max.Y == inner.Max.Y {
			return poses, fmt.Errorf("portrait %s needs one silhouette with a clear margin", pose)
		}
		b = b.Inset(-3).Intersect(cell)
		crops[i] = b
		// Fit the lower crop corners in the bezel, reserving space for raster
		// rounding and pixel blocks. Cap the height at y=42..282 above Menu.
		w, h := float64(b.Dx()), float64(b.Dy())
		const offset, radius = screenSize/2 - portraitCenterY, 168
		cornerScale := 2 * (math.Sqrt(w*w*(radius*radius-offset*offset)+h*h*radius*radius) - offset*h) / (w*w + h*h)
		scale = min(scale, cornerScale, 240/h)
	}
	for i, crop := range crops {
		src := img.SubImage(crop).(*image.RGBA)
		for y := crop.Min.Y; y < crop.Max.Y; y++ {
			for x := crop.Min.X; x < crop.Max.X; x++ {
				c := src.RGBAAt(x, y)
				// Feather the backdrop normalization around antialiased edges.
				weight := min(1, max(0, (math.Sqrt(float64(portraitDiff(c, backgrounds[i])))-8)/20))
				channel := func(v, base uint8) uint8 { return uint8(math.Round(weight*float64(v) + (1-weight)*float64(base))) }
				src.SetRGBA(x, y, imagecolor.RGBA{channel(c.R, bg.R), channel(c.G, bg.G), channel(c.B, bg.B), 255})
			}
		}
		out := image.NewRGBA(image.Rect(0, 0, screenSize, screenSize))
		draw.Draw(out, out.Bounds(), image.NewUniform(bg), image.Point{}, draw.Src)
		w, h := int(math.Round(float64(crop.Dx())*scale)), int(math.Round(float64(crop.Dy())*scale))
		x, y := (screenSize-w)/2, portraitCenterY-h/2
		xdraw.CatmullRom.Scale(out, image.Rect(x, y, x+w, y+h), src, crop, draw.Src, nil)
		poses[i] = out
	}
	return poses, nil
}

func writePortraitPNG(dir, name string, img image.Image) error {
	f, err := os.OpenFile(filepath.Join(dir, name+".png"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	return errors.Join(png.Encode(f, img), f.Close())
}

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
	norm, err := normalizeBackground(img, pixelSpecBox3.logicalW, pixelSpecBox3.logicalH)
	if err != nil {
		return err
	}
	return saveBackgroundPNG(filepath.Join(p.dir, id), norm)
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
	poses, err := slicePortrait(sheet, bg)
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
	id := randomHex(16)
	dir := filepath.Join(p.dir, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", problem(502, "Cannot create portrait")
	}
	if err := func() error {
		if err := writePortraitPNG(dir, "sheet", sheet); err != nil {
			return err
		}
		for i, pose := range portraitPoses {
			if err := writePortraitPNG(dir, pose, poses[i]); err != nil {
				return err
			}
		}
		if normPoses, err := normalizePortraitSheet(sheet, pixelPortraitHeight); err == nil {
			_ = saveNormalizedPoses(dir, normPoses)
		}
		return ctx.Err()
	}(); err != nil {
		p.discard(id)
		return "", problem(502, "Cannot save portrait")
	}
	if spec.Form != "" && !spec.Edit {
		_ = p.generateBackground(ctx, id, spec.Form, spec.Prompt)
	} else if spec.Edit && currentID != "" {
		if bg, err := loadBackgroundPNG(filepath.Join(p.dir, currentID)); err == nil {
			_ = saveBackgroundPNG(dir, bg)
		}
	}
	return id, nil
}

func (p *portraitStore) discard(id string) {
	if p == nil || !portraitIDPattern.MatchString(id) {
		return
	}
	dir := filepath.Join(p.dir, id)
	for _, pose := range portraitPoses {
		_ = os.Remove(filepath.Join(dir, pose+".png"))
		_ = os.Remove(filepath.Join(dir, pixelPoseFilename(pose)))
	}
	_ = os.Remove(filepath.Join(dir, "bg.png"))
	_ = os.Remove(filepath.Join(dir, "sheet.png"))
	_ = os.Remove(dir)
}

func (p *portraitStore) read(id, pose string) ([]byte, error) {
	if p == nil || !portraitIDPattern.MatchString(id) || pose != "sheet" && !slices.Contains(portraitPoses[:], pose) {
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

func (b *larkServer) preparePortrait(ctx context.Context, companionID string, args object, raw any) (*pendingPortrait, error) {
	current, ok := b.companionByID(companionID)
	if !ok {
		return nil, problem(404, "Companion not found")
	}
	if _, err := updateCompanion(current, args, b.voices); err != nil {
		return nil, err
	}
	return b.portraits.prepare(ctx, current, raw)
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

func (p *portraitStore) prepare(ctx context.Context, current companionState, raw any) (*pendingPortrait, error) {
	spec, err := readPortraitSpec(raw)
	if err != nil {
		return nil, err
	}
	if spec.Prompt = strings.TrimSpace(spec.Prompt); spec.Prompt == "" {
		return nil, fieldError("companion.portrait.prompt", "describe the character or edit")
	}
	if spec.Form = strings.TrimSpace(spec.Form); spec.Form == "" {
		spec.Form = current.Form
	}
	spec.Style = portraitPixelStyle
	spec.BG = "#FAF0E6"
	id, err := p.create(ctx, spec, current.PortraitID)
	if err != nil {
		return nil, err
	}
	return &pendingPortrait{id: id, base: current.PortraitID, form: spec.Form}, nil
}

func (b *larkServer) servePortrait(w http.ResponseWriter, r *http.Request, path, scope string) error {
	id, pose, found := strings.Cut(strings.TrimPrefix(path, "/api/portraits/"), "/")
	pose, pngFile := strings.CutSuffix(pose, ".png")
	if !found || !pngFile || !portraitIDPattern.MatchString(id) || !slices.Contains(portraitPoses[:], pose) {
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
