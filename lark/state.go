package main

import (
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	terminalIDRE  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	companionIDRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	sessionIDRE   = regexp.MustCompile(`^ses[A-Za-z0-9_-]{1,128}$`)
	characterRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._'’-]{0,39}$`)
	directRE      = regexp.MustCompile(`(?i)^(?:hey\s+[\pL\pN_-]+,?\s+)?(?:tell|message|intercom|walkie|say\s+to|send)\s+`)
	composeRE     = regexp.MustCompile(`(?i)^(?:about|(?:(?:an?|the|some)\s+(?:[\pL\pN_'-]+\s+){0,5})?(?:joke|story|fact|riddle|quiz))(?:[^\pL\pN_]|$)`)
	callPrefix    = regexp.MustCompile(`(?i)^(?:(?:hey(?: [\pL\pN_-]+,)?|please|(?:can|could|will|would) you)[, ]+)*`)
	callSuffix    = regexp.MustCompile(`(?i)[, ]+please$`)
	callRE        = regexp.MustCompile(`(?i)^(?:(?:call|ring) (.+)|give (.+) an? call)$`)
)

func runeLen(s string) int { return utf8.RuneCountInString(s) }

const (
	lilyVoice  = "bf_lily"
	fableVoice = "bm_fable"
	heartVoice = "af_heart"
)

type voiceChoice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Lang string `json:"lang"`
	SID  int    `json:"-"`
}

// SIDs match the speaker2id metadata in kokoro-multi-lang-v1_0/model.onnx.
var voiceCatalog = [...]voiceChoice{
	{heartVoice, "Heart", "en-us", 3},
	{fableVoice, "Fable", "en-gb", 25},
	{lilyVoice, "Lily", "en-gb", 23},
	{"bm_george", "George", "en-gb", 26},
	{"af_bella", "Bella", "en-us", 2},
	{"am_puck", "Puck", "en-us", 18},
	{"bf_alice", "Alice", "en-gb", 20},
	{"bf_isabella", "Isabella", "en-gb", 22},
}

func voiceByID(id string) voiceChoice {
	for _, voice := range voiceCatalog {
		if voice.ID == id {
			return voice
		}
	}
	return voiceChoice{}
}

func voiceName(id string) string { return voiceByID(id).Name }

func storedVoice(id string) string {
	switch {
	case id == "", voiceByID(id).ID != "":
		return id
	case strings.HasPrefix(id, "bf_"):
		return lilyVoice
	case strings.HasPrefix(id, "bm_"):
		return fableVoice
	default:
		return heartVoice
	}
}

type companionVoices struct {
	db       *larkDB
	fallback string
}

func companionVoiceStore(db *larkDB, fallback string) companionVoices {
	fallback = storedVoice(fallback)
	if fallback == "" {
		fallback = heartVoice
	}
	return companionVoices{db: db, fallback: fallback}
}

func (v companionVoices) allowed(voice string) bool {
	return voiceName(voice) != ""
}

func (v companionVoices) defaultFor(theme string) string {
	if theme == "mint" {
		return fableVoice
	}
	if v.fallback != "" {
		return v.fallback
	}
	return heartVoice
}

func (v companionVoices) get(id, theme string) string {
	id = strings.ToLower(id)
	var voice string
	if v.db != nil {
		_ = v.db.readDB().QueryRow(`SELECT voice FROM companions WHERE id=?`, id).Scan(&voice)
	}
	return v.resolve(voice, theme)
}

func (v companionVoices) resolve(voice, theme string) string {
	if voice := storedVoice(voice); voice != "" {
		return voice
	}
	return v.defaultFor(theme)
}

func (v companionVoices) save(id, voice string) error {
	if v.db == nil {
		return problem(503, "Voice settings unavailable")
	}
	res, err := v.db.Exec(`UPDATE companions SET voice=? WHERE id=?`, voice, strings.ToLower(id))
	if err != nil {
		return problem(502, "Cannot save voice")
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return problem(404, "Companion not found")
	}
	return nil
}

func lanIP(value string, loopback bool) (string, error) {
	ip, err := netip.ParseAddr(value)
	if err != nil {
		return "", problem(400, "Expected a numeric LAN IP address")
	}
	if ip.Is4In6() {
		return "", problem(400, "Peer address must be on the LAN")
	}
	if ip.IsPrivate() || (ip.Is4() && ip.IsLinkLocalUnicast()) || (loopback && ip.IsLoopback()) {
		return ip.String(), nil
	}
	return "", problem(400, "Peer address must be on the LAN")
}

type companionTarget struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type intercomMessage struct{ target, text string }

var broadcastNames = []string{"all", "everyone", "everybody"}

func broadcast(target string) bool {
	for _, name := range broadcastNames {
		if strings.EqualFold(target, name) {
			return true
		}
	}
	return false
}

func targets(others []companionTarget, target string) []companionTarget {
	if broadcast(target) {
		return others
	}
	var ids, names []companionTarget
	for _, c := range others {
		if strings.EqualFold(target, c.ID) {
			ids = append(ids, c)
		} else if strings.EqualFold(target, c.Name) {
			names = append(names, c)
		}
	}
	if len(ids) != 0 {
		return ids
	}
	return names
}

func directBody(text string) string {
	text = strings.TrimSpace(text)
	loc := directRE.FindStringIndex(text)
	if loc == nil {
		return ""
	}
	return text[loc[1]:]
}

func directMessage(others []companionTarget, text string) *intercomMessage {
	if text == "" {
		return nil
	}
	names := append(make([]string, 0, len(broadcastNames)+len(others)*2), broadcastNames...)
	for _, c := range others {
		names = append(names, c.Name, c.ID)
	}
	sort.SliceStable(names, func(i, j int) bool { return runeLen(names[i]) > runeLen(names[j]) })
	for _, name := range names {
		head := truncate(text, runeLen(name))
		if !strings.EqualFold(name, head) || len(head) == len(text) {
			continue
		}
		tail := text[len(head):]
		r, width := utf8.DecodeRuneInString(tail)
		switch {
		case unicode.IsSpace(r) || r == ',' || r == ':':
		case strings.ContainsRune("-–—", r):
			next, _ := utf8.DecodeRuneInString(tail[width:])
			if !unicode.IsSpace(next) {
				continue
			}
		default:
			continue
		}
		tail = strings.TrimSpace(strings.TrimLeft(tail, " \t\r\n,:-–—"))
		if tail != "" && !composeRE.MatchString(tail) {
			return &intercomMessage{head, tail}
		}
		return nil
	}
	return nil
}
