package main

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

var pgStart = regexp.MustCompile(`(?m)^\*\*\* ?START OF (?:THE|THIS) PROJECT GUTENBERG[^\n]*\n`)
var pgEnd = regexp.MustCompile(`(?m)^\*\*\* ?END OF (?:THE|THIS) PROJECT GUTENBERG[^\n]*\n?`)

func gutenbergBody(text string) string {
	if m := pgStart.FindStringIndex(text); m != nil {
		text = text[m[1]:]
	}
	if m := pgEnd.FindStringIndex(text); m != nil {
		text = text[:m[0]]
	}
	return strings.TrimSpace(text)
}

func libraryTwee(title, text string) (string, error) {
	title = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(title, "\n", " "), "::", ":"))
	if title == "" {
		title = "Library Story"
	}
	passages := splitLibraryPassages(text, libraryPassage)
	if len(passages) == 0 {
		passages = []string{"Once upon a time."}
	}
	if len(passages) > libraryPassages {
		passages = passages[:libraryPassages]
	}
	var out strings.Builder
	fmt.Fprintf(&out, ":: StoryTitle\n%s\n\n", title)
	for i, passage := range passages {
		if i+1 == len(passages) {
			fmt.Fprintf(&out, ":: Chapter %d {\"end\":true}\n%s\n", i+1, passage)
			continue
		}
		fmt.Fprintf(&out, ":: Chapter %d\n%s\n\n[[Continue->Chapter %d]]\n\n", i+1, passage, i+2)
	}
	twee := out.String()
	if _, err := parseTwee(twee); err != nil {
		return "", fieldError("twee", "generated story is invalid: %s", err)
	}
	return twee, nil
}

var tweeUnsafe = strings.NewReplacer("[", "(", "]", ")", "<", "‹", ">", "›")

func splitLibraryPassages(text string, maxRunes int) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	paragraphs := strings.Split(text, "\n\n")
	var passages []string
	for _, paragraph := range paragraphs {
		paragraph = strings.Join(strings.Fields(paragraph), " ")
		if paragraph == "" {
			continue
		}
		paragraph = tweeUnsafe.Replace(paragraph)
		for len([]rune(paragraph)) > maxRunes {
			cut := maxRunes
			runes := []rune(paragraph)
			for i := cut; i > maxRunes/2; i-- {
				if runes[i] == ' ' || runes[i] == '.' || runes[i] == '!' || runes[i] == '?' {
					cut = i + 1
					break
				}
			}
			passages = append(passages, strings.TrimSpace(string(runes[:cut])))
			paragraph = strings.TrimSpace(string(runes[cut:]))
		}
		if paragraph != "" {
			passages = append(passages, strings.TrimSpace(paragraph))
		}
	}
	return passages
}

var wikiComment = regexp.MustCompile(`(?s)<!--.*?-->`)
var wikiRef = regexp.MustCompile(`(?is)<ref[^>]*?/>|<ref[^>]*?>.*?</ref>`)
var wikiTag = regexp.MustCompile(`(?s)<[^>]*>`)
var wikiExternal = regexp.MustCompile(`\[(?:https?|ftp)://[^\s\]]+(?:\s+([^\]]*))?\]`)
var wikiHeading = regexp.MustCompile(`(?m)^={2,6}\s*(.*?)\s*={2,6}\s*$`)
var wikiMagic = regexp.MustCompile(`__[A-Z]+__`)

func wikiTailHeading(line string) bool {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "references", "external links", "see also", "notes", "further reading",
		"bibliography", "sources", "citations", "footnotes", "gallery", "media":
		return true
	}
	return false
}

// wikiPlainText is deliberately simple: drop templates, tables, files, and
// references, keep link labels and prose. Good enough for retrieval and read.
func wikiPlainText(source string) string {
	s := strings.ReplaceAll(source, "\r\n", "\n")
	s = wikiComment.ReplaceAllString(s, "")
	s = wikiRef.ReplaceAllString(s, "")
	lines := strings.Split(s, "\n")
	var out strings.Builder
	templateDepth, tableDepth := 0, 0
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "{|") {
			tableDepth++
			continue
		}
		if tableDepth > 0 {
			if strings.HasPrefix(strings.TrimSpace(line), "|}") {
				tableDepth--
			}
			continue
		}
		var kept strings.Builder
		for i := 0; i < len(line); i++ {
			if i+1 < len(line) && line[i] == '{' && line[i+1] == '{' {
				templateDepth++
				i++
				continue
			}
			if i+1 < len(line) && line[i] == '}' && line[i+1] == '}' && templateDepth > 0 {
				templateDepth--
				i++
				continue
			}
			if templateDepth > 0 {
				continue
			}
			kept.WriteByte(line[i])
		}
		line = kept.String()
		if templateDepth > 0 {
			continue
		}
		line = wikiHeading.ReplaceAllString(line, "$1")
		line = strings.TrimLeft(line, "*#:; ")
		if wikiTailHeading(line) {
			break
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	s = out.String()
	for {
		start := strings.Index(s, "[[")
		if start < 0 {
			break
		}
		end := strings.Index(s[start+2:], "]]")
		if end < 0 {
			break
		}
		body := s[start+2 : start+2+end]
		keep := body
		if pipe := strings.LastIndexByte(body, '|'); pipe >= 0 {
			keep = body[pipe+1:]
		}
		lower := strings.ToLower(strings.TrimSpace(body))
		if strings.HasPrefix(lower, "file:") || strings.HasPrefix(lower, "image:") ||
			strings.HasPrefix(lower, "category:") || strings.HasPrefix(lower, "template:") {
			keep = ""
		}
		s = s[:start] + keep + s[start+2+end+2:]
	}
	s = strings.ReplaceAll(s, "[[", "")
	s = strings.ReplaceAll(s, "]]", "")
	s = wikiExternal.ReplaceAllString(s, "$1")
	s = wikiTag.ReplaceAllString(s, "")
	s = wikiMagic.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "'''", "")
	s = strings.ReplaceAll(s, "''", "")
	s = html.UnescapeString(s)
	s = strings.Join(strings.Fields(s), " ")
	// Reflow into paragraphs at sentence boundaries so read/import can chunk.
	var b strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		b.WriteRune(r)
		if (r == '.' || r == '!' || r == '?') && i+2 < len(runes) && runes[i+1] == ' ' && runes[i+2] >= 'A' && runes[i+2] <= 'Z' && b.Len() > 240 {
			b.WriteString("\n\n")
		}
	}
	return strings.TrimSpace(b.String())
}
