package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

func cliStatus(ctx context.Context, c *cliClient, args []string, out, errOut io.Writer) error {
	if len(args) != 0 {
		fmt.Fprintln(errOut, "Usage: lark status")
		return errors.New("unexpected arguments")
	}
	health, err := c.do(ctx, http.MethodGet, "/health", nil)
	if err != nil {
		return err
	}
	comps, err := c.do(ctx, http.MethodGet, "/api/companions", nil)
	if err != nil {
		return err
	}
	terms, err := c.do(ctx, http.MethodGet, "/api/terminals", nil)
	if err != nil {
		return err
	}
	lib, err := c.do(ctx, http.MethodGet, "/api/library", nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Server: ok=%v opencode=%v stt=%v tts=%v",
		health["ok"], health["opencode"], health["stt"], health["tts"])
	if dc, ok := health["deltachat"]; ok {
		fmt.Fprintf(out, " deltachat=%v", dc)
	}
	fmt.Fprintln(out)
	if models, ok := health["models"].(object); ok {
		fmt.Fprint(out, "Models:")
		for _, name := range []string{"parakeet", "kokoro", "smart_turn", "silero"} {
			model, _ := models[name].(object)
			if model == nil {
				continue
			}
			state := str(model["state"])
			if msg := str(model["error"]); msg != "" {
				state += " (" + msg + ")"
			}
			fmt.Fprintf(out, " %s=%s", name, state)
		}
		fmt.Fprintln(out)
	}
	printLibraryPlan(out, lib)
	compList, _ := comps["companions"].([]any)
	termList, _ := terms["terminals"].([]any)
	fmt.Fprintf(out, "Companions (%d):\n", len(compList))
	for _, item := range compList {
		m, _ := item.(object)
		printCompanionRow(out, m)
	}
	fmt.Fprintf(out, "Terminals (%d):\n", len(termList))
	for _, item := range termList {
		m, _ := item.(object)
		printTerminalRow(out, m)
	}
	return nil
}

func printLibraryPlan(out io.Writer, lib object) {
	fmt.Fprintf(out, "Library (%s):\n", str(lib["dir"]))
	if msg := str(lib["error"]); msg != "" {
		fmt.Fprintf(out, "  unavailable: %s\n", msg)
		return
	}
	fmt.Fprintf(out, "  disk: %s free of %s, budget %s\n",
		humanBytes(jsonInt(lib["free_bytes"])), humanBytes(jsonInt(lib["total_bytes"])),
		humanBytes(jsonInt(lib["budget_bytes"])))
	packs, _ := lib["packs"].([]any)
	for _, item := range packs {
		m, _ := item.(object)
		switch str(m["state"]) {
		case packStateInstalled:
			fmt.Fprintf(out, "  %s\tinstalled\t%s\n", str(m["name"]), humanBytes(jsonInt(m["installed_bytes"])))
		case packStateBuilding:
			fmt.Fprintf(out, "  %s\tbuilding\t%d%%\n", str(m["name"]), jsonInt(m["percent"]))
		case packStateSkipped:
			fmt.Fprintf(out, "  %s\tskipped\t%s\n", str(m["name"]), str(m["reason"]))
		default:
			fmt.Fprintf(out, "  %s\t%s\t%s needed\n", str(m["name"]), str(m["state"]),
				humanBytes(jsonInt(m["needed_bytes"])))
		}
	}
}
