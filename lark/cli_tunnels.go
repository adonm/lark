package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

func cliTunnels(ctx context.Context, c *cliClient, args []string, out, errOut io.Writer) error {
	switch {
	case len(args) == 0:
		res, err := c.do(ctx, http.MethodGet, "/api/remote/tunnels", nil)
		if err != nil {
			return err
		}
		list, _ := res["tunnels"].([]any)
		for _, item := range list {
			m, _ := item.(object)
			fmt.Fprintf(out, "%s\tactive=%v\tcontacts=%v\t%s\n",
				str(m["companion_id"]), m["active"], m["contacts"], str(m["url"]))
		}
		return nil
	case len(args) == 2 && args[0] == "stop":
		if _, err := c.do(ctx, http.MethodDelete, "/api/remote/tunnels/"+args[1], nil); err != nil {
			return err
		}
		fmt.Fprintf(out, "Stopped remote controls for %s\n", args[1])
		return nil
	}
	fmt.Fprintln(errOut, "Usage: lark tunnels [stop <companion>]")
	return errors.New("invalid tunnels arguments")
}
