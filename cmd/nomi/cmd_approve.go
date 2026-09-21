package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
)

type approvalRow struct {
	ID         string         `json:"id"`
	RunID      string         `json:"run_id"`
	Status     string         `json:"status"`
	Capability string         `json:"capability"`
	Context    map[string]any `json:"context"`
}

// approveCmd resolves a pending tool approval card (or lists them).
// Pair with `nomi deny` — Claude Code SSH parity after `nomi review`
// closed the plan_review attach gap.
//
//	nomi approve
//	nomi approve <id-or-prefix>
//	nomi approve --list
//	nomi deny …
func approveCmd(common *commonFlags, args []string) int {
	return resolveApprovalCmd(common, args, true)
}

func denyCmd(common *commonFlags, args []string) int {
	return resolveApprovalCmd(common, args, false)
}

func resolveApprovalCmd(common *commonFlags, args []string, approved bool) int {
	name := "approve"
	if !approved {
		name = "deny"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	bindCommonFlags(fs, common)
	listOnly := fs.Bool("list", false, "list pending tool approvals and exit")
	_ = fs.Parse(args)

	cli, err := NewClient(common)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	pending, err := listPendingApprovals(cli)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	if *listOnly {
		return printPendingApprovals(common, pending)
	}

	rest := fs.Args()
	var id string
	switch {
	case len(rest) >= 1:
		id, err = resolveApprovalID(rest[0], pending)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	case len(pending) == 0:
		fmt.Fprintf(os.Stderr, "nomi %s: no pending tool approvals (try `nomi list approvals`)\n", name)
		return 1
	case len(pending) == 1:
		id = pending[0].ID
	default:
		_ = printPendingApprovals(common, pending)
		fmt.Fprintf(os.Stderr, "nomi %s: multiple pending — pass an approval id (or prefix)\n", name)
		return 2
	}

	var target *approvalRow
	for i := range pending {
		if pending[i].ID == id {
			target = &pending[i]
			break
		}
	}
	if target == nil {
		fmt.Fprintf(os.Stderr, "nomi %s: approval %s not pending\n", name, short(id))
		return 1
	}

	body := map[string]any{"approved": approved}
	if err := cli.Post("/approvals/"+id+"/resolve", body, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	verb := "approved"
	if !approved {
		verb = "denied"
	}
	fmt.Fprintf(os.Stderr, "✓ %s %s", verb, target.Capability)
	if t, _ := target.Context["tool"].(string); t != "" {
		fmt.Fprintf(os.Stderr, " (tool: %s)", t)
	}
	if in, _ := target.Context["input"].(string); in != "" {
		fmt.Fprintf(os.Stderr, " %q", truncateRunes(in, 80))
	}
	fmt.Fprintf(os.Stderr, " · run %s\n", short(target.RunID))
	return 0
}

func listPendingApprovals(cli *Client) ([]approvalRow, error) {
	var resp struct {
		Approvals []approvalRow `json:"approvals"`
	}
	if err := cli.Get("/approvals", &resp); err != nil {
		return nil, err
	}
	out := make([]approvalRow, 0)
	for _, a := range resp.Approvals {
		if a.Status == "pending" {
			out = append(out, a)
		}
	}
	return out, nil
}

func printPendingApprovals(c *commonFlags, rows []approvalRow) int {
	if c.JSON {
		printJSON(map[string]any{"approvals": rows})
		return 0
	}
	if len(rows) == 0 {
		fmt.Println("(no pending tool approvals)")
		return 0
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tRUN\tCAPABILITY\tTOOL")
	for _, a := range rows {
		tool, _ := a.Context["tool"].(string)
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			short(a.ID), short(a.RunID), a.Capability, trunc(tool, 40))
	}
	_ = w.Flush()
	return 0
}

func resolveApprovalID(hint string, pending []approvalRow) (string, error) {
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return "", fmt.Errorf("approval id required")
	}
	var matches []string
	for _, a := range pending {
		if a.ID == hint || strings.HasPrefix(a.ID, hint) {
			matches = append(matches, a.ID)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("pending approval %q not found", hint)
	default:
		return "", fmt.Errorf("ambiguous approval id %q matches %d pending", hint, len(matches))
	}
}
