package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/tools"
)

// repoTools registers the github.repos.* family.
//
// file_read covers ~80% of code-reading agent flows without paying
// the clone overhead. clone is the escape hatch for cross-file
// reasoning + writes that need a real working tree.
func (p *Plugin) repoTools() []toolDef {
	return []toolDef{
		{
			name:       "github.repos.file_read",
			capability: "github.read",
			desc:       "Read a single file at a given ref (default: main). Inputs: connection_id, owner, repo, path, ref?. Caps at 1MiB; rejects directories.",
			run:        p.reposFileRead,
		},
		{
			name:       "github.repos.clone",
			capability: "filesystem.write",
			desc:       "Shallow-clone a repo into the assistant's workspace. Inputs: connection_id, owner, repo, path? (workspace-relative). Permission-gated as filesystem.write; clone target must resolve inside workspace_root.",
			run:        p.reposClone,
		},
		{
			name:       "github.repos.search_code",
			capability: "github.read",
			desc:       "Search code across repositories. Inputs: connection_id, query (required, plain GitHub search syntax — see docs), owner?+repo? (narrow to a single repo), per_page? (default 30, max 100), page?. When neither owner+repo nor an explicit `repo:` qualifier is in the query, the connection's repo allowlist is auto-applied to keep results scoped.",
			run:        p.reposSearchCode,
		},
	}
}

// reposSearchCode wraps the GitHub /search/code endpoint. Two safety
// rails on top of the raw API:
//
//  1. If the caller passes owner+repo, we validate against the
//     allowlist and pin the search to that single repo via a
//     `repo:owner/name` qualifier. This is the common agent path
//     ("search this one repo I just told you about").
//
//  2. Otherwise, if the connection has a non-empty allowlist and the
//     query doesn't already include a `repo:` qualifier, we OR every
//     allowlisted repo into the query. Without this, a broad query
//     like `func main` would search the entire public internet
//     visible to the App installation — surprising, slow, and
//     irrelevant to the assistant's actual context.
//
// We don't trim the result shape down as aggressively as
// trimIssueShape because callers usually want the path + repo + URL
// to follow up with file_read; preserving `score` lets agents rank.
func (p *Plugin) reposSearchCode(ctx context.Context, conn *domain.Connection, input map[string]any) (map[string]any, error) {
	rawQuery, _ := input["query"].(string)
	rawQuery = strings.TrimSpace(rawQuery)
	if rawQuery == "" {
		return nil, fmt.Errorf("github.repos.search_code: query required")
	}
	owner, _ := input["owner"].(string)
	repo, _ := input["repo"].(string)

	q := rawQuery
	switch {
	case owner != "" && repo != "":
		if err := p.assertRepoAllowed(conn, owner, repo); err != nil {
			return nil, err
		}
		q = fmt.Sprintf("%s repo:%s/%s", rawQuery, owner, repo)
	case strings.Contains(strings.ToLower(rawQuery), "repo:"):
		// Caller already pinned the search; trust their qualifier.
	default:
		allow := stringSliceFromConfig(conn.Config, configRepoAllowlist)
		if len(allow) > 0 {
			parts := make([]string, 0, len(allow))
			for _, entry := range allow {
				if e := strings.TrimSpace(entry); e != "" {
					parts = append(parts, "repo:"+e)
				}
			}
			if len(parts) > 0 {
				q = rawQuery + " " + strings.Join(parts, " ")
			}
		}
	}

	values := url.Values{}
	values.Set("q", q)
	if pp := intFromInput(input, "per_page", 0); pp > 0 {
		if pp > 100 {
			pp = 100
		}
		values.Set("per_page", fmt.Sprintf("%d", pp))
	}
	if pg := intFromInput(input, "page", 0); pg > 0 {
		values.Set("page", fmt.Sprintf("%d", pg))
	}

	cli, err := p.clientFor(conn)
	if err != nil {
		return nil, err
	}

	var raw struct {
		TotalCount        int              `json:"total_count"`
		IncompleteResults bool             `json:"incomplete_results"`
		Items             []map[string]any `json:"items"`
	}
	if err := cli.Do(ctx, "GET", "/search/code?"+values.Encode(), nil, &raw); err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(raw.Items))
	for _, it := range raw.Items {
		items = append(items, trimCodeSearchHit(it))
	}
	return map[string]any{
		"items":              items,
		"total_count":        raw.TotalCount,
		"incomplete_results": raw.IncompleteResults,
		"effective_query":    q,
	}, nil
}

func trimCodeSearchHit(raw map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"name", "path", "sha", "html_url", "score"} {
		if v, ok := raw[k]; ok {
			out[k] = v
		}
	}
	if r, ok := raw["repository"].(map[string]any); ok {
		if full, ok := r["full_name"].(string); ok {
			out["repository"] = full
		}
	}
	return out
}

