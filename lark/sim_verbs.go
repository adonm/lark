package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

func simAttach(s *simTerminal, ctx context.Context, line string, fields []string, out object) error {
	if len(fields) != 2 {
		return errors.New("usage: attach <companion>")
	}
	s.mu.Lock()
	prevFrames := s.frameCount
	s.mu.Unlock()
	if _, err := s.client.do(ctx, http.MethodPost, "/api/terminals/"+s.id+"/attach", object{"companion_id": fields[1]}); err != nil {
		return err
	}
	s.waitFor(5*time.Second, func() bool { return s.frameCount > prevFrames })
	out["companion"] = fields[1]
	return nil
}

func simWait(s *simTerminal, ctx context.Context, line string, fields []string, out object) error {
	if len(fields) < 2 {
		return errors.New("usage: wait <state> [timeout]")
	}
	want := fields[1]
	timeout := 30 * time.Second
	if len(fields) >= 3 {
		if d, err := time.ParseDuration(fields[2]); err == nil {
			timeout = d
		}
	}
	ok := s.waitFor(timeout, func() bool { return s.stateReached(want) })
	if !ok {
		s.mu.Lock()
		got := s.state
		s.mu.Unlock()
		return fmt.Errorf("timed out waiting for state %q (current %q)", want, got)
	}
	out["state"] = want
	return nil
}

func simWaitCurrent(s *simTerminal, ctx context.Context, line string, fields []string, out object) error {
	if len(fields) < 2 {
		return errors.New("usage: wait-current <state> [timeout]")
	}
	want := fields[1]
	timeout := 30 * time.Second
	if len(fields) >= 3 {
		if d, err := time.ParseDuration(fields[2]); err == nil {
			timeout = d
		}
	}
	ok := s.waitFor(timeout, func() bool { return s.state == want })
	if !ok {
		s.mu.Lock()
		got := s.state
		s.mu.Unlock()
		return fmt.Errorf("timed out waiting for current state %q (current %q)", want, got)
	}
	out["state"] = want
	return nil
}

func simSleep(s *simTerminal, ctx context.Context, line string, fields []string, out object) error {
	if len(fields) != 2 {
		return errors.New("usage: sleep <ms>")
	}
	ms, err := strconv.Atoi(fields[1])
	if err != nil || ms < 0 || ms > 30000 {
		return errors.New("sleep must be 0..30000 ms")
	}
	time.Sleep(time.Duration(ms) * time.Millisecond)
	out["ms"] = ms
	return nil
}

func simState(s *simTerminal, ctx context.Context, line string, fields []string, out object) error {
	s.mu.Lock()
	out["state"] = s.state
	s.mu.Unlock()
	return nil
}

func simWaitRec(s *simTerminal, ctx context.Context, line string, fields []string, out object) error {
	timeout := 10 * time.Second
	if len(fields) >= 2 {
		if d, err := time.ParseDuration(fields[1]); err == nil {
			timeout = d
		}
	}
	if !s.waitFor(timeout, func() bool { return s.recActive }) {
		return fmt.Errorf("timed out waiting for REC_CTRL start")
	}
	out["rec"] = true
	return nil
}

func simHits(s *simTerminal, ctx context.Context, line string, fields []string, out object) error {
	res, err := s.client.do(ctx, http.MethodGet, "/api/terminals/"+s.id+"/hits", nil)
	if err != nil {
		return err
	}
	for _, key := range []string{"screen", "rev"} {
		if v, ok := res[key]; ok {
			out[key] = v
		}
	}
	if hits, ok := res["hits"].([]any); ok {
		out["hits"] = hits
	}
	return nil
}

func simScreen(s *simTerminal, ctx context.Context, line string, fields []string, out object) error {
	if len(fields) != 2 {
		return errors.New("usage: screen <out.png>")
	}
	s.mu.Lock()
	fbCopy := append([]uint16(nil), s.fb...)
	rev := s.frameRev
	s.mu.Unlock()
	if err := writeFBToPNG(fbCopy, s.spec.width, s.spec.height, fields[1]); err != nil {
		return err
	}
	out["path"], out["rev"] = fields[1], rev
	return nil
}

func simRecord(s *simTerminal, ctx context.Context, line string, fields []string, out object) error {
	if len(fields) != 2 {
		return errors.New("usage: record <out.wav>")
	}
	s.waitFor(30*time.Second, func() bool { return !s.ttsActive && len(s.ttsBuf) > 0 })
	s.mu.Lock()
	pcm := append([]byte(nil), s.ttsBuf...)
	s.mu.Unlock()
	if err := writeWAV16k(fields[1], pcm); err != nil {
		return err
	}
	out["path"], out["bytes"] = fields[1], len(pcm)
	return nil
}

func simTimeline(s *simTerminal, ctx context.Context, line string, fields []string, out object) error {
	s.mu.Lock()
	out["events"] = append([]object(nil), s.timeline...)
	s.mu.Unlock()
	return nil
}
