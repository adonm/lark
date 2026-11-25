package stories

import (
	"slices"
	"strings"
	"unicode"
)

func (cs *Story) EvalPassage(pIdx int, flags []Value, applySets bool) (string, []Choice) {
	if pIdx < 0 || pIdx >= len(cs.Passages) {
		return "", nil
	}
	ops := cs.Passages[pIdx].Ops
	var text strings.Builder
	var choices []Choice
	for pc := 0; pc < len(ops); {
		op := ops[pc]
		switch op.kind {
		case opText:
			text.WriteString(op.text)
			pc++
		case opChoice:
			if len(choices) < MaxChoices {
				choices = append(choices, Choice{Label: op.text, Target: op.target})
			}
			pc++
		case opSet:
			if applySets && int(op.flag) < len(flags) {
				flags[op.flag] = op.val
			}
			pc++
		case opIfNot:
			if int(op.flag) >= len(flags) || !flags[op.flag].Truthy() {
				pc = op.target
			} else {
				pc++
			}
		case opJump:
			pc = op.target
		default:
			pc++
		}
	}
	return strings.TrimSpace(text.String()), choices
}

func (cs *Story) FlagsSlice(m map[string]Value) []Value {
	arr := make([]Value, len(cs.Flags))
	for name, idx := range cs.FlagIdx {
		if v, ok := m[name]; ok {
			arr[idx] = v
		}
	}
	return arr
}

func (cs *Story) FlagsMap(arr []Value) map[string]Value {
	out := map[string]Value{}
	for i, name := range cs.Flags {
		if i < len(arr) && arr[i].Truthy() || (i < len(arr) && arr[i].IsNum) {
			out[name] = arr[i]
		}
	}
	return out
}

// MatchChoice matches spoken text against choice labels using normalized
// token overlap and ordinal phrases without calling an LLM.
func MatchChoice(spoken string, choices []Choice) (int, bool) {
	if len(choices) == 0 {
		return 0, false
	}
	tokens := normalizeSpeechTokens(spoken)
	if len(tokens) == 0 {
		return 0, false
	}

	// Score token overlap against each choice.
	bestIdx, bestScore, secondScore := -1, 0.0, 0.0
	spokenJoined := strings.Join(tokens, " ")
	for i, ch := range choices {
		cTokens := filterStopTokens(normalizeSpeechTokens(ch.Label))
		if len(cTokens) == 0 {
			cTokens = normalizeSpeechTokens(ch.Label)
		}
		if len(cTokens) == 0 {
			continue
		}
		hits := 0
		for _, ct := range cTokens {
			if slices.Contains(tokens, ct) {
				hits++
			}
		}
		if hits == 0 {
			continue
		}
		score := float64(hits) / float64(len(cTokens))
		if strings.Contains(spokenJoined, strings.Join(cTokens, " ")) {
			score += 0.5
		}
		if score > bestScore {
			secondScore = bestScore
			bestScore = score
			bestIdx = i
		} else if score > secondScore {
			secondScore = score
		}
	}
	if bestIdx >= 0 && bestScore >= 0.5 && bestScore > secondScore {
		return bestIdx, true
	}

	// Check ordinals: "first", "second", "third", "last", "one", "two", "three".
	ord := -1
	for _, t := range tokens {
		switch t {
		case "first", "1st", "one", "1":
			ord = 0
		case "second", "2nd", "two", "2":
			ord = 1
		case "third", "3rd", "three", "3":
			ord = 2
		case "last":
			ord = len(choices) - 1
		}
		if ord >= 0 {
			break
		}
	}
	if ord >= 0 && ord < len(choices) {
		return ord, true
	}
	return 0, false
}

func normalizeSpeechTokens(s string) []string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	return strings.Fields(b.String())
}

func filterStopTokens(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		switch t {
		case "the", "a", "an", "to", "and", "or", "of", "in", "on", "at", "for", "with":
			continue
		}
		out = append(out, t)
	}
	return out
}

// SlugFromTitle derives a story id from its title.
func SlugFromTitle(title string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "story"
	}
	return s
}

// BGPrompt builds the pixel-art background prompt for a passage scene.
func BGPrompt(desc string) string {
	return `Create a cozy indie-game pixel art background scene (Stardew Valley or GBA style) depicting: ` + strings.TrimSpace(desc) +
		`. Use crisp pixel clusters, soft atmospheric lighting, and a harmonious limited palette. No characters, no creatures, no text, no UI, no borders, no frames.`
}
