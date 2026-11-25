package stories

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type rawPassage struct {
	name   string
	bg     string
	end    bool
	lineNo int
	lines  []rawLine
}

type rawLine struct {
	lineNo int
	text   string
}

func isValidFlagName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_') {
				return false
			}
		} else {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_') {
				return false
			}
		}
	}
	return true
}

func containsHTML(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '<' {
			continue
		}
		if i+1 < len(s) && s[i+1] == '<' {
			i++
			continue
		}
		if i+1 < len(s) {
			next := s[i+1]
			if (next >= 'a' && next <= 'z') || (next >= 'A' && next <= 'Z') || next == '/' || next == '!' {
				return true
			}
		}
	}
	return false
}

func parsePassageHeader(line string, lineNo int) (name, bg string, end bool, err error) {
	rest := strings.TrimSpace(line[2:])
	if rest == "" {
		return "", "", false, fmt.Errorf("line %d: empty passage name", lineNo)
	}
	if idx := strings.IndexByte(rest, '{'); idx >= 0 {
		name = strings.TrimSpace(rest[:idx])
		metaStr := strings.TrimSpace(rest[idx:])
		var meta map[string]any
		dec := json.NewDecoder(strings.NewReader(metaStr))
		dec.UseNumber()
		if err := dec.Decode(&meta); err != nil {
			return "", "", false, fmt.Errorf("line %d: invalid passage metadata JSON: %v", lineNo, err)
		}
		for k, v := range meta {
			switch k {
			case "bg":
				s, ok := v.(string)
				if !ok {
					return "", "", false, fmt.Errorf("line %d: metadata bg must be a string", lineNo)
				}
				bg = strings.TrimSpace(s)
				if len(bg) > 160 {
					return "", "", false, fmt.Errorf("line %d: metadata bg exceeds 160 characters", lineNo)
				}
			case "end":
				b, ok := v.(bool)
				if !ok {
					return "", "", false, fmt.Errorf("line %d: metadata end must be a boolean", lineNo)
				}
				end = b
			default:
				return "", "", false, fmt.Errorf("line %d: unsupported metadata key %q", lineNo, k)
			}
		}
	} else {
		name = rest
	}
	if name == "" {
		return "", "", false, fmt.Errorf("line %d: empty passage name", lineNo)
	}
	if strings.ContainsAny(name, "[]") {
		return "", "", false, fmt.Errorf("line %d: invalid passage name %q", lineNo, name)
	}
	return name, bg, end, nil
}

// lexTwee splits the source into passages and validates each header. Lines
// before the first header must be blank.
func lexTwee(src string) ([]rawPassage, error) {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	rawLines := strings.Split(src, "\n")

	var rawPassages []rawPassage
	var cur *rawPassage
	for i, line := range rawLines {
		lineNo := i + 1
		if strings.HasPrefix(line, "::") {
			if len(line) < 3 || line[2] != ' ' {
				return nil, fmt.Errorf("line %d: passage header must start with \":: \"", lineNo)
			}
			name, bg, end, err := parsePassageHeader(line, lineNo)
			if err != nil {
				return nil, err
			}
			rawPassages = append(rawPassages, rawPassage{name: name, bg: bg, end: end, lineNo: lineNo})
			cur = &rawPassages[len(rawPassages)-1]
			continue
		}
		if cur == nil {
			if strings.TrimSpace(line) == "" {
				continue
			}
			return nil, fmt.Errorf("line %d: content outside passage", lineNo)
		}
		cur.lines = append(cur.lines, rawLine{lineNo: lineNo, text: line})
	}
	return rawPassages, nil
}

