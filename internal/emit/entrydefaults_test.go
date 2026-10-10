package emit

import (
	"errors"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/apperror"
	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
)

// TestCheckEntryFileDefaults checks a baked file default is rejected when its
// extension, the only part of its name a stage-in script ever sees, holds a
// character Nextflow leaves unquoted for bash; glob characters elsewhere in
// the path are copied by Go and stay legal.
func TestCheckEntryFileDefaults(t *testing.T) {
	structs := map[string]*ir.StructType{
		"Cfg": {Name: "Cfg", Fields: []ir.Param{{Name: "ref", BaseType: "txt", IsFile: true}}},
	}
	params := []ir.Param{
		{Name: "reads", BaseType: "txt", IsFile: true, ArrayDim: 1},
		{Name: "cfg", BaseType: "Cfg"},
	}

	cases := []struct {
		name    string
		payload map[string]any
		bad     string
	}{
		{"plain", map[string]any{"reads": []any{"/in/a.txt"}, "cfg": map[string]any{"ref": "/in/r.txt"}}, ""},
		{"glob chars in dir and stem", map[string]any{"reads": []any{"/run[1]/s*?{a,b}.txt"}}, ""},
		{"brace in extension", map[string]any{"reads": []any{"/in/a.txt", "/in/b.t{x,y}"}}, "'{'"},
		{"dollar in struct field extension", map[string]any{"cfg": map[string]any{"ref": "/in/r.$x"}}, "'$'"},
		{"tab in extension", map[string]any{"reads": []any{"/in/a.t\tx"}}, `'\t'`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkEntryFileDefaults(tc.payload, params, types.NewTable(structs))

			if tc.bad == "" {
				if err != nil {
					t.Errorf("err = %v, want nil", err)
				}

				return
			}

			if !errors.Is(err, apperror.ErrUnsupported) || !strings.Contains(err.Error(), tc.bad) ||
				!strings.Contains(err.Error(), "Nextflow staging limitation") {
				t.Errorf("err = %v, want an unsupported error naming %s as a Nextflow staging limitation", err, tc.bad)
			}
		})
	}
}
