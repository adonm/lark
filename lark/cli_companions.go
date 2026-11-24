package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

func cliCompanions(ctx context.Context, c *cliClient, args []string, out, errOut io.Writer) error {
	switch len(args) {
	case 0:
		res, err := c.do(ctx, http.MethodGet, "/api/companions", nil)
		if err != nil {
			return err
		}
		list, _ := res["companions"].([]any)
		for _, item := range list {
			m, _ := item.(object)
			printCompanionRow(out, m)
		}
		return nil
	case 2:
		if args[0] == "rm" {
			if _, err := c.do(ctx, http.MethodDelete, "/api/companions/"+args[1], nil); err != nil {
				return err
			}
			fmt.Fprintf(out, "Removed companion %s\n", args[1])
			return nil
		}
	case 3:
		if args[0] == "add" {
			res, err := c.do(ctx, http.MethodPost, "/api/companions", object{"name": args[1], "form": args[2]})
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Added companion %s (%s)\n", str(res["id"]), str(res["name"]))
			return nil
		}
	}
	fmt.Fprintln(errOut, "Usage: lark companions [add <name> <form> | rm <id>]")
	return errors.New("invalid companions arguments")
}

func printCompanionRow(out io.Writer, m object) {
	fmt.Fprintf(out, "%s\t%s\tonline=%v\tdormant=%v", str(m["id"]), str(m["name"]), m["online"], m["dormant"])
	if dcID := m["dc_account_id"]; dcID != nil {
		fmt.Fprintf(out, "\tdc=%v", dcID)
	}
	fmt.Fprintln(out)
}
