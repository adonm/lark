package main

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"unicode"

	"lark/internal/audio"
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
		s = audio.PlainText(s)
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
	targetRE = regexp.MustCompile(`^[\pL\pN_ .'’-]{1,64}$`)
	fenced   = regexp.MustCompile("(?ms)^[ \t]*```[^\n]*(?:\n|\\z).*?(^[ \t]*```[ \t]*\r?$|\\z)")
)
