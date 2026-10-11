package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
)

// TestOverlayValues checks the value-overlay merge: a supplied value replaces the
// baked default, a null (or absent) value keeps the default, and whole-number
// floats survive as json.Number so type coercion downstream sees the original.
func TestOverlayValues(t *testing.T) {
	payload := map[string]any{
		"a": json.Number("1"),
		"b": json.Number("2"),
		"c": json.Number("3"),
	}

	overlayValues(payload, map[string]any{
		"a": json.Number("5"),
		"b": nil,
		"d": json.Number("21.0"),
	})

	if got := fmt.Sprint(payload["a"]); got != "5" {
		t.Errorf("a = %s, want 5 (overridden)", got)
	}
	if got := fmt.Sprint(payload["b"]); got != "2" {
		t.Errorf("b = %s, want 2 (null override keeps the baked default)", got)
	}
	if got := fmt.Sprint(payload["c"]); got != "3" {
		t.Errorf("c = %s, want 3 (untouched)", got)
	}
	if _, ok := payload["d"].(json.Number); !ok {
		t.Errorf("d = %v (%T), want a json.Number (whole-number float not collapsed)", payload["d"], payload["d"])
	}
}

// entryTable builds a type table + params covering a scalar file, a file array,
// a file map and a struct with a file field — the shapes file-typed entry
// inputs take.
func entryTable() (*types.Table, []ir.Param) {
	tbl := types.NewTable(map[string]*ir.StructType{
		"Cfg": {Name: "Cfg", Fields: []ir.Param{
			{Name: "ref", BaseType: "file", IsFile: true},
			{Name: "n", BaseType: "int"},
		}},
	})
	params := []ir.Param{
		{Name: "reads", BaseType: "file", IsFile: true},
		{Name: "fastqs", BaseType: "file", ArrayDim: 1, IsFile: true},
		{Name: "named", BaseType: "file", MapDim: 1, IsFile: true},
		{Name: "cfg", BaseType: "Cfg", IsFile: true},
		{Name: "scale", BaseType: "float"},
	}

	return tbl, params
}

// TestReconstructFiles checks the unified file reconstruction: for each supplied
// file-bearing input, the override's file leaves are replaced with the staged
// paths in canonical order (scalar, array in order, struct field), the key is
// removed from over (so the raw value is not re-copied), and an unset input keeps
// its baked default.
func TestReconstructFiles(t *testing.T) {
	tbl, params := entryTable()

	payload := map[string]any{
		"reads":  "baked/reads.txt",
		"fastqs": []any{"baked/x"},
		"cfg":    map[string]any{"ref": "baked/ref", "n": json.Number("9")},
		"scale":  json.Number("2"),
	}
	over := map[string]any{
		"reads":  "s3://b/reads.fastq",
		"fastqs": []any{"s3://b/a.fastq", "s3://b/b.fastq"},
		"cfg":    nil, // unset → keep baked
		"scale":  json.Number("5"),
	}
	flatMap := map[string][]string{
		"reads":  {"/work/reads.fastq"},
		"fastqs": {"/work/a.fastq", "/work/b.fastq"},
		"cfg":    {},
	}

	if err := reconstructFiles(payload, over, flatMap, params, tbl); err != nil {
		t.Fatalf("reconstructFiles: %v", err)
	}

	if got := payload["reads"]; got != "/work/reads.fastq" {
		t.Errorf("reads = %v, want the staged path", got)
	}
	if got := payload["fastqs"]; !reflect.DeepEqual(got, []any{"/work/a.fastq", "/work/b.fastq"}) {
		t.Errorf("fastqs = %v, want the two staged paths in order", got)
	}
	cfg, _ := payload["cfg"].(map[string]any)
	if got := cfg["ref"]; got != "baked/ref" {
		t.Errorf("cfg.ref = %v, want the baked default (cfg was unset)", got)
	}

	// reconstructed keys are removed from over; non-file values remain.
	for _, k := range []string{"reads", "fastqs"} {
		if _, ok := over[k]; ok {
			t.Errorf("over[%q] should have been consumed", k)
		}
	}
	if _, ok := over["scale"]; !ok {
		t.Error("over[scale] (a non-file value) should remain for overlayValues")
	}
}

