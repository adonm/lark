package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func replay(ctx context.Context, f fixture, label, numbers, base, workspace string, timeout time.Duration) ([]sessionScore, string, error) {
	var user []message
	found := false
	for _, h := range f.Sessions {
		if h.Label == label {
			found = true
			for _, m := range h.Messages {
				if m.Type == "user" {
					user = append(user, m)
				}
			}
			break
		}
	}
	if !found {
		return nil, "", errors.New("select a session label from score with -session")
	}
	numbersList := strings.Split(numbers, ",")
	if len(numbersList) > 3 {
		return nil, "", errors.New("at most three replay turns per invocation")
	}
	ordinals := make([]int, 0, len(numbersList))
	seen := make(map[int]bool)
	for _, raw := range numbersList {
		i, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || i < 1 || i > len(user) || seen[i] {
			return nil, "", errors.New("select distinct existing turns with -turns")
		}
		if text := user[i-1].Text; strings.TrimSpace(text) == "" || len(text) > 4096 {
			return nil, "", errors.New("replay requires a nonempty prompt of at most 4096 bytes")
		}
		ordinals = append(ordinals, i)
		seen[i] = true
	}

	mode := "stub"
	run := func(_ context.Context, text string) ([]message, error) { return stubModel(text), nil }
	if base != "" {
		if timeout <= 0 || timeout > 90*time.Second {
			return nil, "", errors.New("live replay timeout must be greater than zero and at most 90s")
		}
		if !strings.HasPrefix(label, "Synthetic") && os.Getenv("LARK_EVAL_ALLOW_PRIVATE") != "1" {
			return nil, "", errors.New("private replay requires LARK_EVAL_ALLOW_PRIVATE=1; your provider may receive the selected prompts")
		}
		isolated, err := isolatedReplayURL(base)
		if err != nil {
			return nil, "", err
		}
		if !filepath.IsAbs(workspace) {
			return nil, "", errors.New("live replay requires an absolute disposable -workspace")
		}
		api := newOpenCode(isolated, os.Getenv("LARK_EVAL_PASSWORD"))
		defer api.client.CloseIdleConnections()
		agent, err := replayAgent(ctx, api, workspace)
		if err != nil {
			return nil, "", err
		}
		mode = "live"
		run = func(ctx context.Context, text string) ([]message, error) {
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			return replayTurn(ctx, api, workspace, agent, text)
		}
	}
	out := make([]sessionScore, 0, len(ordinals))
	for _, i := range ordinals {
		msgs, err := run(ctx, user[i-1].Text)
		if err != nil {
			return nil, "", fmt.Errorf("replay turn %d: %w", i, err)
		}
		s := score(history{fmt.Sprintf("%s#%d", label, i), msgs})
		if len(s.Turns) != 1 {
			return nil, "", errors.New("replay did not project exactly one user turn")
		}
		s.Turns[0].Turn = i
		out = append(out, s)
	}
	return out, mode, nil
}

func isolatedReplayURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err == nil && u.Scheme == "http" && u.Hostname() == "127.0.0.1" && u.User == nil &&
		(u.Path == "" || u.Path == "/") && u.Fragment == "" && !u.ForceQuery && u.RawQuery == "" {
		port, err := strconv.Atoi(u.Port())
		if err == nil && port > 0 && port <= 65535 && port != 49375 && port != 49376 && port != 8765 {
			return fmt.Sprintf("http://127.0.0.1:%d", port), nil
		}
	}
	return "", errors.New("live replay requires http://127.0.0.1 on a separate, non-production port")
}

type agentModel struct {
	ProviderID string `json:"providerID"`
	ID         string `json:"id"`
	Variant    string `json:"variant,omitempty"`
}

func replayAgent(ctx context.Context, oc openCode, workspace string) (agentModel, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	query := "?" + url.Values{"location[directory]": {workspace}}.Encode()
	type server struct {
		Name   string
		Status struct{ Status string }
	}
	mcps, err := data[[]server](ctx, oc, http.MethodGet, "/api/mcp"+query, nil)
	if err != nil {
		return agentModel{}, err
	}
	if len(mcps) != 1 || mcps[0].Name != "lark" || mcps[0].Status.Status != "connected" {
		return agentModel{}, errors.New("disposable OpenCode must have only the connected lark MCP")
	}
	a, err := data[struct {
		ID    string
		Model agentModel
	}](ctx, oc, http.MethodGet, "/api/agent/lark"+query, nil)
	if err != nil {
		return agentModel{}, err
	}
	if a.ID != "lark" || a.Model.ProviderID == "" || a.Model.ID == "" {
		return agentModel{}, errors.New("configure the Lark agent's model in disposable OpenCode")
	}
	return a.Model, nil
}

func replayTurn(ctx context.Context, oc openCode, workspace string, model agentModel, text string) (_ []message, err error) {
	input := map[string]any{
		"title":    "Lark eval (disposable)",
		"location": map[string]string{"directory": workspace},
		"agent":    "lark",
		"model":    model,
		"permissions": []map[string]string{
			{"action": "*", "resource": "*", "effect": "deny"},
			{"action": "execute", "resource": "*", "effect": "allow"},
			{"action": "lark_*", "resource": "*", "effect": "allow"},
		},
	}
	// Don't cancel a create request on Ctrl-C before we know which ID to clean up.
	create, stopCreate := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stopCreate()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	s, err := data[session](create, oc, http.MethodPost, "/api/session", input)
	if err != nil {
		return nil, err
	}
	if !sessionID.MatchString(s.ID) {
		return nil, errors.New("invalid disposable session ID")
	}
	path := "/api/session/" + s.ID
	settled := false
	defer func() {
		if !settled {
			abort, stop := context.WithTimeout(context.Background(), 3*time.Second)
			_ = oc.call(abort, http.MethodPost, path+"/interrupt", nil, nil)
			stop()
		}
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if e := oc.call(cleanup, http.MethodDelete, path, nil, nil); e != nil {
			err = errors.Join(err, fmt.Errorf("cannot delete disposable OpenCode session: %w", e))
		}
	}()
	_, err = data[struct{ ID string }](ctx, oc, http.MethodPost, path+"/prompt", map[string]any{
		"text":     text,
		"metadata": map[string]string{"larkSource": "typed"},
	})
	if err != nil {
		return nil, err
	}
	if err := oc.call(ctx, http.MethodPost, "/api/experimental"+path+"/wait", nil, nil); err != nil {
		return nil, err
	}
	settled = true
	return historyMessages(ctx, oc, s.ID)
}

// This model makes no network calls and uses logical timestamps. It exists to
// check the CLI and scorer, not to predict actual latency or reply quality.
func stubModel(prompt string) []message {
	return []message{
		{Type: "user", ID: "synthetic-user", Time: timing{Created: 1000}, Text: prompt},
		{Type: "assistant", ID: "synthetic-tool", Time: timing{Created: 1001, FirstToken: 1010, Completed: 1030},
			Finish: "tool-calls", Tokens: &tokens{Input: 10, Output: 4},
			Content: []part{{Type: "tool", Name: "app", State: &toolState{Status: "completed", Content: json.RawMessage(`{"layout_warnings":["synthetic warning"]}`)}}}},
		{Type: "assistant", ID: "synthetic-answer", Time: timing{Created: 1031, FirstToken: 1040, Completed: 1060},
			Finish: "stop", Tokens: &tokens{Input: 12, Output: 3}, Content: []part{{Type: "text", Text: "Synthetic reply."}}},
		{Type: "idle", ID: "synthetic-idle", Time: timing{Created: 1061}, Outcome: "succeeded"},
	}
}
