package types_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
)

// TestValidateOutputs pins mrp's stage output validation (core/argument_map.go
// ValidateOutputs over syntax IsValidJson, #274): a declared output must be
// present, and a non-null value must match its declared type — a struct needs
// every member key, an int must be a JSON integer, a typed map of files needs
// filename-legal keys. Null is always valid, extra keys are not errors, and a
// user filetype accepts anything (mrp only alarms).
func TestValidateOutputs(t *testing.T) {
	tbl := types.NewTable(map[string]*ir.StructType{
		"S": {Name: "S", Fields: []ir.Param{{Name: "a", BaseType: "int"}, {Name: "b", BaseType: "int"}}},
		"T": {Name: "T", Fields: []ir.Param{{Name: "s", BaseType: "S", Type: "S"}}},
	})

	cases := []struct {
		name    string
		param   ir.Param
		outs    string
		wantErr string
	}{
		{"struct complete", ir.Param{Name: "s", Type: "S", BaseType: "S"}, `{"s":{"a":1,"b":2,"x":3}}`, ""},
		{"struct missing key", ir.Param{Name: "s", Type: "S", BaseType: "S"}, `{"s":{"a":1}}`, "Expected S output value 's' missing key: b"},
		{"nested struct missing key", ir.Param{Name: "t", Type: "T", BaseType: "T"}, `{"t":{"s":{"b":1}}}`, "key s: missing key: a"},
		{"struct null member", ir.Param{Name: "s", Type: "S", BaseType: "S"}, `{"s":{"a":null,"b":2}}`, ""},
		{"null output", ir.Param{Name: "s", Type: "S", BaseType: "S"}, `{"s":null}`, ""},
		{"missing output", ir.Param{Name: "s", Type: "S", BaseType: "S"}, `{}`, "Missing output value 's'"},
		{"int", ir.Param{Name: "n", Type: "int", BaseType: "int"}, `{"n":-7}`, ""},
		{"whole float for int", ir.Param{Name: "n", Type: "int", BaseType: "int"}, `{"n":2.0}`, "output value 'n'"},
		{"bool for int", ir.Param{Name: "n", Type: "int", BaseType: "int"}, `{"n":true}`, "output value 'n'"},
		{"int for float", ir.Param{Name: "f", Type: "float", BaseType: "float"}, `{"f":3}`, ""},
		{"number for string", ir.Param{Name: "x", Type: "string", BaseType: "string"}, `{"x":3}`, "output value 'x'"},
		{"int array element", ir.Param{Name: "xs", Type: "int[]", BaseType: "int", ArrayDim: 1}, `{"xs":[1,"2"]}`, "element 1"},
		{"array of structs", ir.Param{Name: "ss", Type: "S[]", BaseType: "S", ArrayDim: 1}, `{"ss":[{"a":1,"b":2},{"a":1}]}`, "element 1: missing key: b"},
		{"typed map of structs", ir.Param{Name: "m", Type: "map<S>", BaseType: "S", MapDim: 1}, `{"m":{"k":{"b":1}}}`, "key k: missing key: a"},
		{"map of int arrays", ir.Param{Name: "m", Type: "map<int[]>", BaseType: "int", MapDim: 2}, `{"m":{"k":[1,2]}}`, ""},
		{"file map key", ir.Param{Name: "m", Type: "map<file>", BaseType: "file", MapDim: 1, IsFile: true}, `{"m":{"a/b":"x"}}`, "key a/b"},
		{"non-file map key", ir.Param{Name: "m", Type: "map<int>", BaseType: "int", MapDim: 1}, `{"m":{"a/b":1}}`, ""},
		{"user filetype", ir.Param{Name: "f", Type: "txt", BaseType: "txt", IsFile: true}, `{"f":5}`, ""},
		{"untyped map", ir.Param{Name: "m", Type: "map", BaseType: "map"}, `{"m":[1]}`, "output value 'm'"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec := json.NewDecoder(strings.NewReader(tc.outs))
			dec.UseNumber()

			var outs map[string]any
			if err := dec.Decode(&outs); err != nil {
				t.Fatalf("decode: %v", err)
			}

			err := tbl.ValidateOutputs([]ir.Param{tc.param}, outs)

			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("ValidateOutputs = %v, want valid", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("ValidateOutputs = %v, want an error containing %q", err, tc.wantErr)
			case tc.wantErr != "" && !errors.Is(err, types.ErrInvalidOutputs):
				t.Errorf("ValidateOutputs = %v, want ErrInvalidOutputs", err)
			}
		})
	}
}
