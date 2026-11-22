package main

import (
	"context"
	"encoding/json"
	"fmt"
	imagecolor "image/color"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type weatherKind uint8

const (
	weatherClear weatherKind = iota
	weatherCloudy
	weatherRain
	weatherStorm
	weatherFog
	weatherSnow
)

func (w weatherKind) String() string {
	switch w {
	case weatherCloudy:
		return "cloudy"
	case weatherRain:
		return "rain"
	case weatherStorm:
		return "storm"
	case weatherFog:
		return "fog"
	case weatherSnow:
		return "snow"
	default:
		return "clear"
	}
}

func mapWMOCode(code int) weatherKind {
	switch {
	case code == 2 || code == 3:
		return weatherCloudy
	case code == 45 || code == 48:
		return weatherFog
	case (code >= 51 && code <= 67) || (code >= 80 && code <= 82):
		return weatherRain
	case (code >= 71 && code <= 77) || code == 85 || code == 86:
		return weatherSnow
	case code >= 95 && code <= 99:
		return weatherStorm
	default:
		return weatherClear
	}
}

const (
	openMeteoGeocodeURL  = "https://geocoding-api.open-meteo.com/v1/search"
	openMeteoForecastURL = "https://api.open-meteo.com/v1/forecast"
	weatherPollInterval  = 15 * time.Minute
)

type weatherService struct {
	mu          sync.RWMutex
	location    string
	lat, lon    float64
	hasCoords   bool
	current     weatherKind
	lastCode    int
	updatedAt   time.Time
	geocodeURL  string
	forecastURL string
	client      *http.Client
	db          *larkDB
}

func newWeatherService(location string, db *larkDB) *weatherService {
	return &weatherService{
		location:    strings.TrimSpace(location),
		geocodeURL:  openMeteoGeocodeURL,
		forecastURL: openMeteoForecastURL,
		client:      &http.Client{Timeout: 10 * time.Second},
		db:          db,
	}
}

func (w *weatherService) kind() weatherKind {
	if w == nil {
		return weatherClear
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.current
}

func (w *weatherService) setKind(k weatherKind) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.current = k
	w.mu.Unlock()
}

func (w *weatherService) ensureCoords(ctx context.Context) (float64, float64, error) {
	w.mu.RLock()
	if w.hasCoords {
		lat, lon := w.lat, w.lon
		w.mu.RUnlock()
		return lat, lon, nil
	}
	loc := w.location
	w.mu.RUnlock()
	if loc == "" {
		return 0, 0, fmt.Errorf("no weather location configured")
	}
	if w.db != nil {
		cachedLoc, _ := w.db.getMeta(ctx, "weather_location")
		cachedLat, _ := w.db.getMeta(ctx, "weather_lat")
		cachedLon, _ := w.db.getMeta(ctx, "weather_lon")
		if cachedLoc == loc && cachedLat != "" && cachedLon != "" {
			if lat, err1 := strconv.ParseFloat(cachedLat, 64); err1 == nil {
				if lon, err2 := strconv.ParseFloat(cachedLon, 64); err2 == nil {
					w.mu.Lock()
					w.lat, w.lon, w.hasCoords = lat, lon, true
					w.mu.Unlock()
					return lat, lon, nil
				}
			}
		}
	}
	u := fmt.Sprintf("%s?name=%s&count=1&format=json", w.geocodeURL, url.QueryEscape(loc))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, 0, err
	}
	res, err := w.client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("geocode HTTP %d", res.StatusCode)
	}
	var out struct {
		Results []struct {
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
		} `json:"results"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return 0, 0, err
	}
	if len(out.Results) == 0 {
		return 0, 0, fmt.Errorf("location %q not found", loc)
	}
	lat, lon := out.Results[0].Latitude, out.Results[0].Longitude
	w.mu.Lock()
	w.lat, w.lon, w.hasCoords = lat, lon, true
	w.mu.Unlock()
	if w.db != nil {
		_ = w.db.setMeta(ctx, "weather_location", loc)
		_ = w.db.setMeta(ctx, "weather_lat", strconv.FormatFloat(lat, 'f', 6, 64))
		_ = w.db.setMeta(ctx, "weather_lon", strconv.FormatFloat(lon, 'f', 6, 64))
	}
	return lat, lon, nil
}

func (w *weatherService) pollOnce(ctx context.Context) error {
	if w == nil || w.location == "" {
		return nil
	}
	lat, lon, err := w.ensureCoords(ctx)
	if err != nil {
		return err
	}
	u := fmt.Sprintf("%s?latitude=%.4f&longitude=%.4f&current=weather_code", w.forecastURL, lat, lon)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	res, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("forecast HTTP %d", res.StatusCode)
	}
	var out struct {
		Current struct {
			WeatherCode int `json:"weather_code"`
		} `json:"current"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return err
	}
	w.mu.Lock()
	w.lastCode = out.Current.WeatherCode
	w.current = mapWMOCode(out.Current.WeatherCode)
	w.updatedAt = time.Now()
	w.mu.Unlock()
	return nil
}

