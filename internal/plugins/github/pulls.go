package github

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"go.klarlabs.de/nomi/internal/domain"
)

// pullTools registers the github.pulls.* family.
func (p *Plugin) pullTools() []toolDef {
	return []toolDef{
		{
			name:       "github.pulls.list",
			capability: "github.read",
			desc:       "List pull requests on a repo. Inputs: connection_id, owner, repo, state? (open|closed|all), per_page?, page?",
			run:        p.pullsList,
		},
		{
			name:       "github.pulls.get",
			capability: "github.read",
			desc:       "Get a single PR with unified diff, linked issues, and checkrun status. Inputs: connection_id, owner, repo, pull_number",
			run:        p.pullsGet,
		},
		{
			name:       "github.pulls.comment",
			capability: "github.write",
			desc:       "Add a top-level comment to a PR (issue-style, not inline review). Inputs: connection_id, owner, repo, pull_number, body",
			run:        p.pullsComment,
		},
		{
			name:       "github.pulls.review",
			capability: "github.write",
			desc:       "Submit a PR review. Inputs: connection_id, owner, repo, pull_number, event (APPROVE|REQUEST_CHANGES|COMMENT), body. Empty body with COMMENT is rejected.",
			run:        p.pullsReview,
		},
		{
			name:       "github.pulls.create",
			capability: "github.write",
			desc:       "Open a new pull request. Inputs: connection_id, owner, repo, title, head (branch ref or owner:branch for fork), base (target branch), body?, draft? (default false). Returns the created PR.",
			run:        p.pullsCreate,
		},
	}
}

func (p *Plugin) pullsList(ctx context.Context, conn *domain.Connection, input map[string]any) (map[string]any, error) {
	owner, _ := input["owner"].(string)
	repo, _ := input["repo"].(string)
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("github.pulls.list: owner and repo required")
	}
	if err := p.assertRepoAllowed(conn, owner, repo); err != nil {
		return nil, err
	}
	q := url.Values{}
	if s, _ := input["state"].(string); s != "" {
		q.Set("state", s)
	}
	if pp := intFromInput(input, "per_page"); pp > 0 {
		q.Set("per_page", fmt.Sprintf("%d", pp))
	}
	if pg := intFromInput(input, "page"); pg > 0 {
		q.Set("page", fmt.Sprintf("%d", pg))
	}
	cli, err := p.clientFor(conn)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls", owner, repo)
	if s := q.Encode(); s != "" {
		path = path + "?" + s
	}
	var raw []map[string]any
	if err := cli.Do(ctx, "GET", path, nil, &raw); err != nil {
		return nil, err
	}
	trimmed := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		trimmed = append(trimmed, trimPullShape(r))
	}
	return map[string]any{"pulls": trimmed, "count": len(trimmed)}, nil
}

func (p *Plugin) pullsGet(ctx context.Context, conn *domain.Connection, input map[string]any) (map[string]any, error) {
	owner, _ := input["owner"].(string)
	repo, _ := input["repo"].(string)
	num := intFromInput(input, "pull_number")
	if owner == "" || repo == "" || num <= 0 {
		return nil, fmt.Errorf("github.pulls.get: owner, repo, pull_number required")
	}
	if err := p.assertRepoAllowed(conn, owner, repo); err != nil {
		return nil, err
	}
	cli, err := p.clientFor(conn)
	if err != nil {
		return nil, err
	}
	// One round-trip per concern: PR metadata, unified diff, linked
	// checkruns. Issue refs are extracted from the PR body — GitHub
	// resolves "Fixes #123" into a linked issue server-side, but the
	// API doesn't return that mapping; substring detection is good
	// enough for an agent-facing summary.
	var pr map[string]any
	if err := cli.Do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, num), nil, &pr); err != nil {
		return nil, err
	}
	// The "head" SHA is what checkruns are anchored to.
	headSHA, _ := nestedString(pr, "head", "sha")
	checkRuns := []map[string]any{}
	var checkRunsError string
	if headSHA != "" {
		var checks struct {
			TotalCount int              `json:"total_count"`
			CheckRuns  []map[string]any `json:"check_runs"`
		}
		if err := cli.Do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs", owner, repo, headSHA), nil, &checks); err != nil {
			// A check-runs failure shouldn't fail the whole tool —
			// agents can still review without check status — so we
			// surface a warning field instead of erroring out.
			checkRunsError = err.Error()
		} else {
			for _, cr := range checks.CheckRuns {
				checkRuns = append(checkRuns, trimCheckRunShape(cr))
			}
		}
	}

	body, _ := pr["body"].(string)
	pull := trimPullShape(pr)
	if checkRunsError != "" {
		pull["check_runs_error"] = checkRunsError
	}
	out := map[string]any{
		"pull":          pull,
		"diff":          pr["diff_url"], // download lazily; agents that need the raw diff fetch it
		"check_runs":    checkRuns,
		"linked_issues": extractIssueRefs(body),
	}
	return out, nil
}