// collectStoryPassages separates StoryTitle and StoryData from the story's
// own passages and rejects special passages the runtime does not support.
func collectStoryPassages(rawPassages []rawPassage) (title, startName string, storyRaw []rawPassage, err error) {
	seenNames := map[string]bool{}
	for _, rp := range rawPassages {
		if seenNames[rp.name] {
			return "", "", nil, fmt.Errorf("line %d: duplicate passage %q", rp.lineNo, rp.name)
		}
		seenNames[rp.name] = true

		switch rp.name {
		case "StoryTitle":
			var parts []string
			for _, l := range rp.lines {
				parts = append(parts, l.text)
			}
			title = strings.TrimSpace(strings.Join(parts, "\n"))
			if title == "" {
				return "", "", nil, fmt.Errorf("line %d: StoryTitle cannot be empty", rp.lineNo)
			}
		case "StoryData":
			var parts []string
			for _, l := range rp.lines {
				parts = append(parts, l.text)
			}
			body := strings.TrimSpace(strings.Join(parts, "\n"))
			if body != "" {
				var data map[string]any
				if err := json.Unmarshal([]byte(body), &data); err != nil {
					return "", "", nil, fmt.Errorf("line %d: invalid StoryData JSON: %v", rp.lineNo, err)
				}
				if s, ok := data["start"].(string); ok {
					startName = strings.TrimSpace(s)
				}
			}
		case "StoryIncludes", "StoryStylesheet", "StoryScript":
			return "", "", nil, fmt.Errorf("line %d: unsupported Twee special passage %q", rp.lineNo, rp.name)
		default:
			storyRaw = append(storyRaw, rp)
		}
	}
	return title, startName, storyRaw, nil
}

// Parse compiles Twee 3 source into a validated story.
func Parse(src string) (*Story, error) {
	rawPassages, err := lexTwee(src)
	if err != nil {
		return nil, err
	}
	title, startName, storyRaw, err := collectStoryPassages(rawPassages)
	if err != nil {
		return nil, err
	}

	if title == "" {
		return nil, errors.New("missing StoryTitle passage")
	}
	if len(storyRaw) == 0 {
		return nil, errors.New("story has no passages")
	}
	if len(storyRaw) > MaxPassages {
		return nil, fmt.Errorf("story has %d passages (max %d)", len(storyRaw), MaxPassages)
	}

	cs := &Story{
		Title:    title,
		Passages: make([]Passage, len(storyRaw)),
		ByName:   make(map[string]int, len(storyRaw)),
		FlagIdx:  make(map[string]uint16),
	}
	for i, rp := range storyRaw {
		cs.ByName[rp.name] = i
		cs.Passages[i] = Passage{Name: rp.name, BG: rp.bg, End: rp.end}
	}

	if startName != "" {
		idx, ok := cs.ByName[startName]
		if !ok {
			return nil, fmt.Errorf("StoryData start passage %q does not exist", startName)
		}
		cs.Start = idx
	} else if idx, ok := cs.ByName["Start"]; ok {
		cs.Start = idx
	} else {
		cs.Start = 0
	}

	for pIdx, rp := range storyRaw {
		ops, linkCount, textLen, err := compilePassage(cs, rp)
		if err != nil {
			return nil, err
		}
		if linkCount > MaxChoices {
			return nil, fmt.Errorf("passage %q has %d links (max %d)", rp.name, linkCount, MaxChoices)
		}
		if textLen > MaxPassageText {
			return nil, fmt.Errorf("passage %q text is %d characters (max %d)", rp.name, textLen, MaxPassageText)
		}
		if rp.end && linkCount > 0 {
			return nil, fmt.Errorf("ending passage %q must not have links", rp.name)
		}
		if !rp.end && linkCount == 0 {
			return nil, fmt.Errorf("passage %q is a dead end (add a link or {\"end\": true})", rp.name)
		}
		cs.Passages[pIdx].Ops = ops
	}

	if err := validateReachable(cs); err != nil {
		return nil, err
	}
	return cs, nil
}

// internFlag returns the index of a story flag, adding it on first use.
func (cs *Story) internFlag(name string, lineNo int) (uint16, error) {
	if idx, ok := cs.FlagIdx[name]; ok {
		return idx, nil
	}
	if len(cs.Flags) >= MaxFlags {
		return 0, fmt.Errorf("line %d: too many flags (max %d)", lineNo, MaxFlags)
	}
	idx := uint16(len(cs.Flags))
	cs.Flags = append(cs.Flags, name)
	cs.FlagIdx[name] = idx
	return idx, nil
}

type tweeIfFrame struct {
	ifNotOp int
	jumpOp  int
	hasElse bool
	lineNo  int
}

