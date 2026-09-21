package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

type runListRow struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Goal      string `json:"goal"`
	CreatedAt string `json:"created_at"`
}

// reviewCmd attaches to an existing plan_review run (channels / email /
// tray left it waiting) and drives Plan→Diff→Approve without creating
// a new run — Claude Code-style "pick up the pending review" from SSH.
//
//	nomi review
//	nomi review <run-id-or-prefix>
//	nomi review --list
func reviewCmd(common *commonFlags, args []string) int {
	fs := flag.NewFlagSet("review", flag.ExitOnError)
	bindCommonFlags(fs, common)
	listOnly := fs.Bool("list", false, "list runs awaiting plan review and exit")
	autoApprove := fs.Bool("auto-approve", false, "auto-approve confirm-mode capabilities (DANGEROUS)")
	timeout := fs.Duration("timeout", 5*time.Minute, "give up after this long if the run doesn't reach a terminal state")
	_ = fs.Parse(args)

	cli, err := NewClient(common)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	pending, err := listPlanReviewRuns(cli)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	if *listOnly {
		return printPlanReviewList(common, pending)
	}

	rest := fs.Args()
	var runID string
	switch {
	case len(rest) >= 1:
		runID, err = resolveRunID(cli, rest[0], pending)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	case len(pending) == 0:
		fmt.Fprintln(os.Stderr, "nomi review: no runs awaiting plan review (try `nomi list runs`)")
		return 1
	case len(pending) == 1:
		runID = pending[0].ID
	default:
		_ = printPlanReviewList(common, pending)
		fmt.Fprintln(os.Stderr, "nomi review: multiple pending — pass a run id (or prefix)")
		return 2
	}

	var detail struct {
		Run struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Goal   string `json:"goal"`
		} `json:"run"`
	}
	if err := cli.Get("/runs/"+runID, &detail); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if detail.Run.Status != "plan_review" {
		fmt.Fprintf(os.Stderr, "nomi review: run %s is %s (want plan_review)\n",
			short(runID), detail.Run.Status)
		return 1
	}

	fmt.Fprintf(os.Stderr, "▶ attaching to run %s\n", short(runID))
	return driveRun(cli, runID, driveOpts{
		Review:      true,
		AutoApprove: *autoApprove,
		Timeout:     *timeout,
	}, bufio.NewReader(os.Stdin))
}

func listPlanReviewRuns(cli *Client) ([]runListRow, error) {
	var resp struct {
		Runs []runListRow `json:"runs"`
	}
	if err := cli.Get("/runs", &resp); err != nil {
		return nil, err
	}
	out := make([]runListRow, 0)
	for _, r := range resp.Runs {
		if r.Status == "plan_review" {
			out = append(out, r)
		}
	}
	return out, nil
}

func printPlanReviewList(c *commonFlags, rows []runListRow) int {
	if c.JSON {
		printJSON(map[string]any{"runs": rows})
		return 0
	}
	if len(rows) == 0 {
		fmt.Println("(no runs awaiting plan review)")
		return 0
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tSTATUS\tCREATED\tGOAL")
	for _, r := range rows {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			short(r.ID), r.Status, trunc(r.CreatedAt, 19), trunc(r.Goal, 60))
	}
	_ = w.Flush()
	return 0
}

// resolveRunID accepts a full UUID or a unique prefix (list prints 8 chars).
func resolveRunID(cli *Client, hint string, pending []runListRow) (string, error) {
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return "", fmt.Errorf("run id required")
	}
	// Exact / unique prefix among pending first (fast path).
	var matches []string
	for _, r := range pending {
		if r.ID == hint || strings.HasPrefix(r.ID, hint) {
			matches = append(matches, r.ID)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("ambiguous run id %q matches %d pending runs", hint, len(matches))
	}

	// Fall back: scan all runs for prefix (user may pass an id that just left plan_review).
	var resp struct {
		Runs []runListRow `json:"runs"`
	}
	if err := cli.Get("/runs", &resp); err != nil {
		return "", err
	}
	matches = matches[:0]
	for _, r := range resp.Runs {
		if r.ID == hint || strings.HasPrefix(r.ID, hint) {
			matches = append(matches, r.ID)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("run %q not found", hint)
	default:
		return "", fmt.Errorf("ambiguous run id %q matches %d runs", hint, len(matches))
	}
}
