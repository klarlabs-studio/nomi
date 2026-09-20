package mcpbridge

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/secrets"
)

// httpTokenCredKey is the reserved credential_refs key that maps to the
// HTTP Authorization header value — never injected into stdio env.
const httpTokenCredKey = "token"

var envVarNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// resolveSpawnEnv builds the subprocess environment for a stdio MCP
// connection: host environ + config.env literals + credential_refs
// (every key except "token"). Secrets are resolved via the store so
// they never land in SQLite config plaintext.
func (p *Plugin) resolveSpawnEnv(conn *domain.Connection) ([]string, map[string]string, error) {
	merged := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			merged[kv[:i]] = kv[i+1:]
		}
	}

	for k, v := range parseConfigEnv(conn.Config["env"]) {
		if !envVarNameRE.MatchString(k) {
			return nil, nil, fmt.Errorf("mcp: invalid env key %q", k)
		}
		merged[k] = v
	}

	if p.secrets != nil {
		for key, ref := range conn.CredentialRefs {
			if key == httpTokenCredKey || ref == "" {
				continue
			}
			if !envVarNameRE.MatchString(key) {
				return nil, nil, fmt.Errorf("mcp: invalid env credential key %q", key)
			}
			val, err := secrets.Resolve(p.secrets, ref)
			if err != nil {
				return nil, nil, fmt.Errorf("mcp: resolve env %s: %w", key, err)
			}
			merged[key] = val
		}
	}

	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	return out, merged, nil
}

// parseConfigEnv accepts either a map[string]any (JSON object) or a
// legacy KEY=value newline string. Empty / missing returns nil.
func parseConfigEnv(raw any) map[string]string {
	out := map[string]string{}
	switch v := raw.(type) {
	case map[string]any:
		for k, val := range v {
			s, ok := val.(string)
			if !ok || s == "" || k == "" {
				continue
			}
			out[k] = s
		}
	case map[string]string:
		for k, s := range v {
			if s == "" || k == "" {
				continue
			}
			out[k] = s
		}
	case string:
		for _, line := range strings.Split(v, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			i := strings.IndexByte(line, '=')
			if i <= 0 {
				continue
			}
			out[strings.TrimSpace(line[:i])] = strings.TrimSpace(line[i+1:])
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// expandArgsSubstitutions replaces ${VAR} in the args string using the
// merged env map (secrets already resolved). Unresolved vars are left
// intact so the MCP server can fail loudly rather than silently.
func expandArgsSubstitutions(args string, env map[string]string) string {
	if args == "" || !strings.Contains(args, "${") {
		return args
	}
	return os.Expand(args, func(key string) string {
		if v, ok := env[key]; ok {
			return v
		}
		return "${" + key + "}"
	})
}
