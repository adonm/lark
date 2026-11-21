package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func writeFakeESPImage(t *testing.T, dir, name, version string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var buf [96]byte
	buf[0] = 0xe9
	copy(buf[32:36], []byte{0x32, 0x54, 0xcd, 0xab})
	copy(buf[48:80], version)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf[:], 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestESPImageVersion(t *testing.T) {
	dir := t.TempDir()
	path := writeFakeESPImage(t, dir, "lark.bin", "0.10.2")
	ver, size, err := espImageVersion(path)
	if err != nil || ver != "0.10.2" || size != 96 {
		t.Fatalf("got (%q, %d, %v)", ver, size, err)
	}

	bad := filepath.Join(dir, "bad.bin")
	if err := os.WriteFile(bad, make([]byte, 96), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := espImageVersion(bad); err == nil {
		t.Fatal("expected error for invalid header")
	}
}

func TestRunOTAFlow(t *testing.T) {
	repo := t.TempDir()
	writeFakeESPImage(t, filepath.Join(repo, "firmware", "build"), "lark.bin", "0.10.2")

	// Pre-create firmware/build dir before writing.
	t.Setenv("LARK_REPO_DIR", repo)
	t.Setenv("LARK_TOKEN", "secret-token")

	var polls atomic.Int32
	var uploaded atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			n := polls.Add(1)
			st := otaStatus{
				Board:            "esp-vocat",
				Firmware:         "0.10.1",
				RunningPartition: "ota_0",
				Connected:        true,
				OTAPending:       false,
			}
			if uploaded.Load() {
				if n == 2 {
					st.Firmware = "0.10.2"
					st.RunningPartition = "ota_1"
					st.Connected = false
					st.OTAPending = true
				} else {
					st.Firmware = "0.10.2"
					st.RunningPartition = "ota_1"
					st.Connected = true
					st.OTAPending = false
				}
			}
			_ = json.NewEncoder(w).Encode(st)
		case "/ota/upload":
			if r.Header.Get("X-Lark-Token") != "secret-token" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			uploaded.Store(true)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	var out, errBuf bytes.Buffer
	if err := runOTA(context.Background(), []string{"--no-build", host}, &out, &errBuf); err != nil {
		t.Fatalf("runOTA failed: %v (stderr: %s)", err, errBuf.String())
	}
	if !uploaded.Load() {
		t.Fatal("expected upload")
	}
	if !strings.Contains(out.String(), `"firmware":"0.10.2"`) || !strings.Contains(out.String(), `"running_partition":"ota_1"`) {
		t.Fatalf("unexpected output: %s", out.String())
	}
}

func TestWaitOTAConfirmRejectsPendingOrDisconnected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(otaStatus{
			Board:            "esp-vocat",
			Firmware:         "0.10.2",
			RunningPartition: "ota_1",
			Connected:        true,
			OTAPending:       true,
		})
	}))
	defer srv.Close()

	var out bytes.Buffer
	err := waitOTAConfirm(context.Background(), srv.Client(), srv.URL, "esp-vocat", "ota_0", "0.10.2", 2, time.Millisecond, &out)
	if err == nil {
		t.Fatal("expected error when ota_pending is true")
	}
}
