//go:build live

package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestVirtualE2E(t *testing.T) {
	switch mode := os.Getenv("LARK_E2E_MODE"); mode {
	case "stub", "real":
		s := startE2E(t, mode == "real")
		t.Run("IdleWake", func(t *testing.T) {
			t.Parallel()
			c := s.board(t, "esp-vocat", "Idle")
			s.idleWake(t, c, time.Now())
		})
	default:
		t.Skip("use scripts/check.sh --live or --e2e-real")
	}
}

func e2eFields(f wsFrame, op string, pairs ...string) bool {
	if f.op != wsBinary {
		return false
	}
	mt, _, p, ok := unpackProtoPacket(f.data)
	if !ok {
		return false
	}
	switch op {
	case "chime":
		if mt != msgChime || len(p) < 1 {
			return false
		}
		if len(pairs) >= 2 && pairs[0] == "kind" {
			var want uint8
			switch strings.Trim(pairs[1], `"`) {
			case "tap":
				want = chimeTap
			case "cancel":
				want = chimeCancel
			case "ring":
				want = chimeRing
			case "intercom":
				want = chimeIntercom
			case "think":
				want = chimeThink
			}
			return p[0] == want
		}
		return true
	case "rec":
		if mt != msgRecCtrl || len(p) < 5 {
			return false
		}
		for i := 0; i < len(pairs); i += 2 {
			if pairs[i] == "action" && strings.Trim(pairs[i+1], `"`) == "start" && p[0] != recOpStart {
				return false
			}
			if pairs[i] == "action" && strings.Trim(pairs[i+1], `"`) == "stop" && p[0] != recOpStop && p[0] != recOpCancel {
				return false
			}
			if pairs[i] == "cancelled" && pairs[i+1] == "true" && p[0] != recOpCancel {
				return false
			}
			if pairs[i] == "endpoint" && p[1]&recTargetBridgeE == 0 {
				return false
			}
		}
		return true
	case "tts":
		if mt != msgTTSCtrl || len(p) < 9 {
			return false
		}
		for i := 0; i < len(pairs); i += 2 {
			if pairs[i] == "state" {
				st := strings.Trim(pairs[i+1], `"`)
				if st == "start" && p[0] != ttsOpStart || st == "end" && p[0] != ttsOpEnd || st == "abort" && p[0] != ttsOpStop {
					return false
				}
			}
			if pairs[i] == "audio" && fmt.Sprint(binary.LittleEndian.Uint64(p[1:9])) != pairs[i+1] {
				return false
			}
		}
		return true
	case "power":
		if mt != msgPower || len(p) < 1 {
			return false
		}
		if len(pairs) >= 2 && pairs[0] == "brightness" {
			return fmt.Sprint(p[0]) == pairs[1]
		}
		return true
	case "capabilities":
		return mt == msgWelcome && len(p) >= 2 && p[1]&1 != 0
	case "error":
		return mt == msgReject
	}
	return false
}

func (c *e2eCompanion) op(t *testing.T, op string, pairs ...string) wsFrame {
	t.Helper()
	return c.expect(t, op+" "+strings.Join(pairs, " "), func(f wsFrame) bool { return e2eFields(f, op, pairs...) })
}

func (s *e2eSuite) idleWake(t *testing.T, c *e2eCompanion, start time.Time) {
	c.timeout = 130 * time.Second
	c.op(t, "power", "brightness", "12")
	if elapsed := time.Since(start); elapsed < 118*time.Second || elapsed > 125*time.Second {
		t.Fatalf("unexpected idle sleep timing: %s", elapsed)
	} else {
		t.Logf("idle → panel-off power %s", elapsed.Round(time.Millisecond))
	}
	c.syncFrames(t)
	start = time.Now()
	c.input(t, "tap", 180, 145, c.revision)
	c.op(t, "power", "brightness", "85")
	c.sendProto(t, msgPing, []byte("wake"))
	c.expect(t, "wake without also starting Talk", func(f wsFrame) bool {
		if e2eFields(f, "rec", "action", `"start"`) {
			t.Fatal("wake tap also started recording")
		}
		return hasProto(msgPong, "wake")(f)
	})
	t.Logf("wake touch → active power %s", time.Since(start).Round(time.Millisecond))
}