func (w *weatherService) run(ctx context.Context) {
	if w == nil || w.location == "" {
		return
	}
	_ = w.pollOnce(ctx)
	ticker := time.NewTicker(weatherPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = w.pollOnce(ctx)
		}
	}
}

var pxCloudSprite = []string{
	"...cccccc...",
	".ccwwwwwwcc.",
	"cwwwwwwwwwwc",
	"cwwwwwwwwwwc",
	".dddddddddd.",
}

var pxCloudPalette = map[string]imagecolor.RGBA{
	".": {},
	"w": {238, 242, 248, 220},
	"c": {210, 218, 228, 210},
	"d": {170, 182, 198, 210},
}

// applyWeatherEffects applies a cheap weather grade and particle/sprite overlay
// to the world/companion logical canvas. Never applied to UI panels or Pair QR.
func applyWeatherEffects(c *pixelCanvas, kind weatherKind, tick int) {
	if kind == weatherClear || c == nil {
		return
	}
	pix := c.img.Pix
	switch kind {
	case weatherCloudy:
		// Desaturating grade (35% toward luminance).
		for i := 0; i < len(pix); i += 4 {
			r, g, b := int(pix[i]), int(pix[i+1]), int(pix[i+2])
			lum := (r*54 + g*183 + b*18) >> 8
			pix[i] = uint8((r*65 + lum*35) / 100)
			pix[i+1] = uint8((g*65 + lum*35) / 100)
			pix[i+2] = uint8((b*65 + lum*35) / 100)
		}
		// Drifting cloud sprites near the top.
		for i := range 2 {
			cx := ((tick*2 + i*64) % (c.w + 24)) - 12
			cy := 8 + i*10
			c.blitSprite(cx, cy, pxCloudSprite, pxCloudPalette, 1)
		}
	case weatherRain, weatherStorm:
		// Cool blue-slate grade.
		dark := 82
		if kind == weatherStorm {
			dark = 70
		}
		for i := 0; i < len(pix); i += 4 {
			r, g, b := int(pix[i]), int(pix[i+1]), int(pix[i+2])
			pix[i] = uint8((r * (dark - 12)) / 100)
			pix[i+1] = uint8((g * (dark - 4)) / 100)
			pix[i+2] = uint8(min(255, (b*(dark+10))/100+6))
		}
		// Storm flash on occasional ticks.
		if kind == weatherStorm && tick%23 == 0 {
			for i := 0; i < len(pix); i += 4 {
				pix[i] = uint8(min(255, int(pix[i])+55))
				pix[i+1] = uint8(min(255, int(pix[i+1])+58))
				pix[i+2] = uint8(min(255, int(pix[i+2])+70))
			}
		}
		dropCol := imagecolor.RGBA{185, 215, 245, 200}
		count := 24
		if kind == weatherStorm {
			count = 36
		}
		for i := range count {
			rx := (i*29 + tick*3) % c.w
			ry := (i*19 + tick*7) % c.h
			c.set(rx, ry, dropCol)
			c.set(rx-1, ry+1, dropCol)
			c.set(rx-1, ry+2, dropCol)
		}
	case weatherFog:
		// Light pale haze.
		for i := 0; i < len(pix); i += 4 {
			pix[i] = uint8((int(pix[i])*78 + 215*22) / 100)
			pix[i+1] = uint8((int(pix[i+1])*78 + 222*22) / 100)
			pix[i+2] = uint8((int(pix[i+2])*78 + 232*22) / 100)
		}
	case weatherSnow:
		// Soft cool winter tint + falling snow particles.
		for i := 0; i < len(pix); i += 4 {
			pix[i] = uint8((int(pix[i])*90 + 210*10) / 100)
			pix[i+1] = uint8((int(pix[i+1])*92 + 220*8) / 100)
			pix[i+2] = uint8((int(pix[i+2])*94 + 240*6) / 100)
		}
		flake := imagecolor.RGBA{245, 250, 255, 220}
		for i := range 22 {
			sx := (i*31 + tick + (i%3)*(tick/2)) % c.w
			sy := (i*17 + tick*3) % c.h
			c.set(sx, sy, flake)
			if i%3 == 0 {
				c.set(sx+1, sy, flake)
			}
		}
	}
}
