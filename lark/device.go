package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func resolveTerminalIP(ctx context.Context, c *cliClient, id string) (string, error) {
	res, err := c.do(ctx, http.MethodGet, "/api/terminals", nil)
	if err != nil {
		return "", err
	}
	want := strings.ToLower(strings.TrimSpace(id))
	list, _ := res["terminals"].([]any)
	for _, item := range list {
		m, _ := item.(object)
		if strings.ToLower(str(m["id"])) == want {
			ip := strings.TrimSpace(str(m["ip"]))
			if ip == "" {
				return "", fmt.Errorf("terminal %s has no IP", id)
			}
			return ip, nil
		}
	}
	return "", fmt.Errorf("terminal %q not found", id)
}

func deviceHTTPRequest(ctx context.Context, client *http.Client, method, url, token, contentType string, body io.Reader) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, nil, err
	}
	if token != "" {
		req.Header.Set("X-Lark-Token", token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, nil, err
	}
	if res.StatusCode >= 400 {
		return nil, res.Header, fmt.Errorf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, res.Header, nil
}

func decodeRGB565BE(raw []byte, w, h int) ([]uint16, error) {
	if w <= 0 || h <= 0 || len(raw) != w*h*2 {
		return nil, fmt.Errorf("expected %d bytes for %dx%d RGB565, got %d", w*h*2, w, h, len(raw))
	}
	fb := make([]uint16, w*h)
	for i := range fb {
		fb[i] = uint16(raw[i*2])<<8 | uint16(raw[i*2+1])
	}
	return fb, nil
}

func runDevice(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "Usage: lark device <terminal> <tap X Y | tap-label LABEL | mic file.wav | screen out.png | status>")
		return errors.New("invalid device arguments")
	}
	termID, sub := args[0], args[1]
	rest := args[2:]

	c, err := newCLIClient()
	if err != nil {
		return err
	}
	token, err := loadOTAToken()
	if err != nil {
		return err
	}
	ip, err := resolveTerminalIP(ctx, c, termID)
	if err != nil {
		return err
	}
	devClient := &http.Client{
		Timeout:   20 * time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	defer devClient.CloseIdleConnections()
	return runDeviceAction(ctx, c, devClient, "http://"+ip, token, termID, sub, rest, stdout)
}

func runDeviceAction(ctx context.Context, srv *cliClient, dev *http.Client, baseURL, token, termID, sub string, rest []string, stdout io.Writer) error {
	switch sub {
	case "status":
		if len(rest) != 0 {
			return errors.New("usage: lark device <terminal> status")
		}
		raw, _, err := deviceHTTPRequest(ctx, dev, http.MethodGet, baseURL+"/status", "", "", nil)
		if err != nil {
			return err
		}
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, raw, "", "  "); err == nil {
			fmt.Fprintln(stdout, pretty.String())
		} else {
			fmt.Fprintln(stdout, string(raw))
		}
		return nil

	case "tap":
		if len(rest) != 2 {
			return errors.New("usage: lark device <terminal> tap X Y")
		}
		x, err := strconv.Atoi(rest[0])
		if err != nil {
			return err
		}
		y, err := strconv.Atoi(rest[1])
		if err != nil {
			return err
		}
		body, _ := json.Marshal(object{"x": x, "y": y})
		raw, _, err := deviceHTTPRequest(ctx, dev, http.MethodPost, baseURL+"/debug/tap", token, jsonType, bytes.NewReader(body))
		if err != nil {
			return err
		}
		fmt.Fprint(stdout, string(raw))
		return nil

	case "tap-label":
		if len(rest) < 1 {
			return errors.New("usage: lark device <terminal> tap-label LABEL")
		}
		label := strings.Join(rest, " ")
		hitsRes, err := srv.do(ctx, http.MethodGet, "/api/terminals/"+termID+"/hits", nil)
		if err != nil {
			return err
		}
		x, y, matched, err := resolveHitLabel(hitsRes, label)
		if err != nil {
			return err
		}
		body, _ := json.Marshal(object{"x": x, "y": y})
		if _, _, err := deviceHTTPRequest(ctx, dev, http.MethodPost, baseURL+"/debug/tap", token, jsonType, bytes.NewReader(body)); err != nil {
			return err
		}
		b, _ := json.Marshal(object{"ok": true, "label": label, "matched": matched, "x": x, "y": y})
		fmt.Fprintln(stdout, string(b))
		return nil

	case "mic":
		if len(rest) != 1 {
			return errors.New("usage: lark device <terminal> mic file.wav")
		}
		pcm, err := readWAVPCM16k(rest[0])
		if err != nil {
			return err
		}
		raw, _, err := deviceHTTPRequest(ctx, dev, http.MethodPost, baseURL+"/debug/mic", token, "application/octet-stream", bytes.NewReader(pcm))
		if err != nil {
			return err
		}
		fmt.Fprint(stdout, string(raw))
		return nil

	case "screen":
		if len(rest) != 1 {
			return errors.New("usage: lark device <terminal> screen out.png")
		}
		raw, hdr, err := deviceHTTPRequest(ctx, dev, http.MethodGet, baseURL+"/debug/screen", token, "", nil)
		if err != nil {
			return err
		}
		w, _ := strconv.Atoi(hdr.Get("X-Width"))
		h, _ := strconv.Atoi(hdr.Get("X-Height"))
		fb, err := decodeRGB565BE(raw, w, h)
		if err != nil {
			return err
		}
		if err := writeFBToPNG(fb, w, h, rest[0]); err != nil {
			return err
		}
		b, _ := json.Marshal(object{"ok": true, "path": rest[0], "width": w, "height": h})
		fmt.Fprintln(stdout, string(b))
		return nil

	default:
		return fmt.Errorf("unknown device action %q", sub)
	}
}
