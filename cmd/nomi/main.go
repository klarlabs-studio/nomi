// Package main is the `nomi` CLI client — a terminal companion for a
// headless nomid daemon. SSH into a server, run `nomi run "summarize
// notes.md"`, watch the plan, approve, see the output. No browser, no
// curl loops.
//
// The CLI is intentionally a thin client: every action it performs is a
// REST call against the same surface the desktop UI consumes. Adding a
// new subcommand is one HTTP request plus a printer.
//
// Future work: a `nomi tui` subcommand backed by bubbletea for users
// who want a richer interactive shell. The current CLI design separates
// transport (client.go) from rendering (cmd_*.go) so that swap is cheap.
package main

import (
	"flag"
	"fmt"
	"os"

	"go.klarlabs.de/nomi/internal/buildinfo"
)

const usage = `nomi — CLI client for the Nomi runtime daemon (nomid).

USAGE
    nomi <subcommand> [flags] [args]

SUBCOMMANDS
    run "<goal>"           Submit a goal, auto-approve plans, prompt on
                           confirm-mode capabilities, print the output.
    tail                   Follow the server-sent event stream live.
    list runs              Show the most recent runs.
    list assistants        Show every configured assistant.
    list providers         Show every configured LLM provider.
    list approvals         Show pending approval cards.
    list memory            Show stored memory entries.
    status                 Show daemon health + version + active default.
    seed <path>            Apply a seed.yaml manifest against the running
                           daemon (useful for ad-hoc reconfig).
    export [-o file]       Snapshot the daemon's full config as YAML
                           (providers, default LLM, assistants,
                           settings, preferences, plugin states). Secrets
                           are exported as references only.
    import <path>          Apply an exported snapshot idempotently —
                           reproduce a setup on another machine.
    version                Print the CLI version.

FLAGS (apply to every subcommand)
    --url        Override the daemon URL (default: read from
                 $NOMI_DATA_DIR/api.endpoint, fallback http://127.0.0.1:8080)
    --token      Override the auth token (default: read from
                 $NOMI_DATA_DIR/auth.token, or $NOMI_TOKEN env var)
    --json       Emit machine-readable JSON instead of human-formatted output

EXAMPLES
    nomi run "summarize notes.md in one sentence"
    nomi list runs
    nomi tail
    NOMI_TOKEN=$(ssh server 'docker exec nomi cat /data/auth.token') \
        nomi --url=https://nomi.example.com run "what changed today?"
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	// Subcommand routing happens before flag.Parse so we can give each
	// subcommand its own flag set with subcommand-specific flags.
	sub := os.Args[1]
	args := os.Args[2:]

	// Common flags every subcommand honours. Each subcommand's own
	// FlagSet inherits these defaults via parseCommonFlags.
	common := newCommonFlags()

	switch sub {
	case "run":
		os.Exit(runCmd(common, args))
	case "tail":
		os.Exit(tailCmd(common, args))
	case "list", "ls":
		os.Exit(listCmd(common, args))
	case "status":
		os.Exit(statusCmd(common, args))
	case "seed":
		os.Exit(seedCmd(common, args))
	case "export":
		os.Exit(exportCmd(common, args))
	case "import":
		os.Exit(importCmd(common, args))
	case "memory":
		os.Exit(memoryCmd(common, args))
	case "version", "--version", "-v":
		info := buildinfo.Current()
		fmt.Printf("nomi cli v%s (%s, %s)\n", info.Version, info.Commit, info.BuildDate)
		os.Exit(0)
	case "help", "--help", "-h":
		fmt.Print(usage)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "nomi: unknown subcommand %q\n\n", sub)
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

// commonFlags carries the URL / token / format flags every subcommand
// understands. Each subcommand binds them onto its own FlagSet so
// `nomi run --url=… "goal"` works the same as `nomi list runs --json`.
type commonFlags struct {
	URL   string
	Token string
	JSON  bool
}

func newCommonFlags() *commonFlags { return &commonFlags{} }

func bindCommonFlags(fs *flag.FlagSet, c *commonFlags) {
	fs.StringVar(&c.URL, "url", "", "daemon URL (default: api.endpoint or http://127.0.0.1:8080)")
	fs.StringVar(&c.Token, "token", "", "bearer token (default: $NOMI_TOKEN or $NOMI_DATA_DIR/auth.token)")
	fs.BoolVar(&c.JSON, "json", false, "emit machine-readable JSON")
}
