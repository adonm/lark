// Package eval scores Lark's private OpenCode histories without printing text.
package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
)

type fixture struct {
	Version  int       `json:"version"`
	Sessions []history `json:"sessions"`
}

type history struct {
	Label    string    `json:"label"`
	Messages []message `json:"messages"`
}

type timing struct {
	Created    int64 `json:"created"`
	FirstToken int64 `json:"firstToken,omitempty"`
	Completed  int64 `json:"completed,omitempty"`
}

type tokens struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	Reasoning int64 `json:"reasoning"`
	Cache     struct {
		Read  int64 `json:"read"`
		Write int64 `json:"write"`
	} `json:"cache"`
}

func (t *tokens) add(other tokens) {
	t.Input += other.Input
	t.Output += other.Output
	t.Reasoning += other.Reasoning
	t.Cache.Read += other.Cache.Read
	t.Cache.Write += other.Cache.Write
}

type message struct {
	ID      string    `json:"id"`
	Type    string    `json:"type"`
	Time    timing    `json:"time"`
	Text    string    `json:"text,omitempty"`
	Content []part    `json:"content,omitempty"`
	Tokens  *tokens   `json:"tokens,omitempty"`
	Finish  string    `json:"finish,omitempty"`
	Outcome string    `json:"outcome,omitempty"`
	Error   *msgError `json:"error,omitempty"`
}

type msgError struct {
	Type   string `json:"type,omitempty"`
	Status int    `json:"status,omitempty"`
}

type part struct {
	Type  string     `json:"type"`
	Name  string     `json:"name,omitempty"`
	Text  string     `json:"text,omitempty"`
	State *toolState `json:"state,omitempty"`
}

