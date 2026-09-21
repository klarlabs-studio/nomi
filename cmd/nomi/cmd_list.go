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
		fmt.Fprintln(os.Stderr, "nomi list: target required (runs, assistants, providers, approvals, memory, schedules, skills, recipes)")
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
	case "schedules":
		return listSchedules(cli, common, *limit)
	case "skills":
		return listSkills(cli, common, *limit)
	case "recipes":
		return listRecipes(cli, common, *limit)
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
	_, _ = fmt.Fprintln(w, "ID\tSTATUS\tCREATED\tGOAL")
	for i, r := range resp.Runs {
		if i >= n {
			break
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
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
	_, _ = fmt.Fprintln(w, "ID\tNAME\tROLE\tCAPABILITIES")
	for i, a := range resp.Assistants {
		if i >= n {
			break
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
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
	_, _ = fmt.Fprintln(w, "ID\tNAME\tTYPE\tENDPOINT\tMODELS")
	for i, p := range resp.Profiles {
		if i >= n {
			break
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
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
	_, _ = fmt.Fprintln(w, "ID\tRUN\tSTATUS\tCAPABILITY")
	for i, a := range resp.Approvals {
		if i >= n {
			break
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
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
	_, _ = fmt.Fprintln(w, "ID\tSCOPE\tCREATED\tCONTENT")
	for i, m := range resp.Memories {
		if i >= n {
			break
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			short(m.ID), m.Scope, ago(m.CreatedAt), trunc(m.Content, 80))
	}
	return doFlush(w)
}

func listSchedules(cli *Client, c *commonFlags, n int) int {
	var resp struct {
		Schedules []struct {
			ID         string  `json:"id"`
			Prompt     string  `json:"prompt"`
			CronExpr   string  `json:"cron_expr"`
			NLPhrase   string  `json:"nl_phrase"`
			Enabled    bool    `json:"enabled"`
			NextFireAt string  `json:"next_fire_at"`
			LastFireAt *string `json:"last_fire_at"`
			LastRunID  string  `json:"last_run_id"`
			LastError  string  `json:"last_error"`
		} `json:"schedules"`
	}
	if err := cli.Get("/schedules", &resp); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if c.JSON {
		printJSON(resp)
		return 0
	}
	w := newTab()
	_, _ = fmt.Fprintln(w, "ID\tON\tNEXT\tLAST\tLAST_RUN\tERROR\tPROMPT")
	for i, s := range resp.Schedules {
		if i >= n {
			break
		}
		on := "no"
		if s.Enabled {
			on = "yes"
		}
		next := formatWhen(s.NextFireAt)
		last := "-"
		if s.LastFireAt != nil && *s.LastFireAt != "" {
			last = ago(*s.LastFireAt)
		}
		lastRun := "-"
		if s.LastRunID != "" {
			lastRun = short(s.LastRunID)
		}
		errCol := trunc(s.LastError, 24)
		if errCol == "" {
			errCol = "-"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			short(s.ID), on, next, last, lastRun, errCol, trunc(s.Prompt, 40))
	}
	return doFlush(w)
}

func listSkills(cli *Client, c *commonFlags, n int) int {
	var resp struct {
		Suggestions []struct {
			ID                 string   `json:"id"`
			RepresentativeGoal string   `json:"representative_goal"`
			CommonTokens       []string `json:"common_tokens"`
			SourceRunIDs       []string `json:"source_run_ids"`
			Size               int      `json:"size"`
		} `json:"suggestions"`
	}
	if err := cli.Get("/skills/suggestions", &resp); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if c.JSON {
		printJSON(resp)
		return 0
	}
	w := newTab()
	_, _ = fmt.Fprintln(w, "ID\tSIZE\tRUNS\tTOKENS\tGOAL")
	for i, s := range resp.Suggestions {
		if i >= n {
			break
		}
		tokens := strings.Join(s.CommonTokens, ",")
		if tokens == "" {
			tokens = "-"
		}
		_, _ = fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\n",
			short(s.ID), s.Size, len(s.SourceRunIDs), trunc(tokens, 24), trunc(s.RepresentativeGoal, 50))
	}
	return doFlush(w)
}

func listRecipes(cli *Client, c *commonFlags, n int) int {
	var resp struct {
		Recipes []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Version string `json:"version"`
			Source  string `json:"source"`
			SHA256  string `json:"sha256"`
		} `json:"recipes"`
	}
	if err := cli.Get("/recipes", &resp); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if c.JSON {
		printJSON(resp)
		return 0
	}
	w := newTab()
	_, _ = fmt.Fprintln(w, "ID\tSOURCE\tVERSION\tNAME")
	for i, r := range resp.Recipes {
		if i >= n {
			break
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", trunc(r.ID, 28), r.Source, r.Version, trunc(r.Name, 40))
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

// formatWhen renders a timestamp that may be in the future (next fire)
// as "in 5m" / "in 3h" or falls back to ago() for past times.
func formatWhen(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t, err = time.Parse(time.RFC3339Nano, ts)
	}
	if err != nil {
		return ts
	}
	d := time.Until(t)
	if d <= 0 {
		return ago(ts)
	}
	switch {
	case d < time.Minute:
		return "soon"
	case d < time.Hour:
		return fmt.Sprintf("in %dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("in %dh", int(d.Hours()))
	default:
		return fmt.Sprintf("in %dd", int(d.Hours()/24))
	}
}
