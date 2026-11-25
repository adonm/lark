package main

import (
	"fmt"
	"strings"

	"lark/internal/audio"
)

func melodyInput(raw any, path string) ([]string, []byte, error) {
	if s, ok := raw.(string); ok {
		list := strings.Fields(s)
		notes := make([]any, len(list))
		for i, n := range list {
			notes[i] = n
		}
		raw = notes
	}
	return parseMelody(raw, path)
}

func parseMelody(raw any, path string) ([]string, []byte, error) {
	var notes []string
	switch v := raw.(type) {
	case []any:
		if len(v) <= 32 {
			notes = make([]string, len(v))
			for i, token := range v {
				s, ok := token.(string)
				if !ok {
					return nil, nil, fieldError(fmt.Sprintf("%s[%d]", path, i), "expected PITCH:MS")
				}
				notes[i] = s
			}
		}
	case []string:
		notes = v
	}
	pcm, err := audio.SynthesizeMelody(notes)
	if err != nil {
		return nil, nil, fieldError(path, "%s", err)
	}
	return notes, pcm, nil
}
