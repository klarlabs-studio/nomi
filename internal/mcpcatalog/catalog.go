// Package mcpcatalog fetches and normalizes remote MCP server preset
// catalogs for the Plugins → MCP UI. Built-in presets stay in the
// desktop client; this package only handles the Goose-style remote URL
// catalog so orgs can point nomid at a private/community JSON index.
//
// Accepted wire formats:
//  1. Nomi envelope: {"schema_version":1,"presets":[...]}
//  2. Nomi bare array of presets (same fields as the UI McpServerPreset)
//  3. Goose servers.json (documentation/static/servers.json) — mapped
//     into the Nomi preset shape. Goose builtins (is_builtin) are skipped.
package mcpcatalog

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// SchemaVersion pins the Nomi envelope. Additive fields are OK;
// incompatible bumps must raise this and reject older clients carefully.
const SchemaVersion = 1

// SettingKey is the app_settings key for the catalog URL.
const SettingKey = "mcp_preset_catalog_url"

// DefaultGooseCatalogURL is the public Goose extension index — the
// compete reference for "point at a remote marketplace JSON".
const DefaultGooseCatalogURL = "https://raw.githubusercontent.com/block/goose/main/documentation/static/servers.json"

// EnvCredential is a secret env var the UI collects before create.
type EnvCredential struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Required    bool   `json:"required"`
	Description string `json:"description,omitempty"`
}

// Preset is the JSON shape shared with the desktop MCP catalog UI.
type Preset struct {
	ID             string          `json:"id"`
	Label          string          `json:"label"`
	Description    string          `json:"description"`
	SuggestedName  string          `json:"suggested_name"`
	Transport      string          `json:"transport"` // stdio | http
	Category       string          `json:"category"`
	Runtime        string          `json:"runtime"` // npx | uvx | docker | http | manual
	Command        string          `json:"command,omitempty"`
	Args           string          `json:"args,omitempty"`
	Endpoint       string          `json:"endpoint,omitempty"`
	SetupNote      string          `json:"setup_note"`
	DocURL         string          `json:"doc_url"`
	ReadyToCreate  bool            `json:"ready_to_create"`
	EnvCredentials []EnvCredential `json:"env_credentials,omitempty"`
	Source         string          `json:"source,omitempty"` // remote | builtin
	Endorsed       bool            `json:"endorsed,omitempty"`
	CatalogOrigin  string          `json:"catalog_origin,omitempty"` // host of the catalog URL
}

// Envelope is the Nomi-native catalog document.
type Envelope struct {
	SchemaVersion int      `json:"schema_version"`
	Presets       []Preset `json:"presets"`
}

// gooseEntry is a subset of block/goose documentation/static/servers.json.
type gooseEntry struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	Description          string `json:"description"`
	Command              string `json:"command"`
	URL                  string `json:"url"`
	Link                 string `json:"link"`
	InstallationNotes    string `json:"installation_notes"`
	IsBuiltin            bool   `json:"is_builtin"`
	Endorsed             bool   `json:"endorsed"`
	Type                 string `json:"type"` // "streamable-http" or empty (stdio)
	EnvironmentVariables []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Required    bool   `json:"required"`
	} `json:"environmentVariables"`
	Headers []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Required    bool   `json:"required"`
	} `json:"headers"`
}

