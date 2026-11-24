package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"image"
	imagecolor "image/color"
	"image/png"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"golang.org/x/image/draw"
)

// Oat's stylesheet is pinned on jsDelivr; everything else stays same-origin.
const remoteCSP = "default-src 'none'; script-src 'self'; style-src 'self' https://cdn.jsdelivr.net; img-src 'self' data:; media-src 'self'; connect-src 'self'; manifest-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

//go:embed web/remote/*.html web/remote/*.css web/remote/*.js
//go:embed web/remote/*.html web/remote/*.css web/remote/*.js
var remoteAssets embed.FS

func (b *larkServer) serveRemoteAsset(w http.ResponseWriter, path string) {
	name := strings.TrimPrefix(path, remotePath+"/")
	switch name {
	case "page.css", "app.js":
	default:
		sendError(w, problem(404, "Not found"))
		return
	}
	data, err := remoteAssets.ReadFile("web/remote/" + name)
	if err != nil {
		sendError(w, problem(404, "Not found"))
		return
	}
	kind := "application/octet-stream"
	switch filepath.Ext(name) {
	case ".css":
		kind = "text/css; charset=utf-8"
	case ".js":
		kind = "text/javascript; charset=utf-8"
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", remoteCSP)
	sendBytes(w, 200, kind, data)
}

func (b *larkServer) serveRemotePage(w http.ResponseWriter, r *http.Request, tok remoteToken, raw string) {
	index, err := remoteAssets.ReadFile("web/remote/index.html")
	if err != nil {
		sendError(w, problem(500, "Remote page unavailable"))
		return
	}
	query := url.QueryEscape(raw)
	page := strings.ReplaceAll(string(index), "{{TOKEN}}", query)
	page = strings.ReplaceAll(page, "{{TOKEN_HTML}}", htmlEscape(raw))
	page = strings.ReplaceAll(page, "{{NAME}}", htmlEscape(remoteDisplayName(b.companionFor(tok.CompanionID))))
	// The scoped page is normally reached through the opentunnel HTTPS route;
	// always mark the cookie Secure so it is never sent over cleartext from a
	// LAN client. Loopback HTTP is local to this host.
	http.SetCookie(w, &http.Cookie{Name: remoteCookieName, Value: raw, Path: remotePath,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", remoteCSP)
	w.WriteHeader(200)
	_, _ = w.Write([]byte(page))
}

func htmlEscape(s string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;")
	return replacer.Replace(s)
}

func remoteDisplayName(c companionState) string {
	name := strings.TrimSpace(c.Name)
	if name == "" {
		name = strings.TrimSpace(c.ID)
	}
	if name == "" {
		return "Lark"
	}
	return name
}

func (b *larkServer) serveRemotePWA(w http.ResponseWriter, r *http.Request, path string, tok remoteToken, raw string) {
	c := b.companionFor(tok.CompanionID)
	name := remoteDisplayName(c)
	switch path {
	case remotePath + "/pwa/manifest.webmanifest":
		icons := []object{}
		for _, icon := range []struct {
			name string
			size int
			use  string
		}{
			{"icon-192.png", 192, "any"},
			{"icon-512.png", 512, "any"},
			{"maskable-512.png", 512, "maskable"},
		} {
			icons = append(icons, object{
				"src":     remotePath + "/pwa/" + icon.name,
				"type":    "image/png",
				"sizes":   fmt.Sprintf("%dx%d", icon.size, icon.size),
				"purpose": icon.use,
			})
		}
		// The installed app starts without a query token and relies on the
		// lark_remote cookie set when the page was first opened.
		data, _ := json.Marshal(object{
			"id":               remotePath + "?companion=" + url.QueryEscape(c.ID),
			"start_url":        remotePath,
			"scope":            remotePath,
			"name":             name,
			"short_name":       name,
			"display":          "standalone",
			"orientation":      "portrait",
			"theme_color":      "#6f5bd3",
			"background_color": "#faf7f2",
			"icons":            icons,
		})
		w.Header().Set("X-Content-Type-Options", "nosniff")
		sendBytes(w, 200, "application/manifest+json", data)
		return
	case remotePath + "/pwa/icon-192.png", remotePath + "/pwa/icon-512.png", remotePath + "/pwa/maskable-512.png":
		size := 512
		if strings.Contains(path, "192") {
			size = 192
		}
		icon, err := b.remoteIcon(c, size, strings.Contains(path, "maskable"))
		if err != nil {
			sendError(w, problem(502, "Cannot render remote icon"))
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		sendBytes(w, 200, "image/png", icon)
		return
	}
	sendError(w, problem(404, "Not found"))
}

func (b *larkServer) remoteIcon(c companionState, size int, maskable bool) ([]byte, error) {
	raw, err := companionAvatarPNG(b, c)
	if err != nil {
		return nil, err
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
	if maskable {
		bg := imagecolor.NRGBA{R: 250, G: 247, B: 242, A: 255}
		draw.Draw(dst, dst.Bounds(), &image.Uniform{C: bg}, image.Point{}, draw.Src)
		pad := size / 10
		draw.CatmullRom.Scale(dst, image.Rect(pad, pad, size-pad, size-pad), src, src.Bounds(), draw.Over, nil)
	} else {
		draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	}
	var out bytes.Buffer
	if err := png.Encode(&out, dst); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