// compilePassage turns one passage's raw lines into runtime ops and returns
// the link and text lengths used by the size limits.
func compilePassage(cs *Story, rp rawPassage) ([]op, int, int, error) {
	var ops []op
	var stack []tweeIfFrame
	linkCount := 0
	textLen := 0

	for _, rl := range rp.lines {
		if containsHTML(rl.text) {
			return nil, 0, 0, fmt.Errorf("line %d: HTML tags are not allowed", rl.lineNo)
		}
		line := rl.text
		emittedContentOnLine := false
		pos := 0
		for pos < len(line) {
			nextMacro := strings.Index(line[pos:], "<<")
			nextLink := strings.Index(line[pos:], "[[")
			nextIdx := -1
			kind := 0 // 1=macro, 2=link
			if nextMacro >= 0 && (nextLink < 0 || nextMacro < nextLink) {
				nextIdx = pos + nextMacro
				kind = 1
			} else if nextLink >= 0 {
				nextIdx = pos + nextLink
				kind = 2
			}
			if nextIdx < 0 {
				chunk := line[pos:]
				if chunk != "" {
					ops = append(ops, op{kind: opText, text: chunk})
					textLen += len(chunk)
					emittedContentOnLine = true
				}
				break
			}
			if nextIdx > pos {
				chunk := line[pos:nextIdx]
				ops = append(ops, op{kind: opText, text: chunk})
				textLen += len(chunk)
				emittedContentOnLine = true
			}
			if kind == 1 {
				end := strings.Index(line[nextIdx+2:], ">>")
				if end < 0 {
					return nil, 0, 0, fmt.Errorf("line %d: unclosed macro <<", rl.lineNo)
				}
				body := strings.TrimSpace(line[nextIdx+2 : nextIdx+2+end])
				pos = nextIdx + 2 + end + 2
				var macroErr error
				ops, stack, macroErr = cs.macroOps(body, ops, stack, rl.lineNo)
				if macroErr != nil {
					return nil, 0, 0, macroErr
				}
			} else {
				end := strings.Index(line[nextIdx+2:], "]]")
				if end < 0 {
					return nil, 0, 0, fmt.Errorf("line %d: unclosed link [[", rl.lineNo)
				}
				body := strings.TrimSpace(line[nextIdx+2 : nextIdx+2+end])
				pos = nextIdx + 2 + end + 2
				chOp, err := cs.linkOp(body, rl.lineNo)
				if err != nil {
					return nil, 0, 0, err
				}
				linkCount++
				ops = append(ops, chOp)
			}
		}
		if emittedContentOnLine || len(rp.lines) > 1 {
			if emittedContentOnLine {
				ops = append(ops, op{kind: opText, text: "\n"})
				textLen++
			} else if strings.TrimSpace(line) == "" && textLen > 0 {
				ops = append(ops, op{kind: opText, text: "\n"})
				textLen++
			}
		}
	}

	if len(stack) > 0 {
		return nil, 0, 0, fmt.Errorf("line %d: unclosed <<if>> in passage %q", stack[len(stack)-1].lineNo, rp.name)
	}
	return ops, linkCount, textLen, nil
}

