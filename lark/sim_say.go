package main

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"strings"
	"time"
)

func simSay(s *simTerminal, ctx context.Context, line string, fields []string, out object) error {
	if len(fields) < 2 {
		return errors.New("usage: say <wav|text>")
	}
	arg := strings.TrimSpace(strings.TrimPrefix(line, "say"))
	var pcm []byte
	var err error
	if strings.HasSuffix(strings.ToLower(arg), ".wav") {
		pcm, err = readWAVPCM16k(arg)
	} else if _, statErr := os.Stat(arg); statErr == nil {
		pcm, err = readWAVPCM16k(arg)
	} else {
		pcm, err = s.ttsWAV(ctx, arg)
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	active := s.recActive
	s.mu.Unlock()
	if !active {
		s.recSess++
		b := binary.LittleEndian.AppendUint16(nil, s.recSess)
		b = appendStr8(b, "")
		if err := s.sendProto(msgRecStart, b); err != nil {
			return err
		}
		if !s.waitFor(2*time.Second, func() bool { return s.recActive }) {
			return errors.New("timed out waiting for REC_CTRL start")
		}
	}
	pkt := make([]byte, rtpPacketBytes)
	sentBytes := 0
	for off := 0; off < len(pcm); off += rtpPCMBytes {
		s.mu.Lock()
		stillActive := s.recActive
		s.mu.Unlock()
		if !stillActive {
			break
		}
		var chunk [rtpPCMBytes]byte
		copy(chunk[:], pcm[off:min(len(pcm), off+rtpPCMBytes)])
		s.rtpSeq++
		s.rtpTS += rtpSamples
		n := encodeRTPPacket(pkt, rtpPTRecPCM, s.rtpSeq, s.rtpTS, s.ssrc, chunk[:])
		if _, err := s.udp.WriteToUDP(pkt[:n], s.srvUDP); err != nil {
			return err
		}
		sentBytes += rtpPCMBytes
		time.Sleep(20 * time.Millisecond)
	}
	s.mu.Lock()
	s.speechEndAt = time.Now()
	stillActive := s.recActive
	bridgeEP := s.recBridgeEP
	s.mu.Unlock()
	if stillActive && bridgeEP {
		// Feed silence tail so Silero endpoint detects end-of-speech. The
		// bridge's cap is 1.2 s, so leave a little margin.
		var silence [rtpPCMBytes]byte
		for range 90 {
			s.mu.Lock()
			ok := s.recActive
			s.mu.Unlock()
			if !ok {
				break
			}
			s.rtpSeq++
			s.rtpTS += rtpSamples
			n := encodeRTPPacket(pkt, rtpPTRecPCM, s.rtpSeq, s.rtpTS, s.ssrc, silence[:])
			_, _ = s.udp.WriteToUDP(pkt[:n], s.srvUDP)
			time.Sleep(20 * time.Millisecond)
		}
	}
	s.mu.Lock()
	stillActive = s.recActive
	s.mu.Unlock()
	if stillActive {
		b := binary.LittleEndian.AppendUint16(nil, s.recSess)
		b = append(b, 0)
		b = appendStr8(b, "")
		_ = s.sendProto(msgRecEnd, b)
	}
	out["bytes"] = sentBytes
	return nil
}
