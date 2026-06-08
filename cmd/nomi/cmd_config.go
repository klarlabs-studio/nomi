package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
)

// exportCmd writes the daemon's current YAML snapshot to stdout (or
// the path given by -o).
//
//	nomi export                  # YAML to stdout
//	nomi export -o config.yaml   # YAML to file
func exportCmd(common *commonFlags, args []string) int {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	bindCommonFlags(fs, common)
	out := fs.String("o", "", "write to file instead of stdout")
	_ = fs.Parse(args)

	cli, err := NewClient(common)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	req, _ := http.NewRequest(http.MethodGet, cli.URL+"/config/export", nil)
	req.Header.Set("Authorization", "Bearer "+cli.Token)
	resp, err := cli.HTTP.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "export: HTTP %d: %s\n", resp.StatusCode, body)
		return 1
	}
	if *out != "" {
		if err := os.WriteFile(*out, body, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "✓ wrote %d bytes to %s\n", len(body), *out)
		return 0
	}
	_, _ = os.Stdout.Write(body)
	return 0
}

// importCmd POSTs a YAML file to /config/import. Idempotent on the
// daemon — re-running with the same file is a no-op for matching
// rows; differences are applied as updates.
//
//	nomi import config.yaml
func importCmd(common *commonFlags, args []string) int {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	bindCommonFlags(fs, common)
	_ = fs.Parse(args)

	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "nomi import: path required (e.g. nomi import config.yaml)")
		return 2
	}
	path := rest[0]

	cli, err := NewClient(common)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	body, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// Send raw YAML rather than reusing cli.Post (which sets
	// Content-Type: application/json and would mis-label the payload).
	req, _ := http.NewRequest(http.MethodPost, cli.URL+"/config/import",
		newBufferReader(body))
	req.Header.Set("Authorization", "Bearer "+cli.Token)
	req.Header.Set("Content-Type", "application/x-yaml")
	resp, err := cli.HTTP.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "import: HTTP %d: %s\n", resp.StatusCode, respBody)
		return 1
	}
	if common.JSON {
		_, _ = os.Stdout.Write(respBody)
		return 0
	}
	fmt.Fprintf(os.Stderr, "✓ imported %s\n", path)
	_, _ = os.Stdout.Write(respBody)
	return 0
}
