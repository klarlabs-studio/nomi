package github

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"go.klarlabs.de/nomi/internal/domain"
	gh "go.klarlabs.de/nomi/internal/integrations/github"
	"go.klarlabs.de/nomi/internal/plugins"
)

// IssueTools constructs the slice of github.issues.* tools the plugin
// contributes. Returned as a separate function so the plugin's Tools()
// can compose this with future families (pulls, repos) without
// growing one giant function.
func (p *Plugin) issueTools() []toolDef {
	return []toolDef{
		{
			name:       "github.issues.list",
			capability: "github.read",
			desc:       "List issues on a repo. Inputs: connection_id, owner, repo, state? (open|closed|all), labels?, assignee?, per_page?, page?",
			run:        p.issuesList,
		},
		{
			name:       "github.issues.get",
			capability: "github.read",
			desc:       "Get a single issue with body + comments. Inputs: connection_id, owner, repo, issue_number",
			run:        p.issuesGet,
		},
		{
			name:       "github.issues.create",
			capability: "github.write",
			desc:       "Open a new issue. Inputs: connection_id, owner, repo, title, body?, labels?, assignees?",
			run:        p.issuesCreate,
		},
		{
			name:       "github.issues.comment",
			capability: "github.write",
			desc:       "Add a comment to an existing issue. Inputs: connection_id, owner, repo, issue_number, body",
			run:        p.issuesComment,
		},
	}
}

// toolDef bundles the metadata and Execute closure for a single
// plugin-contributed tool. We can't reuse plugins.ToolContribution
// directly because it carries no Execute; this internal type maps
// 1:1 with both ToolContribution (for the manifest) and tools.Tool
// (for the registry).
type toolDef struct {
	name       string
	capability string
	desc       string
	run        func(ctx context.Context, conn *domain.Connection, input map[string]any) (map[string]any, error)
}

// asContribution renders this tool def into the manifest shape.
func (t toolDef) asContribution() plugins.ToolContribution {
	return plugins.ToolContribution{
		Name:               t.name,
		Capability:         t.capability,
		RequiresConnection: true,
		Description:        t.desc,
	}
}

// asTool wraps a toolDef as a tools.Tool by carrying the plugin
// pointer for connection-resolution.
type pluginTool struct {
	plugin *Plugin
	def    toolDef
}

func (p *pluginTool) Name() string       { return p.def.name }
func (p *pluginTool) Capability() string { return p.def.capability }
func (p *pluginTool) Execute(ctx context.Context, input map[string]any) (map[string]any, error) {
	conn, err := p.plugin.resolveConnection(input, p.def.name)
	if err != nil {
		return nil, err
	}
	return p.def.run(ctx, conn, input)
}

// resolveConnection performs the connection_id presence + binding +
// enabled checks shared across every tool, matching the calendar
// plugin's pattern. Returns the live Connection on success.
func (p *Plugin) resolveConnection(input map[string]any, toolName string) (*domain.Connection, error) {
	connectionID, _ := input["connection_id"].(string)
	if connectionID == "" {
		return nil, fmt.Errorf("%s: connection_id is required", toolName)
	}
	assistantID, _ := input["__assistant_id"].(string)
	if assistantID != "" && p.bindings != nil {
		ok, err := p.bindings.HasBinding(assistantID, connectionID, domain.BindingRoleTool)
		if err != nil {
			return nil, fmt.Errorf("%s: binding check failed: %w", toolName, err)
		}
		if !ok {
			return nil, plugins.ConnectionNotBoundError(assistantID, connectionID, PluginID)
		}
	}
	conn, err := p.connections.GetByID(connectionID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", toolName, err)
	}
	if !conn.Enabled {
		return nil, fmt.Errorf("%s: connection %s is disabled", toolName, connectionID)
	}
	return conn, nil
}

// clientFor builds an authenticated GitHub HTTP client for a
// Connection. Pulls the cached AuthClient via authClientFor, then
// binds the installation_id from config. Test-mode path:
// authOverride may already wrap a fully-stubbed client.
func (p *Plugin) clientFor(conn *domain.Connection) (*gh.Client, error) {
	auth, err := p.authClientFor(conn)
	if err != nil {
		return nil, err
	}
	installationID, err := configInt(conn.Config, configInstallationID)
	if err != nil {
		return nil, err
	}
	return gh.NewClient(auth, installationID), nil
}

