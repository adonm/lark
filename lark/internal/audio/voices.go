package audio

import "os"

// Voice is one Kokoro speaker. SIDs match the speaker2id metadata in the
// kokoro-multi-lang-v1_0 model pack.
type Voice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Lang string `json:"lang"`
	SID  int    `json:"-"`
}

const (
	LilyVoice  = "bf_lily"
	FableVoice = "bm_fable"
	HeartVoice = "af_heart"
)

var Catalog = [...]Voice{
	{HeartVoice, "Heart", "en-us", 3},
	{FableVoice, "Fable", "en-gb", 25},
	{LilyVoice, "Lily", "en-gb", 23},
	{"bm_george", "George", "en-gb", 26},
	{"af_bella", "Bella", "en-us", 2},
	{"am_puck", "Puck", "en-us", 18},
	{"bf_alice", "Alice", "en-gb", 20},
	{"bf_isabella", "Isabella", "en-gb", 22},
}

// ByID returns the catalog entry for id, or the zero Voice.
func ByID(id string) Voice {
	for _, voice := range Catalog {
		if voice.ID == id {
			return voice
		}
	}
	return Voice{}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