func (p *Plugin) pullsComment(ctx context.Context, conn *domain.Connection, input map[string]any) (map[string]any, error) {
	owner, _ := input["owner"].(string)
	repo, _ := input["repo"].(string)
	num := intFromInput(input, "pull_number")
	body, _ := input["body"].(string)
	if owner == "" || repo == "" || num <= 0 || body == "" {
		return nil, fmt.Errorf("github.pulls.comment: owner, repo, pull_number, body required")
	}
	if err := p.assertRepoAllowed(conn, owner, repo); err != nil {
		return nil, err
	}
	cli, err := p.clientFor(conn)
	if err != nil {
		return nil, err
	}
	// Top-level PR comments are issue-style: the same /issues/:num/comments
	// endpoint backs them. Inline review comments are a separate
	// review-creation flow exposed through .review.
	var comment map[string]any
	if err := cli.Do(ctx, "POST", fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, num), map[string]any{"body": body}, &comment); err != nil {
		return nil, err
	}
	return map[string]any{"comment": trimCommentShape(comment)}, nil
}

func (p *Plugin) pullsReview(ctx context.Context, conn *domain.Connection, input map[string]any) (map[string]any, error) {
	owner, _ := input["owner"].(string)
	repo, _ := input["repo"].(string)
	num := intFromInput(input, "pull_number")
	event, _ := input["event"].(string)
	body, _ := input["body"].(string)
	if owner == "" || repo == "" || num <= 0 || event == "" {
		return nil, fmt.Errorf("github.pulls.review: owner, repo, pull_number, event required")
	}
	switch strings.ToUpper(event) {
	case "APPROVE", "REQUEST_CHANGES", "COMMENT":
	default:
		return nil, fmt.Errorf("github.pulls.review: event must be APPROVE | REQUEST_CHANGES | COMMENT, got %q", event)
	}
	// COMMENT review with no body is meaningless and historically
	// the source of accidental "approve" submissions when an agent
	// produced an empty string.
	if strings.ToUpper(event) == "COMMENT" && strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("github.pulls.review: COMMENT review requires a non-empty body")
	}
	if err := p.assertRepoAllowed(conn, owner, repo); err != nil {
		return nil, err
	}
	cli, err := p.clientFor(conn)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{"event": strings.ToUpper(event)}
	if body != "" {
		payload["body"] = body
	}
	var review map[string]any
	if err := cli.Do(ctx, "POST", fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", owner, repo, num), payload, &review); err != nil {
		return nil, err
	}
	return map[string]any{"review": trimReviewShape(review)}, nil
}

