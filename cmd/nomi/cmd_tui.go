package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// tuiCmd is a thin Bubble Tea dashboard over the same REST surface as
// `nomi status` / `list` / `approve` / `review`. Not a full Claude Code
// TUI — just SSH-friendly live status + one-key approve/deny/cancel.
//
//	nomi tui
func tuiCmd(common *commonFlags, args []string) int {
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	bindCommonFlags(fs, common)
	_ = fs.Parse(args)

	cli, err := NewClient(common)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	m := newTUIModel(cli)
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

type tuiTab int

const (
	tabRuns tuiTab = iota
	tabApprovals
	tabReviews
)

func (t tuiTab) String() string {
	switch t {
	case tabApprovals:
		return "Approvals"
	case tabReviews:
		return "Reviews"
	default:
		return "Runs"
	}
}

type tuiSnapshot struct {
	URL       string
	Health    string
	Version   string
	Pending   pendingSummary
	Runs      []runListRow
	Approvals []approvalRow
	Reviews   []runListRow
	Err       string
	At        time.Time
}

type tickMsg time.Time
type snapMsg tuiSnapshot
type actionDoneMsg struct {
	ok  string
	err string
}

type tuiModel struct {
	cli    *Client
	tab    tuiTab
	cursor int
	snap   tuiSnapshot
	status string
	width  int
	height int
}

func newTUIModel(cli *Client) tuiModel {
	return tuiModel{
		cli:  cli,
		tab:  tabRuns,
		snap: tuiSnapshot{URL: cli.URL, Health: "…"},
	}
}

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(m.fetchSnap(), tickCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m tuiModel) fetchSnap() tea.Cmd {
	cli := m.cli
	return func() tea.Msg {
		return snapMsg(loadTUISnapshot(cli))
	}
}

func loadTUISnapshot(cli *Client) tuiSnapshot {
	s := tuiSnapshot{URL: cli.URL, At: time.Now()}
	var health struct {
		Status string `json:"status"`
	}
	if err := cli.Get("/health", &health); err != nil {
		s.Err = err.Error()
		s.Health = "unreachable"
		return s
	}
	s.Health = health.Status
	var version struct {
		Version string `json:"version"`
	}
	_ = cli.Get("/version", &version)
	s.Version = version.Version
	s.Pending = loadPendingSummary(cli)

	var runsResp struct {
		Runs []runListRow `json:"runs"`
	}
	if err := cli.Get("/runs", &runsResp); err == nil {
		// Prefer active / interesting first, then recent.
		s.Runs = prioritizeRuns(runsResp.Runs, 40)
	}
	if a, err := listPendingApprovals(cli); err == nil {
		s.Approvals = a
	}
	if r, err := listPlanReviewRuns(cli); err == nil {
		s.Reviews = r
	}
	return s
}

func prioritizeRuns(in []runListRow, limit int) []runListRow {
	active := make([]runListRow, 0)
	rest := make([]runListRow, 0)
	for _, r := range in {
		switch r.Status {
		case "plan_review", "awaiting_approval", "executing", "paused", "planning":
			active = append(active, r)
		default:
			rest = append(rest, r)
		}
	}
	out := append(active, rest...)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tickMsg:
		return m, tea.Batch(m.fetchSnap(), tickCmd())

	case snapMsg:
		m.snap = tuiSnapshot(msg)
		m.clampCursor()
		return m, nil

	case actionDoneMsg:
		if msg.err != "" {
			m.status = "error: " + msg.err
		} else {
			m.status = msg.ok
		}
		return m, m.fetchSnap()

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "r", "ctrl+r":
			m.status = "refreshing…"
			return m, m.fetchSnap()
		case "tab", "right", "l":
			m.tab = (m.tab + 1) % 3
			m.cursor = 0
			return m, nil
		case "shift+tab", "left", "h":
			m.tab = (m.tab + 2) % 3
			m.cursor = 0
			return m, nil
		case "1":
			m.tab, m.cursor = tabRuns, 0
			return m, nil
		case "2":
			m.tab, m.cursor = tabApprovals, 0
			return m, nil
		case "3":
			m.tab, m.cursor = tabReviews, 0
			return m, nil
		case "j", "down":
			m.cursor++
			m.clampCursor()
			return m, nil
		case "k", "up":
			m.cursor--
			m.clampCursor()
			return m, nil
		case "a":
			return m, m.cmdApprove(true)
		case "d":
			return m, m.cmdApprove(false)
		case "c":
			return m, m.cmdCancel()
		case "p":
			return m, m.cmdPauseResume(true)
		case "u":
			return m, m.cmdPauseResume(false)
		case "enter":
			// On reviews tab, Enter = approve plan (thin; full edit → nomi review).
			if m.tab == tabReviews {
				return m, m.cmdApprovePlan()
			}
			if m.tab == tabApprovals {
				return m, m.cmdApprove(true)
			}
			return m, nil
		}
	}
	return m, nil
}

