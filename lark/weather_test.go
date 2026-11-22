package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestMapWMOCode(t *testing.T) {
	cases := []struct {
		code int
		want weatherKind
	}{
		{0, weatherClear},
		{1, weatherClear},
		{2, weatherCloudy},
		{3, weatherCloudy},
		{45, weatherFog},
		{48, weatherFog},
		{51, weatherRain},
		{61, weatherRain},
		{80, weatherRain},
		{71, weatherSnow},
		{86, weatherSnow},
		{95, weatherStorm},
		{99, weatherStorm},
	}
	for _, tc := range cases {
		if got := mapWMOCode(tc.code); got != tc.want {
			t.Errorf("mapWMOCode(%d) = %v, want %v", tc.code, got, tc.want)
		}
	}
}

func TestWeatherServiceGeocodeCacheAndPoll(t *testing.T) {
	var geoHits atomic.Int32
	var weatherCode atomic.Int32
	weatherCode.Store(61) // rain

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/geocode":
			geoHits.Add(1)
			_, _ = fmt.Fprint(w, `{"results":[{"latitude":-31.9505,"longitude":115.8605}]}`)
		case "/forecast":
			_, _ = fmt.Fprintf(w, `{"current":{"weather_code":%d}}`, weatherCode.Load())
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	db, err := openLarkDB(t.Context(), filepath.Join(t.TempDir(), "lark.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ws := newWeatherService("Perth, Australia", db)
	ws.geocodeURL = srv.URL + "/geocode"
	ws.forecastURL = srv.URL + "/forecast"

	if err := ws.pollOnce(t.Context()); err != nil {
		t.Fatalf("first pollOnce: %v", err)
	}
	if ws.kind() != weatherRain {
		t.Fatalf("kind = %v, want rain", ws.kind())
	}
	if geoHits.Load() != 1 {
		t.Fatalf("geocode hits = %d, want 1", geoHits.Load())
	}

	// Second service instance with the same DB should use cached lat/lon in meta without re-geocoding.
	weatherCode.Store(95) // storm
	ws2 := newWeatherService("Perth, Australia", db)
	ws2.geocodeURL = srv.URL + "/geocode"
	ws2.forecastURL = srv.URL + "/forecast"
	if err := ws2.pollOnce(t.Context()); err != nil {
		t.Fatalf("second pollOnce: %v", err)
	}
	if ws2.kind() != weatherStorm {
		t.Fatalf("kind = %v, want storm", ws2.kind())
	}
	if geoHits.Load() != 1 {
		t.Fatalf("expected cached geocode in meta, got %d hits", geoHits.Load())
	}
}

func TestWeatherEffectsModifyCanvas(t *testing.T) {
	for _, kind := range []weatherKind{weatherCloudy, weatherRain, weatherStorm, weatherFog, weatherSnow} {
		base := newPixelCanvas(120, 120)
		drawDefaultPixelBackground(base)
		wx := newPixelCanvas(120, 120)
		drawDefaultPixelBackground(wx)
		applyWeatherEffects(wx, kind)
		diff := 0
		for i := 0; i < len(base.img.Pix); i += 4 {
			if base.img.Pix[i] != wx.img.Pix[i] || base.img.Pix[i+1] != wx.img.Pix[i+1] || base.img.Pix[i+2] != wx.img.Pix[i+2] {
				diff++
			}
		}
		if diff == 0 {
			t.Fatalf("weather %v made no visual change", kind)
		}
	}
}
