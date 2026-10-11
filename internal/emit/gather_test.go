package emit_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/emit"
	"github.com/eunmann/mro2nf/internal/frontend"
)

// gatherCmd matches an `mre merge` invocation or a join-phase stage command.
var gatherCmd = regexp.MustCompile(`(?m)^.*' (merge|join) .*$`)

// legacyForkMeta matches the fork sidecar names from before the pruned field.
var legacyForkMeta = regexp.MustCompile(`forkmeta(_\w+)?\.(mro2nf\.)?json`)

// TestGeneratedGathersTakeNoArgvList is the #323/#324 property over every
// transpiled fixture, default and -native: no generated script builds a fork
// or chunk list in the shell (`ls | sort -V | paste -sd,`), which hits the
// kernel's 128 KiB per-argument limit at a few thousand entries and hides a
// failed listing as an empty gather. Every merge and join instead names the
// staged dir mre lists and the sidecar it counts against.
func TestGeneratedGathersTakeNoArgvList(t *testing.T) {
	fixtures, err := filepath.Glob("../../testdata/*/pipeline.mro")
	if err != nil || len(fixtures) == 0 {
		t.Fatalf("no fixtures found (err %v)", err)
	}

	seen := map[string]int{}

	for _, mro := range fixtures {
		for _, native := range []bool{false, true} {
			for path, src := range emitNF(t, mro, native) {
				checkGathers(t, fmt.Sprintf("%s (native=%v) %s", mro, native, path), src, seen)
			}
		}
	}

	if seen["merge"] == 0 || seen["join"] == 0 {
		t.Errorf("property is vacuous: %d merges, %d joins across the fixtures", seen["merge"], seen["join"])
	}
}

// checkGathers asserts one generated .nf file's gathers take no shell-built
// list and name their staged dir and count sidecar, tallying each gather kind
// into seen.
func checkGathers(t *testing.T, where, src string, seen map[string]int) {
	t.Helper()

	for _, banned := range []string{"paste -sd", "sort -V"} {
		if strings.Contains(src, banned) {
			t.Errorf("%s builds a gather list with %q", where, banned)
		}
	}

	// The fork sidecar is named forkmeta2: a producer cached under -resume
	// from before a format change holds only a legacy sidecar (forkkeys,
	// then forkmeta before the pruned field, #436), so it misses its output
	// and re-runs instead of handing the merge a sidecar it misreads.
	if strings.Contains(src, "forkkeys") || legacyForkMeta.MatchString(src) {
		t.Errorf("%s names a legacy fork sidecar", where)
	}

	flags := map[string][]string{
		"merge": {"-forksdir ", "-keysfile "},
		"join":  {"-chunksfile chunks.json", "-chunkdir "},
	}

	for _, m := range gatherCmd.FindAllStringSubmatch(src, -1) {
		seen[m[1]]++

		for _, flag := range flags[m[1]] {
			if !strings.Contains(m[0], flag) {
				t.Errorf("%s: %s lacks %q:\n%s", where, m[1], flag, m[0])
			}
		}
	}
}

// emitNF transpiles one fixture and returns every generated .nf file's source
// by its project-relative path.
func emitNF(t *testing.T, mro string, native bool) map[string]string {
	t.Helper()

	base := filepath.Dir(mro)

	ast, err := frontend.Parse(mro, []string{base}, false)
	if err != nil {
		t.Fatalf("parse %s: %v", mro, err)
	}

	prog, err := frontend.Lower(ast, nil)
	if err != nil {
		t.Fatalf("lower %s: %v", mro, err)
	}

	dir := t.TempDir()
	if err := emit.Emit(prog, emit.Options{
		OutDir: dir, Mre: stubMre(t), Shell: "/x/martian_shell.py", Mrjob: "/x/mrjob",
		MROFile: "pipeline.mro", MRODir: base, Native: native,
	}); err != nil {
		t.Fatalf("emit %s (native=%v): %v", mro, native, err)
	}

	out := map[string]string{}

	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".nf" {
			return err
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		rel, _ := filepath.Rel(dir, path)
		out[rel] = string(data)

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}

	return out
}
