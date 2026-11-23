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
		fmt.Fprintf(out, "Server: ok=%v opencode=%v stt=%v tts=%v",
			health["ok"], health["opencode"], health["stt"], health["tts"])
		if dc, ok := health["deltachat"]; ok {
			fmt.Fprintf(out, " deltachat=%v", dc)
		}
		fmt.Fprintln(out)
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
				str(m["id"]), str(m["companion_id"]), m["ws_connected"], str(m["ip"]), str(m["firmware"]))
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
					str(m["id"]), str(m["companion_id"]), m["ws_connected"], str(m["ip"]), str(m["firmware"]))
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
