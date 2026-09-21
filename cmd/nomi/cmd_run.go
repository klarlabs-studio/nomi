package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// runCmd: submit a goal, drive the run end-to-end, return its output.
//
//	nomi run "summarize notes.md"
//	nomi run --assistant=Researcher --auto-approve "ack"
//	nomi run --review "fix the flaky test"
//
// Plans are auto-approved by default (typical headless flow). Pass
// --review for an interactive Plan→Diff→Approve loop (Claude Code
// style). Confirm-mode capabilities (filesystem.write, command.exec)
// still prompt on stdin unless --auto-approve is passed.
func runCmd(common *commonFlags, args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	bindCommonFlags(fs, common)
	assistant := fs.String("assistant", "", "assistant name (default: first configured)")
	autoApprove := fs.Bool("auto-approve", false, "auto-approve confirm-mode capabilities (DANGEROUS)")
	review := fs.Bool("review", false, "interactive plan review: print plan + diffs, Approve/Deny/Edit (drop steps / skip hunks)")
	timeout := fs.Duration("timeout", 5*time.Minute, "give up after this long if the run doesn't reach a terminal state")
	_ = fs.Parse(args)

	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "nomi run: goal required")
		return 2
	}
	goal := strings.Join(rest, " ")

	cli, err := NewClient(common)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	// Resolve assistant id from name (or pick first if unspecified).
	asID, asName, err := resolveAssistant(cli, *assistant)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := cli.Post("/runs", map[string]any{"goal": goal, "assistant_id": asID}, &created); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "▶ run %s submitted to %s\n", created.ID[:8], asName)

	return driveRun(cli, created.ID, driveOpts{
		Review:      *review,
		AutoApprove: *autoApprove,
		Timeout:     *timeout,
	}, bufio.NewReader(os.Stdin))
}

type driveOpts struct {
	Review      bool
	AutoApprove bool
	Timeout     time.Duration
}

