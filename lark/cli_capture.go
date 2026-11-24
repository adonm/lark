package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

func cliCapture(ctx context.Context, c *cliClient, args []string, out, errOut io.Writer) error {
	if len(args) < 1 || len(args) > 2 {
		return captureUsage()
	}
	seconds := 20
	if len(args) == 2 {
		n, convErr := strconv.Atoi(args[1])
		if convErr != nil || n < 1 || n > 60 {
			return errors.New("seconds must be between 1 and 60")
		}
		seconds = n
	}
	slow := *c
	slow.http = &http.Client{Timeout: time.Duration(seconds+20) * time.Second}
	fmt.Fprintf(errOut, "Recording %ds of microphone audio; stay quiet or speak normally.\n", seconds)
	res, err := slow.do(ctx, http.MethodPost, "/api/capture", object{"companion_id": args[0], "seconds": seconds})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s\t%v ms\n", str(res["path"]), res["audio_ms"])
	return nil
}
