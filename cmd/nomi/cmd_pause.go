package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
)

// Pauseable from SSH (mirrors Runtime.PauseRun).
var pausableStatuses = map[string]bool{
	"executing":         true,
	"awaiting_approval": true,
}

func isPausableStatus(status string) bool {
	return pausableStatuses[status]
}

func isPausedStatus(status string) bool {
	return status == "paused"
}

// pauseCmd soft-stops an executing / awaiting_approval run.
//
//	nomi pause
//	nomi pause <run-id-or-prefix>
//	nomi pause --list
func pauseCmd(common *commonFlags, args []string) int {
	return pauseResumeCmd(common, args, "pause", isPausableStatus, "/pause", "paused", "pausable")
}

// resumeCmd continues a paused run.
//
//	nomi resume
//	nomi resume <run-id-or-prefix>
//	nomi resume --list
func resumeCmd(common *commonFlags, args []string) int {
	return pauseResumeCmd(common, args, "resume", isPausedStatus, "/resume", "resumed", "paused")
}

func pauseResumeCmd(
	common *commonFlags,
	args []string,
	name string,
	match func(string) bool,
	pathSuffix string,
	doneVerb string,
	listNoun string,
) int {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	bindCommonFlags(fs, common)
	listOnly := fs.Bool("list", false, "list "+listNoun+" runs and exit")
	_ = fs.Parse(args)

	cli, err := NewClient(common)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	candidates, err := listRunsMatching(cli, match)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	if *listOnly {
		return printRunStatusList(common, candidates, listNoun)
	}

	rest := fs.Args()
	var runID string
	switch {
	case len(rest) >= 1:
		runID, err = resolveRunID(cli, rest[0], candidates)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	case len(candidates) == 0:
		fmt.Fprintf(os.Stderr, "nomi %s: no %s runs (try `nomi list runs`)\n", name, listNoun)
		return 1
	case len(candidates) == 1:
		runID = candidates[0].ID
	default:
		_ = printRunStatusList(common, candidates, listNoun)
		fmt.Fprintf(os.Stderr, "nomi %s: multiple %s — pass a run id (or prefix)\n", name, listNoun)
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
	if !match(detail.Run.Status) {
		fmt.Fprintf(os.Stderr, "nomi %s: run %s is %s (not %s)\n",
			name, short(runID), detail.Run.Status, listNoun)
		return 1
	}

	if err := cli.Post("/runs/"+runID+pathSuffix, map[string]any{}, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "✓ %s %s (%s)\n", doneVerb, short(runID), trunc(detail.Run.Goal, 60))
	return 0
}

func listRunsMatching(cli *Client, match func(string) bool) ([]runListRow, error) {
	var resp struct {
		Runs []runListRow `json:"runs"`
	}
	if err := cli.Get("/runs", &resp); err != nil {
		return nil, err
	}
	out := make([]runListRow, 0)
	for _, r := range resp.Runs {
		if match(r.Status) {
			out = append(out, r)
		}
	}
	return out, nil
}

func printRunStatusList(c *commonFlags, rows []runListRow, emptyNoun string) int {
	if c.JSON {
		printJSON(map[string]any{"runs": rows})
		return 0
	}
	if len(rows) == 0 {
		fmt.Printf("(no %s runs)\n", emptyNoun)
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
