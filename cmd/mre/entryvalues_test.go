package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
)

// overrideParams declares one entry input of each type class typeOverrides
// distinguishes: the three parsed scalars, the scalars left alone (string, the
// builtin file and a user filetype), and the non-scalar shapes.
func overrideParams() (*types.Table, []ir.Param) {
	tbl := types.NewTable(map[string]*ir.StructType{
		"Cfg": {Name: "Cfg", Fields: []ir.Param{
			{Name: "n", Type: "int", BaseType: "int"},
			{Name: "x", Type: "float", BaseType: "float"},
		}},
	})
	params := []ir.Param{
		{Name: "n", Type: "int", BaseType: "int"},
		{Name: "f", Type: "float", BaseType: "float"},
		{Name: "b", Type: "bool", BaseType: "bool"},
		{Name: "s", Type: "string", BaseType: "string"},
		{Name: "reads", Type: "file", BaseType: "file", IsFile: true},
		{Name: "aln", Type: "bam", BaseType: "bam", IsFile: true},
		{Name: "xs", Type: "int[]", BaseType: "int", ArrayDim: 1},
		{Name: "m", Type: "map<int>", BaseType: "int", MapDim: 1},
		{Name: "fs", Type: "float[]", BaseType: "float", ArrayDim: 1},
		{Name: "fm", Type: "map<float[]>", BaseType: "float", MapDim: 2},
		{Name: "um", Type: "map", BaseType: "map"},
		{Name: "cfg", Type: "Cfg", BaseType: "Cfg"},
	}

	return tbl, params
}

