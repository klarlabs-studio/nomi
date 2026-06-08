package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
)

// memoryCmd dispatches `nomi memory <action>`. Two actions ship:
//
//	nomi memory export [--scope=workspace] [--key=...] [-o file.jsonl]
//	nomi memory import file.jsonl
//
// Both go through the daemon's REST surface so the same binary works
// against a local or remote nomid (-url / -token honored). ADR 0004 §8.
func memoryCmd(common *commonFlags, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "nomi memory: action required (export | import)")
		return 2
	}
	action := args[0]
	rest := args[1:]
	switch action {
	case "export":
		return memoryExportCmd(common, rest)
	case "import":
		return memoryImportCmd(common, rest)
	default:
		fmt.Fprintf(os.Stderr, "nomi memory: unknown action %q (export | import)\n", action)
		return 2
	}
}

func memoryExportCmd(common *commonFlags, args []string) int {
	fs := flag.NewFlagSet("memory export", flag.ExitOnError)
	bindCommonFlags(fs, common)
	scope := fs.String("scope", "workspace", "scope kind: workspace | profile | preferences | session | org")
	key := fs.String("key", "", "scope key (defaults to 'default' for workspace; empty for profile/preferences)")
	out := fs.String("o", "", "output file (default: stdout)")
	_ = fs.Parse(args)

	cli, err := NewClient(common)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	q := url.Values{}
	q.Set("scope", *scope)
	if *key != "" {
		q.Set("key", *key)
	}
	req, err := http.NewRequest(http.MethodGet, cli.URL+"/memory/export?"+q.Encode(), nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	req.Header.Set("Authorization", "Bearer "+cli.Token)
	resp, err := cli.HTTP.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory export:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "memory export: HTTP %d: %s\n", resp.StatusCode, string(body))
		return 1
	}

	dst := os.Stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer func() { _ = f.Close() }()
		dst = f
	}
	if _, err := io.Copy(dst, resp.Body); err != nil {
		fmt.Fprintln(os.Stderr, "memory export: stream:", err)
		return 1
	}
	if *out != "" {
		fmt.Fprintf(os.Stderr, "exported scope=%s key=%s to %s\n", *scope, *key, *out)
	}
	return 0
}

func memoryImportCmd(common *commonFlags, args []string) int {
	fs := flag.NewFlagSet("memory import", flag.ExitOnError)
	bindCommonFlags(fs, common)
	_ = fs.Parse(args)

	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "nomi memory import: path required (use '-' for stdin)")
		return 2
	}
	path := rest[0]

	cli, err := NewClient(common)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	var src io.Reader
	if path == "-" {
		src = os.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer func() { _ = f.Close() }()
		src = f
	}

	req, err := http.NewRequest(http.MethodPost, cli.URL+"/memory/import", src)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	req.Header.Set("Authorization", "Bearer "+cli.Token)
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := cli.HTTP.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory import:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "memory import: HTTP %d: %s\n", resp.StatusCode, string(body))
		return 1
	}
	var out struct {
		Imported int `json:"imported"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		fmt.Fprintln(os.Stderr, "memory import: parse response:", err)
		return 1
	}
	fmt.Printf("imported %d entries\n", out.Imported)
	return 0
}