// TestReconstructFilesStruct checks a struct file field is replaced while sibling
// scalar fields are preserved.
func TestReconstructFilesStruct(t *testing.T) {
	tbl, params := entryTable()

	payload := map[string]any{}
	over := map[string]any{"cfg": map[string]any{"ref": "s3://b/ref.csv", "n": json.Number("3")}}
	flatMap := map[string][]string{"cfg": {"/work/ref.csv"}}

	if err := reconstructFiles(payload, over, flatMap, params, tbl); err != nil {
		t.Fatalf("reconstructFiles: %v", err)
	}

	cfg, ok := payload["cfg"].(map[string]any)
	if !ok {
		t.Fatalf("cfg = %v, want a map", payload["cfg"])
	}
	if cfg["ref"] != "/work/ref.csv" {
		t.Errorf("cfg.ref = %v, want the staged path", cfg["ref"])
	}
	if fmt.Sprint(cfg["n"]) != "3" {
		t.Errorf("cfg.n = %v, want 3 (non-file field preserved)", cfg["n"])
	}
}

// TestReconstructFilesStagedCountMismatch checks the staged-count guards: fewer
// staged paths than file leaves fails (errStagedFew) and more staged paths than
// leaves fails (errStagedMany) — both signal the override value and the staged
// paths disagree on shape, which would otherwise mis-bind or drop files silently.
func TestReconstructFilesStagedCountMismatch(t *testing.T) {
	tbl, params := entryTable()

	// A 2-element file array but only one staged path: too few.
	few := reconstructFiles(
		map[string]any{}, map[string]any{"fastqs": []any{"s3://b/a", "s3://b/b"}},
		map[string][]string{"fastqs": {"/work/a"}}, params, tbl)
	if !errors.Is(few, errStagedFew) {
		t.Errorf("too-few staged: err = %v, want errStagedFew", few)
	}

	// A single scalar file leaf but two staged paths: too many.
	many := reconstructFiles(
		map[string]any{}, map[string]any{"reads": "s3://b/r"},
		map[string][]string{"reads": {"/work/r1", "/work/r2"}}, params, tbl)
	if !errors.Is(many, errStagedMany) {
		t.Errorf("too-many staged: err = %v, want errStagedMany", many)
	}
}

// TestReconstructFilesEmptyOverride checks an override with zero file leaves
// (#261) — an empty file[], an empty map<file>, a struct whose file field is
// null — arrives with zero staged files and replaces the baked default, as the
// same literals do under mrp.
func TestReconstructFilesEmptyOverride(t *testing.T) {
	tbl, params := entryTable()

	payload := map[string]any{
		"fastqs": []any{"baked/x"},
		"named":  map[string]any{"a": "baked/a"},
		"cfg":    map[string]any{"ref": "baked/ref", "n": json.Number("9")},
	}
	over := map[string]any{
		"fastqs": []any{},
		"named":  map[string]any{},
		"cfg":    map[string]any{"ref": nil, "n": json.Number("3")},
	}
	flatMap := map[string][]string{"fastqs": {}, "named": {}, "cfg": {}}

	if err := reconstructFiles(payload, over, flatMap, params, tbl); err != nil {
		t.Fatalf("reconstructFiles: %v", err)
	}

	want := map[string]any{
		"fastqs": []any{},
		"named":  map[string]any{},
		"cfg":    map[string]any{"ref": nil, "n": json.Number("3")},
	}
	if diff := cmp.Diff(want, payload); diff != "" {
		t.Errorf("payload mismatch (-want +got):\n%s", diff)
	}
}

// TestReconstructFilesUnsetStaged checks an unset input that nonetheless has
// staged files fails loudly: with no placeholder file staged for an unset
// input, a staged path there means the flatten and the values disagree.
func TestReconstructFilesUnsetStaged(t *testing.T) {
	tbl, params := entryTable()

	err := reconstructFiles(
		map[string]any{"reads": "baked/r"}, map[string]any{"reads": nil},
		map[string][]string{"reads": {"/work/r"}}, params, tbl)
	if !errors.Is(err, errStagedMany) {
		t.Errorf("unset input with a staged file: err = %v, want errStagedMany", err)
	}
}

