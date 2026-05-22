package mnemos

import (
	_ "embed"

	"github.com/felixgeelhaar/nomi/internal/plugins"
)

// PluginID is the stable reverse-DNS identifier. Do not change without
// a migration — the persistence layer keys connection rows on it.
const PluginID = "com.nomi.mnemos"

// PluginVersion is the plugin's own semver, independent of the nomid
// binary. Bumped when the plugin's external contract changes (new tool,
// changed manifest, breaking input shape).
const PluginVersion = "0.1.0"

// Capability strings. Split so users can grant read-only access without
// allowing writes — a common posture for "let the planner consult the
// knowledge graph but don't let agents pollute it without a human in
// the loop."
const (
	CapRead  = "mnemos.read"
	CapWrite = "mnemos.write"
)

// Tool names. Mirrored as the registry keys; must be unique across all
// plugins. Prefix matches the plugin family so the planner can group
// related tools and the UI can render them under one heading.
const (
	ToolEventsAppend        = "mnemos.events.append"
	ToolClaimsAppend        = "mnemos.claims.append"
	ToolClaimsList          = "mnemos.claims.list"
	ToolRelationshipsList   = "mnemos.relationships.list"
	ToolEmbeddingsAppend  = "mnemos.embeddings.append"
	// ToolSearch covers hybrid retrieval (vector + lexical) over the
	// claim store. Maps to client.Search upstream — the "embeddings
	// similar" name from the original ADR sketch turned out to be a
	// poor fit for what real Mnemos exposes.
	ToolSearch = "mnemos.search"
)

// ContextSourceName is the identifier the planner uses when invoking
// the Mnemos context source. Matches the manifest contribution name.
const ContextSourceName = "mnemos.claims"

// buildManifest returns the static plugin description the registry
// reads at registration time. Deterministic; the registry caches the
// result.
func buildManifest() plugins.PluginManifest {
	return plugins.PluginManifest{
		ID:          PluginID,
		Name:        "Mnemos",
		Version:     PluginVersion,
		Author:      "Nomi",
		Description: "Append events, claims, evidence, relationships, and embeddings to a Mnemos knowledge-graph service. Pull recent relevant claims into planner context.",
		Cardinality: plugins.ConnectionMulti,
		Capabilities: []string{
			CapRead,
			CapWrite,
		},
		Contributes: plugins.Contributions{
			Tools: []plugins.ToolContribution{
				{
					Name:               ToolEventsAppend,
					Capability:         CapWrite,
					Description:        "Append one or more knowledge events to Mnemos. Use when capturing raw observations the assistant has gathered (a meeting note, a doc skim, a tool output worth retaining).",
					RequiresConnection: true,
				},
				{
					Name:               ToolClaimsAppend,
					Capability:         CapWrite,
					Description:        "Append claims (fact / hypothesis / decision / test_result) with optional evidence links pointing back at the events that support them.",
					RequiresConnection: true,
				},
				{
					Name:               ToolClaimsList,
					Capability:         CapRead,
					Description:        "List claims, optionally filtered by type and status. Use to consult prior decisions or open hypotheses before planning.",
					RequiresConnection: true,
				},
				{
					Name:               ToolRelationshipsList,
					Capability:         CapRead,
					Description:        "List relationships between claims (supports / contradicts). Use to surface contradictions before acting on a claim.",
					RequiresConnection: true,
				},
				{
					Name:               ToolEmbeddingsAppend,
					Capability:         CapWrite,
					Description:        "Append vectors for semantic recall.",
					RequiresConnection: true,
				},
				{
					Name:               ToolSearch,
					Capability:         CapRead,
					Description:        "Hybrid retrieval (vector + lexical) over the Mnemos claim store. Returns the top-k claims relevant to the query plus any contradictions among them. Use when the planner wants prior context for the current goal.",
					RequiresConnection: true,
				},
			},
			ContextSources: []plugins.ContextSourceContribution{
				{
					Name:        ContextSourceName,
					Description: "Pulls recent relevant claims from Mnemos into the planner context at run start. Read-only.",
				},
			},
		},
		Requires: plugins.Requirements{
			Credentials: []plugins.CredentialSpec{
				{
					Kind:        "bearer_token",
					Key:         "token",
					Label:       "Mnemos bearer token",
					Required:    false,
					Description: "Required for write endpoints; reads are open. Generate one via `mnemos token issue` on the Mnemos server.",
				},
			},
			ConfigSchema: map[string]plugins.ConfigField{
				"base_url": {
					Type:        "string",
					Label:       "Mnemos base URL",
					Required:    true,
					Description: "Server URL the plugin will talk to, e.g. https://mnemos.example.com.",
				},
				"visibility_default": {
					Type:        "enum",
					Label:       "Default visibility for writes",
					Required:    false,
					Default:     "team",
					Description: "Applied to new claims when the tool input omits an explicit value.",
					Options: []plugins.ConfigOption{
						{Value: "personal"},
						{Value: "team"},
						{Value: "org"},
					},
				},
			},
			NetworkAllowlist: nil, // Per-connection base_url, supplied at runtime.
		},
	}
}
