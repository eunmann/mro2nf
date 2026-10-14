package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
	"github.com/martian-lang/martian/martian/syntax"
)

// overrideParams declares one entry input of each type class typeOverrides
// distinguishes: the three parsed scalars, the scalars left alone (string, the
// builtin file and a user filetype), and the non-scalar shapes, among them
// the struct Martian makes of a stage's outputs.
func overrideParams(t *testing.T) (*types.Table, []*syntax.StructMember) {
	t.Helper()

	tbl := compileTypes(t, `
filetype bam;

struct Cfg(
    int   n,
    float x,
)

stage MAKE(
    out float size,
    src py    "make.py",
)
`)
	params := fields(t, "n int", "f float", "b bool", "s string", "reads file", "aln bam",
		"xs int[]", "m map<int>", "fs float[]", "um map", "cfg Cfg", "made MAKE")

	return tbl, params
}

// compileTypes is the walk over the types the .mro declarations src compile
// to.
func compileTypes(t *testing.T, src string) *types.Table {
	t.Helper()

	_, _, ast, err := syntax.ParseSourceBytes([]byte(src), "types.mro", nil, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	return types.NewTable(&ast.TypeTable)
}

// fields declares each "name type" pair, the type as an .mro spells it, as a
// param.
func fields(t *testing.T, decls ...string) []*syntax.StructMember {
	t.Helper()

	out := make([]*syntax.StructMember, len(decls))

	for i, d := range decls {
		name, tname, _ := strings.Cut(d, " ")

		var id syntax.TypeId
		if err := id.UnmarshalText([]byte(tname)); err != nil {
			t.Fatalf("type of %s: %v", d, err)
		}

		out[i] = &syntax.StructMember{Id: name, Tname: id}
	}

	return out
}

// TestTypeOverrides checks each accepted override: a string for an int, float or
// bool input becomes its typed value (int64 exactly at the int64 bounds, a
// float as the number it spells), a string for a string or file input is kept,
// and any other value that is already typed, null, nested or undeclared passes
// through, a number with its text.
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
		{name: "float_exponent", key: "f", in: "1e6", want: json.Number("1e6")},
		{name: "float_whole", key: "f", in: "2.0", want: json.Number("2.0")},
		{name: "float_integer_text", key: "f", in: "5", want: json.Number("5")},
		{name: "float_past_2^53", key: "f", in: "9007199254740993", want: json.Number("9007199254740993")},
		{name: "float_negative_zero", key: "f", in: "-0.0", want: json.Number("-0.0")},
		{name: "float_small_exponent", key: "f", in: "1E-7", want: json.Number("1E-7")},
		{name: "bool_true", key: "b", in: "true", want: true},
		{name: "bool_false", key: "b", in: "false", want: false},
		{name: "string_kept", key: "s", in: "5", want: "5"},
		{name: "file_kept", key: "reads", in: "s3://b/reads.fq", want: "s3://b/reads.fq"},
		{name: "user_filetype_kept", key: "aln", in: "true", want: "true"},
		{name: "typed_int", key: "n", in: json.Number("5"), want: json.Number("5")},
		{name: "typed_float_whole", key: "f", in: json.Number("2.0"), want: json.Number("2.0")},
		{name: "typed_float_out_of_range", key: "f", in: json.Number("1e400"), want: json.Number("1e400")},
		{name: "typed_float_array", key: "fs", in: []any{json.Number("2.0"), json.Number("0.50")}, want: []any{json.Number("2.0"), json.Number("0.50")}},
		{name: "typed_float_struct_field", key: "cfg", in: map[string]any{"n": "5", "x": json.Number("4.0")}, want: map[string]any{"n": "5", "x": json.Number("4.0")}},
		{name: "typed_bool", key: "b", in: false, want: false},
		{name: "typed_array", key: "xs", in: []any{json.Number("1")}, want: []any{json.Number("1")}},
		{name: "typed_map", key: "m", in: map[string]any{"k": json.Number("1")}, want: map[string]any{"k": json.Number("1")}},
		{name: "typed_struct_nested_string", key: "cfg", in: map[string]any{"n": "5"}, want: map[string]any{"n": "5"}},
		{name: "null", key: "n", in: nil, want: nil},
		{name: "undeclared", key: "ghost", in: "5", want: "5"},
	}

	tbl, params := overrideParams(t)

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
		{name: "callable_struct", key: "made", in: `{"size":1}`, want: errOverrideShape},
	}

	tbl, params := overrideParams(t)
	declared := make(map[string]string, len(params))
	for _, p := range params {
		declared[p.Id] = p.Tname.String()
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
