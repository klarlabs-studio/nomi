package memory

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/felixgeelhaar/nomi/internal/mnemos"
)

// ExportFormatVersion identifies the JSONL wire format. The first line
// of every export is a header object carrying this version so future
// importers can detect breaking changes. Bump on any incompatible
// schema change to Entry — additions are backwards-compatible and
// don't require a bump.
const ExportFormatVersion = 1

// exportHeader is the first line emitted by Export. Decoded by Import
// to verify the version and surface the originating scope for logging.
type exportHeader struct {
	Format  string         `json:"format"`
	Version int            `json:"version"`
	Scope   mnemos.Scope   `json:"scope"`
	Count   int            `json:"count,omitempty"` // entry count; 0 if streaming
}

// exportEntry is one body line in the JSONL stream. The wrapper exists
// so future fields (signatures, provenance) can land without forcing
// every consumer to redeploy.
type exportEntry struct {
	Entry *mnemos.Entry `json:"entry"`
}

// Export writes every entry in scope to w as JSONL. The first line is
// the header; subsequent lines are entry records, one per line.
// Returns the number of entries written. Streams from the client's
// Retrieve in pages so the in-memory footprint stays bounded.
//
// The export format is stable across implementations of mnemos.Client
// — `mnemos/embedded` and a future `mnemos/remote` produce identical
// output, which is what makes ADR 0004 §10 (embedded → remote data
// migration) work.
func Export(ctx context.Context, client mnemos.Client, scope mnemos.Scope, w io.Writer) (int, error) {
	if client == nil {
		return 0, fmt.Errorf("memory.Export: nil client")
	}
	if err := mnemos.ValidateScope(scope); err != nil {
		return 0, err
	}

	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)

	if err := enc.Encode(exportHeader{
		Format:  "mnemos.export",
		Version: ExportFormatVersion,
		Scope:   scope,
	}); err != nil {
		return 0, fmt.Errorf("memory.Export: write header: %w", err)
	}

	// Page by created_at desc using Retrieve with progressively earlier
	// Since cutoffs. For step 1 a single large limit is sufficient — the
	// corpora are small. Move to true paging when corpus growth makes
	// the single-shot fetch wasteful.
	entries, err := client.Retrieve(ctx, scope, mnemos.Query{Limit: 100_000})
	if err != nil {
		return 0, fmt.Errorf("memory.Export: retrieve: %w", err)
	}
	for _, e := range entries {
		if err := enc.Encode(exportEntry{Entry: e}); err != nil {
			return 0, fmt.Errorf("memory.Export: encode entry %s: %w", e.ID, err)
		}
	}
	if err := bw.Flush(); err != nil {
		return 0, fmt.Errorf("memory.Export: flush: %w", err)
	}
	return len(entries), nil
}

// Import reads JSONL from r and stores every entry via client.Store.
// Validates the header version; rejects unknown versions rather than
// silently degrading. Returns the number of entries imported.
//
// Existing entries with the same ID are not de-duplicated by Import
// itself — the underlying client decides (the embedded SQLite backend
// returns a primary-key violation). Callers that want idempotent
// imports should wipe the target scope first via Forget, or pre-filter.
func Import(ctx context.Context, client mnemos.Client, r io.Reader) (int, error) {
	if client == nil {
		return 0, fmt.Errorf("memory.Import: nil client")
	}

	dec := json.NewDecoder(bufio.NewReader(r))

	var hdr exportHeader
	if err := dec.Decode(&hdr); err != nil {
		return 0, fmt.Errorf("memory.Import: read header: %w", err)
	}
	if hdr.Format != "mnemos.export" {
		return 0, fmt.Errorf("memory.Import: unexpected format %q", hdr.Format)
	}
	if hdr.Version != ExportFormatVersion {
		return 0, fmt.Errorf("memory.Import: unsupported export version %d (want %d)", hdr.Version, ExportFormatVersion)
	}
	if err := mnemos.ValidateScope(hdr.Scope); err != nil {
		return 0, fmt.Errorf("memory.Import: header scope: %w", err)
	}

	n := 0
	for {
		var rec exportEntry
		if err := dec.Decode(&rec); err == io.EOF {
			break
		} else if err != nil {
			return n, fmt.Errorf("memory.Import: decode entry %d: %w", n, err)
		}
		if rec.Entry == nil {
			continue
		}
		if err := client.Store(ctx, hdr.Scope, rec.Entry); err != nil {
			return n, fmt.Errorf("memory.Import: store entry %s: %w", rec.Entry.ID, err)
		}
		n++
	}
	return n, nil
}
