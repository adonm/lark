package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type cliClient struct {
	base  string
	token string
	http  *http.Client
}

func newCLIClient() (*cliClient, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	config := env("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	endpoint, token, err := mcpProxyConfig(filepath.Join(config, "lark", "lark.env"))
	if err != nil {
		return nil, err
	}
	base := os.Getenv("LARK_URL")
	if base == "" {
		base = strings.TrimSuffix(endpoint, "/mcp")
	}
	return &cliClient{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http:  &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func (c *cliClient) postRaw(ctx context.Context, path string, body []byte) (object, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Lark-Token", c.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var out object
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		_ = dec.Decode(&out)
	}
	if res.StatusCode >= 400 {
		if msg := str(out["error"]); msg != "" {
			return nil, errors.New(msg)
		}
		return nil, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return out, nil
}

func (c *cliClient) do(ctx context.Context, method, path string, body any) (object, error) {
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Lark-Token", c.token)
	if body != nil {
		req.Header.Set("Content-Type", jsonType)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	var out object
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&out); err != nil {
			if res.StatusCode >= 400 {
				return nil, fmt.Errorf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(raw)))
			}
			return nil, err
		}
	}
	if res.StatusCode >= 400 {
		if msg := str(out["error"]); msg != "" {
			return nil, errors.New(msg)
		}
		return nil, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return out, nil
}

func runCLI(ctx context.Context, cmd string, args []string, out, errOut io.Writer) error {
	c, err := newCLIClient()
	if err != nil {
		return err
	}
	return runCLIWithClient(ctx, c, cmd, args, out, errOut)
}

func benchFiles(args []string) ([]string, error) {
	var files []string
	for _, arg := range args {
		info, err := os.Stat(arg)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			files = append(files, arg)
			continue
		}
		entries, err := os.ReadDir(arg)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".wav") {
				files = append(files, filepath.Join(arg, e.Name()))
			}
		}
	}
	return files, nil
}

func jsonNumberText(v any) string {
	switch n := v.(type) {
	case json.Number:
		return n.String()
	case float64:
		return fmt.Sprintf("%.0f", n)
	}
	return "?"
}

func speedup(res object) float64 {
	stt, _ := strconv.ParseFloat(jsonNumberText(res["stt_ms"]), 64)
	audio, _ := strconv.ParseFloat(jsonNumberText(res["audio_ms"]), 64)
	if stt <= 0 {
		return 0
	}
	return audio / stt
}

func printLibraryPlan(out io.Writer, lib object) {
	fmt.Fprintf(out, "Library (%s):\n", str(lib["dir"]))
	if msg := str(lib["error"]); msg != "" {
		fmt.Fprintf(out, "  unavailable: %s\n", msg)
		return
	}
	fmt.Fprintf(out, "  disk: %s free of %s, budget %s\n",
		humanBytes(jsonInt(lib["free_bytes"])), humanBytes(jsonInt(lib["total_bytes"])),
		humanBytes(jsonInt(lib["budget_bytes"])))
	packs, _ := lib["packs"].([]any)
	for _, item := range packs {
		m, _ := item.(object)
		switch str(m["state"]) {
		case packStateInstalled:
			fmt.Fprintf(out, "  %s\tinstalled\t%s\n", str(m["name"]), humanBytes(jsonInt(m["installed_bytes"])))
		case packStateBuilding:
			fmt.Fprintf(out, "  %s\tbuilding\t%d%%\n", str(m["name"]), jsonInt(m["percent"]))
		case packStateSkipped:
			fmt.Fprintf(out, "  %s\tskipped\t%s\n", str(m["name"]), str(m["reason"]))
		default:
			fmt.Fprintf(out, "  %s\t%s\t%s needed\n", str(m["name"]), str(m["state"]),
				humanBytes(jsonInt(m["needed_bytes"])))
		}
	}
}

func jsonInt(value any) int64 {
	switch v := value.(type) {
	case json.Number:
		n, _ := v.Int64()
		return n
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	}
	return 0
}