// Parse accepts Goose JSON, a Nomi envelope, or a bare Nomi preset array.
// originHost is stamped onto each preset for UI attribution (may be empty).
func Parse(raw []byte, originHost string) ([]Preset, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, fmt.Errorf("mcpcatalog: empty body")
	}

	// Try Nomi envelope first (object with presets).
	if strings.HasPrefix(trimmed, "{") {
		var env Envelope
		if err := json.Unmarshal(raw, &env); err == nil && (env.Presets != nil || env.SchemaVersion > 0) {
			if env.SchemaVersion != 0 && env.SchemaVersion != SchemaVersion {
				return nil, fmt.Errorf("mcpcatalog: unsupported schema_version %d", env.SchemaVersion)
			}
			return normalizePresets(env.Presets, originHost), nil
		}
		// Fall through — might be something else; try Goose-shaped object later.
	}

	// Bare JSON array: detect Goose vs Nomi by probing first element keys.
	if strings.HasPrefix(trimmed, "[") {
		var probe []json.RawMessage
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, fmt.Errorf("mcpcatalog: invalid JSON array: %w", err)
		}
		if len(probe) == 0 {
			return nil, nil
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(probe[0], &keys); err != nil {
			return nil, fmt.Errorf("mcpcatalog: array element not object: %w", err)
		}
		if _, ok := keys["label"]; ok {
			var presets []Preset
			if err := json.Unmarshal(raw, &presets); err != nil {
				return nil, fmt.Errorf("mcpcatalog: nomi presets: %w", err)
			}
			return normalizePresets(presets, originHost), nil
		}
		// Goose (name/command/link) or similar.
		var goose []gooseEntry
		if err := json.Unmarshal(raw, &goose); err != nil {
			return nil, fmt.Errorf("mcpcatalog: goose catalog: %w", err)
		}
		return fromGoose(goose, originHost), nil
	}

	return nil, fmt.Errorf("mcpcatalog: unrecognized catalog JSON")
}

func normalizePresets(in []Preset, originHost string) []Preset {
	out := make([]Preset, 0, len(in))
	seen := map[string]struct{}{}
	for _, p := range in {
		p = sanitizePreset(p)
		if p.ID == "" || p.Label == "" {
			continue
		}
		if _, dup := seen[p.ID]; dup {
			continue
		}
		seen[p.ID] = struct{}{}
		p.Source = "remote"
		p.CatalogOrigin = originHost
		out = append(out, p)
	}
	return out
}

func sanitizePreset(p Preset) Preset {
	p.ID = strings.TrimSpace(p.ID)
	p.Label = strings.TrimSpace(p.Label)
	p.Description = strings.TrimSpace(p.Description)
	p.SuggestedName = strings.TrimSpace(p.SuggestedName)
	if p.SuggestedName == "" {
		p.SuggestedName = p.Label
	}
	p.Transport = strings.ToLower(strings.TrimSpace(p.Transport))
	if p.Transport != "http" {
		p.Transport = "stdio"
	}
	p.Category = strings.ToLower(strings.TrimSpace(p.Category))
	if p.Category == "" {
		if p.Transport == "http" {
			p.Category = "remote"
		} else {
			p.Category = "cloud"
		}
	}
	p.Runtime = strings.ToLower(strings.TrimSpace(p.Runtime))
	if p.Runtime == "" {
		p.Runtime = inferRuntime(p.Command, p.Transport)
	}
	p.Command = strings.TrimSpace(p.Command)
	p.Args = strings.TrimSpace(p.Args)
	p.Endpoint = strings.TrimSpace(p.Endpoint)
	p.SetupNote = strings.TrimSpace(p.SetupNote)
	p.DocURL = strings.TrimSpace(p.DocURL)
	if p.DocURL == "" {
		p.DocURL = "https://modelcontextprotocol.io/examples"
	}
	if p.SetupNote == "" {
		p.SetupNote = "Remote catalog preset."
	}
	// Path placeholders or required secrets → not ready.
	if p.ReadyToCreate {
		if strings.Contains(p.Args, "/path/to/") || strings.Contains(p.Endpoint, "example.com") {
			p.ReadyToCreate = false
		}
		for _, c := range p.EnvCredentials {
			if c.Required {
				p.ReadyToCreate = false
				break
			}
		}
	}
	return p
}

