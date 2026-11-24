package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type rawStoryPassage struct {
	name   string
	bg     string
	end    bool
	lineNo int
	lines  []rawStoryLine
}

type rawStoryLine struct {
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

func parseTwee(src string) (*compiledStory, error) {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	rawLines := strings.Split(src, "\n")

	var rawPassages []rawStoryPassage
	var cur *rawStoryPassage
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
			rawPassages = append(rawPassages, rawStoryPassage{name: name, bg: bg, end: end, lineNo: lineNo})
			cur = &rawPassages[len(rawPassages)-1]
			continue
		}
		if cur == nil {
			if strings.TrimSpace(line) == "" {
				continue
			}
			return nil, fmt.Errorf("line %d: content outside passage", lineNo)
		}
		cur.lines = append(cur.lines, rawStoryLine{lineNo: lineNo, text: line})
	}

	var title string
	var startName string
	var storyRaw []rawStoryPassage
	seenNames := map[string]bool{}

	for _, rp := range rawPassages {
		if seenNames[rp.name] {
			return nil, fmt.Errorf("line %d: duplicate passage %q", rp.lineNo, rp.name)
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
				return nil, fmt.Errorf("line %d: StoryTitle cannot be empty", rp.lineNo)
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
					return nil, fmt.Errorf("line %d: invalid StoryData JSON: %v", rp.lineNo, err)
				}
				if s, ok := data["start"].(string); ok {
					startName = strings.TrimSpace(s)
				}
			}
		case "StoryIncludes", "StoryStylesheet", "StoryScript":
			return nil, fmt.Errorf("line %d: unsupported Twee special passage %q", rp.lineNo, rp.name)
		default:
			storyRaw = append(storyRaw, rp)
		}
	}

	if title == "" {
		return nil, errors.New("missing StoryTitle passage")
	}
	if len(storyRaw) == 0 {
		return nil, errors.New("story has no passages")
	}
	if len(storyRaw) > maxStoryPassages {
		return nil, fmt.Errorf("story has %d passages (max %d)", len(storyRaw), maxStoryPassages)
	}

	cs := &compiledStory{
		Title:    title,
		Passages: make([]storyPassage, len(storyRaw)),
		ByName:   make(map[string]int, len(storyRaw)),
		FlagIdx:  make(map[string]uint16),
	}
	for i, rp := range storyRaw {
		cs.ByName[rp.name] = i
		cs.Passages[i] = storyPassage{Name: rp.name, BG: rp.bg, End: rp.end}
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

	internFlag := func(name string, lineNo int) (uint16, error) {
		if idx, ok := cs.FlagIdx[name]; ok {
			return idx, nil
		}
		if len(cs.Flags) >= maxStoryFlags {
			return 0, fmt.Errorf("line %d: too many flags (max %d)", lineNo, maxStoryFlags)
		}
		idx := uint16(len(cs.Flags))
		cs.Flags = append(cs.Flags, name)
		cs.FlagIdx[name] = idx
		return idx, nil
	}

	type ifFrame struct {
		ifNotOp int
		jumpOp  int
		hasElse bool
		lineNo  int
	}

	for pIdx, rp := range storyRaw {
		var ops []storyOp
		var stack []ifFrame
		linkCount := 0
		textLen := 0

		for _, rl := range rp.lines {
			if containsHTML(rl.text) {
				return nil, fmt.Errorf("line %d: HTML tags are not allowed", rl.lineNo)
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
						ops = append(ops, storyOp{kind: opText, text: chunk})
						textLen += len(chunk)
						emittedContentOnLine = true
					}
					break
				}
				if nextIdx > pos {
					chunk := line[pos:nextIdx]
					ops = append(ops, storyOp{kind: opText, text: chunk})
					textLen += len(chunk)
					emittedContentOnLine = true
				}
				if kind == 1 {
					end := strings.Index(line[nextIdx+2:], ">>")
					if end < 0 {
						return nil, fmt.Errorf("line %d: unclosed macro <<", rl.lineNo)
					}
					body := strings.TrimSpace(line[nextIdx+2 : nextIdx+2+end])
					pos = nextIdx + 2 + end + 2

					switch {
					case strings.HasPrefix(body, "set "):
						rest := strings.TrimSpace(body[4:])
						if !strings.HasPrefix(rest, "$") {
							return nil, fmt.Errorf("line %d: <<set>> variable must start with $", rl.lineNo)
						}
						toIdx := strings.Index(rest, " to ")
						if toIdx < 0 {
							return nil, fmt.Errorf("line %d: <<set>> must use 'to' (e.g. <<set $flag to true>>)", rl.lineNo)
						}
						flagName := strings.TrimSpace(rest[1:toIdx])
						if !isValidFlagName(flagName) {
							return nil, fmt.Errorf("line %d: invalid flag name %q", rl.lineNo, flagName)
						}
						valStr := strings.TrimSpace(rest[toIdx+4:])
						var val storyVal
						switch valStr {
						case "true":
							val = storyVal{Bool: true}
						case "false":
							val = storyVal{Bool: false}
						default:
							num, err := strconv.ParseFloat(valStr, 64)
							if err != nil {
								return nil, fmt.Errorf("line %d: <<set>> value %q must be true, false, or a number", rl.lineNo, valStr)
							}
							val = storyVal{IsNum: true, Num: num}
						}
						fIdx, err := internFlag(flagName, rl.lineNo)
						if err != nil {
							return nil, err
						}
						ops = append(ops, storyOp{kind: opSet, flag: fIdx, val: val})

					case strings.HasPrefix(body, "if "):
						rest := strings.TrimSpace(body[3:])
						if !strings.HasPrefix(rest, "$") {
							return nil, fmt.Errorf("line %d: <<if>> variable must start with $", rl.lineNo)
						}
						flagName := strings.TrimSpace(rest[1:])
						if !isValidFlagName(flagName) {
							return nil, fmt.Errorf("line %d: invalid <<if>> condition %q", rl.lineNo, rest)
						}
						fIdx, err := internFlag(flagName, rl.lineNo)
						if err != nil {
							return nil, err
						}
						opIdx := len(ops)
						ops = append(ops, storyOp{kind: opIfNot, flag: fIdx})
						stack = append(stack, ifFrame{ifNotOp: opIdx, jumpOp: -1, lineNo: rl.lineNo})

					case body == "else":
						if len(stack) == 0 {
							return nil, fmt.Errorf("line %d: <<else>> without matching <<if>>", rl.lineNo)
						}
						top := &stack[len(stack)-1]
						if top.hasElse {
							return nil, fmt.Errorf("line %d: duplicate <<else>>", rl.lineNo)
						}
						top.hasElse = true
						top.jumpOp = len(ops)
						ops = append(ops, storyOp{kind: opJump})
						ops[top.ifNotOp].target = len(ops)

					case body == "endif":
						if len(stack) == 0 {
							return nil, fmt.Errorf("line %d: <<endif>> without matching <<if>>", rl.lineNo)
						}
						top := stack[len(stack)-1]
						stack = stack[:len(stack)-1]
						if top.hasElse {
							ops[top.jumpOp].target = len(ops)
						} else {
							ops[top.ifNotOp].target = len(ops)
						}

					default:
						return nil, fmt.Errorf("line %d: unsupported macro <<%s>>", rl.lineNo, body)
					}
				} else {
					end := strings.Index(line[nextIdx+2:], "]]")
					if end < 0 {
						return nil, fmt.Errorf("line %d: unclosed link [[", rl.lineNo)
					}
					body := strings.TrimSpace(line[nextIdx+2 : nextIdx+2+end])
					pos = nextIdx + 2 + end + 2
					if body == "" {
						return nil, fmt.Errorf("line %d: empty link [[]]", rl.lineNo)
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
						return nil, fmt.Errorf("line %d: invalid link [[%s]]", rl.lineNo, body)
					}
					targetIdx, ok := cs.ByName[target]
					if !ok {
						return nil, fmt.Errorf("line %d: link target %q does not exist", rl.lineNo, target)
					}
					linkCount++
					ops = append(ops, storyOp{kind: opChoice, text: label, target: targetIdx})
				}
			}
			if emittedContentOnLine || len(rp.lines) > 1 {
				if emittedContentOnLine {
					ops = append(ops, storyOp{kind: opText, text: "\n"})
					textLen++
				} else if strings.TrimSpace(line) == "" && textLen > 0 {
					ops = append(ops, storyOp{kind: opText, text: "\n"})
					textLen++
				}
			}
		}

		if len(stack) > 0 {
			return nil, fmt.Errorf("line %d: unclosed <<if>> in passage %q", stack[len(stack)-1].lineNo, rp.name)
		}
		if linkCount > maxStoryChoices {
			return nil, fmt.Errorf("passage %q has %d links (max %d)", rp.name, linkCount, maxStoryChoices)
		}
		if textLen > maxStoryPassageText {
			return nil, fmt.Errorf("passage %q text is %d characters (max %d)", rp.name, textLen, maxStoryPassageText)
		}
		if rp.end && linkCount > 0 {
			return nil, fmt.Errorf("ending passage %q must not have links", rp.name)
		}
		if !rp.end && linkCount == 0 {
			return nil, fmt.Errorf("passage %q is a dead end (add a link or {\"end\": true})", rp.name)
		}
		cs.Passages[pIdx].Ops = ops
	}

	// Reachability check from Start.
	visited := make([]bool, len(cs.Passages))
	queue := []int{cs.Start}
	visited[cs.Start] = true
	for len(queue) > 0 {
		curIdx := queue[0]
		queue = queue[1:]
		for _, op := range cs.Passages[curIdx].Ops {
			if op.kind == opChoice && !visited[op.target] {
				visited[op.target] = true
				queue = append(queue, op.target)
			}
		}
	}
	for i, p := range cs.Passages {
		if !visited[i] {
			return nil, fmt.Errorf("passage %q is unreachable from %q", p.Name, cs.Passages[cs.Start].Name)
		}
	}

	return cs, nil
}
