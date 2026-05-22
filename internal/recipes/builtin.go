package recipes

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed builtin/*.yaml
var builtinFS embed.FS

// BuiltInCatalog parses every YAML file under builtin/ and returns the
// validated recipes sorted by ID. Embed errors and parse errors are
// fatal — a malformed built-in catalog is a packaging bug.
func BuiltInCatalog() ([]*Recipe, error) {
	entries, err := fs.ReadDir(builtinFS, "builtin")
	if err != nil {
		return nil, fmt.Errorf("read builtin catalog: %w", err)
	}
	out := []*Recipe{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		raw, err := builtinFS.ReadFile("builtin/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read builtin recipe %s: %w", e.Name(), err)
		}
		r, err := Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("parse builtin recipe %s: %w", e.Name(), err)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// BuiltInByID returns the catalog entry whose ID matches id, or an error
// if no such recipe exists. Used by the install endpoint to materialise
// a recipe from a stable URL parameter.
func BuiltInByID(id string) (*Recipe, error) {
	all, err := BuiltInCatalog()
	if err != nil {
		return nil, err
	}
	for _, r := range all {
		if r.ID == id {
			return r, nil
		}
	}
	return nil, fmt.Errorf("recipe %q not found in built-in catalog", id)
}