func fromGoose(entries []gooseEntry, originHost string) []Preset {
	out := make([]Preset, 0, len(entries))
	seen := map[string]struct{}{}
	for _, e := range entries {
		if e.IsBuiltin {
			continue // Goose-native; not installable via com.nomi.mcp
		}
		id := strings.TrimSpace(e.ID)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		name := strings.TrimSpace(e.Name)
		if name == "" {
			name = id
		}

		p := Preset{
			ID:            id,
			Label:         name,
			Description:   strings.TrimSpace(e.Description),
			SuggestedName: name,
			SetupNote:     strings.TrimSpace(e.InstallationNotes),
			DocURL:        strings.TrimSpace(e.Link),
			Endorsed:      e.Endorsed,
			Source:        "remote",
			CatalogOrigin: originHost,
			Category:      "cloud",
		}
		if p.SetupNote == "" {
			p.SetupNote = "Imported from remote MCP catalog."
		}
		if p.DocURL == "" {
			p.DocURL = "https://modelcontextprotocol.io/examples"
		}

		isHTTP := strings.EqualFold(e.Type, "streamable-http") || strings.TrimSpace(e.URL) != ""
		if isHTTP && strings.TrimSpace(e.URL) != "" {
			p.Transport = "http"
			p.Runtime = "http"
			p.Category = "remote"
			p.Endpoint = strings.TrimSpace(e.URL)
			p.ReadyToCreate = !strings.Contains(p.Endpoint, "localhost") &&
				!strings.Contains(p.Endpoint, "127.0.0.1") &&
				len(e.EnvironmentVariables) == 0 &&
				len(e.Headers) == 0
		} else {
			cmd := strings.TrimSpace(e.Command)
			if cmd == "" {
				continue // nothing to launch
			}
			bin, args := splitCommand(cmd)
			p.Transport = "stdio"
			p.Command = bin
			p.Args = args
			p.Runtime = inferRuntime(bin, "stdio")
			p.ReadyToCreate = len(e.EnvironmentVariables) == 0 &&
				!strings.Contains(args, "/path/to/")
		}

		for _, ev := range e.EnvironmentVariables {
			key := strings.TrimSpace(ev.Name)
			if key == "" {
				continue
			}
			// Treat Goose env vars as required secrets unless explicitly optional.
			req := true
			if !ev.Required && strings.Contains(strings.ToLower(ev.Description), "optional") {
				req = false
			}
			p.EnvCredentials = append(p.EnvCredentials, EnvCredential{
				Key:         key,
				Label:       humanizeEnv(key),
				Required:    req,
				Description: strings.TrimSpace(ev.Description),
			})
			p.ReadyToCreate = false
		}
		// Headers on HTTP remotes → surface as a single bearer-style note;
		// Nomi stores HTTP auth via the connection credential field, not env.
		if len(e.Headers) > 0 && p.Transport == "http" {
			var names []string
			for _, h := range e.Headers {
				if n := strings.TrimSpace(h.Name); n != "" {
					names = append(names, n)
				}
			}
			if len(names) > 0 {
				note := "Requires HTTP header(s): " + strings.Join(names, ", ") +
					". Set a bearer token in the connection credential if applicable."
				if p.SetupNote != "" {
					p.SetupNote = p.SetupNote + " " + note
				} else {
					p.SetupNote = note
				}
				p.ReadyToCreate = false
			}
		}

		seen[id] = struct{}{}
		out = append(out, sanitizePreset(p))
	}
	return out
}

func splitCommand(cmd string) (bin, args string) {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return "", ""
	}
	if len(fields) == 1 {
		return fields[0], ""
	}
	return fields[0], strings.Join(fields[1:], " ")
}

func inferRuntime(command, transport string) string {
	if transport == "http" {
		return "http"
	}
	switch strings.ToLower(strings.TrimSpace(command)) {
	case "npx", "npm", "node":
		return "npx"
	case "uvx", "uv":
		return "uvx"
	case "docker", "podman":
		return "docker"
	default:
		if command == "" {
			return "manual"
		}
		return "manual"
	}
}

func humanizeEnv(key string) string {
	parts := strings.Split(key, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		runes := []rune(strings.ToLower(p))
		runes[0] = unicode.ToUpper(runes[0])
		parts[i] = string(runes)
	}
	return strings.Join(parts, " ")
}
