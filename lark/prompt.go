package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func formatLocalTime(t time.Time) string {
	return formatLocalTimeIn(t, time.Local)
}

func formatLocalTimeIn(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	return t.In(loc).Format("2006-01-02 15:04 MST (-07:00)")
}

func turnHeader(companionID, source string, companion companionState, ch turnChannel, now time.Time) string {
	var prefix string
	switch {
	case ch.channel == "deltachat":
		kind := ch.senderKind
		if kind == "" {
			kind = "contact"
		}
		sender := ch.sender
		if sender == "" {
			sender = "Friend"
		}
		if ch.private {
			prefix = fmt.Sprintf("You are %s (%s). Speaker: %s via Delta Chat (%s) · private", companion.Name, companionID, sender, kind)
		} else {
			prefix = fmt.Sprintf("You are %s (%s). Speaker: %s via Delta Chat (%s)", companion.Name, companionID, sender, kind)
		}
	case ch.channel == "timer" || source == "timer":
		prefix = fmt.Sprintf("You are %s (%s). Event: timer", companion.Name, companionID)
	case ch.reply != nil && ch.reply.name != "":
		prefix = fmt.Sprintf("You are %s (%s). Speaker: someone in person at your device (name unknown) · replying to %s (Delta Chat) · %s", companion.Name, companionID, ch.reply.name, source)
	default:
		prefix = fmt.Sprintf("You are %s (%s). Speaker: someone in person at your device (name unknown) · %s", companion.Name, companionID, source)
	}
	if !companion.Configured {
		extra := " · setup"
		if companion.Personality != "" {
			extra += " · vibe: " + companion.Personality
		}
		return "[" + prefix + extra + "]"
	}
	if companion.Personality != "" {
		return "[" + prefix + " · vibe: " + companion.Personality + "]"
	}
	return "[" + prefix + "]"
}

func turnStateLine(now time.Time, companion companionState, terminals, timers, stories string) string {
	parts := []string{"now: " + formatLocalTimeIn(now, now.Location())}
	if companion.Form != "" {
		parts = append(parts, "form: "+companion.Form)
	}
	if companion.ListenSeconds > 0 {
		parts = append(parts, fmt.Sprintf("listen: on (%ds spans)", companion.ListenSeconds))
	} else {
		parts = append(parts, "listen: off")
	}
	if terminals != "" {
		parts = append(parts, "terminals: "+terminals)
	}
	if timers != "" {
		parts = append(parts, "timers: "+timers)
	}
	if stories != "" {
		parts = append(parts, "stories: "+stories)
	}
	return "[state: " + strings.Join(parts, " · ") + "]"
}

func turnPrompt(companionID string, source, text string, companion companionState) string {
	return turnPromptAt(companionID, source, text, companion, time.Now())
}

func turnPromptAt(companionID string, source, text string, companion companionState, now time.Time) string {
	return turnPromptChannelAt(companionID, source, text, companion, turnChannel{}, now)
}

func turnPromptChannelAt(companionID, source, text string, companion companionState, ch turnChannel, now time.Time) string {
	return turnHeader(companionID, source, companion, ch, now) + "\n" + text + "\n" + turnStateLine(now, companion, "", "", "")
}

func storyRequest(text string) bool {
	text = strings.ToLower(text)
	for _, cue := range []string{"story", "tale", "read me", "once upon"} {
		if strings.Contains(text, cue) {
			return true
		}
	}
	return false
}

func (b *larkServer) turnPromptWithState(ctx context.Context, companionID, source, text string, companion companionState, ch turnChannel, now time.Time) string {
	var termParts []string
	if terms, err := b.terminalList(false); err == nil {
		for _, t := range terms {
			if !strings.EqualFold(str(t["companion_id"]), companionID) || str(t["kind"]) != "hardware" {
				continue
			}
			if online, _ := t["online"].(bool); !online {
				continue
			}
			fields := []string{str(t["id"])}
			if board := str(t["board"]); board != "" {
				fields = append(fields, board)
			}
			fields = append(fields, "online")
			if pct, ok := t["battery_pct"].(int); ok && pct >= 0 {
				fields = append(fields, fmt.Sprintf("battery:%d", pct))
			}
			if sleeping, _ := t["sleeping"].(bool); sleeping {
				fields = append(fields, "sleeping")
			}
			if vol := t["volume"]; vol != nil {
				fields = append(fields, fmt.Sprintf("vol:%v", vol))
			}
			termParts = append(termParts, fields[0]+"("+strings.Join(fields[1:], ",")+")")
		}
	}
	terminals := "none"
	if len(termParts) > 0 {
		terminals = strings.Join(termParts, ",")
	}

	var timerParts []string
	if b.timers != nil {
		if list, err := b.timers.list(ctx, companionID); err == nil {
			for _, tm := range list {
				rem := max(0, int(tm.FireAt.Sub(now).Round(time.Second)/time.Second))
				if tm.Label != "" {
					timerParts = append(timerParts, fmt.Sprintf("%s(%q,%ds)", tm.ID, tm.Label, rem))
				} else {
					timerParts = append(timerParts, fmt.Sprintf("%s(%ds)", tm.ID, rem))
				}
			}
		}
	}
	timers := "none"
	if len(timerParts) > 0 {
		timers = strings.Join(timerParts, ",")
	}

	var storyParts []string
	if list, err := b.listStories(ctx); err == nil {
		for _, s := range list {
			if s.Title != "" && s.Title != s.ID {
				storyParts = append(storyParts, fmt.Sprintf("%s(%q)", s.ID, s.Title))
			} else {
				storyParts = append(storyParts, s.ID)
			}
		}
	}
	stories := "none"
	if len(storyParts) > 0 {
		stories = strings.Join(storyParts, ",")
	}

	prompt := turnHeader(companionID, source, companion, ch, now) + "\n" + text
	if b.libraryInstalled() {
		hint := "[library: search before facts]"
		if storyRequest(text) {
			hint = "[story: story get a listed story and read its passages aloud; library import only when absent; say so if a book will not open]"
		}
		prompt += "\n" + hint
	}
	return prompt + "\n" + turnStateLine(now, companion, terminals, timers, stories)
}

func (b *larkServer) libraryInstalled() bool {
	if b == nil || b.library == "" {
		return false
	}
	for _, source := range librarySourceOrder {
		if _, err := os.Stat(filepath.Join(b.libraryDir(), librarySources[source].db)); err == nil {
			return true
		}
	}
	return false
}
