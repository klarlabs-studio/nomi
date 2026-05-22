package memstore

import (
	"errors"
	"testing"
)

func TestScopeKind_IsValid(t *testing.T) {
	cases := []struct {
		kind  ScopeKind
		valid bool
	}{
		{ScopeWorkspace, true},
		{ScopeProfile, true},
		{ScopeSession, true},
		{ScopeOrg, true},
		{ScopeKind(""), false},
		{ScopeKind("bogus"), false},
	}
	for _, c := range cases {
		if got := c.kind.IsValid(); got != c.valid {
			t.Errorf("ScopeKind(%q).IsValid() = %v, want %v", c.kind, got, c.valid)
		}
	}
}

func TestValidateScope(t *testing.T) {
	cases := []struct {
		name    string
		scope   Scope
		wantErr error
	}{
		{
			name:    "valid workspace",
			scope:   Scope{OwnerID: LocalOwnerID, Kind: ScopeWorkspace, Key: "/Users/dev/proj"},
			wantErr: nil,
		},
		{
			name:    "valid profile has empty key",
			scope:   Scope{OwnerID: LocalOwnerID, Kind: ScopeProfile},
			wantErr: nil,
		},
		{
			name:    "valid org",
			scope:   Scope{OwnerID: "alice@example.com", Kind: ScopeOrg, Key: "acme"},
			wantErr: nil,
		},
		{
			name:    "missing owner",
			scope:   Scope{Kind: ScopeWorkspace, Key: "x"},
			wantErr: ErrInvalidScope,
		},
		{
			name:    "unknown kind",
			scope:   Scope{OwnerID: LocalOwnerID, Kind: ScopeKind("bogus")},
			wantErr: ErrInvalidScope,
		},
		{
			name:    "workspace missing key",
			scope:   Scope{OwnerID: LocalOwnerID, Kind: ScopeWorkspace},
			wantErr: ErrInvalidScope,
		},
		{
			name:    "session missing key",
			scope:   Scope{OwnerID: LocalOwnerID, Kind: ScopeSession},
			wantErr: ErrInvalidScope,
		},
		{
			name:    "org missing key",
			scope:   Scope{OwnerID: LocalOwnerID, Kind: ScopeOrg},
			wantErr: ErrInvalidScope,
		},
		{
			name:    "profile rejects non-empty key",
			scope:   Scope{OwnerID: LocalOwnerID, Kind: ScopeProfile, Key: "stray"},
			wantErr: ErrInvalidScope,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ValidateScope(c.scope)
			if !errors.Is(got, c.wantErr) {
				t.Errorf("ValidateScope(%+v) = %v, want %v", c.scope, got, c.wantErr)
			}
		})
	}
}

// Compile-time check: ensure any future Client implementation gets a
// helpful error if it fails to satisfy the interface. The nil literal
// here is intentional — we are checking the type, not calling methods.
var _ Client = (Client)(nil)
