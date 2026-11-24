package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func cliSTTBench(ctx context.Context, c *cliClient, args []string, out, errOut io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: lark stt-bench <wav|dir>...")
	}
	files, err := benchFiles(args)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("no WAV files found")
	}
	for _, file := range files {
		pcm, err := readWAVPCM16k(file)
		if err != nil {
			fmt.Fprintf(out, "%s\tERROR\t%v\n", file, err)
			continue
		}
		res, err := c.postRaw(ctx, "/api/stt", pcm)
		if err != nil {
			fmt.Fprintf(out, "%s\tERROR\t%v\n", file, err)
			continue
		}
		fmt.Fprintf(out, "%s\t%s ms (%s audio, %.0fx)\t%s\n", file,
			jsonNumberText(res["stt_ms"]), jsonNumberText(res["audio_ms"]),
			speedup(res), str(res["transcript"]))
	}
	return nil
}

func benchFiles(args []string) ([]string, error) {
	var files []string
	for _, arg := range args {
		info, err := os.Stat(arg)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			files = append(files, arg)
			continue
		}
		entries, err := os.ReadDir(arg)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".wav") {
				files = append(files, filepath.Join(arg, e.Name()))
			}
		}
	}
	return files, nil
}

func jsonNumberText(v any) string {
	switch n := v.(type) {
	case json.Number:
		return n.String()
	case float64:
		return fmt.Sprintf("%.0f", n)
	}
	return "?"
}

func speedup(res object) float64 {
	stt, _ := strconv.ParseFloat(jsonNumberText(res["stt_ms"]), 64)
	audio, _ := strconv.ParseFloat(jsonNumberText(res["audio_ms"]), 64)
	if stt <= 0 {
		return 0
	}
	return audio / stt
}