// --- issue tool implementations ---

func (p *Plugin) issuesList(ctx context.Context, conn *domain.Connection, input map[string]any) (map[string]any, error) {
	owner, _ := input["owner"].(string)
	repo, _ := input["repo"].(string)
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("github.issues.list: owner and repo required")
	}
	if err := p.assertRepoAllowed(conn, owner, repo); err != nil {
		return nil, err
	}
	q := url.Values{}
	if s, _ := input["state"].(string); s != "" {
		q.Set("state", s)
	}
	if l, _ := input["labels"].(string); l != "" {
		q.Set("labels", l)
	}
	if a, _ := input["assignee"].(string); a != "" {
		q.Set("assignee", a)
	}
	if pp := intFromInput(input, "per_page", 0); pp > 0 {
		q.Set("per_page", fmt.Sprintf("%d", pp))
	}
	if pg := intFromInput(input, "page", 0); pg > 0 {
		q.Set("page", fmt.Sprintf("%d", pg))
	}

	cli, err := p.clientFor(conn)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/repos/%s/%s/issues", owner, repo)
	if s := q.Encode(); s != "" {
		path = path + "?" + s
	}
	var raw []map[string]any
	if err := cli.Do(ctx, "GET", path, nil, &raw); err != nil {
		return nil, err
	}
	// Trim the GitHub response shape down to the fields agents care
	// about. The full upstream blob is ~30 fields per issue; agents
	// rarely need most of them and large blobs eat LLM context.
	trimmed := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		// PRs are also returned by /issues; filter them so .list is
		// truly issue-only. github.pulls.list covers PRs.
		if _, isPR := r["pull_request"]; isPR {
			continue
		}
		trimmed = append(trimmed, trimIssueShape(r))
	}
	return map[string]any{"issues": trimmed, "count": len(trimmed)}, nil
}

func (p *Plugin) issuesGet(ctx context.Context, conn *domain.Connection, input map[string]any) (map[string]any, error) {
	owner, _ := input["owner"].(string)
	repo, _ := input["repo"].(string)
	num := intFromInput(input, "issue_number", 0)
	if owner == "" || repo == "" || num <= 0 {
		return nil, fmt.Errorf("github.issues.get: owner, repo, issue_number required")
	}
	if err := p.assertRepoAllowed(conn, owner, repo); err != nil {
		return nil, err
	}
	cli, err := p.clientFor(conn)
	if err != nil {
		return nil, err
	}
	var issue map[string]any
	if err := cli.Do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/issues/%d", owner, repo, num), nil, &issue); err != nil {
		return nil, err
	}
	// Fetch comments in the same call so the agent doesn't make a
	// follow-up round trip.
	var comments []map[string]any
	if err := cli.Do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=100", owner, repo, num), nil, &comments); err != nil {
		return nil, err
	}
	return map[string]any{
		"issue":    trimIssueShape(issue),
		"comments": trimCommentShapes(comments),
	}, nil
}

func (p *Plugin) issuesCreate(ctx context.Context, conn *domain.Connection, input map[string]any) (map[string]any, error) {
	owner, _ := input["owner"].(string)
	repo, _ := input["repo"].(string)
	title, _ := input["title"].(string)
	if owner == "" || repo == "" || title == "" {
		return nil, fmt.Errorf("github.issues.create: owner, repo, title required")
	}
	if err := p.assertRepoAllowed(conn, owner, repo); err != nil {
		return nil, err
	}
	body := map[string]any{"title": title}
	if b, _ := input["body"].(string); b != "" {
		body["body"] = b
	}
	if labels := stringSlice(input, "labels"); len(labels) > 0 {
		body["labels"] = labels
	}
	if assignees := stringSlice(input, "assignees"); len(assignees) > 0 {
		body["assignees"] = assignees
	}
	cli, err := p.clientFor(conn)
	if err != nil {
		return nil, err
	}
	var issue map[string]any
	if err := cli.Do(ctx, "POST", fmt.Sprintf("/repos/%s/%s/issues", owner, repo), body, &issue); err != nil {
		return nil, err
	}
	return map[string]any{"issue": trimIssueShape(issue)}, nil
}