// reposFileRead pulls a single file via the contents API. We
// deliberately don't use the raw.githubusercontent.com host even
// though it's also in the allowlist — the contents API gives us
// metadata (sha, size, encoding) the LLM benefits from when
// reasoning about whether to read more of the file.
func (p *Plugin) reposFileRead(ctx context.Context, conn *domain.Connection, input map[string]any) (map[string]any, error) {
	owner, _ := input["owner"].(string)
	repo, _ := input["repo"].(string)
	path, _ := input["path"].(string)
	ref, _ := input["ref"].(string)
	if owner == "" || repo == "" || path == "" {
		return nil, fmt.Errorf("github.repos.file_read: owner, repo, path required")
	}
	if err := p.assertRepoAllowed(conn, owner, repo); err != nil {
		return nil, err
	}
	cli, err := p.clientFor(conn)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if ref != "" {
		q.Set("ref", ref)
	}
	apiPath := fmt.Sprintf("/repos/%s/%s/contents/%s", owner, repo, url.PathEscape(path))
	if s := q.Encode(); s != "" {
		apiPath = apiPath + "?" + s
	}
	var raw map[string]any
	if err := cli.Do(ctx, "GET", apiPath, nil, &raw); err != nil {
		return nil, err
	}
	// The contents API returns either a single file object or, for
	// directories, an array. Tools should be unambiguous: refuse
	// directories and tell the agent to pick a specific file.
	if t, _ := raw["type"].(string); t != "file" {
		return nil, fmt.Errorf("github.repos.file_read: %s/%s:%s is not a file (got type=%v)", owner, repo, path, raw["type"])
	}
	const maxBytes = 1 << 20 // 1 MiB
	if size, ok := raw["size"].(float64); ok && int64(size) > int64(maxBytes) {
		return nil, fmt.Errorf("github.repos.file_read: %s exceeds 1MiB (%d bytes); use github.repos.clone for large files", path, int64(size))
	}
	encoded, _ := raw["content"].(string)
	encoding, _ := raw["encoding"].(string)
	var contentBytes []byte
	switch encoding {
	case "base64", "":
		// The API folds long base64 across newlines; strip them.
		clean := strings.ReplaceAll(encoded, "\n", "")
		decoded, err := base64.StdEncoding.DecodeString(clean)
		if err != nil {
			return nil, fmt.Errorf("github.repos.file_read: decode base64: %w", err)
		}
		contentBytes = decoded
	default:
		return nil, fmt.Errorf("github.repos.file_read: unsupported content encoding %q", encoding)
	}
	out := map[string]any{
		"path":     raw["path"],
		"sha":      raw["sha"],
		"size":     raw["size"],
		"html_url": raw["html_url"],
		"content":  string(contentBytes),
	}
	return out, nil
}

// reposClone shells out to `git clone --depth 1` into the
// assistant's workspace. The workspace_root sandbox mirrors the
// existing filesystem.read/write tools — clone target must resolve
// inside it, and we reject any attempt to clone outside.
//
// The installation token is passed via the URL credential helper
// rather than via SSH/keys; GitHub accepts `https://x-access-token:<token>@github.com/...`
// for the duration of the token's life.
func (p *Plugin) reposClone(ctx context.Context, conn *domain.Connection, input map[string]any) (map[string]any, error) {
	owner, _ := input["owner"].(string)
	repo, _ := input["repo"].(string)
	rel, _ := input["path"].(string)
	if rel == "" {
		rel = repo // default: clone into ./<repo>/ relative to workspace
	}
	root, _ := input["workspace_root"].(string)
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("github.repos.clone: owner and repo required")
	}
	if root == "" {
		return nil, fmt.Errorf("github.repos.clone: workspace_root not set; clone target unsandboxed")
	}
	if err := p.assertRepoAllowed(conn, owner, repo); err != nil {
		return nil, err
	}
	target, err := tools.ResolveWithinRoot(root, rel)
	if err != nil {
		return nil, fmt.Errorf("github.repos.clone: %w", err)
	}
	// Refuse to overwrite an existing path. Less surprising than the
	// alternative for an agent making repeated calls.
	if _, statErr := os.Stat(target); statErr == nil {
		return nil, fmt.Errorf("github.repos.clone: target %q already exists; remove it first", target)
	}
	auth, err := p.authClientFor(conn)
	if err != nil {
		return nil, err
	}
	installationID, err := configInt(conn.Config, configInstallationID)
	if err != nil {
		return nil, err
	}
	tok, err := auth.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	cloneURL := fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git", tok.Token, owner, repo)

	// `git clone` doesn't strictly need a parent dir, but creating
	// it explicitly keeps the error surface deterministic.
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, fmt.Errorf("github.repos.clone: prepare parent: %w", err)
	}

	// --depth 1 is the right default for agent-flavored "show me
	// this repo" use cases. Agents that want history can pass
	// depth=0 explicitly later.
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--quiet", cloneURL, target)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Scrub the token from any error path before returning to the
		// agent. cloneURL embedded the token; git's own error
		// messages can echo it back.
		safe := strings.ReplaceAll(string(out), tok.Token, "<redacted>")
		return nil, fmt.Errorf("github.repos.clone: git failed: %s: %w", safe, err)
	}
	return map[string]any{
		"path":       target,
		"size_bytes": dirSize(target),
	}, nil
}

// dirSize sums file sizes under root. Used to give the agent a
// sense-check for the cloned repo without parsing porcelain.
func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}
