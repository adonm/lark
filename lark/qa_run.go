package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// QA runs against the installed server must leave it exactly as they found it.
// Each test records the rows it creates and the guard removes only those ids,
// then asserts that the companion, terminal, and Delta Chat account sets match
// the pre-run snapshot. Rows that appear during the run (a sim hello creates a
// blank companion and terminal) are captured by diffing against that snapshot;
// a row that merely looks like a QA row but predates the run is never touched.

type qaSnapshot struct {
	companions    map[string]bool
	terminals     map[string]bool
	linked        map[string]string // terminal id -> companion id
	accounts      map[string]bool
	accountsKnown bool
}

type qaRunGuard struct {
	c       *cliClient
	before  qaSnapshot
	created qaSnapshot
}

var qaGuards sync.Map // *cliClient -> *qaRunGuard

func newQASnapshot() qaSnapshot {
	return qaSnapshot{
		companions: map[string]bool{},
		terminals:  map[string]bool{},
		linked:     map[string]string{},
		accounts:   map[string]bool{},
	}
}

func qaFetchSnapshot(ctx context.Context, c *cliClient) (qaSnapshot, error) {
	snap := newQASnapshot()
	res, err := c.do(ctx, http.MethodGet, "/api/companions", nil)
	if err != nil {
		return snap, fmt.Errorf("GET /api/companions: %w", err)
	}
	if items, ok := res["companions"].([]any); ok {
		for _, item := range items {
			m, _ := item.(object)
			if id := str(m["id"]); id != "" {
				snap.companions[id] = true
			}
		}
	}
	res, err = c.do(ctx, http.MethodGet, "/api/terminals", nil)
	if err != nil {
		return snap, fmt.Errorf("GET /api/terminals: %w", err)
	}
	if items, ok := res["terminals"].([]any); ok {
		for _, item := range items {
			m, _ := item.(object)
			id := str(m["id"])
			if id == "" {
				continue
			}
			snap.terminals[id] = true
			if cid := str(m["companion_id"]); cid != "" {
				snap.linked[id] = cid
			}
		}
	}
	// Accounts come from diagnostics. An old server without the ids field
	// leaves accountsKnown false, so the invariant only compares what it can
	// actually see.
	if diag, err := c.do(ctx, http.MethodGet, "/api/diagnostics", nil); err == nil {
		if accounts, ok := diag["deltachat_accounts"].(object); ok {
			if raw, ok := accounts["ids"].([]any); ok {
				snap.accountsKnown = true
				for _, v := range raw {
					if id := jsonInt(v); id > 0 {
						snap.accounts[fmt.Sprint(id)] = true
					}
				}
			}
		}
	}
	return snap, nil
}

func newQARunGuard(ctx context.Context, c *cliClient) (*qaRunGuard, error) {
	before, err := qaFetchSnapshot(ctx, c)
	if err != nil {
		return nil, err
	}
	return &qaRunGuard{c: c, before: before, created: newQASnapshot()}, nil
}

func qaGuardFor(c *cliClient) *qaRunGuard {
	if g, ok := qaGuards.Load(c); ok {
		return g.(*qaRunGuard)
	}
	return nil
}

// trackCompanion records an id this run created. Ids are the only protection:
// a companion that becomes configured or terminal-linked mid-test is still
// removed, and an equally named row that predates the run is never removed.
func (g *qaRunGuard) trackCompanion(id string) {
	if id != "" {
		g.created.companions[id] = true
	}
}

func (g *qaRunGuard) trackTerminal(id string) {
	if id != "" {
		g.created.terminals[id] = true
	}
}