// macroOps appends the op for one <<...>> body and keeps the <<if>> stack in
// sync. It returns the updated slices because append can reallocate.
func (cs *Story) macroOps(body string, ops []op, stack []tweeIfFrame, lineNo int) ([]op, []tweeIfFrame, error) {
	switch {
	case strings.HasPrefix(body, "set "):
		rest := strings.TrimSpace(body[4:])
		if !strings.HasPrefix(rest, "$") {
			return nil, nil, fmt.Errorf("line %d: <<set>> variable must start with $", lineNo)
		}
		toIdx := strings.Index(rest, " to ")
		if toIdx < 0 {
			return nil, nil, fmt.Errorf("line %d: <<set>> must use 'to' (e.g. <<set $flag to true>>)", lineNo)
		}
		flagName := strings.TrimSpace(rest[1:toIdx])
		if !isValidFlagName(flagName) {
			return nil, nil, fmt.Errorf("line %d: invalid flag name %q", lineNo, flagName)
		}
		valStr := strings.TrimSpace(rest[toIdx+4:])
		var val Value
		switch valStr {
		case "true":
			val = Value{Bool: true}
		case "false":
			val = Value{Bool: false}
		default:
			num, err := strconv.ParseFloat(valStr, 64)
			if err != nil {
				return nil, nil, fmt.Errorf("line %d: <<set>> value %q must be true, false, or a number", lineNo, valStr)
			}
			val = Value{IsNum: true, Num: num}
		}
		fIdx, err := cs.internFlag(flagName, lineNo)
		if err != nil {
			return nil, nil, err
		}
		ops = append(ops, op{kind: opSet, flag: fIdx, val: val})

	case strings.HasPrefix(body, "if "):
		rest := strings.TrimSpace(body[3:])
		if !strings.HasPrefix(rest, "$") {
			return nil, nil, fmt.Errorf("line %d: <<if>> variable must start with $", lineNo)
		}
		flagName := strings.TrimSpace(rest[1:])
		if !isValidFlagName(flagName) {
			return nil, nil, fmt.Errorf("line %d: invalid <<if>> condition %q", lineNo, rest)
		}
		fIdx, err := cs.internFlag(flagName, lineNo)
		if err != nil {
			return nil, nil, err
		}
		opIdx := len(ops)
		ops = append(ops, op{kind: opIfNot, flag: fIdx})
		stack = append(stack, tweeIfFrame{ifNotOp: opIdx, jumpOp: -1, lineNo: lineNo})

	case body == "else":
		if len(stack) == 0 {
			return nil, nil, fmt.Errorf("line %d: <<else>> without matching <<if>>", lineNo)
		}
		top := &stack[len(stack)-1]
		if top.hasElse {
			return nil, nil, fmt.Errorf("line %d: duplicate <<else>>", lineNo)
		}
		top.hasElse = true
		top.jumpOp = len(ops)
		ops = append(ops, op{kind: opJump})
		ops[top.ifNotOp].target = len(ops)

	case body == "endif":
		if len(stack) == 0 {
			return nil, nil, fmt.Errorf("line %d: <<endif>> without matching <<if>>", lineNo)
		}
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if top.hasElse {
			ops[top.jumpOp].target = len(ops)
		} else {
			ops[top.ifNotOp].target = len(ops)
		}

	default:
		return nil, nil, fmt.Errorf("line %d: unsupported macro <<%s>>", lineNo, body)
	}
	return ops, stack, nil
}

// linkOp resolves one [[...]] body to a choice op.
func (cs *Story) linkOp(body string, lineNo int) (op, error) {
	if body == "" {
		return op{}, fmt.Errorf("line %d: empty link [[]]", lineNo)
	}
	var label, target string
	if arrow := strings.Index(body, "->"); arrow >= 0 {
		label = strings.TrimSpace(body[:arrow])
		target = strings.TrimSpace(body[arrow+2:])
	} else if pipe := strings.IndexByte(body, '|'); pipe >= 0 {
		label = strings.TrimSpace(body[:pipe])
		target = strings.TrimSpace(body[pipe+1:])
	} else {
		label = body
		target = body
	}
	if label == "" || target == "" {
		return op{}, fmt.Errorf("line %d: invalid link [[%s]]", lineNo, body)
	}
	targetIdx, ok := cs.ByName[target]
	if !ok {
		return op{}, fmt.Errorf("line %d: link target %q does not exist", lineNo, target)
	}
	return op{kind: opChoice, text: label, target: targetIdx}, nil
}

// validateReachable checks every passage is reachable from Start.
func validateReachable(cs *Story) error {
	visited := make([]bool, len(cs.Passages))
	queue := []int{cs.Start}
	visited[cs.Start] = true
	for len(queue) > 0 {
		curIdx := queue[0]
		queue = queue[1:]
		for _, o := range cs.Passages[curIdx].Ops {
			if o.kind == opChoice && !visited[o.target] {
				visited[o.target] = true
				queue = append(queue, o.target)
			}
		}
	}
	for i, p := range cs.Passages {
		if !visited[i] {
			return fmt.Errorf("passage %q is unreachable from %q", p.Name, cs.Passages[cs.Start].Name)
		}
	}
	return nil
}
