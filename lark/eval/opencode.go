package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const productionOC = "http://127.0.0.1:49375"

type openCode struct {
	base, password string
	client         *http.Client
}

func newOpenCode(base, password string) openCode {
	return openCode{base, password, &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (oc openCode) call(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			return errors.New("cannot encode OpenCode input")
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, oc.base+path, body)
	if err != nil {
		return errors.New("invalid OpenCode request")
	}
	if oc.password != "" {
		req.SetBasicAuth("opencode", oc.password)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := oc.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("cannot reach OpenCode")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("OpenCode returned HTTP %d", res.StatusCode)
	}
	if output == nil {
		return nil
	}
	const max = 16 << 20
	raw, err := io.ReadAll(io.LimitReader(res.Body, max+1))
	if err != nil || len(raw) > max || json.Unmarshal(raw, output) != nil {
		return errors.New("invalid or oversized OpenCode response")
	}
	return nil
}

func data[T any](ctx context.Context, oc openCode, method, path string, input any) (T, error) {
	var out struct{ Data *T }
	var zero T
	if err := oc.call(ctx, method, path, input, &out); err != nil {
		return zero, err
	}
	if out.Data == nil {
		return zero, errors.New("OpenCode response missing data")
	}
	return *out.Data, nil
}

type session struct{ ID, Title string }

func pages[T any](ctx context.Context, oc openCode, path string, params url.Values) ([]T, error) {
	var out []T
	seen := map[string]bool{}
	for range 256 {
		var page struct {
			Data   []T
			Cursor struct{ Next string }
		}
		if err := oc.call(ctx, http.MethodGet, path+"?"+params.Encode(), nil, &page); err != nil {
			return nil, err
		}
		if page.Data == nil {
			return nil, errors.New("OpenCode page missing data")
		}
		out = append(out, page.Data...)
		next := page.Cursor.Next
		if len(page.Data) == 0 || next == "" {
			return out, nil
		}
		if seen[next] {
			return nil, errors.New("OpenCode repeated a page cursor")
		}
		seen[next] = true
		params.Del("order") // the cursor already encodes direction
		params.Set("cursor", next)
	}
	return nil, errors.New("OpenCode history exceeded the page limit")
}

var sessionID = regexp.MustCompile(`^ses[-_][A-Za-z0-9_-]{1,120}$`)
var publicLabel = regexp.MustCompile(`^(Cake|Mint|greg|Synthetic)(/[1-9][0-9]*)?$`)
var companions = []string{"Cake", "Mint", "greg"}

func companionMatches(label, title string) bool {
	companion, ok := strings.CutPrefix(title, "Lark: ")
	if !ok {
		return false
	}
	if stem, ok := strings.CutSuffix(companion, ")"); ok {
		if split := strings.LastIndex(stem, " ("); split >= 0 {
			companion = stem[:split]
		}
	}
	return strings.EqualFold(companion, label)
}

func historyMessages(ctx context.Context, oc openCode, id string) ([]message, error) {
	if !sessionID.MatchString(id) {
		return nil, errors.New("invalid OpenCode session ID")
	}
	return pages[message](ctx, oc, "/api/session/"+id+"/message", url.Values{"limit": {"100"}, "order": {"asc"}})
}

func collect(ctx context.Context, oc openCode) (fixture, error) {
	list, err := pages[session](ctx, oc, "/api/session", url.Values{"limit": {"100"}})
	if err != nil {
		return fixture{}, err
	}
	out := fixture{Version: 1}
	for _, label := range companions {
		var matches []session
		for _, s := range list {
			if companionMatches(label, s.Title) {
				matches = append(matches, s)
			}
		}
		if len(matches) == 0 {
			return fixture{}, fmt.Errorf("no matching history for %s", label)
		}
		for i, s := range matches {
			messages, err := historyMessages(ctx, oc, s.ID)
			if err != nil {
				return fixture{}, fmt.Errorf("cannot read %s history: %w", label, err)
			}
			name := label
			if len(matches) > 1 {
				name += "/" + strconv.Itoa(i+1)
			}
			out.Sessions = append(out.Sessions, history{name, messages})
		}
	}
	return out, nil
}

func productionClient() (openCode, error) {
	config := os.Getenv("XDG_CONFIG_HOME")
	if config == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return openCode{}, errors.New("cannot resolve home directory")
		}
		config = filepath.Join(home, ".config")
	}
	raw, err := os.ReadFile(filepath.Join(config, "lark", "lark.env"))
	if err != nil {
		return openCode{}, errors.New("cannot read private Lark configuration")
	}
	pass := ""
	for line := range strings.SplitSeq(string(raw), "\n") {
		k, value, _ := strings.Cut(line, "=")
		if strings.TrimSpace(k) == "LARK_OPENCODE_PASSWORD" {
			value = strings.TrimSpace(value)
			if len(value) >= 2 && (value[0] == '\'' || value[0] == '"') && value[0] == value[len(value)-1] {
				value = value[1 : len(value)-1]
			}
			pass = value
		}
	}
	if pass == "" {
		return openCode{}, errors.New("Lark OpenCode password is missing")
	}
	return newOpenCode(productionOC, pass), nil
}
