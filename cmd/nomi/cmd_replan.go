package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"time"
)

// Statuses ManualReplan accepts (terminal). Prefer failed for --list;
// completed/cancelled are legal API-wise but rarely what users mean.
var replanableStatuses = map[string]bool{
	"failed":    true,
	"cancelled": true,
}

func isReplanableStatus(status string) bool {
	return replanableStatuses[status]
}

// replanCmd asks the planner for a corrective plan on a failed run
// (desktop "Fix this with the agent" / POST /runs/:id/replan).
//
//	nomi replan
//	nomi replan <run-id-or-prefix>
//	nomi replan --list
//	nomi replan --watch   # attach after replan (driveRun)
func replanCmd(common *commonFlags, args []string) int {
	fs := flag.NewFlagSet("replan", flag.ExitOnError)
	bindCommonFlags(fs, common)
	listOnly := fs.Bool("list", false, "list replanable (failed/cancelled) runs and exit")
	watch := fs.Bool("watch", false, "attach and drive the run after replan")
	review := fs.Bool("review", false, "with --watch: interactive plan review")
	autoApprove := fs.Bool("auto-approve", false, "with --watch: auto-approve confirm-mode capabilities")
	timeout := fs.Duration("timeout", 5*time.Minute, "with --watch: give up after this long")
	_ = fs.Parse(args)

	cli, err := NewClient(common)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	candidates, err := listRunsMatching(cli, isReplanableStatus)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	if *listOnly {
		return printRunStatusList(common, candidates, "replanable")
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
		fmt.Fprintln(os.Stderr, "nomi replan: no failed/cancelled runs (try `nomi list runs`)")
		return 1
	case len(candidates) == 1:
		runID = candidates[0].ID
	default:
		_ = printRunStatusList(common, candidates, "replanable")
		fmt.Fprintln(os.Stderr, "nomi replan: multiple candidates — pass a run id (or prefix)")
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
	switch detail.Run.Status {
	case "failed", "cancelled", "completed":
		// ManualReplan accepts any terminal run.
	default:
		fmt.Fprintf(os.Stderr, "nomi replan: run %s is %s (want failed/cancelled/completed)\n",
			short(runID), detail.Run.Status)
		return 1
	}

	var resp struct {
		Status    string `json:"status"`
		StepCount int    `json:"step_count"`
	}
	if err := cli.Post("/runs/"+runID+"/replan", map[string]any{}, &resp); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "✓ replanned %s (%d step(s)) — %s\n",
		short(runID), resp.StepCount, trunc(detail.Run.Goal, 60))

	if !*watch {
		fmt.Fprintln(os.Stderr, "  tip: nomi watch "+short(runID)+"   # or nomi replan --watch")
		return 0
	}

	fmt.Fprintf(os.Stderr, "▶ watching run %s after replan\n", short(runID))
	return driveRun(cli, runID, driveOpts{
		Review:      *review,
		AutoApprove: *autoApprove,
		Timeout:     *timeout,
	}, bufio.NewReader(os.Stdin))
}
