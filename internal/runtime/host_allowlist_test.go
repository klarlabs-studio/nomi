package runtime

import (
	"reflect"
	"testing"
)

// TestExtractHostAllowlistShapes covers the two JSON-decode shapes
// rule.Constraints can carry, plus the edge cases that must produce a
// nil result (no key, wrong type, empty entries).
func TestExtractHostAllowlistShapes(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]interface{}
		want []string
	}{
		{"nil map", nil, nil},
		{"missing key", map[string]interface{}{"other": "x"}, nil},
		{"wrong type", map[string]interface{}{"host_allowlist": "api.openai.com"}, nil},
		{
			"string slice",
			map[string]interface{}{"host_allowlist": []string{"api.openai.com", "  ", "graph.facebook.com"}},
			[]string{"api.openai.com", "graph.facebook.com"},
		},
		{
			"interface slice",
			map[string]interface{}{"host_allowlist": []interface{}{"api.openai.com", "", "graph.facebook.com"}},
			[]string{"api.openai.com", "graph.facebook.com"},
		},
		{
			"interface slice with whitespace",
			map[string]interface{}{"host_allowlist": []interface{}{"  api.openai.com  "}},
			[]string{"api.openai.com"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractHostAllowlist(c.in)
			if c.want == nil {
				if len(got) != 0 {
					t.Fatalf("expected empty, got %v", got)
				}
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}
