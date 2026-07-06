package emit_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/apperror"
	"github.com/eunmann/mro2nf/internal/emit"
	"github.com/eunmann/mro2nf/internal/frontend"
	"github.com/eunmann/mro2nf/internal/ir"
)

// TestParseTargetInvalid checks an unknown -target fails as ErrUnsupported.
func TestParseTargetInvalid(t *testing.T) {
	if _, err := emit.ParseTarget("bogus"); !errors.Is(err, apperror.ErrUnsupported) {
		t.Errorf("ParseTarget(bogus): want ErrUnsupported, got %v", err)
	}

	if tgt, err := emit.ParseTarget(""); err != nil || tgt != emit.TargetLocal {
		t.Errorf("ParseTarget(\"\") = (%v, %v), want (local, nil)", tgt, err)
	}
}

// TestEmitNoEntry checks a program without a top-level call is rejected before
// any output is written.
func TestEmitNoEntry(t *testing.T) {
	dir := t.TempDir()

	err := emit.Emit(&ir.Program{}, emit.Options{OutDir: dir})
	if err == nil || !strings.Contains(err.Error(), "no entry call") {
		t.Fatalf("Emit without entry: want 'no entry call' error, got %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(dir, "main.nf")); statErr == nil {
		t.Error("Emit wrote main.nf despite rejecting the program")
	}
}

// TestEmitContainerMissingSources checks the container-target fail-loud
// contract: a missing -mre (and a nonexistent path) fail the transpile instead
// of surfacing later as a cryptic docker-build COPY error.
func TestEmitContainerMissingSources(t *testing.T) {
	ast, err := frontend.Parse("../../testdata/split_test/pipeline.mro", []string{"../../testdata/split_test"}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	prog, err := frontend.Lower(ast)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	opts := emit.Options{
		OutDir: t.TempDir(), Target: emit.TargetAWSBatch, MROFile: "pipeline.mro",
		StageCode: map[string]string{"SUM_SQUARES": t.TempDir(), "REPORT": t.TempDir()},
	}

	if err := emit.Emit(prog, opts); err == nil || !strings.Contains(err.Error(), "-mre") {
		t.Errorf("container target without -mre: want error naming -mre, got %v", err)
	}

	opts.Mre = filepath.Join(t.TempDir(), "does-not-exist")
	if err := emit.Emit(prog, opts); err == nil || !strings.Contains(err.Error(), opts.Mre) {
		t.Errorf("container target with missing mre file: want error naming the path, got %v", err)
	}

	opts.Mre = filepath.Join(t.TempDir(), "mre")
	if err := os.WriteFile(opts.Mre, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	opts.Mrjob = filepath.Join(t.TempDir(), "does-not-exist")
	if err := emit.Emit(prog, opts); err == nil || !strings.Contains(err.Error(), opts.Mrjob) {
		t.Errorf("container target with missing mrjob file: want error naming the path, got %v", err)
	}
}

// TestWarningsPreflightUngateable checks every preflight that cannot act as
// mrp's early gate warns and names its actual trigger — a mapped or disabled
// preflight runs in DAG order too, not only one bound to a call output — and
// that a plain input-bound preflight (the gateable case) warns nothing.
func TestWarningsPreflightUngateable(t *testing.T) {
	selfBind := []ir.Binding{{Param: "x", Value: ir.Value{Ref: &ir.Ref{Kind: "self", ID: "n"}}}}
	callBind := []ir.Binding{{Param: "x", Value: ir.Value{Ref: &ir.Ref{Kind: "call", ID: "A", Output: "y"}}}}

	cases := []struct {
		name string
		call ir.Call
		want string // The named trigger; "" = no preflight warning at all.
	}{
		{
			"call-output bound",
			ir.Call{Name: "CHECK", Callable: "S", Preflight: true, Bindings: callBind},
			"is bound to a call output",
		},
		{
			"mapped",
			ir.Call{Name: "CHECK", Callable: "S", Preflight: true, Mapped: true, Bindings: selfBind},
			"is a map call",
		},
		{
			"disabled",
			ir.Call{Name: "CHECK", Callable: "S", Preflight: true, Disabled: &ir.Ref{Kind: "self", ID: "skip"}, Bindings: selfBind},
			"carries a `disabled` gate",
		},
		{
			"gateable input-bound",
			ir.Call{Name: "CHECK", Callable: "S", Preflight: true, Bindings: selfBind},
			"",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prog := &ir.Program{
				Stages: map[string]*ir.Stage{"S": {Name: "S", Out: []ir.Param{{Name: "y", BaseType: "int"}}}},
				Pipelines: map[string]*ir.Pipeline{
					"T": {Name: "T", Calls: []ir.Call{{Name: "A", Callable: "S"}, tc.call}},
				},
			}

			var got []string

			for _, w := range emit.Warnings(prog) {
				if strings.Contains(w, "T.CHECK") && strings.Contains(w, "preflight") {
					got = append(got, w)
				}
			}

			switch {
			case tc.want == "" && len(got) > 0:
				t.Errorf("gateable preflight: want no warning, got %v", got)
			case tc.want != "" && (len(got) != 1 ||
				!strings.Contains(got[0], tc.want) || !strings.Contains(got[0], "runs in DAG order")):
				t.Errorf("want one warning naming %q with the DAG-order consequence, got %v", tc.want, got)
			}
		})
	}
}

// TestEmitHealthOmicsTemplateEntries checks every entry input is declared in
// parameter-template.json (undeclared parameters are rejected by HealthOmics,
// which would silently disable the override path): by default with the S3
// wording for file-bearing inputs, and under -native — where the values are
// baked and the workflow rejects a supplied entry parameter (#116) — with a
// description stating the bake instead of inviting an override.
func TestEmitHealthOmicsTemplateEntries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		native   bool
		wantDesc string
	}{
		{name: "default_invites_override", native: false, wantDesc: "S3 URI"},
		{name: "native_states_bake", native: true, wantDesc: "cannot be overridden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := emitHealthOmicsEntryFile(t, tc.native)

			data, err := os.ReadFile(filepath.Join(dir, "parameter-template.json"))
			if err != nil {
				t.Fatal(err)
			}

			var tmpl map[string]struct {
				Description string `json:"description"`
				Optional    bool   `json:"optional"`
			}

			if err := json.Unmarshal(data, &tmpl); err != nil {
				t.Fatalf("parse template: %v", err)
			}

			reads, ok := tmpl["reads"]
			if !ok || !reads.Optional || !strings.Contains(reads.Description, tc.wantDesc) {
				t.Errorf("file input 'reads' entry = %+v (present=%v), want optional with %q in the description", reads, ok, tc.wantDesc)
			}

			if _, ok := tmpl["container"]; !ok {
				t.Error("template missing the required 'container' parameter")
			}

			assertEntryBake(t, dir, tc.native)
		})
	}
}

