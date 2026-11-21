package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image/png"
	"net/http"
	"net/url"
	"strings"
)

const phonePWAPath = "/app/pwa/"

func (b *bridge) phoneCookie() string {
	mac := hmac.New(sha256.New, []byte(b.token))
	mac.Write([]byte("lark phone cat install artwork"))
	return hex.EncodeToString(mac.Sum(nil))
}

func (b *bridge) setPhoneCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: "lark-pwa", Value: b.phoneCookie(), Path: phonePWAPath,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
}

var phoneWorkers = [2][]byte{phoneWorker(false), phoneWorker(true)}

func phoneWorker(scoped bool) []byte {
	paths := []string{"/app", "/app/style.css", "/app/lark.svg"}
	files, _ := webFiles.ReadDir("web")
	for _, file := range files {
		name := file.Name()
		if strings.HasSuffix(name, ".js") && !strings.HasSuffix(name, ".test.js") && name != "sw.js" && name != "parent.js" &&
			!(scoped && name == "photo-library.js") {
			paths = append(paths, "/app/"+name)
		}
	}
	worker, _ := webFiles.ReadFile("web/sw.js")
	hash := sha256.New()
	for _, path := range paths {
		name := strings.TrimPrefix(path, "/app/")
		if path == "/app" {
			name = "index.html"
		}
		data, _ := webFiles.ReadFile("web/" + name)
		fmt.Fprintf(hash, "%s\x00%x\n", path, sha256.Sum256(data))
	}
	hash.Write(worker)
	mode := "lan"
	if scoped {
		mode = "scoped"
	}
	shell, _ := json.Marshal(paths)
	return append(fmt.Appendf(nil, "const CACHE = \"lark-shell-%s-%x\", SHELL = %s;\n", mode, hash.Sum(nil), shell), worker...)
}

func (b *bridge) servePhonePWA(w http.ResponseWriter, r *http.Request, path string, scope *virtualClient, token string) bool {
	if path == "/app/sw.js" {
		worker := phoneWorkers[0]
		if scope != nil {
			worker = phoneWorkers[1]
		}
		w.Header().Set("Service-Worker-Allowed", "/app")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		sendBytes(w, 200, "text/javascript; charset=utf-8", worker)
		return true
	}
	const manifest = phonePWAPath + "manifest.webmanifest"
	if !strings.HasPrefix(path, phonePWAPath) {
		return false
	}
	icons := map[string]int{"icon-192.png": 192, "icon-512.png": 512, "maskable-512.png": 512}
	size := icons[path[len(phonePWAPath):]]
	if path != manifest && size == 0 {
		return false
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	query := r.URL.Query()
	id := query.Get("device_id")
	start := url.URL{Path: "/app"}
	iconQuery := url.Values{}
	var cat catState
	if scope != nil {
		if !tokenMatches(query["token"], token) {
			sendError(w, problem(401, "Unauthorized"))
			return true
		}
		if query.Has("device_id") {
			sendError(w, problem(403, "Device out of scope"))
			return true
		}
		id = scope.ID
		iconQuery.Set("token", token)
		// Installed Safari apps can start with separate website storage.
		// Use the original scoped share capability, never a LAN credential.
		start.RawQuery = iconQuery.Encode()
		cat = b.catFor(peer{DeviceID: id, Character: scope.Character})
	} else {
		cookie, err := r.Cookie("lark-pwa")
		if b.token == "" || err != nil || !tokenMatches([]string{cookie.Value}, b.phoneCookie()) {
			sendError(w, problem(401, "Unauthorized"))
			return true
		}
		if v, ok := b.virtualByID(id); ok {
			cat = b.catFor(peer{DeviceID: id, Character: v.Character})
		} else if phoneID.MatchString(id) {
			cat = b.catFor(peer{DeviceID: id})
		} else {
			sendError(w, problem(404, "Virtual companion not found"))
			return true
		}
		start.RawQuery = url.Values{"device_id": {id}}.Encode()
		iconQuery.Set("device_id", id)
	}
	if path == manifest {
		iconList := make([]object, 0, len(icons))
		for _, name := range []string{"icon-192.png", "icon-512.png", "maskable-512.png"} {
			purpose := "any"
			if strings.HasPrefix(name, "maskable") {
				purpose = "maskable"
			}
			iconList = append(iconList, object{"src": phonePWAPath + name + "?" + iconQuery.Encode(), "type": "image/png",
				"sizes": fmt.Sprintf("%dx%d", icons[name], icons[name]), "purpose": purpose})
		}
		name := cat.Name
		if !cat.Configured && name == "New Cat" {
			name = "Phone Lark"
		}
		data, _ := json.Marshal(object{
			"id": "/app?device_id=" + id, "start_url": start.String(), "scope": "/app",
			"name": name, "short_name": name, "display": "standalone", "orientation": "portrait",
			"theme_color": cat.MarkColor, "background_color": cat.FaceBG, "icons": iconList,
		})
		sendBytes(w, 200, "application/manifest+json", data)
		return true
	}
	svg := sceneView{bg: cat.FaceBG}.svg(catArt(cat, catPose{mood: "idle"}))
	scene, err := prepareScene(svg, false)
	if err == nil {
		side := size
		if strings.HasSuffix(path, "/maskable-512.png") {
			// The whole square of face artwork fits into the 40% safe radius.
			side = size * 56 / 100
		}
		pad := float64(size-side) / 2
		img, err := scene.raster(size, size, pad, pad, side, scene.background, nil)
		var buf bytes.Buffer
		if err == nil && png.Encode(&buf, img) == nil {
			sendBytes(w, 200, "image/png", buf.Bytes())
			return true
		}
	}
	sendError(w, problem(502, "Cannot render companion icon"))
	return true
}
