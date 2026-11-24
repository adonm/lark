package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

func cliCapture(ctx context.Context, c *cliClient, args []string, out, errOut io.Writer) error {
	if len(args) < 1 || len(args) > 2 {
		return captureUsage()
	}
	seconds := captureDefaultSeconds
	if len(args) == 2 {
		var parseErr error
		seconds, parseErr = parseCaptureSeconds(args[1])
		if parseErr != nil {
			return parseErr
		}
	}
	slow := *c
	slow.http = &http.Client{Timeout: time.Duration(seconds)*time.Second + captureCLIRequestSlack}
	fmt.Fprintf(errOut, "Recording %ds of microphone audio; stay quiet or speak normally.\n", seconds)
	res, err := slow.do(ctx, http.MethodPost, "/api/capture", object{"companion_id": args[0], "seconds": seconds})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s\t%v ms\n", str(res["path"]), res["audio_ms"])
	return nil
}
