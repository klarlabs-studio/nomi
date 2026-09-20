package mcpbridge

import (
	"strings"
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
)

func TestParseConfigEnv_MapAndString(t *testing.T) {
	fromMap := parseConfigEnv(map[string]any{"FOO": "bar", "empty": ""})
	if fromMap["FOO"] != "bar" {
		t.Fatalf("map: %+v", fromMap)
	}
	if _, ok := fromMap["empty"]; ok {
		t.Fatal("empty values should be skipped")
	}

	fromStr := parseConfigEnv("A=1\n# comment\nB = two\n")
	if fromStr["A"] != "1" || fromStr["B"] != "two" {
		t.Fatalf("string: %+v", fromStr)
	}
}

func TestExpandArgsSubstitutions(t *testing.T) {
	env := map[string]string{"DATABASE_URL": "postgres://local/db"}
	got := expandArgsSubstitutions("-y @modelcontextprotocol/server-postgres ${DATABASE_URL}", env)
	if !strings.Contains(got, "postgres://local/db") {
		t.Fatalf("expand failed: %s", got)
	}
	if strings.Contains(got, "${DATABASE_URL}") {
		t.Fatalf("placeholder left: %s", got)
	}
	unresolved := expandArgsSubstitutions("x ${MISSING}", env)
	if unresolved != "x ${MISSING}" {
		t.Fatalf("unresolved should remain: %s", unresolved)
	}
}

func TestResolveSpawnEnv_LiteralsWithoutSecretsStore(t *testing.T) {
	p := &Plugin{}
	conn := &domain.Connection{
		Config: map[string]any{
			"env": map[string]any{"LITERAL": "yes"},
		},
		CredentialRefs: map[string]string{
			"token":                        "secret://ignored",
			"GITHUB_PERSONAL_ACCESS_TOKEN": "secret://also-ignored-without-store",
		},
	}
	envSlice, envMap, err := p.resolveSpawnEnv(conn)
	if err != nil {
		t.Fatal(err)
	}
	if envMap["LITERAL"] != "yes" {
		t.Fatalf("literal missing: %+v", envMap)
	}
	if _, ok := envMap["GITHUB_PERSONAL_ACCESS_TOKEN"]; ok {
		t.Fatal("credential should not inject without secrets store")
	}
	if len(envSlice) == 0 {
		t.Fatal("expected host environ at least")
	}
}

func TestResolveSpawnEnv_RejectsBadKey(t *testing.T) {
	p := &Plugin{}
	conn := &domain.Connection{
		Config: map[string]any{
			"env": map[string]any{"BAD-KEY": "x"},
		},
	}
	if _, _, err := p.resolveSpawnEnv(conn); err == nil {
		t.Fatal("expected error for invalid env key")
	}
}