func runCLIWithClient(ctx context.Context, c *cliClient, cmd string, args []string, out, errOut io.Writer) error {
	switch cmd {
	case "status":
		if len(args) != 0 {
			fmt.Fprintln(errOut, "Usage: lark status")
			return errors.New("unexpected arguments")
		}
		health, err := c.do(ctx, http.MethodGet, "/health", nil)
		if err != nil {
			return err
		}
		comps, err := c.do(ctx, http.MethodGet, "/api/companions", nil)
		if err != nil {
			return err
		}
		terms, err := c.do(ctx, http.MethodGet, "/api/terminals", nil)
		if err != nil {
			return err
		}
		lib, err := c.do(ctx, http.MethodGet, "/api/library", nil)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Server: ok=%v opencode=%v stt=%v tts=%v",
			health["ok"], health["opencode"], health["stt"], health["tts"])
		if dc, ok := health["deltachat"]; ok {
			fmt.Fprintf(out, " deltachat=%v", dc)
		}
		fmt.Fprintln(out)
		printLibraryPlan(out, lib)
		compList, _ := comps["companions"].([]any)
		termList, _ := terms["terminals"].([]any)
		fmt.Fprintf(out, "Companions (%d):\n", len(compList))
		for _, item := range compList {
			m, _ := item.(object)
			fmt.Fprintf(out, "  %s\t%s\tonline=%v\tdormant=%v", str(m["id"]), str(m["name"]), m["online"], m["dormant"])
			if dcID := m["dc_account_id"]; dcID != nil {
				fmt.Fprintf(out, "\tdc=%v", dcID)
			}
			fmt.Fprintln(out)
		}
		fmt.Fprintf(out, "Terminals (%d):\n", len(termList))
		for _, item := range termList {
			m, _ := item.(object)
			fmt.Fprintf(out, "  %s\tcompanion=%s\tonline=%v\tip=%s\tfw=%s\n",
				str(m["id"]), str(m["companion_id"]), m["connected"], str(m["ip"]), str(m["firmware"]))
		}
		return nil

	case "companions":
		switch len(args) {
		case 0:
			res, err := c.do(ctx, http.MethodGet, "/api/companions", nil)
			if err != nil {
				return err
			}
			list, _ := res["companions"].([]any)
			for _, item := range list {
				m, _ := item.(object)
				fmt.Fprintf(out, "%s\t%s\tonline=%v\tdormant=%v", str(m["id"]), str(m["name"]), m["online"], m["dormant"])
				if dcID := m["dc_account_id"]; dcID != nil {
					fmt.Fprintf(out, "\tdc=%v", dcID)
				}
				fmt.Fprintln(out)
			}
			return nil
		case 2:
			if args[0] == "rm" {
				if _, err := c.do(ctx, http.MethodDelete, "/api/companions/"+args[1], nil); err != nil {
					return err
				}
				fmt.Fprintf(out, "Removed companion %s\n", args[1])
				return nil
			}
		case 3:
			if args[0] == "add" {
				res, err := c.do(ctx, http.MethodPost, "/api/companions", object{"name": args[1], "form": args[2]})
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "Added companion %s (%s)\n", str(res["id"]), str(res["name"]))
				return nil
			}
		}
		fmt.Fprintln(errOut, "Usage: lark companions [add <name> <form> | rm <id>]")
		return errors.New("invalid companions arguments")

	case "terminals":
		switch {
		case len(args) == 0:
			res, err := c.do(ctx, http.MethodGet, "/api/terminals", nil)
			if err != nil {
				return err
			}
			list, _ := res["terminals"].([]any)
			for _, item := range list {
				m, _ := item.(object)
				fmt.Fprintf(out, "%s\tcompanion=%s\tonline=%v\tip=%s\tfw=%s\n",
					str(m["id"]), str(m["companion_id"]), m["connected"], str(m["ip"]), str(m["firmware"]))
			}
			return nil
		case len(args) == 3 && args[0] == "attach":
			res, err := c.do(ctx, http.MethodPost, "/api/terminals/"+args[1]+"/attach", object{"companion_id": args[2]})
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Attached %s to %s\n", str(res["id"]), str(res["companion_id"]))
			return nil
		case len(args) == 2 && args[0] == "detach":
			res, err := c.do(ctx, http.MethodPost, "/api/terminals/"+args[1]+"/detach", object{})
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Detached %s\n", str(res["id"]))
			return nil
		}
		fmt.Fprintln(errOut, "Usage: lark terminals [attach <terminal> <companion> | detach <terminal>]")
		return errors.New("invalid terminals arguments")

	case "capture":
		if len(args) < 1 || len(args) > 2 {
			return captureUsage()
		}
		seconds := 20
		if len(args) == 2 {
			n, convErr := strconv.Atoi(args[1])
			if convErr != nil || n < 1 || n > 60 {
				return errors.New("seconds must be between 1 and 60")
			}
			seconds = n
		}
		slow := *c
		slow.http = &http.Client{Timeout: time.Duration(seconds+20) * time.Second}
		fmt.Fprintf(errOut, "Recording %ds of microphone audio; stay quiet or speak normally.\n", seconds)
		res, err := slow.do(ctx, http.MethodPost, "/api/capture", object{"companion_id": args[0], "seconds": seconds})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s\t%v ms\n", str(res["path"]), res["audio_ms"])
		return nil

	case "stt-bench":
		if len(args) == 0 {
			return errors.New("usage: lark stt-bench <wav|dir>...")
		}
		files, err := benchFiles(args)
		if err != nil {
			return err
		}
		if len(files) == 0 {
			return errors.New("no WAV files found")
		}
		for _, file := range files {
			pcm, err := readWAVPCM16k(file)
			if err != nil {
				fmt.Fprintf(out, "%s\tERROR\t%v\n", file, err)
				continue
			}
			res, err := c.postRaw(ctx, "/api/stt", pcm)
			if err != nil {
				fmt.Fprintf(out, "%s\tERROR\t%v\n", file, err)
				continue
			}
			fmt.Fprintf(out, "%s\t%s ms (%s audio, %.0fx)\t%s\n", file,
				jsonNumberText(res["stt_ms"]), jsonNumberText(res["audio_ms"]),
				speedup(res), str(res["transcript"]))
		}
		return nil

	case "tunnels":
		switch {
		case len(args) == 0:
			res, err := c.do(ctx, http.MethodGet, "/api/remote/tunnels", nil)
			if err != nil {
				return err
			}
			list, _ := res["tunnels"].([]any)
			for _, item := range list {
				m, _ := item.(object)
				fmt.Fprintf(out, "%s\tactive=%v\tcontacts=%v\t%s\n",
					str(m["companion_id"]), m["active"], m["contacts"], str(m["url"]))
			}
			return nil
		case len(args) == 2 && args[0] == "stop":
			if _, err := c.do(ctx, http.MethodDelete, "/api/remote/tunnels/"+args[1], nil); err != nil {
				return err
			}
			fmt.Fprintf(out, "Stopped remote controls for %s\n", args[1])
			return nil
		}
		fmt.Fprintln(errOut, "Usage: lark tunnels [stop <companion>]")
		return errors.New("invalid tunnels arguments")
	}
	return fmt.Errorf("unknown command %q", cmd)
}
