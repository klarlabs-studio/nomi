package memory

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/felixgeelhaar/nomi/internal/memstore"
)

func TestExportImport_RoundTrip(t *testing.T) {
	src, _, _, srcCleanup := newTestClient(t)
	defer srcCleanup()
	ctx := context.Background()
	scope := memstore.LocalWorkspace()

	for _, body := range []string{"alpha", "beta", "gamma", "delta"} {
		if err := src.Store(ctx, scope, &memstore.Entry{Content: body}); err != nil {
			t.Fatalf("seed Store %q: %v", body, err)
		}
	}

	var buf bytes.Buffer
	n, err := Export(ctx, src, scope, &buf)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if n != 4 {
		t.Errorf("Export count = %d, want 4", n)
	}

	dst, _, _, dstCleanup := newTestClient(t)
	defer dstCleanup()
	imported, err := Import(ctx, dst, &buf)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if imported != n {
		t.Errorf("Import count = %d, want %d", imported, n)
	}

	got, err := dst.Retrieve(ctx, scope, memstore.Query{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Errorf("post-import retrieve = %d, want 4", len(got))
	}
}

func TestImport_RejectsUnknownFormat(t *testing.T) {
	c, _, _, cleanup := newTestClient(t)
	defer cleanup()

	body := `{"format":"not-mnemos","version":1,"scope":{"owner_id":"local","kind":"workspace","key":"default"}}` + "\n"
	_, err := Import(context.Background(), c, strings.NewReader(body))
	if err == nil {
		t.Fatal("expected error on unknown format")
	}
	if !strings.Contains(err.Error(), "unexpected format") {
		t.Errorf("error = %v, want 'unexpected format'", err)
	}
}

func TestImport_RejectsBadVersion(t *testing.T) {
	c, _, _, cleanup := newTestClient(t)
	defer cleanup()

	body := `{"format":"memstore.export","version":99,"scope":{"owner_id":"local","kind":"workspace","key":"default"}}` + "\n"
	_, err := Import(context.Background(), c, strings.NewReader(body))
	if err == nil {
		t.Fatal("expected error on bad version")
	}
	if !strings.Contains(err.Error(), "unsupported export version") {
		t.Errorf("error = %v", err)
	}
}

func TestImport_RejectsInvalidScopeInHeader(t *testing.T) {
	c, _, _, cleanup := newTestClient(t)
	defer cleanup()

	// Missing owner_id; ValidateScope rejects.
	body := `{"format":"memstore.export","version":1,"scope":{"kind":"workspace","key":"default"}}` + "\n"
	_, err := Import(context.Background(), c, strings.NewReader(body))
	if err == nil {
		t.Fatal("expected error on invalid scope")
	}
}

func TestExport_RejectsInvalidScope(t *testing.T) {
	c, _, _, cleanup := newTestClient(t)
	defer cleanup()

	var buf bytes.Buffer
	_, err := Export(context.Background(), c, memstore.Scope{}, &buf)
	if err == nil {
		t.Fatal("expected error on invalid scope")
	}
}