type toolState struct {
	Status  string          `json:"status"`
	Input   json.RawMessage `json:"input,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
}

type turnScore struct {
	Turn           int    `json:"turn"`
	Outcome        string `json:"outcome"`
	Aborted        bool   `json:"aborted"` // explicit assistant abort; can also be interrupted
	ToolRounds     int    `json:"tool_round_trips"`
	ToolCalls      int    `json:"tool_calls"`
	ToolErrors     int    `json:"tool_errors"`
	LayoutWarnings int    `json:"layout_warnings"`
	FirstTokenMS   *int64 `json:"first_token_ms"`
	CompletionMS   *int64 `json:"completion_ms"` // user admission to idle (all outcomes)
	NextMessageMS  *int64 `json:"next_message_gap_ms"`
	Tokens         tokens `json:"tokens"`
	TokenSteps     int    `json:"token_steps"`
}

type percentiles struct {
	P50 *int64 `json:"p50"`
	P95 *int64 `json:"p95"`
	N   int    `json:"n"`
}

type sessionScore struct {
	Session          string      `json:"session"`
	TurnCount        int         `json:"turn_count"`
	Succeeded        int         `json:"succeeded"`
	Interrupted      int         `json:"interrupted"`
	Aborted          int         `json:"aborted"`
	Failed           int         `json:"failed"`
	Unfinished       int         `json:"unfinished"`
	ToolRounds       int         `json:"tool_round_trips"`
	ToolRoundsPer    float64     `json:"tool_round_trips_per_turn"`
	ToolErrors       int         `json:"tool_errors"`
	LayoutWarnings   int         `json:"layout_warnings"`
	FirstTokenMS     percentiles `json:"first_token_ms"`
	CompletionMS     percentiles `json:"completion_ms"`
	NextMessageGapMS percentiles `json:"next_message_gap_ms"`
	Tokens           tokens      `json:"tokens"`
	TokenSteps       int         `json:"token_steps"`
	Turns            []turnScore `json:"turns"`
}

func score(h history) sessionScore {
	out := sessionScore{Session: h.Label, Turns: []turnScore{}}
	var turn *turnScore
	var start, lastStop int64
	var first, completion, gaps []int64
	for _, m := range h.Messages {
		switch m.Type {
		case "user":
			if n := len(out.Turns); n > 0 && lastStop > 0 {
				gap := m.Time.Created - lastStop
				out.Turns[n-1].NextMessageMS = &gap
			}
			out.Turns = append(out.Turns, turnScore{Turn: len(out.Turns) + 1, Outcome: "unfinished"})
			turn, start, lastStop = &out.Turns[len(out.Turns)-1], m.Time.Created, 0
		case "assistant":
			if turn == nil {
				continue // compaction, background or synthetic activity is not a user turn
			}
			if m.Error != nil && m.Error.Type == "aborted" {
				turn.Aborted = true
				lastStop = m.Time.Completed
			}
			if t := m.Time.FirstToken; t > 0 && t >= start {
				if ms := t - start; turn.FirstTokenMS == nil || ms < *turn.FirstTokenMS {
					turn.FirstTokenMS = &ms
				}
			}
			if m.Tokens != nil {
				turn.Tokens.add(*m.Tokens)
				turn.TokenSteps++
			}
			round := m.Finish == "tool-calls"
			for _, p := range m.Content {
				if p.Type != "tool" {
					continue
				}
				round = true
				turn.ToolCalls++
				if p.State != nil {
					e, l := toolMetrics(p)
					turn.ToolErrors += e
					turn.LayoutWarnings += l
				}
			}
			if round {
				turn.ToolRounds++
			}
		case "idle":
			if turn == nil {
				continue
			}
			switch m.Outcome {
			case "succeeded", "interrupted", "failed":
				turn.Outcome = m.Outcome
			}
			if m.Time.Created >= start && start > 0 {
				ms := m.Time.Created - start
				turn.CompletionMS = &ms
			}
			if turn.Outcome == "interrupted" {
				lastStop = m.Time.Created
			}
			turn = nil
		case "compaction":
			turn = nil
		}
	}
	out.TurnCount = len(out.Turns)
	for _, t := range out.Turns {
		switch t.Outcome {
		case "succeeded":
			out.Succeeded++
		case "interrupted":
			out.Interrupted++
		case "failed":
			out.Failed++
		default:
			out.Unfinished++
		}
		if t.Aborted {
			out.Aborted++
		}
		if t.FirstTokenMS != nil {
			first = append(first, *t.FirstTokenMS)
		}
		if t.CompletionMS != nil {
			completion = append(completion, *t.CompletionMS)
		}
		if t.NextMessageMS != nil {
			gaps = append(gaps, *t.NextMessageMS)
		}
		out.ToolRounds += t.ToolRounds
		out.ToolErrors += t.ToolErrors
		out.LayoutWarnings += t.LayoutWarnings
		out.Tokens.add(t.Tokens)
		out.TokenSteps += t.TokenSteps
	}
	if out.TurnCount != 0 {
		out.ToolRoundsPer = float64(out.ToolRounds) / float64(out.TurnCount)
	}
	out.FirstTokenMS, out.CompletionMS, out.NextMessageGapMS = quantiles(first), quantiles(completion), quantiles(gaps)
	return out
}

func quantiles(values []int64) percentiles {
	n := len(values)
	if n == 0 {
		return percentiles{}
	}
	slices.Sort(values)
	return percentiles{&values[(n*50+99)/100-1], &values[(n*95+99)/100-1], n}
}

func toolMetrics(p part) (int, int) {
	var payload any
	if p.State.Status == "error" {
		return 1, 0
	}
	if p.State.Status != "completed" || json.Unmarshal(p.State.Content, &payload) != nil {
		return 0, 0
	}
	warnings := p.Name == "app" || p.Name == "lark_app" || p.Name == "execute"
	return resultMetrics(payload, warnings, 0)
}

// V2 stores Code Mode output as MCP text blocks containing JSON. Walk returned
// tool results, not input, prose, app code, or arbitrary memory values.
func resultMetrics(v any, warnings bool, depth int) (errs, layouts int) {
	if depth >= 12 {
		return 0, 0
	}
	switch x := v.(type) {
	case []any:
		for _, item := range x {
			e, l := resultMetrics(item, warnings, depth+1)
			errs, layouts = errs+e, layouts+l
		}
	case string:
		var decoded any
		if s := strings.TrimSpace(x); (strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[")) &&
			json.Unmarshal([]byte(s), &decoded) == nil {
			return resultMetrics(decoded, warnings, depth+1)
		}
	case map[string]any:
		failed, _ := x["isError"].(bool)
		errText, _ := x["error"].(string)
		if failed || errText != "" {
			return 1, 0
		}
		if x["type"] == "text" {
			return resultMetrics(x["text"], warnings, depth+1)
		}
		if ls, ok := x["layout_warnings"].([]any); ok && warnings {
			layouts += len(ls)
		}
		for key, item := range x {
			if key == "layout_warnings" || key == "memory" || key == "devices" || key == "companions" ||
				key == "me" || key == "state" || key == "active_app_state" || key == "messages" {
				continue
			}
			if child, ok := item.(map[string]any); ok {
				e, l := resultMetrics(child, warnings, depth+1)
				errs, layouts = errs+e, layouts+l
			} else if child, ok := item.([]any); ok {
				for _, nested := range child {
					if obj, ok := nested.(map[string]any); ok {
						e, l := resultMetrics(obj, warnings, depth+1)
						errs, layouts = errs+e, layouts+l
					}
				}
			}
		}
	}
	return errs, layouts
}

func table(w io.Writer, scores []sessionScore) {
	tab := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tab, "SESSION\tTURNS\tINTERRUPTED/ABORTED\tFAILED/UNFINISHED\tTOOL ERR\tLAYOUT\tROUNDS/TURN\tFIRST P50\tEND P50\tNEXT P50\tINPUT/OUTPUT")
	for _, s := range scores {
		fmt.Fprintf(tab, "%s\t%d\t%d/%d\t%d/%d\t%d\t%d\t%.2f\t%s\t%s\t%s\t%d/%d\n",
			s.Session, s.TurnCount, s.Interrupted, s.Aborted, s.Failed, s.Unfinished, s.ToolErrors, s.LayoutWarnings,
			s.ToolRoundsPer, millis(s.FirstTokenMS.P50), millis(s.CompletionMS.P50), millis(s.NextMessageGapMS.P50), s.Tokens.Input, s.Tokens.Output)
	}
	_ = tab.Flush()
}

func millis(p *int64) string {
	if p == nil {
		return "—"
	}
	return fmt.Sprintf("%dms", *p)
}
