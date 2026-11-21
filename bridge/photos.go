package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"image"
	_ "image/jpeg"
	"image/png"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	maxPhotoUpload = 2 << 20
	maxPhotos      = 24
)

var photoID = regexp.MustCompile(`^photo-[0-9a-f]{8}$`)

func photoPath(path string) (string, bool) {
	id, ok := strings.CutPrefix(path, "/api/photos/")
	return id, ok && photoID.MatchString(id)
}

func photoType(media string) bool {
	return media == "image/png" || media == "image/jpeg" || media == "image/webp"
}

func (b *bridge) photoState(ctx context.Context, id string) ([]object, error) {
	out := []object{}
	if b.db == nil {
		return out, nil
	}
	id = strings.ToLower(id)
	rows, err := b.db.QueryContext(ctx, `SELECT id,device_id,mime,caption,created_at FROM photos
		WHERE (?='' OR device_id=?) ORDER BY created_at DESC,id DESC LIMIT ?`, id, id, maxPhotos)
	readErr := problem(502, "Cannot list photos")
	if err != nil {
		return nil, readErr
	}
	defer rows.Close()
	for rows.Next() {
		var photo, device, media, caption string
		var stamp int64
		if err := rows.Scan(&photo, &device, &media, &caption, &stamp); err != nil {
			return nil, readErr
		}
		out = append(out, object{"id": photo, "device_id": device, "mime": media, "caption": caption, "created_at": time.UnixMilli(stamp).UTC()})
	}
	if rows.Err() != nil {
		return nil, readErr
	}
	return out, nil
}

func readPhoto(w http.ResponseWriter, r *http.Request) ([]byte, string, error) {
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	caption := r.URL.Query().Get("caption")
	var data []byte
	var err error
	if media == "multipart/form-data" {
		r.Body = http.MaxBytesReader(w, r.Body, maxPhotoUpload+(16<<10))
		var fileMedia string
		reader, err := r.MultipartReader()
		if err != nil {
			return nil, "", problem(400, "Invalid multipart photo")
		}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, "", photoReadError(err)
			}
			switch {
			case part.FormName() == "caption" && part.FileName() == "":
				text, err := io.ReadAll(io.LimitReader(part, 2049))
				if err != nil {
					return nil, "", photoReadError(err)
				}
				if len(text) > 2048 {
					return nil, "", problem(400, "Photo caption too long")
				}
				if value := strings.TrimSpace(string(text)); value != "" {
					caption = value
				}
			case part.FileName() != "" && data == nil:
				fileMedia, _, _ = mime.ParseMediaType(part.Header.Get("Content-Type"))
				data, err = io.ReadAll(io.LimitReader(part, maxPhotoUpload+1))
				if err != nil {
					return nil, "", photoReadError(err)
				}
				if len(data) > maxPhotoUpload {
					return nil, "", problem(413, "Photo exceeds 2 MiB")
				}
			default:
				return nil, "", problem(400, "Expected one photo and optional caption")
			}
		}
		media = fileMedia
	} else if photoType(media) {
		data, err = io.ReadAll(http.MaxBytesReader(w, r.Body, maxPhotoUpload))
		if err != nil {
			return nil, "", photoReadError(err)
		}
	}
	if !photoType(media) {
		return nil, "", problem(415, "Expected PNG, JPEG or WebP")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || media != "image/"+format || cfg.Width <= 0 || cfg.Height <= 0 ||
		cfg.Width > 8192 || cfg.Height > 8192 || int64(cfg.Width)*int64(cfg.Height) > 16_000_000 {
		return nil, "", problem(400, "Invalid or oversized photo")
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", problem(400, "Could not decode photo")
	}
	bounds := img.Bounds()
	side := min(bounds.Dx(), bounds.Dy())
	x, y := bounds.Min.X+(bounds.Dx()-side)/2, bounds.Min.Y+(bounds.Dy()-side)/2
	size := min(side, screenSize)
	square := image.NewNRGBA(image.Rect(0, 0, size, size))
	draw.BiLinear.Scale(square, square.Bounds(), img, image.Rect(x, y, x+side, y+side), draw.Src, nil)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, square); err != nil {
		return nil, "", err
	}
	return encoded.Bytes(), shortText(caption, 256, true), nil
}

func photoReadError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return problem(413, "Photo exceeds 2 MiB")
	}
	return problem(400, "Could not read photo")
}

func (b *bridge) postPhoto(w http.ResponseWriter, r *http.Request, id string) (any, error) {
	who, err := b.lookupDevice(id)
	if err != nil {
		return nil, err
	}
	data, caption, err := readPhoto(w, r)
	if err != nil {
		return nil, err
	}
	entry := newChat("card", "You", caption, "photo")
	if entry.Text == "" {
		entry.Text = "Photo"
	}
	entry.ID, entry.SceneTitle = "photo-"+randomHex(4), "Photo"
	ctx := r.Context()
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO photos VALUES (?,?,'image/png',?,?,?)`,
		entry.ID, strings.ToLower(who.DeviceID), data, caption, entry.Timestamp.UnixMilli()); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM photos WHERE id NOT IN
		(SELECT id FROM photos ORDER BY created_at DESC, rowid DESC LIMIT ?)`, maxPhotos); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, insertMessage, messageValues(who.DeviceID, entry)...); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	b.chat(who.DeviceID, entry, false)
	displayed := b.ws != nil && b.ws.showPhoto(who.DeviceID, entry.ID)
	return object{"id": entry.ID, "caption": caption, "displayed": displayed}, nil
}

func (b *bridge) getPhoto(w http.ResponseWriter, r *http.Request, id, scope string) error {
	var device, media string
	var data []byte
	err := b.db.QueryRowContext(r.Context(), `SELECT device_id,mime,data FROM photos WHERE id=?`, id).Scan(&device, &media, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return problem(404, "Photo not found")
	}
	if err != nil {
		return err
	}
	if scope != "" && device != scope {
		return problem(403, "Photo out of scope")
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	sendBytes(w, 200, media, data)
	return nil
}

func (b *bridge) photoImage(id string) (image.Image, error) {
	if b.db == nil {
		return nil, nil
	}
	var data []byte
	err := b.db.QueryRowContext(b.root, `SELECT data FROM photos WHERE id=?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // Recent-photo retention may expire saved references.
	}
	if err != nil {
		return nil, err
	}
	return png.Decode(bytes.NewReader(data))
}

func (h *wsHub) showPhoto(device, id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.devices[strings.ToLower(device)]
	now := time.Now()
	if d == nil || !h.active(d) || h.parentRefresh(d.key, now) != "" {
		return false
	}
	h.interruptTurn(d, "photo")
	h.stopCompanionTurn(d)
	h.clearView(d)
	h.wake(d, now)
	s := sceneView{bg: "#000000", title: "Photo", art: `<image href="photo:` + id + `" width="360" height="360"/>`}
	s.control(bounds{139, 300, 82, 32}, actionTile("companion", "Home", "", palette{"#FFFAEE", companionInk}))
	d.view = &s
	h.render(d, now, true)
	return true
}

func (b *bridge) buildScreen(raw string, exit bool, geometry screenGeometry) (svgScreen, error) {
	if !strings.Contains(raw, "photo:") {
		return buildScreen(raw, exit, geometry)
	}
	// Photo references can expire; never keep their pixels in the shared SVG cache.
	start := time.Now()
	screen, err := buildScreenWithPhotos(raw, exit, geometry, b.photoImage)
	screen.render = time.Since(start)
	return screen, err
}