func (p *Plugin) pullsCreate(ctx context.Context, conn *domain.Connection, input map[string]any) (map[string]any, error) {
	owner, _ := input["owner"].(string)
	repo, _ := input["repo"].(string)
	title, _ := input["title"].(string)
	head, _ := input["head"].(string)
	base, _ := input["base"].(string)
	if owner == "" || repo == "" || title == "" || head == "" || base == "" {
		return nil, fmt.Errorf("github.pulls.create: owner, repo, title, head, base required")
	}
	if err := p.assertRepoAllowed(conn, owner, repo); err != nil {
		return nil, err
	}
	payload := map[string]any{
		"title": title,
		"head":  head,
		"base":  base,
	}
	if body, _ := input["body"].(string); body != "" {
		payload["body"] = body
	}
	if draft, ok := input["draft"].(bool); ok && draft {
		payload["draft"] = true
	}
	cli, err := p.clientFor(conn)
	if err != nil {
		return nil, err
	}
	var pr map[string]any
	if err := cli.Do(ctx, "POST", fmt.Sprintf("/repos/%s/%s/pulls", owner, repo), payload, &pr); err != nil {
		return nil, err
	}
	return map[string]any{"pull": trimPullShape(pr)}, nil
}

// --- shape helpers ---

func trimPullShape(raw map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"number", "state", "title", "body", "html_url", "draft", "merged", "mergeable", "merged_at", "created_at", "updated_at", "diff_url"} {
		if v, ok := raw[k]; ok {
			out[k] = v
		}
	}
	if user, ok := raw["user"].(map[string]any); ok {
		if login, ok := user["login"].(string); ok {
			out["author"] = login
		}
	}
	if head, ok := raw["head"].(map[string]any); ok {
		if sha, ok := head["sha"].(string); ok {
			out["head_sha"] = sha
		}
		if ref, ok := head["ref"].(string); ok {
			out["head_ref"] = ref
		}
	}
	if base, ok := raw["base"].(map[string]any); ok {
		if ref, ok := base["ref"].(string); ok {
			out["base_ref"] = ref
		}
	}
	return out
}

func trimCheckRunShape(raw map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"name", "status", "conclusion", "details_url", "started_at", "completed_at"} {
		if v, ok := raw[k]; ok {
			out[k] = v
		}
	}
	return out
}

func trimReviewShape(raw map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"id", "state", "body", "html_url", "submitted_at"} {
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

// nestedString descends through a JSON-shaped map looking up successive
// keys; returns ("", false) if any step yields a non-map / missing key.
func nestedString(m map[string]any, keys ...string) (string, bool) {
	cur := m
	for i, k := range keys {
		if i == len(keys)-1 {
			s, ok := cur[k].(string)
			return s, ok
		}
		next, ok := cur[k].(map[string]any)
		if !ok {
			return "", false
		}
		cur = next
	}
	return "", false
}

// extractIssueRefs scans a PR body for GitHub's auto-linking syntax —
// "Fixes #123", "Closes acme/widgets#45", etc. — and returns the
// matched refs. Loose detection: false positives in agent-summary
// land are tolerable; the agent can ignore irrelevant numbers.
func extractIssueRefs(body string) []string {
	if body == "" {
		return nil
	}
	keywords := []string{"close", "closes", "closed", "fix", "fixes", "fixed", "resolve", "resolves", "resolved"}
	lower := strings.ToLower(body)
	var refs []string
	seen := map[string]bool{}
	for _, kw := range keywords {
		idx := 0
		for {
			i := strings.Index(lower[idx:], kw+" ")
			if i < 0 {
				break
			}
			start := idx + i + len(kw) + 1
			// Find the next "#NNN" after the keyword (within ~32 chars
			// to avoid sweeping the whole body).
			var window string
			if start < len(body) {
				window = body[start:]
			}
			if h := strings.Index(window, "#"); h >= 0 && h < 32 {
				j := h + 1
				num := ""
				for j < len(window) && window[j] >= '0' && window[j] <= '9' {
					num += string(window[j])
					j++
				}
				if num != "" {
					ref := "#" + num
					if !seen[ref] {
						refs = append(refs, ref)
						seen[ref] = true
					}
				}
			}
			idx = idx + i + len(kw) + 1
			if idx >= len(lower) {
				break
			}
		}
	}
	return refs
}