func (m *tuiModel) clampCursor() {
	n := m.rowCount()
	if n == 0 {
		m.cursor = 0
		return
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= n {
		m.cursor = n - 1
	}
}

func (m tuiModel) rowCount() int {
	switch m.tab {
	case tabApprovals:
		return len(m.snap.Approvals)
	case tabReviews:
		return len(m.snap.Reviews)
	default:
		return len(m.snap.Runs)
	}
}

func (m tuiModel) cmdApprove(approved bool) tea.Cmd {
	cli := m.cli
	switch m.tab {
	case tabApprovals:
		if m.cursor < 0 || m.cursor >= len(m.snap.Approvals) {
			return nil
		}
		id := m.snap.Approvals[m.cursor].ID
		capName := m.snap.Approvals[m.cursor].Capability
		return func() tea.Msg {
			body := map[string]any{"approved": approved}
			if err := cli.Post("/approvals/"+id+"/resolve", body, nil); err != nil {
				return actionDoneMsg{err: err.Error()}
			}
			verb := "approved"
			if !approved {
				verb = "denied"
			}
			return actionDoneMsg{ok: verb + " " + capName}
		}
	case tabReviews:
		if approved {
			return m.cmdApprovePlan()
		}
		// Deny plan ≈ cancel the run.
		return m.cmdCancelSelectedReview()
	default:
		return nil
	}
}

func (m tuiModel) cmdApprovePlan() tea.Cmd {
	cli := m.cli
	if m.cursor < 0 || m.cursor >= len(m.snap.Reviews) {
		return nil
	}
	id := m.snap.Reviews[m.cursor].ID
	return func() tea.Msg {
		if err := cli.Post("/runs/"+id+"/plan/approve", map[string]any{}, nil); err != nil {
			return actionDoneMsg{err: err.Error()}
		}
		return actionDoneMsg{ok: "plan approved · " + short(id)}
	}
}

func (m tuiModel) cmdCancelSelectedReview() tea.Cmd {
	cli := m.cli
	if m.cursor < 0 || m.cursor >= len(m.snap.Reviews) {
		return nil
	}
	id := m.snap.Reviews[m.cursor].ID
	return func() tea.Msg {
		if err := cli.Post("/runs/"+id+"/cancel", nil, nil); err != nil {
			return actionDoneMsg{err: err.Error()}
		}
		return actionDoneMsg{ok: "denied/cancelled · " + short(id)}
	}
}

func (m tuiModel) cmdCancel() tea.Cmd {
	cli := m.cli
	id := m.selectedRunID()
	if id == "" {
		return nil
	}
	return func() tea.Msg {
		if err := cli.Post("/runs/"+id+"/cancel", nil, nil); err != nil {
			return actionDoneMsg{err: err.Error()}
		}
		return actionDoneMsg{ok: "cancelled · " + short(id)}
	}
}

func (m tuiModel) cmdPauseResume(pause bool) tea.Cmd {
	cli := m.cli
	id := m.selectedRunID()
	if id == "" {
		return nil
	}
	path := "/runs/" + id + "/resume"
	verb := "resumed"
	if pause {
		path = "/runs/" + id + "/pause"
		verb = "paused"
	}
	return func() tea.Msg {
		if err := cli.Post(path, nil, nil); err != nil {
			return actionDoneMsg{err: err.Error()}
		}
		return actionDoneMsg{ok: verb + " · " + short(id)}
	}
}

func (m tuiModel) selectedRunID() string {
	switch m.tab {
	case tabRuns:
		if m.cursor >= 0 && m.cursor < len(m.snap.Runs) {
			return m.snap.Runs[m.cursor].ID
		}
	case tabReviews:
		if m.cursor >= 0 && m.cursor < len(m.snap.Reviews) {
			return m.snap.Reviews[m.cursor].ID
		}
	case tabApprovals:
		if m.cursor >= 0 && m.cursor < len(m.snap.Approvals) {
			return m.snap.Approvals[m.cursor].RunID
		}
	}
	return ""
}

var (
	styleTitle  = lipgloss.NewStyle().Bold(true)
	styleMuted  = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	styleOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("70"))
	styleWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("178"))
	styleErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("167"))
	styleSel    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("255")).Background(lipgloss.Color("238"))
	styleTabOn  = lipgloss.NewStyle().Bold(true).Underline(true)
	styleTabOff = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
)