// driveRun polls a run through plan_review / approvals / terminal states.
// Shared by `nomi run` (after create), `nomi review`, and `nomi watch`.
// Ctrl+C / SIGTERM posts POST /runs/:id/cancel so the daemon stops too.
func driveRun(cli *Client, runID string, opts driveOpts, stdin *bufio.Reader) int {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	interrupted := make(chan struct{})
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "▶ cancelling run (interrupt)")
		if err := cli.Post("/runs/"+runID+"/cancel", map[string]any{}, nil); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		close(interrupted)
	}()

	deadline := time.Now().Add(opts.Timeout)
	planHandled := false
	seenSteps := map[string]string{}
	for time.Now().Before(deadline) {
		select {
		case <-interrupted:
			fmt.Fprintln(os.Stderr, "✗ cancelled")
			return 130
		default:
		}

		var detail struct {
			Run struct {
				ID     string `json:"id"`
				Goal   string `json:"goal"`
				Status string `json:"status"`
			} `json:"run"`
			Plan  *planPayload      `json:"plan"`
			Steps []stepProgressRow `json:"steps"`
		}
		if err := cli.Get("/runs/"+runID, &detail); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}

		printStepProgress(os.Stderr, detail.Steps, detail.Plan, seenSteps)

		switch detail.Run.Status {
		case "plan_review":
			if planHandled {
				if !sleepOrInterrupt(500*time.Millisecond, interrupted) {
					fmt.Fprintln(os.Stderr, "✗ cancelled")
					return 130
				}
				continue
			}
			if opts.Review {
				for {
					select {
					case <-interrupted:
						fmt.Fprintln(os.Stderr, "✗ cancelled")
						return 130
					default:
					}
					fmt.Fprint(os.Stderr, formatPlanReview(detail.Run.Goal, detail.Plan))
					decision, err := promptPlanDecision(stdin)
					if err != nil {
						// Likely interrupted mid-prompt.
						select {
						case <-interrupted:
							fmt.Fprintln(os.Stderr, "✗ cancelled")
							return 130
						default:
							fmt.Fprintln(os.Stderr, err)
							return 1
						}
					}
					switch decision {
					case planDecisionDeny:
						fmt.Fprintln(os.Stderr, "▶ denying plan (cancelling run)")
						if err := cli.Post("/runs/"+runID+"/cancel", map[string]any{}, nil); err != nil {
							fmt.Fprintln(os.Stderr, err)
							return 1
						}
						planHandled = true
					case planDecisionEdit:
						edited, notes, err := interactivePlanEdit(stdin, detail.Plan)
						if err != nil {
							fmt.Fprintln(os.Stderr, err)
							continue
						}
						if edited == nil {
							fmt.Fprintln(os.Stderr, "  (no changes)")
							continue
						}
						for _, n := range notes {
							fmt.Fprintln(os.Stderr, n)
						}
						if err := cli.Post("/runs/"+runID+"/plan/edit", editPlanBody(edited), nil); err != nil {
							fmt.Fprintln(os.Stderr, err)
							return 1
						}
						if err := cli.Get("/runs/"+runID, &detail); err != nil {
							fmt.Fprintln(os.Stderr, err)
							return 1
						}
						printStepProgress(os.Stderr, detail.Steps, detail.Plan, seenSteps)
						continue
					default: // approve
						fmt.Fprintln(os.Stderr, "▶ approving plan")
						if err := cli.Post("/runs/"+runID+"/plan/approve", map[string]any{}, nil); err != nil {
							fmt.Fprintln(os.Stderr, err)
							return 1
						}
						planHandled = true
					}
					break
				}
			} else {
				fmt.Fprintln(os.Stderr, "▶ plan ready, approving")
				if err := cli.Post("/runs/"+runID+"/plan/approve", map[string]any{}, nil); err != nil {
					fmt.Fprintln(os.Stderr, err)
					return 1
				}
				planHandled = true
			}
		case "awaiting_approval":
			if !handleApproval(cli, runID, opts.AutoApprove, stdin) {
				select {
				case <-interrupted:
					fmt.Fprintln(os.Stderr, "✗ cancelled")
					return 130
				default:
					return 1
				}
			}
		case "completed":
			for _, s := range detail.Steps {
				if s.Output != "" {
					fmt.Println(s.Output)
				}
			}
			fmt.Fprintln(os.Stderr, "✓ done")
			return 0
		case "failed":
			fmt.Fprintln(os.Stderr, "✗ failed")
			return 1
		case "cancelled":
			fmt.Fprintln(os.Stderr, "✗ cancelled")
			return 1
		}
		if !sleepOrInterrupt(2*time.Second, interrupted) {
			fmt.Fprintln(os.Stderr, "✗ cancelled")
			return 130
		}
	}
	fmt.Fprintf(os.Stderr, "✗ timed out after %s\n", opts.Timeout)
	return 1
}

// sleepOrInterrupt waits for d or returns false if interrupted.
func sleepOrInterrupt(d time.Duration, interrupted <-chan struct{}) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-interrupted:
		return false
	case <-t.C:
		return true
	}
}

// stepProgressRow is the subset of a run step used for live progress.
type stepProgressRow struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// printStepProgress writes one stderr line per step status change so
// `nomi run` isn't silent between the 2s polls (Claude Code parity).
// Tool name is appended when the plan still carries expected_tool.
func printStepProgress(w io.Writer, steps []stepProgressRow, plan *planPayload, seen map[string]string) {
	toolByID := map[string]string{}
	if plan != nil {
		for _, ps := range plan.Steps {
			if ps.ID != "" && ps.ExpectedTool != "" {
				toolByID[ps.ID] = ps.ExpectedTool
			}
		}
	}
	for i, s := range steps {
		key := s.ID
		if key == "" {
			key = fmt.Sprintf("#%d", i)
		}
		if seen[key] == s.Status {
			continue
		}
		seen[key] = s.Status
		label := strings.TrimSpace(s.Title)
		if label == "" {
			label = "step"
		}
		if tool := toolByID[s.ID]; tool != "" && tool != label {
			label = fmt.Sprintf("%s (%s)", label, tool)
		}
		switch s.Status {
		case "running":
			_, _ = fmt.Fprintf(w, "→ %s\n", label)
		case "retrying":
			_, _ = fmt.Fprintf(w, "↻ %s (retry)\n", label)
		case "blocked":
			_, _ = fmt.Fprintf(w, "⏸ %s (blocked)\n", label)
		case "done":
			_, _ = fmt.Fprintf(w, "✓ %s\n", label)
		case "failed":
			if s.Error != "" {
				_, _ = fmt.Fprintf(w, "✗ %s: %s\n", label, s.Error)
			} else {
				_, _ = fmt.Fprintf(w, "✗ %s\n", label)
			}
		}
	}
}

