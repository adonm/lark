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

const pwaPath = "/app/pwa/"

func (b *larkServer) pwaCookie() string {
	mac := hmac.New(sha256.New, []byte(b.token))
	mac.Write([]byte("lark companion install artwork"))
	return hex.EncodeToString(mac.Sum(nil))
}

func (b *larkServer) setPWACookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: "lark-pwa", Value: b.pwaCookie(), Path: pwaPath,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
}

var pwaWorkers = [2][]byte{pwaWorker(false), pwaWorker(true)}

func pwaWorker(scoped bool) []byte {
	paths := []string{"/app", "/app/style.css", "/app/lark.svg"}
	files, _ := webFiles.ReadDir("web")
	for _, file := range files {
		name := file.Name()
		if strings.HasSuffix(name, ".js") && !strings.HasSuffix(name, ".test.js") && name != "sw.js" && name != "parent.js" {
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

func (b *larkServer) servePWA(w http.ResponseWriter, r *http.Request, path string, scope *companionState, token string) bool {
	if path == "/app/sw.js" {
		worker := pwaWorkers[0]
		if scope != nil {
			worker = pwaWorkers[1]
		}
		w.Header().Set("Service-Worker-Allowed", "/app")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		sendBytes(w, 200, "text/javascript; charset=utf-8", worker)
		return true
	}
	const manifest = pwaPath + "manifest.webmanifest"
	if !strings.HasPrefix(path, pwaPath) {
		return false
	}
	icons := map[string]int{"icon-192.png": 192, "icon-512.png": 512, "maskable-512.png": 512}
	size := icons[path[len(pwaPath):]]
	if path != manifest && size == 0 {
		return false
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	query := r.URL.Query()
	id := strings.ToLower(query.Get("companion_id"))
	start := url.URL{Path: "/app"}
	iconQuery := url.Values{}
	var companion companionState
	if scope != nil {
		if !tokenMatches(query["token"], token) {
			sendError(w, problem(401, "Unauthorized"))
			return true
		}
		if query.Has("companion_id") {
			sendError(w, problem(403, "Companion out of scope"))
			return true
		}
		id = scope.ID
		iconQuery.Set("token", token)
		// Installed Safari apps can start with separate website storage.
		// Use the original scoped share capability, never a LAN credential.
		start.RawQuery = iconQuery.Encode()
		companion = b.companionFor(id)
	} else {
		cookie, err := r.Cookie("lark-pwa")
		if b.token == "" || err != nil || !tokenMatches([]string{cookie.Value}, b.pwaCookie()) {
			sendError(w, problem(401, "Unauthorized"))
			return true
		}
		var ok bool
		companion, ok = b.companionByID(id)
		if !ok {
			sendError(w, problem(404, "Companion not found"))
			return true
		}
		start.RawQuery = url.Values{"companion_id": {id}}.Encode()
		iconQuery.Set("companion_id", id)
	}
	if path == manifest {
		iconList := make([]object, 0, len(icons))
		for _, name := range []string{"icon-192.png", "icon-512.png", "maskable-512.png"} {
			purpose := "any"
			if strings.HasPrefix(name, "maskable") {
				purpose = "maskable"
			}
			iconList = append(iconList, object{"src": pwaPath + name + "?" + iconQuery.Encode(), "type": "image/png",
				"sizes": fmt.Sprintf("%dx%d", icons[name], icons[name]), "purpose": purpose})
		}
		name := companion.Name
		if !companion.Configured && name == "New Companion" {
			name = "Lark"
		}
		data, _ := json.Marshal(object{
			"id": "/app?companion_id=" + id, "start_url": start.String(), "scope": "/app",
			"name": name, "short_name": name, "display": "standalone", "orientation": "portrait",
			"theme_color": companion.MarkColor, "background_color": companion.FaceBG, "icons": iconList,
		})
		sendBytes(w, 200, "application/manifest+json", data)
		return true
	}
	svg := sceneView{bg: companion.FaceBG}.svg(companionArt(companion, companionPose{mood: "idle"}))
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
