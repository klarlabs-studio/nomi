// Package buildinfo holds build-time metadata that's injected via -ldflags
// at link time and read by the --version flag, the GET /version endpoint,
// and any other code that needs to identify the running binary.
package buildinfo

// These vars are populated at link time via:
//
//	-ldflags "-X go.klarlabs.de/nomi/internal/buildinfo.Version=...
//	          -X go.klarlabs.de/nomi/internal/buildinfo.Commit=...
//	          -X go.klarlabs.de/nomi/internal/buildinfo.BuildDate=..."
//
// Defaults apply for `go run` / un-flagged `go build` so the binary
// always returns a non-empty answer to --version.
var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

// Info is the JSON shape exposed via GET /version and the Tauri
// `app_version` command. Field names match the wire format used by
// the Tauri side so a single TypeScript type covers both.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
}

// Current returns the current build info.
func Current() Info {
	return Info{Version: Version, Commit: Commit, BuildDate: BuildDate}
}