type planDecision int

const (
	planDecisionApprove planDecision = iota
	planDecisionDeny
	planDecisionEdit
)

func promptPlanDecision(stdin *bufio.Reader) (planDecision, error) {
	fmt.Fprint(os.Stderr, "? [A]pprove / [D]eny / [E]dit (drop steps / skip hunks): ")
	line, err := stdin.ReadString('\n')
	if err != nil {
		return planDecisionDeny, err
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	switch ans {
	case "a", "approve", "y", "yes":
		return planDecisionApprove, nil
	case "e", "edit", "drop":
		return planDecisionEdit, nil
	case "d", "deny", "n", "no", "":
		return planDecisionDeny, nil
	default:
		fmt.Fprintln(os.Stderr, "  (expected a/d/e — denying)")
		return planDecisionDeny, nil
	}
}

// interactivePlanEdit prompts for step drops and patch hunk skips.
// Returns nil plan when the user made no changes.
func interactivePlanEdit(stdin *bufio.Reader, plan *planPayload) (*planPayload, []string, error) {
	if plan == nil || len(plan.Steps) == 0 {
		return nil, nil, fmt.Errorf("plan has no steps to edit")
	}
	var notes []string
	working := plan

	nums, err := promptDropSteps(stdin, working)
	if err != nil {
		return nil, nil, err
	}
	stepsDropped := false
	if len(nums) > 0 {
		edited, err := dropPlanSteps(working, nums)
		if err != nil {
			return nil, nil, err
		}
		notes = append(notes, fmt.Sprintf("▶ dropping %d step(s), keeping %d",
			len(working.Steps)-len(edited.Steps), len(edited.Steps)))
		working = edited
		stepsDropped = true
	}

	skippedByStep, err := promptSkipHunks(stdin, working)
	if err != nil {
		return nil, nil, err
	}
	hunksChanged := applyHunkSkipsToPlan(working, skippedByStep)
	if hunksChanged {
		notes = append(notes, "▶ skipped patch hunks")
	}
	if !stepsDropped && !hunksChanged {
		return nil, nil, nil
	}
	return working, notes, nil
}

// promptDropSteps asks which 1-based step numbers to remove.
// Empty input keeps all steps (proceed to hunk skip).
func promptDropSteps(stdin *bufio.Reader, plan *planPayload) ([]int, error) {
	n := 0
	if plan != nil {
		n = len(plan.Steps)
	}
	fmt.Fprintf(os.Stderr, "? Drop which steps (1–%d, comma-separated; empty = keep all): ", n)
	line, err := stdin.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, nil
	}
	parts := strings.FieldsFunc(line, func(r rune) bool {
		return r == ',' || r == ' ' || r == ';'
	})
	out := make([]int, 0, len(parts))
	seen := make(map[int]bool)
	for _, p := range parts {
		var v int
		if _, err := fmt.Sscanf(p, "%d", &v); err != nil {
			return nil, fmt.Errorf("invalid step number %q", p)
		}
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out, nil
}

// promptSkipHunks lists hunks for each filesystem.patch step and asks
// which 1-based hunk numbers to skip. Returns stepID → hunk keys.
func promptSkipHunks(stdin *bufio.Reader, plan *planPayload) (map[string][]string, error) {
	out := make(map[string][]string)
	if plan == nil {
		return out, nil
	}
	for si, s := range plan.Steps {
		if s.ExpectedTool != "filesystem.patch" || s.Arguments == nil {
			continue
		}
		diff, _ := s.Arguments["diff"].(string)
		if diff == "" {
			continue
		}
		hunks := listHunks(diff)
		if len(hunks) == 0 {
			continue
		}
		title := s.Title
		if title == "" {
			title = s.ExpectedTool
		}
		fmt.Fprintf(os.Stderr, "  Step %d (%s) hunks:\n", si+1, truncateRunes(title, 40))
		for i, h := range hunks {
			fmt.Fprintf(os.Stderr, "    %d. %s  +%d −%d  %s\n",
				i+1, h.fileLabel, h.added, h.removed, truncateRunes(h.header, 50))
		}
		fmt.Fprintf(os.Stderr, "? Skip which hunks for step %d (comma-separated; empty = keep all): ", si+1)
		line, err := stdin.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.FieldsFunc(line, func(r rune) bool {
			return r == ',' || r == ' ' || r == ';'
		})
		var keys []string
		seen := make(map[int]bool)
		for _, p := range parts {
			var v int
			if _, err := fmt.Sscanf(p, "%d", &v); err != nil {
				return nil, fmt.Errorf("invalid hunk number %q", p)
			}
			if v < 1 || v > len(hunks) {
				return nil, fmt.Errorf("hunk %d out of range (1–%d)", v, len(hunks))
			}
			if seen[v] {
				continue
			}
			seen[v] = true
			keys = append(keys, hunks[v-1].key)
		}
		if len(keys) == 0 {
			continue
		}
		id := s.ID
		if id == "" {
			id = fmt.Sprintf("%d", si)
		}
		out[id] = keys
	}
	return out, nil
}

func resolveAssistant(cli *Client, name string) (id, resolvedName string, err error) {
	var list struct {
		Assistants []struct {
			ID, Name string
		} `json:"assistants"`
	}
	if err := cli.Get("/assistants", &list); err != nil {
		return "", "", err
	}
	if len(list.Assistants) == 0 {
		return "", "", fmt.Errorf("no assistants configured: run `nomi seed` or open the desktop wizard first")
	}
	if name == "" {
		return list.Assistants[0].ID, list.Assistants[0].Name, nil
	}
	for _, a := range list.Assistants {
		if a.Name == name {
			return a.ID, a.Name, nil
		}
	}
	return "", "", fmt.Errorf("assistant %q not found (have: %s)", name, joinNames(list.Assistants))
}

func joinNames(as []struct{ ID, Name string }) string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.Name)
	}
	return strings.Join(out, ", ")
}

