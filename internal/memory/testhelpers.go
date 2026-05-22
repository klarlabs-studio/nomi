package memory

import (
	"testing"

	"github.com/felixgeelhaar/mnemos/embedded"
)

// NewTestClient opens an in-memory embedded.Client for tests and
// registers cleanup. Bundled here so the dozen-plus runtime tests that
// previously constructed *memory.EmbeddedClient have a one-line
// replacement that doesn't depend on a temp file or migration.
//
// Lives in non-test source so packages outside internal/memory (e.g.
// internal/runtime) can use it; gated by *testing.T so it's safe to
// keep out of production builds.
func NewTestClient(t *testing.T) *embedded.Client {
	t.Helper()
	c, err := embedded.Open(":memory:")
	if err != nil {
		t.Fatalf("memory.NewTestClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
