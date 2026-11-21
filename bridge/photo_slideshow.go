package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"slices"
	"strings"
	"time"
)

const slideRasterRef = "photo:photo-00000000"

type photoShow struct {
	handles []string
	index   int
	cancel  context.CancelFunc
}

type photoSlide struct {
	handle string
	ready  bool
}

func (h *wsHub) dropPhotoShow(d *wsDevice) {
	if d.photos != nil && d.photos.cancel != nil {
		d.photos.cancel()
	}
	d.photos = nil
}

func slideshowScene(s *photoShow, status string) sceneView {
	v := sceneView{bg: "#10121A", title: "Photos",
		library: &photoSlide{s.handles[s.index], status == ""}}
	if status == "" {
		v.art = `<image href="` + slideRasterRef + `" x="54" y="54" width="252" height="188"/>`
	} else {
		v.art = svgLabel(180, 164, 20, status, "#FFFAEE")
	}
	v.art += svgLabel(180, 260, 15, fmt.Sprintf("%d / %d", s.index+1, len(s.handles)), "#FFFAEE")
	palette := palette{"#FFFAEE", companionInk}
	if len(s.handles) > 1 {
		v.control(bounds{75, 274, 59, 39}, actionTile("photo-prev", "Prev", "", palette))
		v.control(bounds{226, 274, 59, 39}, actionTile("photo-next", "Next", "", palette))
	}
	v.control(bounds{140, 274, 80, 39}, actionTile("photo-exit", "Exit", "", palette))
	return v
}

func (b *bridge) launchPhotoShow(ctx context.Context, args object) (object, error) {
	device := strings.ToLower(str(args["devices"]))
	data, ok := args["data"].(object)
	if !ok || len(data) != 2 || len(args) != 3 || !deviceIDRE.MatchString(device) || broadcast(device) {
		return nil, fieldError("template", "slideshow uses only an exact devices ID and data")
	}
	p := b.library
	if !p.validateGrant(ctx, device, str(data["access"])) {
		return nil, problem(403, "Photo library unavailable for this turn")
	}
	handles, err := stringList(data["handles"], "data.handles")
	if err != nil || len(handles) < 1 || len(handles) > 8 {
		return nil, fieldError("data.handles", "expected 1–8 library handles")
	}
	if b.ws == nil {
		return nil, problem(503, "Device hub unavailable")
	}
	h := b.ws
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.devices[device]
	if d == nil || !h.active(d) || !d.client.canSeeLibrary() || d.callState != callNone {
		return nil, problem(409, "Photo slideshow needs a connected local companion not in a call")
	}
	for _, id := range handles {
		if _, ok := p.getPhotoHandle(ctx, device, id); !ok {
			return nil, problem(404, "Photo handle unavailable")
		}
	}
	h.clearView(d)
	d.agentSetScene = d.busy
	d.photos = &photoShow{handles: slices.Clone(handles)}
	now := time.Now()
	h.wake(d, now)
	h.showSlide(d, now)
	hits := []string{"photo-exit"}
	if len(handles) > 1 {
		hits = append(hits, "photo-prev", "photo-next")
	}
	return object{"ok": true, "title": "Photos", "count": len(handles), "hit_ids": hits}, nil
}

func (h *wsHub) showSlide(d *wsDevice, now time.Time) {
	s := d.photos
	if s.cancel != nil {
		s.cancel()
	}
	id := s.handles[s.index]
	p := h.b.library
	handle, ok := p.getPhotoHandle(d.client.ctx, d.key, id)
	if !ok {
		h.clearView(d)
		h.render(d, now, true)
		return
	}
	loading := slideshowScene(s, "Loading photo…")
	loading.duringTurn = true
	d.view = &loading
	h.render(d, now, true)
	call, cancel := context.WithTimeout(d.client.ctx, 2*time.Minute)
	s.cancel = cancel
	go func() {
		defer cancel()
		data, _ := p.photoStill(call, handle.photo)
		h.mu.Lock()
		defer h.mu.Unlock()
		if call.Err() != nil && errors.Is(call.Err(), context.Canceled) ||
			!h.active(d) || d.photos != s || d.view != &loading {
			return
		}
		if _, ok := p.getPhotoHandle(d.client.ctx, d.key, id); !ok {
			h.clearView(d)
		} else {
			status := ""
			if len(data) == 0 {
				status = "Photo unavailable"
			}
			v := slideshowScene(s, status)
			v.duringTurn = true
			d.view = &v
		}
		h.render(d, time.Now(), true)
	}()
}

func (h *wsHub) clearInvalidSlides() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, d := range h.devices {
		if d.photos == nil {
			continue
		}
		for _, handle := range d.photos.handles {
			if _, ok := h.b.library.getPhotoHandle(d.client.ctx, d.key, handle); !ok {
				h.clearView(d)
				h.render(d, time.Now(), true)
				break
			}
		}
	}
}

func (p *photoLibrary) slideScreen(ctx context.Context, device string, slide *photoSlide, raw string, geometry screenGeometry) (svgScreen, error) {
	handle, ok := p.getPhotoHandle(ctx, device, slide.handle)
	if !ok {
		return svgScreen{}, errPhotoRevoked
	}
	screen, err := buildScreenWithPhotos(raw, false, geometry, func(_ string) (image.Image, error) {
		data, err := p.derivative(ctx, handle.photo.photoKey)
		if err != nil || len(data) == 0 {
			return nil, err
		}
		return jpeg.Decode(bytes.NewReader(data))
	})
	if err == nil && slide.ready {
		screen.source = strings.Replace(screen.source, slideRasterRef, "photo:"+slide.handle, 1)
	}
	return screen, err
}

var errPhotoRevoked = errors.New("photo access revoked")

func (b *bridge) libraryThumbnail(w http.ResponseWriter, r *http.Request, id string) error {
	if !libraryLAN(r) {
		return problem(404, "Not found")
	}
	handle, ok := b.library.getPhotoHandle(r.Context(), "", id)
	if !ok {
		return problem(404, "Photo unavailable")
	}
	data, err := b.library.derivative(r.Context(), handle.photo.photoKey)
	if _, ok := b.library.getPhotoHandle(r.Context(), "", id); !ok || err != nil || len(data) == 0 {
		return problem(404, "Photo unavailable")
	}
	if face := r.URL.Query().Get("face"); face != "" {
		data, err = b.library.facePreview(r.Context(), handle.photo.photoKey, face, data)
		if err != nil {
			return problem(404, "Photo face unavailable")
		}
	}
	if _, ok := b.library.getPhotoHandle(r.Context(), "", id); !ok {
		return problem(404, "Photo unavailable")
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	sendBytes(w, 200, "image/jpeg", data)
	return nil
}