// handleApproval drains every pending approval card. Returns false on
// fatal error or user abort, true if all approvals were resolved.
func handleApproval(cli *Client, runID string, auto bool, stdin *bufio.Reader) bool {
	var list struct {
		Approvals []struct {
			ID         string         `json:"id"`
			RunID      string         `json:"run_id"`
			Status     string         `json:"status"`
			Capability string         `json:"capability"`
			Context    map[string]any `json:"context"`
		} `json:"approvals"`
	}
	if err := cli.Get("/approvals", &list); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return false
	}
	for _, a := range list.Approvals {
		if a.RunID != runID || a.Status != "pending" {
			continue
		}
		ok := auto
		if !auto {
			fmt.Fprintf(os.Stderr, "? approve %s ", a.Capability)
			if t, _ := a.Context["tool"].(string); t != "" {
				fmt.Fprintf(os.Stderr, "(tool: %s) ", t)
			}
			if in, _ := a.Context["input"].(string); in != "" {
				fmt.Fprintf(os.Stderr, "%q ", truncateRunes(in, 80))
			}
			fmt.Fprint(os.Stderr, "[y/N]: ")
			line, _ := stdin.ReadString('\n')
			ok = strings.EqualFold(strings.TrimSpace(line), "y") ||
				strings.EqualFold(strings.TrimSpace(line), "yes")
		}
		body := map[string]any{"approved": ok}
		if err := cli.Post("/approvals/"+a.ID+"/resolve", body, nil); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return false
		}
	}
	return true
}
