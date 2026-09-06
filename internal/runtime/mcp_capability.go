package runtime

import "strings"

// refineMCPCapability upgrades the generic mcp.call / mcp.tools gate to
// a per-discovered-tool capability (mcp.<connection>.<tool>) when the
// call arguments name an upstream tool that is already hot-registered.
// That way remembered approvals and policy rules apply per tool — Allow
// on search never covers write_file — matching Goose/Cline fine-grained
// MCP consent. If no discovered tool matches, the original capability
// (mcp.tools) is kept.
func (r *Runtime) refineMCPCapability(toolName, capability string, input map[string]interface{}) string {
	if toolName != "mcp.call" {
		return capability
	}
	upstream, _ := input["tool"].(string)
	if upstream == "" {
		return capability
	}
	if r == nil || r.toolExecutor == nil {
		return capability
	}
	wantSuffix := "." + mcpToolSlug(upstream)
	for _, name := range r.toolExecutor.KnownTools() {
		if !strings.HasPrefix(name, "mcp.") {
			continue
		}
		if !strings.HasSuffix(name, wantSuffix) {
			continue
		}
		// Skip the static dispatch tools themselves.
		if name == "mcp.call" || name == "mcp.list_tools" || name == "mcp.tools" {
			continue
		}
		if cap := r.toolExecutor.CapabilityFor(name); cap != "" {
			return cap
		}
		return name
	}
	return capability
}

// mcpToolSlug mirrors mcpbridge.slugify for the upstream tool segment so
// mcp.call{tool:"write_file"} resolves to the same suffix as a
// hot-registered mcp.<conn>.write_file capability. Kept local to avoid
// importing the plugin package from runtime.
func mcpToolSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastUnderscore := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastUnderscore = false
		default:
			if !lastUnderscore && b.Len() > 0 {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	return strings.Trim(b.String(), "_")
}
