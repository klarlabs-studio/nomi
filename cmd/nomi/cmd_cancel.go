package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
)

// Statuses a user might still want to cancel from SSH (anything
// non-terminal). Mirrors Runtime.CancelRun's "already terminal" guard.
var cancelableStatuses = map[string]bool{
	"created":           true,
	"planning":          true,
	"plan_review":       true,
	"awaiting_approval": true,
	"executing":         true,
	"paused":            true,
}

func isCancelableStatus(status string) bool {
	return cancelableStatuses[status]
}

// cancelCmd cancels an active run (or lists cancelable ones).
// Ctrl+C during `nomi run` / `nomi review` also posts cancel — this
// command covers the case where the client already exited.
//
//	nomi cancel
//	nomi cancel <run-id-or-prefix>
//	nomi cancel --list
func cancelCmd(common *commonFlags, args []string) int {
	fs := flag.NewFlagSet("cancel", flag.ExitOnError)
	bindCommonFlags(fs, common)
	listOnly := fs.Bool("list", false, "list cancelable runs and exit")
	_ = fs.Parse(args)

	cli, err := NewClient(common)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	active, err := listCancelableRuns(cli)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	if *listOnly {
		return printCancelableList(common, active)
	}

	rest := fs.Args()
	var runID string
	switch {
	case len(rest) >= 1:
		runID, err = resolveRunID(cli, rest[0], active)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	case len(active) == 0:
		fmt.Fprintln(os.Stderr, "nomi cancel: no active runs (try `nomi list runs`)")
		return 1
	case len(active) == 1:
		runID = active[0].ID
	default:
		_ = printCancelableList(common, active)
		fmt.Fprintln(os.Stderr, "nomi cancel: multiple active — pass a run id (or prefix)")
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
	if !isCancelableStatus(detail.Run.Status) {
		fmt.Fprintf(os.Stderr, "nomi cancel: run %s is %s (already terminal)\n",
			short(runID), detail.Run.Status)
		return 1
	}

	if err := cli.Post("/runs/"+runID+"/cancel", map[string]any{}, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "✓ cancelled %s (%s)\n", short(runID), trunc(detail.Run.Goal, 60))
	return 0
}

func listCancelableRuns(cli *Client) ([]runListRow, error) {
	var resp struct {
		Runs []runListRow `json:"runs"`
	}
	if err := cli.Get("/runs", &resp); err != nil {
		return nil, err
	}
	out := make([]runListRow, 0)
	for _, r := range resp.Runs {
		if isCancelableStatus(r.Status) {
			out = append(out, r)
		}
	}
	return out, nil
}

func printCancelableList(c *commonFlags, rows []runListRow) int {
	if c.JSON {
		printJSON(map[string]any{"runs": rows})
		return 0
	}
	if len(rows) == 0 {
		fmt.Println("(no cancelable runs)")
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
