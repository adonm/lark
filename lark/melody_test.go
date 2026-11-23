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

func TestTerminalMelody(t *testing.T) {
	_, base := wsHarness(t, nil)
	id := "cake"
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, "vocat-0001", "Cake")
	notes := []string{"C5:120", "R:80"}
	pcm, _ := synthesizeMelody(notes)
	check := func() {
		t.Helper()
		start := c.until(t, func(f protocolFrame) bool {
			p := framePayload(f)
			return hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == ttsOpStart
		})
		audio := binary.LittleEndian.Uint64(framePayload(start)[1:9])
		var got []byte
		end := c.until(t, func(f protocolFrame) bool {
			if isPlayback(f) {
				got = append(got, framePayload(f)...)
			}
			p := framePayload(f)
			return hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == ttsOpEnd
		})
		endAudio := binary.LittleEndian.Uint64(framePayload(end)[1:9])
		if endAudio != audio || !bytes.Equal(got, pcm) {
			t.Fatalf("bad melody playback (%d bytes)", len(got))
		}
		c.sendProto(t, msgTTSAck, encodeTTSAck(false, audio))
	}
	toolTest(t, base, "emit", object{"to": id}, true)
	toolTest(t, base, "emit", object{"to": id, "notes": notes}, false)
	check()

}