// captureCreated folds every row that appeared since the pre-run snapshot into
// the created set. sim registers a blank companion and terminal before the
// script runs, so the test never calls track* for those ids.
func (g *qaRunGuard) captureCreated(ctx context.Context) (qaSnapshot, error) {
	now, err := qaFetchSnapshot(ctx, g.c)
	if err != nil {
		return now, err
	}
	for id := range now.companions {
		if !g.before.companions[id] {
			g.created.companions[id] = true
		}
	}
	for id := range now.terminals {
		if !g.before.terminals[id] {
			g.created.terminals[id] = true
		}
	}
	if now.accountsKnown {
		for id := range now.accounts {
			if !g.before.accounts[id] {
				g.created.accounts[id] = true
			}
		}
	}
	return now, nil
}

func sortedQAIDs(set map[string]bool) []string {
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// leftovers lists rows that differ from the pre-run snapshot. Extras are the
// failure this guard exists for; missing rows mean the run deleted somebody
// else's data and are reported the same way.
func (g *qaRunGuard) leftovers(after qaSnapshot) string {
	var parts []string
	add := func(kind string, before, now map[string]bool) {
		var extra, missing []string
		for id := range now {
			if !before[id] {
				extra = append(extra, id)
			}
		}
		for id := range before {
			if !now[id] {
				missing = append(missing, id)
			}
		}
		slices.Sort(extra)
		slices.Sort(missing)
		if len(extra) > 0 {
			parts = append(parts, fmt.Sprintf("%s(s) left behind: %s", kind, strings.Join(extra, ", ")))
		}
		if len(missing) > 0 {
			parts = append(parts, fmt.Sprintf("%s(s) removed: %s", kind, strings.Join(missing, ", ")))
		}
	}
	add("companion", g.before.companions, after.companions)
	add("terminal", g.before.terminals, after.terminals)
	if g.before.accountsKnown && after.accountsKnown {
		add("Delta Chat account", g.before.accounts, after.accounts)
	}
	return strings.Join(parts, "; ")
}

// cleanup removes every id this run created, detaching any terminal first so a
// companion delete is not refused by a live link, and then waits for the
// pre-run sets to come back. Async account removal is why the final check
// retries until the context deadline.
func (g *qaRunGuard) cleanup(ctx context.Context) error {
	now, err := g.captureCreated(ctx)
	if err != nil {
		return fmt.Errorf("QA cleanup snapshot: %w", err)
	}
	var failures []string
	for _, termID := range sortedQAIDs(now.terminals) {
		if cid := now.linked[termID]; cid == "" || !g.created.companions[cid] {
			continue
		}
		if _, err := g.c.do(ctx, http.MethodPost, "/api/terminals/"+termID+"/detach", object{}); err != nil {
			failures = append(failures, fmt.Sprintf("detach terminal %s: %v", termID, err))
		}
	}
	for _, termID := range sortedQAIDs(g.created.terminals) {
		if _, err := g.c.do(ctx, http.MethodDelete, "/api/terminals/"+termID, nil); err != nil {
			failures = append(failures, fmt.Sprintf("remove terminal %s: %v", termID, err))
		}
	}
	for _, compID := range sortedQAIDs(g.created.companions) {
		if _, err := g.c.do(ctx, http.MethodDelete, "/api/companions/"+compID, nil); err != nil {
			failures = append(failures, fmt.Sprintf("remove companion %s: %v", compID, err))
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	last := "no snapshot"
	for {
		after, err := qaFetchSnapshot(ctx, g.c)
		if err == nil {
			// The sets are the guard. A remove call that reported an error
			// for a row that disappeared anyway (a 404 race) is noise.
			if diff := g.leftovers(after); diff == "" {
				return nil
			} else {
				last = diff
			}
		} else {
			last = err.Error()
		}
		if !time.Now().Before(deadline) {
			if len(failures) > 0 {
				last = strings.Join(failures, "; ") + "; " + last
			}
			return fmt.Errorf("QA run did not restore the server: %s", last)
		}
		select {
		case <-ctx.Done():
			if len(failures) > 0 {
				last = strings.Join(failures, "; ") + "; " + last
			}
			return fmt.Errorf("QA run did not restore the server: %s", last)
		case <-time.After(100 * time.Millisecond):
		}
	}
}
