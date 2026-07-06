package emit_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/emit"
	"github.com/eunmann/mro2nf/internal/frontend"
	"github.com/eunmann/mro2nf/internal/ir"
)

// TestStageProcessNameInventory guards the naming contract behind the
// overrides converter's bounded withName selectors (#112): every process name
// (and every process include-alias) the emitter generates must decompose into
// a known data-plane helper name or a family base plus a suffix from the
// exported inventory (PlainStageSuffixes / FusedCallSuffixes /
// ScatterCallSuffixes). A new emitter suffix fails here, forcing the shared
// inventory — and thus the override selectors built from it — to be extended
// in lockstep. The fixture set exercises every inventoried suffix, so a stale
// inventory entry fails the coverage check at the end too.
func TestStageProcessNameInventory(t *testing.T) {
	fixtures := []struct {
		name string
		mode string       // labels the emit mode in failure messages
		opts emit.Options // only the mode flags; emittedProcessNames fills the rest
	}{
		{"split_test", "default", emit.Options{}},                           // plain + fused split triads, fused non-split (#16)
		{"stage_entry", "default", emit.Options{}},                          // bare stage process
		{"fork_min", "default", emit.Options{}},                             // keyed non-split _MAP
		{"fork_min", "native", emit.Options{Native: true}},                  // native scatter: bare fused per-call process (#76)
		{"map_pipe_split", "default", emit.Options{}},                       // keyed split triad (_SPLIT_K/_MAIN_K/_JOIN_K)
		{"map_pipe", "default", emit.Options{}},                             // keyed fused bind+main (_K, #99)
		{"map_pipe_nested", "default", emit.Options{}},                      // keyed element scatter (_KS, #99)
		{"chain_fuse3", "fuse-chains", emit.Options{FuseChains: true}},      // fused linear chain (#59 Lever 4)
		{"fold_disable", "fold-disables", emit.Options{FoldDisables: true}}, // always-off stage pruned (#59 Lever 1)
		{"split_test", "native-runner", emit.Options{NativeRunner: true}},   // direct-call runner stage hop (#79)
	}

	seen := map[string]bool{}

	for _, fx := range fixtures {
		prog := lowerNamesFixture(t, fx.name)
		inv := inventoryNames(prog)

		for _, name := range emittedProcessNames(t, fx.name, prog, fx.opts) {
			tag, ok := inv[name]
			if !ok {
				t.Errorf("%s (%s): process name %q is not in the inventory shared with "+
					"internal/overrides (emit.PlainStageSuffixes/FusedCallSuffixes/ScatterCallSuffixes); "+
					"extend it so override selectors keep covering every stage process", fx.name, fx.mode, name)

				continue
			}

			seen[tag] = true
		}
	}

	for _, s := range emit.PlainStageSuffixes() {
		if !seen["plain:"+s] {
			t.Errorf("inventoried plain suffix %q was never generated; stale entry or missing fixture", s)
		}
	}

	for _, s := range emit.FusedCallSuffixes() {
		if !seen["fused:"+s] {
			t.Errorf("inventoried fused suffix %q was never generated; stale entry or missing fixture", s)
		}
	}

	for _, s := range emit.ScatterCallSuffixes() {
		if !seen["scatter:"+s] {
			t.Errorf("inventoried scatter suffix %q was never generated; stale entry or missing fixture", s)
		}
	}
}

// lowerNamesFixture parses and lowers a testdata fixture (external-package
// twin of plan_test's lowerFixture).
func lowerNamesFixture(t *testing.T, fixture string) *ir.Program {
	t.Helper()

	base := "../../testdata/" + fixture

	ast, err := frontend.Parse(base+"/pipeline.mro", []string{base}, false)
	if err != nil {
		t.Fatalf("parse %s: %v", fixture, err)
	}

	prog, err := frontend.Lower(ast, nil)
	if err != nil {
		t.Fatalf("lower %s: %v", fixture, err)
	}

	return prog
}

