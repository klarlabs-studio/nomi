package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

// listCmd renders compact tables for each surface. The CLI's
// equivalent of clicking a tab in the desktop UI.
//
//	nomi list runs
//	nomi list assistants
//	nomi list providers
//	nomi list approvals
//	nomi list memory
func listCmd(common *commonFlags, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "nomi list: target required (runs, assistants, providers, approvals, memory)")
		return 2
	}
	target := args[0]
	fs := flag.NewFlagSet("list "+target, flag.ExitOnError)
	bindCommonFlags(fs, common)
	limit := fs.Int("limit", 20, "max rows to print")
	_ = fs.Parse(args[1:])

	cli, err := NewClient(common)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	switch target {
	case "runs":
		return listRuns(cli, common, *limit)
	case "assistants":
		return listAssistants(cli, common, *limit)
	case "providers":
		return listProviders(cli, common, *limit)
	case "approvals":
		return listApprovals(cli, common, *limit)
	case "memory":
		return listMemory(cli, common, *limit)
	default:
		fmt.Fprintf(os.Stderr, "nomi list: unknown target %q\n", target)
		return 2
	}
}

func newTab() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
}

func listRuns(cli *Client, c *commonFlags, n int) int {
	var resp struct {
		Runs []struct {
			ID          string `json:"id"`
			Status      string `json:"status"`
			Goal        string `json:"goal"`
			CreatedAt   string `json:"created_at"`
			AssistantID string `json:"assistant_id"`
		} `json:"runs"`
	}
	if err := cli.Get("/runs", &resp); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if c.JSON {
		printJSON(resp)
		return 0
	}
	w := newTab()
	fmt.Fprintln(w, "ID\tSTATUS\tCREATED\tGOAL")
	for i, r := range resp.Runs {
		if i >= n {
			break
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			short(r.ID), r.Status, ago(r.CreatedAt), trunc(r.Goal, 60))
	}
	return doFlush(w)
}

func listAssistants(cli *Client, c *commonFlags, n int) int {
	var resp struct {
		Assistants []struct {
			ID, Name, Role string
			Capabilities   []string
		} `json:"assistants"`
	}
	if err := cli.Get("/assistants", &resp); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if c.JSON {
		printJSON(resp)
		return 0
	}
	w := newTab()
	fmt.Fprintln(w, "ID\tNAME\tROLE\tCAPABILITIES")
	for i, a := range resp.Assistants {
		if i >= n {
			break
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			short(a.ID), a.Name, a.Role, strings.Join(a.Capabilities, ","))
	}
	return doFlush(w)
}

func listProviders(cli *Client, c *commonFlags, n int) int {
	var resp struct {
		Profiles []struct {
			ID, Name, Type, Endpoint string
			ModelIDs                 []string `json:"model_ids"`
			Enabled                  bool
		} `json:"profiles"`
	}
	if err := cli.Get("/provider-profiles", &resp); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if c.JSON {
		printJSON(resp)
		return 0
	}
	w := newTab()
	fmt.Fprintln(w, "ID\tNAME\tTYPE\tENDPOINT\tMODELS")
	for i, p := range resp.Profiles {
		if i >= n {
			break
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			short(p.ID), p.Name, p.Type, p.Endpoint, strings.Join(p.ModelIDs, ","))
	}
	return doFlush(w)
}

func listApprovals(cli *Client, c *commonFlags, n int) int {
	var resp struct {
		Approvals []struct {
			ID, RunID, Status, Capability string
		} `json:"approvals"`
	}
	if err := cli.Get("/approvals", &resp); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if c.JSON {
		printJSON(resp)
		return 0
	}
	w := newTab()
	fmt.Fprintln(w, "ID\tRUN\tSTATUS\tCAPABILITY")
	for i, a := range resp.Approvals {
		if i >= n {
			break
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			short(a.ID), short(a.RunID), a.Status, a.Capability)
	}
	return doFlush(w)
}

func listMemory(cli *Client, c *commonFlags, n int) int {
	var resp struct {
		Memories []struct {
			ID, Scope, Content, CreatedAt string
		} `json:"memories"`
	}
	// Hit each scope so the user sees everything the assistant knows
	// (the API defaults to workspace+profile and elides preferences).
	for _, scope := range []string{"workspace", "profile", "preferences"} {
		var page struct {
			Memories []struct {
				ID, Scope, Content, CreatedAt string
			} `json:"memories"`
		}
		_ = cli.Get("/memory?scope="+scope+"&limit=100", &page)
		resp.Memories = append(resp.Memories, page.Memories...)
	}
	if c.JSON {
		printJSON(resp)
		return 0
	}
	w := newTab()
	fmt.Fprintln(w, "ID\tSCOPE\tCREATED\tCONTENT")
	for i, m := range resp.Memories {
		if i >= n {
			break
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			short(m.ID), m.Scope, ago(m.CreatedAt), trunc(m.Content, 80))
	}
	return doFlush(w)
}

// doFlush writes the buffered table and returns 0/1 for `os.Exit`.
func doFlush(w *tabwriter.Writer) int {
	if err := w.Flush(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// short keeps tables narrow by clipping uuids to 8 chars.
func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func trunc(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// ago renders an RFC3339 timestamp as "5m ago" / "3h ago" / "yesterday".
// Falls back to the raw string if parsing fails.
func ago(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t, err = time.Parse(time.RFC3339Nano, ts)
	}
	if err != nil {
		return ts
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 48*time.Hour:
		return "yesterday"
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
