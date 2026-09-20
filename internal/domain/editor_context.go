package domain

// EditorContext is ephemeral IDE state attached at run create so the
// planner can see open tabs + the active selection without stuffing
// paths into the goal string. Always treated as untrusted data
// (trusted="false") — same contract as workspace_context / plugin_context.
//
// Not persisted on the Run row for v1: planning starts immediately in
// the same process, so an in-memory hand-off is enough.
type EditorContext struct {
	Source           string              `json:"source,omitempty"` // e.g. "vscode"
	WorkspaceFolders []string            `json:"workspace_folders,omitempty"`
	OpenTabs         []string            `json:"open_tabs,omitempty"`
	Active           *EditorActiveFile   `json:"active,omitempty"`
}

// EditorActiveFile is the focused editor buffer metadata + optional selection.
type EditorActiveFile struct {
	Path       string           `json:"path"`
	LanguageID string           `json:"language_id,omitempty"`
	Selection  *EditorSelection `json:"selection,omitempty"`
}

// EditorSelection is a line-bounded snippet from the active editor.
type EditorSelection struct {
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Text      string `json:"text"`
}
