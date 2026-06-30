package emit

import (
	"encoding/json"
	"testing"

	"github.com/eunmann/mro2nf/internal/bind"
	"github.com/eunmann/mro2nf/internal/ir"
)

// TestValueToEntryEmptyComposites guards bug 3: an empty array/object literal
// must survive the bindspec JSON round-trip as an empty composite, not collapse
// to null. Encoding it as a non-nil Array/Object Entry triggers omitempty on
// marshal, so the reloaded all-nil Entry resolves to "null" (bind.go), which
// broke e.g. DETECT_CHEMISTRY's custom_chemistry_defs. Martian round-trips
// empty []/{} as empty composites (core/resolve.go, argument_map.go).
func TestValueToEntryEmptyComposites(t *testing.T) {
	cases := []struct {
		name string
		val  ir.Value
		want string
	}{
		{"empty array", ir.Value{Array: []ir.Value{}}, "[]"},
		{"empty object", ir.Value{Object: map[string]ir.Value{}}, "{}"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := valueToEntry(nil, nil, tc.val)

			// Round-trip the bindspec through JSON exactly as emit -> mre does.
			raw, err := json.Marshal(bind.Spec{"x": entry})
			if err != nil {
				t.Fatalf("marshal spec: %v", err)
			}

			var reloaded bind.Spec
			if err := json.Unmarshal(raw, &reloaded); err != nil {
				t.Fatalf("unmarshal spec: %v", err)
			}

			got, err := bind.Resolve(reloaded, nil, nil)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if string(got) != `{"x":`+tc.want+`}` {
				t.Errorf("resolved args = %s, want x=%s (not null)", got, tc.want)
			}
		})
	}
}