// inventoryNames enumerates every process name the inventory accounts for in
// prog: per stage the plain-family names, per (pipeline, call) the fused- and
// scatter-family names plus the data-plane helpers (bind/fork/merge/disable,
// which run no stage code), and the fixed singleton helpers.
func inventoryNames(prog *ir.Program) map[string]string {
	names := map[string]string{
		"PUBLISH_LEAF": "helper", "LAYOUT": "helper", "BUILD_ENTRY_ARGS": "helper",
	}

	for stage := range prog.Stages {
		for _, s := range emit.PlainStageSuffixes() {
			names[stage+s] = "plain:" + s
		}
	}

	for _, p := range prog.Pipelines {
		calls := make([]string, 0, len(p.Calls)+1)
		calls = append(calls, "return") // the return bind is keyed like a call

		for _, c := range p.Calls {
			calls = append(calls, c.Name)
		}

		for _, call := range calls {
			q := qualifyName(p.Name, call)

			for _, s := range emit.FusedCallSuffixes() {
				names["STAGE_"+q+s] = "fused:" + s
			}

			for _, s := range emit.ScatterCallSuffixes() {
				names["FORK_"+q+s] = "scatter:" + s
			}

			for _, h := range []string{"BIND_" + q, "FORK_" + q, "MERGE_" + q, "DISABLE_" + q} {
				names[h] = "helper"
				names[h+"_K"] = "helper"
			}
		}
	}

	return names
}

// qualifyName mirrors emit's unexported qualify(); the overrides selector
// prefixes (STAGE_[0-9]+_.+__ / FORK_[0-9]+_.+__) assume exactly this shape,
// so a change to it must fail this test.
func qualifyName(pipeline, call string) string {
	return strconv.Itoa(len(pipeline)) + "_" + pipeline + "__" + call
}

var (
	processDefRe   = regexp.MustCompile(`(?m)^process\s+(\S+)\s*\{`)
	includeAliasRe = regexp.MustCompile(`\bas\s+([A-Za-z0-9_]+)`)
)

// stubMre writes a fake mre that satisfies the transpile-time `mre entryargs`
// bake a -native emit runs (it only needs the -o dir to exist and exit 0).
func stubMre(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "mre")
	script := "#!/bin/sh\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = -o ]; then mkdir -p \"$2\"; fi\n  shift\ndone\n"

	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub mre: %v", err)
	}

	return path
}

// emittedProcessNames emits the fixture under opts (mode flags set by the
// caller; the paths are filled in here) and returns every process definition
// name and every capitalized include-alias (a process alias such as the fused
// _MN/_JN imports; lowercase wf_/wfk_ aliases are workflows, which withName
// never targets) across the generated .nf files.
func emittedProcessNames(t *testing.T, fixture string, prog *ir.Program, opts emit.Options) []string {
	t.Helper()

	base := "../../testdata/" + fixture

	code := make(map[string]string, len(prog.Stages))
	for stage := range prog.Stages {
		code[stage] = "/x/" + strings.ToLower(stage)
	}

	dir := t.TempDir()
	opts.OutDir = dir
	opts.Mre = stubMre(t)
	opts.Shell = "/x/martian_shell.py"
	opts.MROFile = "pipeline.mro"
	opts.MRODir = base
	opts.StageCode = code

	if err := emit.Emit(prog, opts); err != nil {
		t.Fatalf("emit %s (opts %+v): %v", fixture, opts, err)
	}

	var names []string

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".nf") {
			return err
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		for _, m := range processDefRe.FindAllStringSubmatch(string(data), -1) {
			names = append(names, m[1])
		}

		for line := range strings.SplitSeq(string(data), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "include") {
				continue
			}

			for _, m := range includeAliasRe.FindAllStringSubmatch(line, -1) {
				if m[1][0] >= 'A' && m[1][0] <= 'Z' {
					names = append(names, m[1])
				}
			}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", fixture, err)
	}

	if len(names) == 0 {
		t.Fatalf("%s: no process names found in the emitted project", fixture)
	}

	return names
}
