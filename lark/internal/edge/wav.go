package edge

import (
	"encoding/binary"
	"errors"
	"os"
)

// readWAVPCM decodes a RIFF/WAVE file into 16 kHz mono little-endian PCM.
// It accepts 16-bit PCM at 16 kHz (the edge's capture format) and rejects
// anything else rather than silently feeding the wrong rate to STT.
func readWAVPCM(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < 44 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		return nil, errors.New("not a WAV file")
	}
	var (
		channels  uint16
		rate      uint32
		bits      uint16
		data      []byte
		foundFmt  bool
		foundData bool
	)
	for off := 12; off+8 <= len(raw); {
		id := string(raw[off : off+4])
		size := int(binary.LittleEndian.Uint32(raw[off+4 : off+8]))
		body := off + 8
		if body+size > len(raw) {
			size = len(raw) - body
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return nil, errors.New("short WAV fmt chunk")
			}
			format := binary.LittleEndian.Uint16(raw[body:])
			channels = binary.LittleEndian.Uint16(raw[body+2:])
			rate = binary.LittleEndian.Uint32(raw[body+4:])
			bits = binary.LittleEndian.Uint16(raw[body+14:])
			if format != 1 || bits != 16 {
				return nil, errors.New("only 16-bit PCM WAV is supported")
			}
			foundFmt = true
		case "data":
			data = raw[body : body+size]
			foundData = true
		}
		off = body + size
		if size&1 == 1 {
			off++
		}
	}
	if !foundFmt || !foundData {
		return nil, errors.New("WAV missing fmt or data chunk")
	}
	if rate != sampleRate {
		return nil, errors.New("WAV must be 16 kHz")
	}
	if channels == 1 {
		return data, nil
	}
	if channels != 2 {
		return nil, errors.New("WAV must be mono or stereo")
	}
	mono := make([]byte, len(data)/2)
	for i := range len(mono) / 2 {
		l := int16(binary.LittleEndian.Uint16(data[4*i:]))
		r := int16(binary.LittleEndian.Uint16(data[4*i+2:]))
		binary.LittleEndian.PutUint16(mono[2*i:], uint16(int16((int32(l)+int32(r))/2)))
	}
	return mono, nil
}
