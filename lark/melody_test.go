package main

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestSynthesizeMelody(t *testing.T) {
	notes := []string{"C5:120", "R:80", "F#4:120", "Bb4:120", "880:60"}
	pcm, err := synthesizeMelody(notes)
	if err != nil || len(pcm) != sampleRate {
		t.Fatalf("got %d bytes: %v", len(pcm), err)
	}
	endFirst := 120 * sampleRate / 500
	if binary.LittleEndian.Uint16(pcm) != 0 || binary.LittleEndian.Uint16(pcm[endFirst-2:]) != 0 ||
		!bytes.Equal(pcm[endFirst:200*sampleRate/500], make([]byte, 80*sampleRate/500)) {
		t.Fatal("note edges or rest are not silent")
	}
	peak := 0
	for sample := 0; sample < len(pcm); sample += 2 {
		v := int(int16(binary.LittleEndian.Uint16(pcm[sample:])))
		peak = max(peak, v, -v)
	}
	if peak < 11000 || peak > 15000 {
		t.Fatalf("peak %d out of range", peak)
	}
	for _, bad := range [][]string{nil, make([]string, 33), {"H4:100"}, {"D7:100"}, {"99:100"}, {"NaN:100"},
		{"C5:29"}, {"R:2001"}, {"R:2000", "R:2000", "R:2000", "R:30"}} {
		if _, err := synthesizeMelody(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestWSMelody(t *testing.T) {
	_, base := wsHarness(t, nil)
	id := "lark-melody"
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, id, "Cake")
	notes := []string{"C5:120", "R:80"}
	pcm, _ := synthesizeMelody(notes)
	check := func() {
		t.Helper()
		start := c.until(t, hasWS("tts", `"state":"start"`))
		audio := testObject(t, start.data)["audio"]
		var got []byte
		end := c.until(t, func(f wsFrame) bool {
			if f.op == wsBinary && len(f.data) > 1 && f.data[0] == pcmPrefix {
				got = append(got, f.data[1:]...)
			}
			return hasWS("tts", `"state":"end"`)(f)
		})
		if testObject(t, end.data)["audio"] != audio || !bytes.Equal(got, pcm) {
			t.Fatalf("bad melody playback (%d bytes)", len(got))
		}
		c.json(t, object{"ev": "tts_ack", "state": "done", "audio": audio})
	}
	toolTest(t, base, "emit", object{"to": id}, true)
	toolTest(t, base, "emit", object{"to": id, "notes": notes}, false)
	check()

}
