package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

func cliTerminals(ctx context.Context, c *cliClient, args []string, out, errOut io.Writer) error {
	switch {
	case len(args) == 0:
		res, err := c.do(ctx, http.MethodGet, "/api/terminals", nil)
		if err != nil {
			return err
		}
		list, _ := res["terminals"].([]any)
		for _, item := range list {
			m, _ := item.(object)
			printTerminalRow(out, m)
		}
		return nil
	case len(args) == 3 && args[0] == "attach":
		res, err := c.do(ctx, http.MethodPost, "/api/terminals/"+args[1]+"/attach", object{"companion_id": args[2]})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Attached %s to %s\n", str(res["id"]), str(res["companion_id"]))
		return nil
	case len(args) == 2 && args[0] == "detach":
		res, err := c.do(ctx, http.MethodPost, "/api/terminals/"+args[1]+"/detach", object{})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Detached %s\n", str(res["id"]))
		return nil
	case len(args) == 2 && args[0] == "rm":
		// Removing the row is how an offline terminal (including a leftover
		// sim-*) goes away; the admin route detaches and forgets it.
		if _, err := c.do(ctx, http.MethodDelete, "/api/terminals/"+args[1], nil); err != nil {
			return err
		}
		fmt.Fprintf(out, "Removed terminal %s\n", args[1])
		return nil
	}
	fmt.Fprintln(errOut, "Usage: lark terminals [attach <terminal> <companion> | detach <terminal> | rm <terminal>]")
	return errors.New("invalid terminals arguments")
}

func printTerminalRow(out io.Writer, m object) {
	fmt.Fprintf(out, "%s\tcompanion=%s\tonline=%v\tip=%s\tfw=%s\n",
		str(m["id"]), str(m["companion_id"]), m["online"], str(m["ip"]), str(m["firmware"]))
}
