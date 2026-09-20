package main

import (
	"flag"
	"fmt"
	"os"
)

// statusCmd: one-shot health + version + active default. Useful as the
// first command after deploying the daemon — confirms reachability,
// build version, and that an LLM is wired. Also surfaces schedule
// health so headless users can answer "is my automation broken?".
//
//	nomi status
func statusCmd(common *commonFlags, args []string) int {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	bindCommonFlags(fs, common)
	_ = fs.Parse(args)

	cli, err := NewClient(common)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	var health struct {
		Status string `json:"status"`
	}
	if err := cli.Get("/health", &health); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var version struct {
		Version, Commit, BuildDate string
	}
	_ = cli.Get("/version", &version)
	var defaults struct {
		ProviderID string `json:"provider_id"`
		ModelID    string `json:"model_id"`
	}
	_ = cli.Get("/settings/llm-default", &defaults)
	var safety struct {
		Profile string
	}
	_ = cli.Get("/settings/safety-profile", &safety)

	schedSummary := loadScheduleSummary(cli)

	if common.JSON {
		printJSON(map[string]any{
			"url":         cli.URL,
			"health":      health.Status,
			"version":     version,
			"llm_default": defaults,
			"safety":      safety.Profile,
			"schedules":   schedSummary,
		})
		return 0
	}

	fmt.Printf("URL:           %s\n", cli.URL)
	fmt.Printf("Health:        %s\n", health.Status)
	fmt.Printf("Version:       %s (commit %s, built %s)\n",
		version.Version, version.Commit, version.BuildDate)
	if defaults.ProviderID != "" {
		fmt.Printf("Default LLM:   %s / %s\n", short(defaults.ProviderID), defaults.ModelID)
	} else {
		fmt.Printf("Default LLM:   (none configured — run `nomi seed` or open the wizard)\n")
	}
	fmt.Printf("Safety:        %s\n", safety.Profile)
	fmt.Printf("Schedules:     %d total, %d enabled", schedSummary.Total, schedSummary.Enabled)
	if schedSummary.WithError > 0 {
		fmt.Printf(", %d with errors", schedSummary.WithError)
	}
	fmt.Println()
	return 0
}

type scheduleSummary struct {
	Total     int `json:"total"`
	Enabled   int `json:"enabled"`
	WithError int `json:"with_error"`
}

func loadScheduleSummary(cli *Client) scheduleSummary {
	var resp struct {
		Schedules []struct {
			Enabled   bool   `json:"enabled"`
			LastError string `json:"last_error"`
		} `json:"schedules"`
	}
	if err := cli.Get("/schedules", &resp); err != nil {
		return scheduleSummary{}
	}
	out := scheduleSummary{Total: len(resp.Schedules)}
	for _, s := range resp.Schedules {
		if s.Enabled {
			out.Enabled++
		}
		if s.LastError != "" {
			out.WithError++
		}
	}
	return out
}
