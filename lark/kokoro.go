package main

import "lark/internal/audio"

// kokoroEngine adapts the audio package's Kokoro engine to the local
// speechEngine interface used by the synthesis worker and its test doubles.
type kokoroEngine struct{ *audio.Kokoro }

func (k kokoroEngine) generate(text string, voice audio.Voice, keepGoing func() bool) ([]float32, int) {
	return k.Generate(text, voice, keepGoing)
}

func (k kokoroEngine) close() { k.Close() }