// TestTypeOverrides checks each accepted override: a string for an int, float or
// bool input becomes its typed value (int64 exactly at the int64 bounds), a
// string for a string or file input is kept, every float leaf, string-parsed or
// typed and at any depth, takes mrp's 'g' encoding, and any other value that is
// already typed, null, nested or undeclared passes through.
func TestTypeOverrides(t *testing.T) {
	tests := []struct {
		name string
		key  string
		in   any
		want any
	}{
		{name: "int", key: "n", in: "5", want: int64(5)},
		{name: "int_negative", key: "n", in: "-3", want: int64(-3)},
		{name: "int_leading_zeros", key: "n", in: "007", want: int64(7)},
		{name: "int_zero", key: "n", in: "0", want: int64(0)},
		{name: "int_max", key: "n", in: "9223372036854775807", want: int64(9223372036854775807)},
		{name: "int_min", key: "n", in: "-9223372036854775808", want: int64(-9223372036854775808)},
		{name: "int_past_2^53", key: "n", in: "9007199254740993", want: int64(9007199254740993)},
		{name: "float", key: "f", in: "2.5", want: json.Number("2.5")},
		{name: "float_exponent", key: "f", in: "1e6", want: json.Number("1e+06")},
		{name: "float_whole", key: "f", in: "2.0", want: json.Number("2")},
		{name: "float_integer_text", key: "f", in: "5", want: json.Number("5")},
		{name: "float_negative_fraction", key: "f", in: "-0.125", want: json.Number("-0.125")},
		{name: "float_small_exponent", key: "f", in: "1E-7", want: json.Number("1e-07")},
		{name: "float_shortest_roundtrip", key: "f", in: "0.1", want: json.Number("0.1")},
		{name: "bool_true", key: "b", in: "true", want: true},
		{name: "bool_false", key: "b", in: "false", want: false},
		{name: "string_kept", key: "s", in: "5", want: "5"},
		{name: "file_kept", key: "reads", in: "s3://b/reads.fq", want: "s3://b/reads.fq"},
		{name: "user_filetype_kept", key: "aln", in: "true", want: "true"},
		{name: "typed_int", key: "n", in: json.Number("5"), want: json.Number("5")},
		{name: "typed_float_whole", key: "f", in: json.Number("2.0"), want: json.Number("2")},
		{name: "typed_float_exponent", key: "f", in: json.Number("1E6"), want: json.Number("1e+06")},
		{name: "typed_float_fraction", key: "f", in: json.Number("2.5"), want: json.Number("2.5")},
		{name: "typed_float_out_of_range", key: "f", in: json.Number("1e400"), want: json.Number("1e400")},
		{name: "typed_float_array", key: "fs", in: []any{json.Number("2.0"), json.Number("0.50")}, want: []any{json.Number("2"), json.Number("0.5")}},
		{name: "typed_float_map_of_array", key: "fm", in: map[string]any{"k": []any{json.Number("3.0")}}, want: map[string]any{"k": []any{json.Number("3")}}},
		{name: "typed_float_struct_field", key: "cfg", in: map[string]any{"n": "5", "x": json.Number("4.0")}, want: map[string]any{"n": "5", "x": json.Number("4")}},
		{name: "typed_bool", key: "b", in: false, want: false},
		{name: "typed_array", key: "xs", in: []any{json.Number("1")}, want: []any{json.Number("1")}},
		{name: "typed_map", key: "m", in: map[string]any{"k": json.Number("1")}, want: map[string]any{"k": json.Number("1")}},
		{name: "typed_struct_nested_string", key: "cfg", in: map[string]any{"n": "5"}, want: map[string]any{"n": "5"}},
		{name: "null", key: "n", in: nil, want: nil},
		{name: "undeclared", key: "ghost", in: "5", want: "5"},
	}

	tbl, params := overrideParams()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			over := map[string]any{tc.key: tc.in}

			if err := typeOverrides(over, params, tbl); err != nil {
				t.Fatalf("typeOverrides: %v", err)
			}

			if diff := cmp.Diff(map[string]any{tc.key: tc.want}, over); diff != "" {
				t.Errorf("override mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestTypeOverridesErrors checks each rejected override fails loudly with an
// error naming the input, its declared type and the value, and pointing at
// -params-file: an invalid scalar literal (errOverrideParse) and any string for
// an array, map or struct input (errOverrideShape), which is never JSON-parsed.
func TestTypeOverridesErrors(t *testing.T) {
	tests := []struct {
		name string
		key  string
		in   string
		want error
	}{
		{name: "int_word", key: "n", in: "abc", want: errOverrideParse},
		{name: "int_empty", key: "n", in: "", want: errOverrideParse},
		{name: "int_fraction", key: "n", in: "5.0", want: errOverrideParse},
		{name: "int_exponent", key: "n", in: "1e3", want: errOverrideParse},
		{name: "int_plus", key: "n", in: "+5", want: errOverrideParse},
		{name: "int_space", key: "n", in: " 5", want: errOverrideParse},
		{name: "int_hex", key: "n", in: "0x10", want: errOverrideParse},
		{name: "int_underscore", key: "n", in: "1_000", want: errOverrideParse},
		{name: "int_over_max", key: "n", in: "9223372036854775808", want: errOverrideParse},
		{name: "int_under_min", key: "n", in: "-9223372036854775809", want: errOverrideParse},
		{name: "float_word", key: "f", in: "abc", want: errOverrideParse},
		{name: "float_empty", key: "f", in: "", want: errOverrideParse},
		{name: "float_nan", key: "f", in: "NaN", want: errOverrideParse},
		{name: "float_inf", key: "f", in: "Inf", want: errOverrideParse},
		{name: "float_hex", key: "f", in: "0x1p-2", want: errOverrideParse},
		{name: "float_bare_point", key: "f", in: ".5", want: errOverrideParse},
		{name: "float_trailing_point", key: "f", in: "5.", want: errOverrideParse},
		{name: "float_leading_zero", key: "f", in: "007.5", want: errOverrideParse},
		{name: "float_overflow", key: "f", in: "1e400", want: errOverrideParse},
		{name: "bool_title_case", key: "b", in: "True", want: errOverrideParse},
		{name: "bool_upper_case", key: "b", in: "FALSE", want: errOverrideParse},
		{name: "bool_one", key: "b", in: "1", want: errOverrideParse},
		{name: "bool_yes", key: "b", in: "yes", want: errOverrideParse},
		{name: "bool_empty", key: "b", in: "", want: errOverrideParse},
		{name: "array", key: "xs", in: "[1,2]", want: errOverrideShape},
		{name: "typed_map", key: "m", in: `{"k":1}`, want: errOverrideShape},
		{name: "untyped_map", key: "um", in: "{}", want: errOverrideShape},
		{name: "struct", key: "cfg", in: `{"n":5}`, want: errOverrideShape},
	}

	tbl, params := overrideParams()
	declared := make(map[string]string, len(params))
	for _, p := range params {
		declared[p.Name] = p.Type
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := typeOverrides(map[string]any{tc.key: tc.in}, params, tbl)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}

			msg := err.Error()
			wantParts := []string{fmt.Sprintf("%q", tc.key), "declared " + declared[tc.key], fmt.Sprintf("%q", tc.in), "-params-file"}
			for _, part := range wantParts {
				if !strings.Contains(msg, part) {
					t.Errorf("error %q does not mention %s", msg, part)
				}
			}
		})
	}
}
