package runtime

import (
	"fmt"
	"strings"
)

// argumentSchema describes the planner-supplied keys a single tool accepts:
// which are allowed at all (the allowlist), which are required (must be
// present and non-empty), and what type each must be. The runtime uses it
// for two checks:
//
//  1. parsePlannerResponse → schema.validate to drop a malformed plan
//     before it persists, so the user never sees an approval card whose
//     arguments would fail at execution time anyway.
//  2. buildToolInput → schema.allowed to filter at merge time, defending
//     against a planner that ignored the prompt.
//
// Keeping the schema declarative (no per-tool Go interface) lets us add a
// new tool by adding a row here rather than wiring three separate places.
type argumentSchema struct {
	// allowed names every key the planner may set. Used by buildToolInput
	// to filter; keys not in this map are silently dropped.
	allowed map[string]argumentField
	// required names the keys that MUST be present and non-empty. A subset
	// of allowed.
	required []string
}

type argumentField struct {
	// kind constrains the JSON type of the value. "string" is the only
	// type we accept today — every existing tool argument is a string.
	// Adding "int" / "bool" later means widening this enum and the
	// type-check in validate.
	kind string
}

// argumentSchemas is the single registry for tool argument shapes. Adding
// a tool means adding a row here AND a case in plannerArgumentAllowlist;
// the registry is the source of truth and the allowlist is derived from
// it (see plannerArgumentAllowlist).
var argumentSchemas = map[string]argumentSchema{
	"llm.chat": {
		allowed: map[string]argumentField{
			"prompt": {kind: "string"},
		},
		// prompt is optional: when the planner omits it, buildToolInput
		// falls back to step.Input (the description). Required would force
		// the planner to repeat the goal verbatim for every chat step.
	},
	"filesystem.read": {
		allowed: map[string]argumentField{
			"path": {kind: "string"},
		},
		required: []string{"path"},
	},
	"filesystem.write": {
		allowed: map[string]argumentField{
			"path":    {kind: "string"},
			"content": {kind: "string"},
		},
		required: []string{"path", "content"},
	},
	"filesystem.patch": {
		allowed: map[string]argumentField{
			"diff": {kind: "string"},
		},
		required: []string{"diff"},
	},
	"filesystem.context": {
		allowed: map[string]argumentField{},
	},
	"command.exec": {
		allowed: map[string]argumentField{
			"command": {kind: "string"},
		},
		required: []string{"command"},
	},
}

// validatePlannerArguments enforces the per-tool schema at planner-parse
// time. Returns nil on a valid (or absent) argument set; returns an error
// listing every field problem on a structurally invalid one. The error is
// suitable for inclusion in a step.Error or planner-failure log; it does
// not surface to end users directly.
func validatePlannerArguments(toolName string, args map[string]interface{}) error {
	schema, ok := argumentSchemas[toolName]
	if !ok {
		// Unknown tool: planner is not allowed to set anything. The merge
		// step would drop everything anyway, but rejecting here keeps a
		// noisy plan from polluting the database.
		if len(args) > 0 {
			return fmt.Errorf("tool %q has no planner argument schema; refusing %d field(s)", toolName, len(args))
		}
		return nil
	}

	var problems []string
	for k, v := range args {
		field, allowed := schema.allowed[k]
		if !allowed {
			problems = append(problems, fmt.Sprintf("unknown field %q", k))
			continue
		}
		if !valueMatchesKind(v, field.kind) {
			problems = append(problems, fmt.Sprintf("field %q must be %s, got %T", k, field.kind, v))
		}
	}
	for _, name := range schema.required {
		v, present := args[name]
		if !present {
			problems = append(problems, fmt.Sprintf("missing required field %q", name))
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			problems = append(problems, fmt.Sprintf("required field %q is empty", name))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid arguments for tool %q: %s", toolName, strings.Join(problems, "; "))
	}
	return nil
}

// valueMatchesKind reports whether v parses out of JSON as the named kind.
// Only "string" is supported today; widening this is a new-tool concern.
func valueMatchesKind(v interface{}, kind string) bool {
	switch kind {
	case "string":
		_, ok := v.(string)
		return ok
	default:
		return false
	}
}
