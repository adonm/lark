package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func simTap(s *simTerminal, ctx context.Context, line string, fields []string, out object) error {
	if len(fields) != 3 {
		return errors.New("usage: tap <x> <y>")
	}
	x, err := strconv.Atoi(fields[1])
	if err != nil {
		return err
	}
	y, err := strconv.Atoi(fields[2])
	if err != nil {
		return err
	}
	s.mu.Lock()
	prevFrames := s.frameCount
	s.mu.Unlock()
	if err := s.sendTap(x, y); err != nil {
		return err
	}
	s.waitFor(500*time.Millisecond, func() bool { return s.frameCount > prevFrames || s.recActive })
	out["x"], out["y"] = x, y
	return nil
}

func simTapLabel(s *simTerminal, ctx context.Context, line string, fields []string, out object) error {
	if len(fields) < 2 {
		return errors.New("usage: tap-label <label>")
	}
	label := strings.Join(fields[1:], " ")
	var res object
	var x, y int
	var matched string
	var err error
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		res, err = s.client.do(ctx, http.MethodGet, "/api/terminals/"+s.id+"/hits", nil)
		if err != nil {
			return err
		}
		x, y, matched, err = resolveHitLabel(res, label)
		if err == nil || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		return err
	}
	if rev, ok := numInt(res["rev"]); ok && rev > 0 {
		s.waitFor(500*time.Millisecond, func() bool { return s.frameRev == uint16(rev) })
	}
	s.mu.Lock()
	prevFrames := s.frameCount
	s.mu.Unlock()
	if err := s.sendTap(x, y); err != nil {
		return err
	}
	s.waitFor(500*time.Millisecond, func() bool { return s.frameCount > prevFrames || s.recActive })
	out["label"], out["matched"], out["x"], out["y"] = label, matched, x, y
	return nil
}
