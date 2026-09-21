package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// recipesCmd shares assistant recipes as YAML between machines.
//
//	nomi recipes import path/to/recipe.yaml
//	nomi recipes export <assistant-id> [-o out.yaml]
//	nomi list recipes   (via listCmd)
func recipesCmd(common *commonFlags, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "nomi recipes: subcommand required (import|export)")
		return 2
	}
	switch args[0] {
	case "import":
		return recipesImportCmd(common, args[1:])
	case "export":
		return recipesExportCmd(common, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "nomi recipes: unknown subcommand %q\n", args[0])
		return 2
	}
}

func recipesImportCmd(common *commonFlags, args []string) int {
	fs := flag.NewFlagSet("recipes import", flag.ExitOnError)
	bindCommonFlags(fs, common)
	_ = fs.Parse(args)
	paths := fs.Args()
	if len(paths) != 1 {
		fmt.Fprintln(os.Stderr, "usage: nomi recipes import <recipe.yaml>")
		return 2
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cli, err := NewClient(common)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var resp struct {
		Recipe struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"recipe"`
		SHA256 string `json:"sha256"`
		Source string `json:"source"`
	}
	if err := cli.Post("/recipes/import", map[string]string{"yaml": string(raw)}, &resp); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if common.JSON {
		printJSON(resp)
		return 0
	}
	fmt.Printf("imported %s (%s) v%s sha256:%s\n", resp.Recipe.ID, resp.Recipe.Name, resp.Recipe.Version, shortHash(resp.SHA256))
	return 0
}

func recipesExportCmd(common *commonFlags, args []string) int {
	fs := flag.NewFlagSet("recipes export", flag.ExitOnError)
	bindCommonFlags(fs, common)
	outPath := fs.String("o", "", "write YAML to file instead of stdout")
	_ = fs.Parse(args)
	ids := fs.Args()
	if len(ids) != 1 {
		fmt.Fprintln(os.Stderr, "usage: nomi recipes export <assistant-id> [-o out.yaml]")
		return 2
	}
	cli, err := NewClient(common)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var resp struct {
		Recipe struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"recipe"`
		SHA256 string `json:"sha256"`
		YAML   string `json:"yaml"`
	}
	path := "/recipes/export?assistant_id=" + ids[0]
	if err := cli.Post(path, map[string]string{}, &resp); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if common.JSON {
		printJSON(resp)
		return 0
	}
	if *outPath != "" {
		if err := os.WriteFile(*outPath, []byte(resp.YAML), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("exported %s → %s (sha256:%s)\n", resp.Recipe.ID, *outPath, shortHash(resp.SHA256))
		return 0
	}
	_, _ = fmt.Fprint(os.Stdout, resp.YAML)
	if !strings.HasSuffix(resp.YAML, "\n") {
		_, _ = fmt.Fprintln(os.Stdout)
	}
	return 0
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
