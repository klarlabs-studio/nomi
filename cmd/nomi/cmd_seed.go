package main

import (
	"flag"
	"fmt"
	"os"

	"go.klarlabs.de/nomi/internal/seed"
)

// seedCmd: apply a YAML manifest against an already-running daemon
// without restarting. Useful when the daemon's data dir isn't on a
// volume the seed file can reach (NOMI_SEED env covers the file-based
// path; this subcommand covers ad-hoc / remote application).
//
//	nomi seed examples/seed.yaml
//
// Implementation note: today this re-implements the same idempotent
// inserts the in-process seed.Apply runs, but goes through HTTP rather
// than touching the database directly — so the CLI can run from a
// different host than nomid. Skipping the file-check / repo-init
// boilerplate would have meant duplicating the logic; reusing the
// package keeps the schema in one place.
func seedCmd(common *commonFlags, args []string) int {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	bindCommonFlags(fs, common)
	_ = fs.Parse(args)

	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "nomi seed: path required (e.g. nomi seed examples/seed.yaml)")
		return 2
	}
	path := rest[0]

	cli, err := NewClient(common)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var f seed.File
	if err := unmarshalYAML(raw, &f); err != nil {
		fmt.Fprintln(os.Stderr, "parse seed:", err)
		return 1
	}

	if f.Provider != nil {
		if err := applyProviderHTTP(cli, *f.Provider); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	for _, a := range f.Assistants {
		if err := applyAssistantHTTP(cli, a); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	if f.Settings != nil {
		if f.Settings.SafetyProfile != "" {
			_ = cli.Put("/settings/safety-profile",
				map[string]any{"profile": f.Settings.SafetyProfile}, nil)
		}
		if f.Settings.OnboardingComplete != nil {
			_ = cli.Put("/settings/onboarding-complete",
				map[string]any{"complete": *f.Settings.OnboardingComplete}, nil)
		}
	}
	fmt.Fprintln(os.Stderr, "✓ seed applied")
	return 0
}

// unmarshalYAML defers to gopkg.in/yaml.v3 (already a transitive dep)
// without requiring the CLI to import it directly — the seed package
// uses it, so re-export through a small bridge function would create
// circular import risk. Instead we marshal+unmarshal via the package's
// own Apply() expectations: parse here, send field-by-field over HTTP.
func unmarshalYAML(raw []byte, out *seed.File) error {
	return yamlUnmarshal(raw, out)
}

func applyProviderHTTP(cli *Client, p seed.ProviderSeed) error {
	// First check whether a profile with this name exists; the daemon
	// will happily create duplicates, but the seed contract is
	// idempotent, so we mirror the in-process behaviour.
	var existing struct {
		Profiles []struct {
			ID, Name string
		} `json:"profiles"`
	}
	if err := cli.Get("/provider-profiles", &existing); err != nil {
		return err
	}
	defaultModel := p.DefaultModel
	if defaultModel == "" && len(p.ModelIDs) > 0 {
		defaultModel = p.ModelIDs[0]
	}
	for _, e := range existing.Profiles {
		if e.Name == p.Name {
			fmt.Fprintf(os.Stderr, "= provider %q already present (id=%s)\n", p.Name, short(e.ID))
			if defaultModel != "" {
				_ = cli.Put("/settings/llm-default",
					map[string]any{"provider_id": e.ID, "model_id": defaultModel}, nil)
			}
			return nil
		}
	}
	body := map[string]any{
		"name":      p.Name,
		"type":      p.Type,
		"endpoint":  p.Endpoint,
		"model_ids": p.ModelIDs,
		"enabled":   true,
	}
	if p.APIKey != "" {
		body["secret_ref"] = p.APIKey
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := cli.Post("/provider-profiles", body, &created); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "+ provider %q created (id=%s)\n", p.Name, short(created.ID))
	if defaultModel != "" {
		_ = cli.Put("/settings/llm-default",
			map[string]any{"provider_id": created.ID, "model_id": defaultModel}, nil)
	}
	return nil
}

func applyAssistantHTTP(cli *Client, a seed.AssistantSeed) error {
	var list struct {
		Assistants []struct{ ID, Name string } `json:"assistants"`
	}
	if err := cli.Get("/assistants", &list); err != nil {
		return err
	}
	displayName := a.Name
	if displayName == "" {
		displayName = a.TemplateID
	}
	for _, e := range list.Assistants {
		if e.Name == displayName {
			fmt.Fprintf(os.Stderr, "= assistant %q already present (id=%s)\n", displayName, short(e.ID))
			return nil
		}
	}
	var tpls struct {
		Templates []map[string]any `json:"templates"`
	}
	if err := cli.Get("/assistants/templates", &tpls); err != nil {
		return err
	}
	var tpl map[string]any
	for _, t := range tpls.Templates {
		if t["template_id"] == a.TemplateID {
			tpl = t
			break
		}
	}
	if tpl == nil {
		return fmt.Errorf("template %q not found", a.TemplateID)
	}
	// Trim to fields the create endpoint accepts; the templates list
	// includes diagnostic fields that POST /assistants rejects.
	body := map[string]any{}
	for _, k := range []string{
		"template_id", "tagline", "role", "best_for", "not_for", "suggested_model",
		"system_prompt", "channels", "channel_configs", "capabilities",
		"memory_policy", "permission_policy",
	} {
		if v, ok := tpl[k]; ok {
			body[k] = v
		}
	}
	if a.Name != "" {
		body["name"] = a.Name
	} else if v, ok := tpl["name"]; ok {
		body["name"] = v
	}
	if a.Workspace != "" {
		body["contexts"] = []map[string]any{{"type": "folder", "path": a.Workspace}}
	} else if v, ok := tpl["contexts"]; ok {
		body["contexts"] = v
	}
	var created struct{ ID string }
	if err := cli.Post("/assistants", body, &created); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "+ assistant %q created (id=%s, template=%s)\n",
		displayName, short(created.ID), a.TemplateID)
	return nil
}