// TestReconstructFilesUnknownParam checks the errNoEntryType arm: a supplied,
// file-bearing entry input with no matching declared param must fail loudly.
// Skipping it silently would leave overlayValues to re-copy the raw (unstaged)
// value, so a downstream stage would read the pre-staging path.
func TestReconstructFilesUnknownParam(t *testing.T) {
	tbl, params := entryTable()

	err := reconstructFiles(
		map[string]any{}, map[string]any{"ghost": "s3://b/x"},
		map[string][]string{"ghost": {"/work/x"}}, params, tbl)
	if !errors.Is(err, errNoEntryType) {
		t.Errorf("unknown entry input: err = %v, want errNoEntryType", err)
	}
}

// TestReadFlatFile checks the -fileflat JSON decoding, including staged paths
// containing the `,`/`;`/`=` characters that broke the old flat-string
// encoding (all legal in filenames), plus the empty and malformed cases.
func TestReadFlatFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fileflat.json")
	payload := `{"reads":["/w/a,b;c=d.fastq"],"fastqs":["/w/x","/w/y"],"unset":[]}`

	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := readFlatFile(path)
	if err != nil {
		t.Fatalf("readFlatFile: %v", err)
	}
	if !reflect.DeepEqual(got["reads"], []string{"/w/a,b;c=d.fastq"}) {
		t.Errorf("reads = %v, want the separator-laden path intact", got["reads"])
	}
	if !reflect.DeepEqual(got["fastqs"], []string{"/w/x", "/w/y"}) {
		t.Errorf("fastqs = %v, want the two staged paths", got["fastqs"])
	}

	if empty, err := readFlatFile(""); err != nil || len(empty) != 0 {
		t.Errorf("readFlatFile(\"\") = %v, %v; want empty map, nil", empty, err)
	}

	bogus := filepath.Join(t.TempDir(), "bogus.json")
	if err := os.WriteFile(bogus, []byte("not-json"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := readFlatFile(bogus); err == nil {
		t.Error("readFlatFile(non-JSON) should error")
	}
}

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

// TestRunEntryArgsTypesCLIStrings drives entryargs through its values-file ->
// bundle seam with the all-string overrides Nextflow 26 hands over for CLI
// params (--n 5 --f 2.5 --b false), and pins the bytes the stage's _args will
// carry: the declared int/float/bool leaves are JSON numbers and booleans in
// mrp's encoding, while a string input that looks numeric stays a string.
func TestRunEntryArgsTypesCLIStrings(t *testing.T) {
	dir := t.TempDir()
	man := types.Manifest{Callables: map[string]types.Callable{
		"P": {In: []ir.Param{
			{Name: "n", Type: "int", BaseType: "int"},
			{Name: "f", Type: "float", BaseType: "float"},
			{Name: "g", Type: "float", BaseType: "float"},
			{Name: "b", Type: "bool", BaseType: "bool"},
			{Name: "s", Type: "string", BaseType: "string"},
		}},
	}}
	manPath := writeTestManifest(t, filepath.Join(dir, "types.json"), man)

	base := filepath.Join(dir, "base")
	writeTestBundle(t, base, map[string]any{"n": 1, "f": 0.5, "g": 0.5, "b": true, "s": "x"})
	values := writeTestFile(t, filepath.Join(dir, "values.json"),
		`{"n":"5","f":"2.5","g":"1e6","b":"false","s":"7"}`)
	out := filepath.Join(dir, "out")

	if err := run(t.Context(), []string{
		"entryargs", "-base", base, "-values", values,
		"-o", out, "-types", manPath, "-callable", "P", "-role", "in",
	}); err != nil {
		t.Fatalf("run entryargs: %v", err)
	}

	raw, err := shim.ReadBundle(out)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode bundle: %v", err)
	}

	want := map[string]json.RawMessage{
		"n": json.RawMessage(`5`),
		"f": json.RawMessage(`2.5`),
		"g": json.RawMessage(`1e+06`),
		"b": json.RawMessage(`false`),
		"s": json.RawMessage(`"7"`),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("entry args bytes mismatch (-want +got):\n%s", diff)
	}
}