func (p *Plugin) issuesComment(ctx context.Context, conn *domain.Connection, input map[string]any) (map[string]any, error) {
	owner, _ := input["owner"].(string)
	repo, _ := input["repo"].(string)
	num := intFromInput(input, "issue_number", 0)
	body, _ := input["body"].(string)
	if owner == "" || repo == "" || num <= 0 || body == "" {
		return nil, fmt.Errorf("github.issues.comment: owner, repo, issue_number, body required")
	}
	if err := p.assertRepoAllowed(conn, owner, repo); err != nil {
		return nil, err
	}
	cli, err := p.clientFor(conn)
	if err != nil {
		return nil, err
	}
	var comment map[string]any
	if err := cli.Do(ctx, "POST", fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, num), map[string]any{"body": body}, &comment); err != nil {
		return nil, err
	}
	return map[string]any{"comment": trimCommentShape(comment)}, nil
}

// assertRepoAllowed enforces the optional repo allowlist on the
// Connection. An empty allowlist means the installation's
// server-side scope is the only gate; a non-empty allowlist further
// narrows what this Connection is permitted to touch (useful when
// one App is installed broadly but a Nomi user only wants a subset
// available to a particular assistant).
func (p *Plugin) assertRepoAllowed(conn *domain.Connection, owner, repo string) error {
	allowlist := stringSliceFromConfig(conn.Config, configRepoAllowlist)
	if len(allowlist) == 0 {
		return nil
	}
	full := strings.ToLower(owner + "/" + repo)
	for _, entry := range allowlist {
		if strings.EqualFold(strings.TrimSpace(entry), full) {
			return nil
		}
	}
	return fmt.Errorf("connection %s does not allow access to %s/%s; current allowlist: %v",
		conn.ID, owner, repo, allowlist)
}

// --- helpers ---

func intFromInput(input map[string]any, key string, def int) int {
	switch v := input[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case string:
		var n int
		_, err := fmt.Sscanf(v, "%d", &n)
		if err != nil {
			return def
		}
		return n
	}
	return def
}

func stringSlice(input map[string]any, key string) []string {
	switch v := input[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if v == "" {
			return nil
		}
		// CSV fallback for UI inputs that arrive as comma-separated
		// strings.
		parts := strings.Split(v, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if t := strings.TrimSpace(p); t != "" {
				out = append(out, t)
			}
		}
		return out
	}
	return nil
}

// stringSliceFromConfig handles the same shapes for plugin_connections.config.
func stringSliceFromConfig(config map[string]any, key string) []string {
	return stringSlice(config, key)
}

// trimIssueShape projects the fields agents actually need out of the
// raw GitHub issue blob. Keeps token usage on the LLM side
// predictable and avoids leaking irrelevant fields.
func trimIssueShape(raw map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"number", "state", "title", "body", "html_url", "comments", "created_at", "updated_at", "closed_at"} {
		if v, ok := raw[k]; ok {
			out[k] = v
		}
	}
	if user, ok := raw["user"].(map[string]any); ok {
		if login, ok := user["login"].(string); ok {
			out["author"] = login
		}
	}
	if labelsAny, ok := raw["labels"].([]any); ok {
		labels := make([]string, 0, len(labelsAny))
		for _, l := range labelsAny {
			if lm, ok := l.(map[string]any); ok {
				if name, ok := lm["name"].(string); ok {
					labels = append(labels, name)
				}
			}
		}
		out["labels"] = labels
	}
	if assigneesAny, ok := raw["assignees"].([]any); ok {
		assignees := make([]string, 0, len(assigneesAny))
		for _, a := range assigneesAny {
			if am, ok := a.(map[string]any); ok {
				if login, ok := am["login"].(string); ok {
					assignees = append(assignees, login)
				}
			}
		}
		out["assignees"] = assignees
	}
	return out
}

func trimCommentShape(raw map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"id", "body", "html_url", "created_at", "updated_at"} {
		if v, ok := raw[k]; ok {
			out[k] = v
		}
	}
	if user, ok := raw["user"].(map[string]any); ok {
		if login, ok := user["login"].(string); ok {
			out["author"] = login
		}
	}
	return out
}

func trimCommentShapes(raw []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		out = append(out, trimCommentShape(r))
	}
	return out
}