// emitHealthOmicsEntryFile emits the entry_file fixture for the HealthOmics
// target (with the entryargs stub standing in for mre) and returns the project dir.
func emitHealthOmicsEntryFile(t *testing.T, native bool) string {
	t.Helper()

	base := "../../testdata/entry_file"

	ast, err := frontend.Parse(base+"/pipeline.mro", []string{base}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	prog, err := frontend.Lower(ast)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	code := map[string]string{}
	for name := range prog.Stages {
		code[name] = t.TempDir()
	}

	dir := t.TempDir()
	if err := emit.Emit(prog, emit.Options{
		OutDir: dir, Mre: writeEntryArgsStub(t), MROFile: "pipeline.mro",
		Target: emit.TargetHealthOmics, StageCode: code, Native: native,
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	return dir
}

// assertEntryBake checks -native baked the resolved entry bundle into the
// project (bakeEntryArgs) — the native workflow stages entry_resolved/ directly
// and has no BUILD_ENTRY_ARGS task to build it at run time — and that a
// non-native emit wrote no such bundle.
func assertEntryBake(t *testing.T, dir string, native bool) {
	t.Helper()

	resolved := filepath.Join(dir, "entry_resolved", "data.json")
	if !native {
		if _, err := os.Stat(resolved); err == nil {
			t.Error("non-native emit wrote entry_resolved/ (the bake is native-only)")
		}

		return
	}

	baked, err := os.ReadFile(resolved)
	if err != nil {
		t.Fatalf("-native did not bake entry_resolved/data.json: %v", err)
	}

	if !strings.Contains(string(baked), "reads") {
		t.Errorf("baked entry args missing the 'reads' input:\n%s", baked)
	}
}

// writeEntryArgsStub writes a runnable stand-in for mre: -native execs
// `mre entryargs -base ... -o ...` to bake the entry args (bakeEntryArgs), and
// with no run-time overrides the resolved bundle equals the baked defaults, so
// the stub copies the -base bundle to -o — the same data.json + f/ bundle shape
// the real command writes and the native workflow stages. The container target
// also copies the stub file into the Docker build context.
func writeEntryArgsStub(t *testing.T) string {
	t.Helper()

	const stub = `#!/bin/sh
set -eu
base=""; out=""
while [ "$#" -gt 0 ]; do
	case "$1" in
	-base) base="$2"; shift 2 ;;
	-o) out="$2"; shift 2 ;;
	*) shift ;;
	esac
done
[ -d "$base" ] && [ -n "$out" ] || exit 9
mkdir -p "$out"
cp -R "$base/." "$out/"
`

	mre := filepath.Join(t.TempDir(), "mre")
	if err := os.WriteFile(mre, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	return mre
}

// TestBindSpecSplitFlag checks a map call's split binding round-trips into its
// written bindspec with "split": true — the flag forkbind forks on.
func TestBindSpecSplitFlag(t *testing.T) {
	dir := emitFixture(t, "fork_min", map[string]string{"SCALE": "/x/scale"})

	specs, err := filepath.Glob(filepath.Join(dir, "_assets", "bindspecs", "BIND_*SCALE*.json"))
	if err != nil || len(specs) == 0 {
		t.Fatalf("no SCALE bindspec found: %v", err)
	}

	data, err := os.ReadFile(specs[0])
	if err != nil {
		t.Fatal(err)
	}

	var spec map[string]struct {
		Split bool `json:"split"`
	}

	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	split := false
	for _, e := range spec {
		split = split || e.Split
	}

	if !split {
		t.Errorf("map-call bindspec has no split:true entry:\n%s", data)
	}
}

// TestEmitRejectsCompStageWithoutMrjob checks a pipeline with a comp-adapter
// stage is rejected up front when -mrjob is absent — the comp stages cannot run
// without the wrapper, so emitting would produce a broken project rather than a
// clear error.
func TestEmitRejectsCompStageWithoutMrjob(t *testing.T) {
	ast, err := frontend.Parse("../../testdata/comp_split/pipeline.mro", []string{"../../testdata/comp_split"}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	prog, err := frontend.Lower(ast)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	code := map[string]string{}
	for n := range prog.Stages {
		code[n] = filepath.Join("/x", n)
	}

	dir := t.TempDir()
	// Mrjob deliberately unset.
	err = emit.Emit(prog, emit.Options{OutDir: dir, Mre: "mre", Shell: "/x/s", MROFile: "pipeline.mro", StageCode: code})
	if !errors.Is(err, apperror.ErrUnsupported) || !strings.Contains(err.Error(), "comp stage") {
		t.Fatalf("Emit(comp stage, no -mrjob): want ErrUnsupported naming a comp stage, got %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(dir, "main.nf")); statErr == nil {
		t.Error("Emit wrote main.nf despite rejecting the comp pipeline")
	}
}

// TestParseRejectsMalformedMRO checks the frontend surfaces a parse error for
// syntactically invalid MRO instead of silently yielding an empty program.
func TestParseRejectsMalformedMRO(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "pipeline.mro")

	if err := os.WriteFile(bad, []byte("stage GEN( this is not valid mro )\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := frontend.Parse(bad, []string{dir}, false); err == nil {
		t.Error("Parse(malformed .mro): want a parse error, got nil")
	}
}

// TestEmitRejectsReservedEntryName is the Emit-level guard for #171: a program
// whose entry input collides with a reserved Nextflow param must be rejected
// before any output is written, so the wiring (not just the helper) is covered
// and a reordering that dropped the check would fail here.
func TestEmitRejectsReservedEntryName(t *testing.T) {
	prog := &ir.Program{
		Entry: &ir.EntryCall{Callable: "TOP"},
		Pipelines: map[string]*ir.Pipeline{
			"TOP": {Name: "TOP", In: []ir.Param{{Name: "outdir", BaseType: "int"}}},
		},
	}

	dir := t.TempDir()
	err := emit.Emit(prog, emit.Options{OutDir: dir, Mre: "mre", Shell: "/x/s", MROFile: "pipeline.mro"})
	if !errors.Is(err, apperror.ErrUnsupported) || !strings.Contains(err.Error(), "outdir") {
		t.Fatalf("Emit(entry input 'outdir'): want ErrUnsupported naming outdir, got %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(dir, "main.nf")); statErr == nil {
		t.Error("Emit wrote main.nf despite rejecting the reserved entry-input name")
	}
}
