package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"time"
)

// Statuses a user can attach to with `nomi watch` (non-terminal).
var watchableStatuses = map[string]bool{
	"created":           true,
	"planning":          true,
	"plan_review":       true,
	"awaiting_approval": true,
	"executing":         true,
	"paused":            true,
}

func isWatchableStatus(status string) bool {
	return watchableStatuses[status]
}

// watchCmd attaches to an in-flight run and drives it to completion
// (live step progress + approval prompts). Unlike `nomi review`, this
// covers executing / awaiting_approval / paused — not only plan_review.
//
//	nomi watch
//	nomi watch <run-id-or-prefix>
//	nomi watch --list
func watchCmd(common *commonFlags, args []string) int {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	bindCommonFlags(fs, common)
	listOnly := fs.Bool("list", false, "list watchable runs and exit")
	autoApprove := fs.Bool("auto-approve", false, "auto-approve confirm-mode capabilities (DANGEROUS)")
	review := fs.Bool("review", false, "interactive plan review if the run hits plan_review")
	timeout := fs.Duration("timeout", 5*time.Minute, "give up after this long if the run doesn't reach a terminal state")
	_ = fs.Parse(args)

	cli, err := NewClient(common)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	active, err := listWatchableRuns(cli)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	if *listOnly {
		return printRunStatusList(common, active, "watchable")
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
		fmt.Fprintln(os.Stderr, "nomi watch: no active runs (try `nomi list runs`)")
		return 1
	case len(active) == 1:
		runID = active[0].ID
	default:
		_ = printRunStatusList(common, active, "watchable")
		fmt.Fprintln(os.Stderr, "nomi watch: multiple active — pass a run id (or prefix)")
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
	if !isWatchableStatus(detail.Run.Status) {
		fmt.Fprintf(os.Stderr, "nomi watch: run %s is %s (already terminal)\n",
			short(runID), detail.Run.Status)
		return 1
	}

	// Interactive plan review when attaching mid plan_review, or when
	// the user passed --review (replans during execute).
	useReview := *review || detail.Run.Status == "plan_review"
	fmt.Fprintf(os.Stderr, "▶ watching run %s (%s)\n", short(runID), detail.Run.Status)
	return driveRun(cli, runID, driveOpts{
		Review:      useReview,
		AutoApprove: *autoApprove,
		Timeout:     *timeout,
	}, bufio.NewReader(os.Stdin))
}

func listWatchableRuns(cli *Client) ([]runListRow, error) {
	return listRunsMatching(cli, isWatchableStatus)
}
