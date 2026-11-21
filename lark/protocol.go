package main

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
)

const (
	jsonType   = "application/json"
	pcmType    = "audio/pcm"
	maxJSON    = 16 << 10
	maxMessage = 400
	maxPeerPCM = 1536 << 10
)

type apiError struct {
	status int
	text   string
}

func (e *apiError) Error() string               { return e.text }
func problem(status int, text string) *apiError { return &apiError{status, text} }

type object = map[string]any

func decodeObject(raw []byte) (object, error) {
	if !json.Valid(raw) {
		return nil, problem(400, "Invalid JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj object
	if dec.Decode(&obj) != nil || obj == nil {
		return nil, problem(400, "Expected a JSON object")
	}
	return obj, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func truncate(s string, n int) string {
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}

func shortText(v any, limit int, plain bool) string {
	s := str(v)
	if plain {
		s = plainText(s)
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s)
	return truncate(strings.TrimSpace(s), limit)
}

var (
	hexColor = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
	targetRE = regexp.MustCompile(`^[\pL\pN_ .'’-]{1,64}$`)
	fenced   = regexp.MustCompile("(?ms)^[ \t]*```[^\n]*(?:\n|\\z).*?(^[ \t]*```[ \t]*\r?$|\\z)")
)

func color(v any) string {
	if s := str(v); hexColor.MatchString(s) {
		return strings.ToUpper(s)
	}
	return ""
}

func cardLines(text string) []string {
	return wrapCardLines(text, 27, 3)
}

func wrapCardLines(text string, width, count int) []string {
	var lines []string
	line := ""
words:
	for word := range strings.FieldsSeq(text) {
		for word != "" {
			n := width - runeLen(line)
			if line != "" {
				n--
				if runeLen(word) > n {
					lines = append(lines, line)
					if len(lines) == count {
						last := &lines[count-1]
						if runeLen(*last) == width {
							if i := strings.LastIndexByte(*last, ' '); i >= 0 {
								*last = (*last)[:i]
							} else {
								*last = truncate(*last, width-1)
							}
						}
						*last += "…"
						break words
					}
					line = ""
					continue
				}
				line += " "
			}
			chunk := truncate(word, n)
			line += chunk
			word = word[len(chunk):]
		}
	}
	if len(lines) < count && line != "" {
		lines = append(lines, line)
	}
	return lines
}
