package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReposFileRead_Base64(t *testing.T) {
	srv := newStubServer(t)
	content := "package main\n\nfunc main() {}\n"
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	srv.stub("GET /repos/o/r/contents/main.go",
		fmt.Sprintf(`{"type":"file","path":"main.go","sha":"abc","size":%d,"encoding":"base64","content":%q}`, len(content), encoded))
	p, conn := stubPlugin(t, srv)
	out, err := p.reposFileRead(context.Background(), conn, map[string]any{
		"connection_id": "c1", "owner": "o", "repo": "r", "path": "main.go",
	})
	if err != nil {
		t.Fatalf("reposFileRead: %v", err)
	}
	if got, _ := out["content"].(string); got != content {
		t.Fatalf("content roundtrip mismatch:\n got: %q\nwant: %q", got, content)
	}
}

func TestReposFileRead_RejectsDirectory(t *testing.T) {
	srv := newStubServer(t)
	// API returns array for directories; we'd reject before unmarshal,
	// but our Do unmarshals into map[string]any — we surface the
	// "not a file" check on the type field of a single-file response.
	// Test the case where the API does return an object but with
	// type=dir (which it does for some responses where the path is a
	// directory; the canonical shape is an array but defensive code
	// handles either).
	srv.stub("GET /repos/o/r/contents/somedir",
		`{"type":"dir","path":"somedir"}`)
	p, conn := stubPlugin(t, srv)
	_, err := p.reposFileRead(context.Background(), conn, map[string]any{
		"connection_id": "c1", "owner": "o", "repo": "r", "path": "somedir",
	})
	if err == nil || !strings.Contains(err.Error(), "not a file") {
		t.Fatalf("expected directory rejection, got %v", err)
	}
}

func TestReposFileRead_RejectsLargeFile(t *testing.T) {
	srv := newStubServer(t)
	srv.stub("GET /repos/o/r/contents/big.bin",
		`{"type":"file","path":"big.bin","sha":"x","size":2000000,"encoding":"base64","content":""}`)
	p, conn := stubPlugin(t, srv)
	_, err := p.reposFileRead(context.Background(), conn, map[string]any{
		"connection_id": "c1", "owner": "o", "repo": "r", "path": "big.bin",
	})
	if err == nil || !strings.Contains(err.Error(), "1MiB") {
		t.Fatalf("expected size rejection, got %v", err)
	}
}

func TestReposClone_RejectsMissingWorkspaceRoot(t *testing.T) {
	srv := newStubServer(t)
	p, conn := stubPlugin(t, srv)
	_, err := p.reposClone(context.Background(), conn, map[string]any{
		"connection_id": "c1", "owner": "o", "repo": "r",
		// no workspace_root
	})
	if err == nil || !strings.Contains(err.Error(), "workspace_root") {
		t.Fatalf("expected workspace_root error, got %v", err)
	}
}

func TestReposClone_RejectsExistingTarget(t *testing.T) {
	srv := newStubServer(t)
	root := t.TempDir()
	target := filepath.Join(root, "r")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	p, conn := stubPlugin(t, srv)
	_, err := p.reposClone(context.Background(), conn, map[string]any{
		"connection_id": "c1", "owner": "o", "repo": "r", "workspace_root": root,
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected refusal to overwrite, got %v", err)
	}
}

func TestReposClone_RejectsPathOutsideRoot(t *testing.T) {
	srv := newStubServer(t)
	root := t.TempDir()
	p, conn := stubPlugin(t, srv)
	_, err := p.reposClone(context.Background(), conn, map[string]any{
		"connection_id": "c1", "owner": "o", "repo": "r",
		"workspace_root": root, "path": "../escape",
	})
	if err == nil {
		t.Fatal("expected sandbox rejection, got nil")
	}
}

func TestReposSearchCode_PinsToOwnerRepoWhenSpecified(t *testing.T) {
	srv := newStubServer(t)
	srv.stub("GET /search/code?q=func+main+repo%3Ao%2Fr",
		`{"total_count":1,"incomplete_results":false,"items":[{"name":"main.go","path":"cmd/x/main.go","sha":"a","html_url":"u","score":1.0,"repository":{"full_name":"o/r"}}]}`)
	p, conn := stubPlugin(t, srv)

	out, err := p.reposSearchCode(context.Background(), conn, map[string]any{
		"connection_id": "c1", "owner": "o", "repo": "r",
		"query": "func main",
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if got, _ := out["total_count"].(int); got != 1 {
		t.Fatalf("total_count: %v", out["total_count"])
	}
	if eff, _ := out["effective_query"].(string); eff != "func main repo:o/r" {
		t.Fatalf("effective_query: %q", eff)
	}
	items, _ := out["items"].([]map[string]any)
	if len(items) != 1 || items[0]["repository"] != "o/r" {
		t.Fatalf("items shape unexpected: %+v", items)
	}
}

func TestReposSearchCode_AppliesAllowlistWhenUnscoped(t *testing.T) {
	srv := newStubServer(t)
	srv.stub("GET /search/code?q=func+main+repo%3Ao%2Fr+repo%3Ao%2Fother",
		`{"total_count":0,"incomplete_results":false,"items":[]}`)
	p, conn := stubPlugin(t, srv)
	conn.Config[configRepoAllowlist] = "o/r,o/other"

	out, err := p.reposSearchCode(context.Background(), conn, map[string]any{
		"connection_id": "c1", "query": "func main",
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if eff, _ := out["effective_query"].(string); eff != "func main repo:o/r repo:o/other" {
		t.Fatalf("effective_query: %q", eff)
	}
}

func TestReposSearchCode_RespectsExplicitRepoQualifier(t *testing.T) {
	srv := newStubServer(t)
	srv.stub("GET /search/code?q=foo+repo%3Auser%2Fexisting",
		`{"total_count":0,"incomplete_results":false,"items":[]}`)
	p, conn := stubPlugin(t, srv)
	conn.Config[configRepoAllowlist] = "o/allowlisted"

	out, err := p.reposSearchCode(context.Background(), conn, map[string]any{
		"connection_id": "c1", "query": "foo repo:user/existing",
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if eff, _ := out["effective_query"].(string); eff != "foo repo:user/existing" {
		t.Fatalf("explicit qualifier should not be amended; got %q", eff)
	}
}

func TestReposSearchCode_RequiresQuery(t *testing.T) {
	p, conn := stubPlugin(t, newStubServer(t))
	if _, err := p.reposSearchCode(context.Background(), conn, map[string]any{
		"connection_id": "c1", "query": "  ",
	}); err == nil {
		t.Fatal("expected error for empty query")
	}
}

func TestReposSearchCode_RejectsOwnerRepoOutsideAllowlist(t *testing.T) {
	srv := newStubServer(t)
	p, conn := stubPlugin(t, srv)
	conn.Config[configRepoAllowlist] = "o/permitted"

	if _, err := p.reposSearchCode(context.Background(), conn, map[string]any{
		"connection_id": "c1", "owner": "o", "repo": "blocked", "query": "x",
	}); err == nil {
		t.Fatal("expected allowlist refusal")
	}
}