func (m tuiModel) View() string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("nomi tui"))
	b.WriteString("  ")
	b.WriteString(styleMuted.Render(m.snap.URL))
	b.WriteString("\n")

	health := m.snap.Health
	if health == "ok" || health == "healthy" {
		b.WriteString(styleOK.Render("● " + health))
	} else if health == "unreachable" {
		b.WriteString(styleErr.Render("● " + health))
	} else {
		b.WriteString(styleWarn.Render("● " + health))
	}
	if m.snap.Version != "" {
		b.WriteString(styleMuted.Render("  v" + m.snap.Version))
	}
	p := m.snap.Pending
	b.WriteString(fmt.Sprintf("  reviews:%d  approvals:%d  active:%d",
		p.PlanReviews, p.ToolApprovals, p.ActiveRuns))
	if !m.snap.At.IsZero() {
		b.WriteString(styleMuted.Render("  · " + m.snap.At.Format("15:04:05")))
	}
	b.WriteString("\n\n")

	for i, t := range []tuiTab{tabRuns, tabApprovals, tabReviews} {
		label := fmt.Sprintf(" %d:%s ", i+1, t.String())
		if t == m.tab {
			b.WriteString(styleTabOn.Render(label))
		} else {
			b.WriteString(styleTabOff.Render(label))
		}
		b.WriteString(" ")
	}
	b.WriteString("\n\n")

	if m.snap.Err != "" {
		b.WriteString(styleErr.Render(m.snap.Err))
		b.WriteString("\n\n")
	}

	b.WriteString(m.renderRows())
	b.WriteString("\n")
	if m.status != "" {
		b.WriteString(styleMuted.Render(m.status))
		b.WriteString("\n")
	}
	b.WriteString(styleMuted.Render(
		"tab/1-3 switch · j/k move · a approve · d deny · c cancel · p pause · u resume · r refresh · q quit",
	))
	if m.tab == tabReviews {
		b.WriteString("\n")
		b.WriteString(styleMuted.Render("enter = approve plan (for step edit use: nomi review <id>)"))
	}
	return b.String()
}

func (m tuiModel) renderRows() string {
	switch m.tab {
	case tabApprovals:
		return m.renderApprovals()
	case tabReviews:
		return m.renderReviews()
	default:
		return m.renderRuns()
	}
}

func (m tuiModel) renderRuns() string {
	if len(m.snap.Runs) == 0 {
		return styleMuted.Render("(no runs)")
	}
	var b strings.Builder
	b.WriteString(styleMuted.Render(fmt.Sprintf("%-10s %-18s %s\n", "ID", "STATUS", "GOAL")))
	for i, r := range m.snap.Runs {
		line := fmt.Sprintf("%-10s %-18s %s", short(r.ID), r.Status, trunc(r.Goal, 56))
		if i == m.cursor {
			b.WriteString(styleSel.Render(line))
		} else {
			b.WriteString(line)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (m tuiModel) renderApprovals() string {
	if len(m.snap.Approvals) == 0 {
		return styleMuted.Render("(no pending tool approvals)")
	}
	var b strings.Builder
	b.WriteString(styleMuted.Render(fmt.Sprintf("%-10s %-28s %s\n", "ID", "CAPABILITY", "RUN")))
	for i, a := range m.snap.Approvals {
		tool, _ := a.Context["tool"].(string)
		cap := a.Capability
		if tool != "" {
			cap = cap + " · " + tool
		}
		line := fmt.Sprintf("%-10s %-28s %s", short(a.ID), trunc(cap, 28), short(a.RunID))
		if i == m.cursor {
			b.WriteString(styleSel.Render(line))
		} else {
			b.WriteString(line)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (m tuiModel) renderReviews() string {
	if len(m.snap.Reviews) == 0 {
		return styleMuted.Render("(no runs awaiting plan review)")
	}
	var b strings.Builder
	b.WriteString(styleMuted.Render(fmt.Sprintf("%-10s %s\n", "ID", "GOAL")))
	for i, r := range m.snap.Reviews {
		line := fmt.Sprintf("%-10s %s", short(r.ID), trunc(r.Goal, 64))
		if i == m.cursor {
			b.WriteString(styleSel.Render(line))
		} else {
			b.WriteString(line)
		}
		b.WriteString("\n")
	}
	return b.String()
}
