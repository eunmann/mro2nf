package emit_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/emit"
	"github.com/eunmann/mro2nf/internal/frontend"
	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/google/go-cmp/cmp"
)

const hygieneMRO = `filetype txt;

stage COUNT(
    in  path reads,
    out int  total,
    src py   "stages/count",
)

pipeline EP(
    in  path reads,
    out int  total,
)
{
    call COUNT(
        reads = self.reads,
    )

    return (
        total = COUNT.total,
    )
}

call EP(
    reads = %q,
)
`

// hygieneTree is a pipeline source tree (the .mro's dir), a host directory
// outside it holding a secret, and the tools a container build bakes.
type hygieneTree struct {
	root, outside, mre, shell string
}

func newHygieneTree(t *testing.T) hygieneTree {
	t.Helper()

	tr := hygieneTree{root: t.TempDir(), outside: t.TempDir()}
	tools := t.TempDir()
	tr.mre = filepath.Join(tools, "mre")
	tr.shell = filepath.Join(tools, "martian_shell.py")

	writeTreeFile(t, filepath.Join(tr.root, "stages", "count", "__init__.py"), "")
	writeTreeFile(t, filepath.Join(tr.root, "input", "reads.txt"), "reads")
	writeTreeFile(t, filepath.Join(tr.root, "input", "refdir", "a.txt"), "a")
	writeTreeFile(t, filepath.Join(tr.outside, "secret.txt"), "SECRET")
	writeTreeFile(t, filepath.Join(tr.outside, "stage", "__init__.py"), "")
	writeTreeFile(t, tr.mre, "#!/bin/sh\n")
	writeTreeFile(t, tr.shell, "# adapter\n")

	return tr
}

func writeTreeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func symlinkT(t *testing.T, target, link string) {
	t.Helper()

	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// hygieneCase sets up one transpile of the hygiene pipeline: setup links or
// writes extra files and returns the entry default and the COUNT stage code.
type hygieneCase struct {
	name    string
	setup   func(t *testing.T, tr hygieneTree) (string, string)
	target  emit.Target
	mroPath bool
	allow   bool
}

func (c hygieneCase) emit(t *testing.T) error {
	t.Helper()

	tr := newHygieneTree(t)
	def, code := c.setup(t, tr)

	mro := filepath.Join(tr.root, "pipeline.mro")
	writeTreeFile(t, mro, fmt.Sprintf(hygieneMRO, def))

	prog := parseLowered(t, mro, tr.root)
	opts := emit.Options{
		OutDir: t.TempDir(), Mre: tr.mre, Shell: tr.shell, MROFile: "pipeline.mro",
		MRODir: tr.root, Target: c.target, AllowExternalFiles: c.allow,
		StageCode: map[string]string{"COUNT": code},
	}

	if c.mroPath {
		opts.MROPath = []string{tr.outside}
	}

	return emit.Emit(prog, opts)
}

func parseLowered(t *testing.T, mro, dir string) *ir.Program {
	t.Helper()

	ast, err := frontend.Parse(mro, []string{dir}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	prog, err := frontend.Lower(ast, nil)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	return prog
}

func inTreeCode(tr hygieneTree) string {
	return filepath.Join(tr.root, "stages", "count")
}

// TestEmitRefusesExternalFiles checks a transpile refuses to copy a host file
// outside the .mro's directory and the -mropath roots into the project (an
// entry default) or the image (a vendored stage src), however it is reached.
func TestEmitRefusesExternalFiles(t *testing.T) {
	cases := []hygieneCase{
		{
			name: "entry_default_outside",
			setup: func(_ *testing.T, tr hygieneTree) (string, string) {
				return filepath.Join(tr.outside, "secret.txt"), inTreeCode(tr)
			},
		},
		{
			name: "entry_default_link_escaping",
			setup: func(t *testing.T, tr hygieneTree) (string, string) {
				t.Helper()
				symlinkT(t, filepath.Join(tr.outside, "secret.txt"), filepath.Join(tr.root, "input", "link.txt"))

				return "input/link.txt", inTreeCode(tr)
			},
		},
		{
			name: "entry_default_dir_holding_escaping_link",
			setup: func(t *testing.T, tr hygieneTree) (string, string) {
				t.Helper()
				symlinkT(t, filepath.Join(tr.outside, "secret.txt"), filepath.Join(tr.root, "input", "refdir", "s.txt"))

				return "input/refdir", inTreeCode(tr)
			},
		},
		{
			name: "entry_default_dir_linking_a_dir_holding_escaping_link",
			setup: func(t *testing.T, tr hygieneTree) (string, string) {
				t.Helper()
				other := filepath.Join(tr.root, "other")
				writeTreeFile(t, filepath.Join(other, "b.txt"), "b")
				symlinkT(t, filepath.Join(tr.outside, "secret.txt"), filepath.Join(other, "s.txt"))
				symlinkT(t, other, filepath.Join(tr.root, "input", "refdir", "other"))

				return "input/refdir", inTreeCode(tr)
			},
		},
		{
			name:   "stage_src_outside_container",
			target: emit.TargetAWSBatch,
			setup: func(_ *testing.T, tr hygieneTree) (string, string) {
				return "input/reads.txt", filepath.Join(tr.outside, "stage")
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.emit(t); !errors.Is(err, emit.ErrExternalFile) {
				t.Errorf("Emit: want ErrExternalFile, got %v", err)
			}
		})
	}
}

// TestEmitAdmitsTreeFiles checks the files a transpile may copy: anything that
// resolves inside the .mro's directory or a -mropath root, an external file
// under -allow-external-files, and an external stage src on the local target,
// which runs it in place instead of copying it.
func TestEmitAdmitsTreeFiles(t *testing.T) {
	cases := []hygieneCase{
		{
			name: "entry_default_in_tree",
			setup: func(_ *testing.T, tr hygieneTree) (string, string) {
				return "input/reads.txt", inTreeCode(tr)
			},
		},
		{
			name: "entry_default_link_within_tree",
			setup: func(t *testing.T, tr hygieneTree) (string, string) {
				t.Helper()
				symlinkT(t, filepath.Join(tr.root, "input", "reads.txt"), filepath.Join(tr.root, "input", "refdir", "r.txt"))

				return "input/refdir", inTreeCode(tr)
			},
		},
		{
			name:    "entry_default_under_mropath_root",
			mroPath: true,
			setup: func(_ *testing.T, tr hygieneTree) (string, string) {
				return filepath.Join(tr.outside, "secret.txt"), inTreeCode(tr)
			},
		},
		{
			name:  "entry_default_outside_allowed",
			allow: true,
			setup: func(_ *testing.T, tr hygieneTree) (string, string) {
				return filepath.Join(tr.outside, "secret.txt"), inTreeCode(tr)
			},
		},
		{
			name:   "stage_src_outside_allowed_container",
			target: emit.TargetAWSBatch,
			allow:  true,
			setup: func(_ *testing.T, tr hygieneTree) (string, string) {
				return "input/reads.txt", filepath.Join(tr.outside, "stage")
			},
		},
		{
			name: "stage_src_outside_local",
			setup: func(_ *testing.T, tr hygieneTree) (string, string) {
				return "input/reads.txt", filepath.Join(tr.outside, "stage")
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.emit(t); err != nil {
				t.Errorf("Emit: %v", err)
			}
		})
	}
}

// TestEmitContainerVendorsOnlyWhatRuns checks the build context holds the
// named -mre, -mrjob and -shell files (with the adapter's martian.py) but
// nothing else from their directories, and the stage code without its junk.
func TestEmitContainerVendorsOnlyWhatRuns(t *testing.T) {
	tr := newHygieneTree(t)
	tools := filepath.Dir(tr.mre)
	mrjob := filepath.Join(tools, "mrjob")

	for _, p := range []string{
		mrjob, filepath.Join(tools, "notes.txt"), filepath.Join(tools, "martian.py"),
		filepath.Join(tools, ".env"), filepath.Join(tools, "tester", "x.py"),
		filepath.Join(tr.root, "stages", "count", ".git", "config"),
		filepath.Join(tr.root, "stages", "count", "__pycache__", "m.pyc"),
	} {
		writeTreeFile(t, p, "x")
	}

	mro := filepath.Join(tr.root, "pipeline.mro")
	writeTreeFile(t, mro, fmt.Sprintf(hygieneMRO, "input/reads.txt"))

	opts := emit.Options{
		OutDir: t.TempDir(), Mre: tr.mre, Shell: tr.shell, Mrjob: mrjob, MROFile: "pipeline.mro",
		MRODir: tr.root, Target: emit.TargetAWSBatch,
		StageCode: map[string]string{"COUNT": inTreeCode(tr)},
	}
	if err := emit.Emit(parseLowered(t, mro, tr.root), opts); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	want := []string{
		"adapters", "adapters/martian.py", "adapters/martian_shell.py", "mre", "mrjob",
		"stages", "stages/COUNT", "stages/COUNT/__init__.py",
	}
	if diff := cmp.Diff(want, relEntries(t, filepath.Join(opts.OutDir, "runtime"))); diff != "" {
		t.Errorf("build context (-want +got):\n%s", diff)
	}

	opts.OutDir, opts.Mre = t.TempDir(), tools
	if err := emit.Emit(parseLowered(t, mro, tr.root), opts); err == nil {
		t.Error("Emit with -mre naming a directory: want an error, not a copy of the directory")
	}
}

func relEntries(t *testing.T, dir string) []string {
	t.Helper()

	var got []string

	err := filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}

		rel, err := filepath.Rel(dir, p)
		got = append(got, filepath.ToSlash(rel))

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	return got
}

// emitHealthOmics emits a fixture for HealthOmics with its own stage code.
func emitHealthOmics(t *testing.T, fixture string, native bool) string {
	t.Helper()

	base := filepath.Join("..", "..", "testdata", fixture)
	prog := parseLowered(t, filepath.Join(base, "pipeline.mro"), base)

	code := map[string]string{}
	for name, s := range prog.Stages {
		code[name] = s.SrcPath
	}

	dir := t.TempDir()
	if err := emit.Emit(prog, emit.Options{
		OutDir: dir, Mre: writeEntryArgsStub(t), MROFile: "pipeline.mro", MRODir: base,
		Target: emit.TargetHealthOmics, StageCode: code, Native: native,
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	return dir
}

// fakeZip puts a zip on PATH that records its arguments, one per line.
func fakeZip(t *testing.T) (string, string) {
	t.Helper()

	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsFile + "'\n"

	if err := os.WriteFile(filepath.Join(bin, "zip"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	return bin + string(os.PathListSeparator) + os.Getenv("PATH"), argsFile
}

// TestPackageScriptZipsOnlyWorkflow checks package.sh hands zip exactly the
// files the workflow reads (entry_resolved instead of entry_args under
// -native, nulls/ only when a call can be disabled), whatever else a run or
// the user left in the project dir.
func TestPackageScriptZipsOnlyWorkflow(t *testing.T) {
	common := []string{"main.nf", "nextflow.config", "parameter-template.json", "modules", "lib", "_assets"}

	for _, tc := range []struct {
		name, fixture string
		native        bool
		want          []string
	}{
		{"default", "entry_file", false, append(append([]string{}, common...), "entry_args")},
		{"native", "entry_file", true, append(append([]string{}, common...), "entry_resolved")},
		{"disabled_calls", "disabled_plain_skip", false, append(append([]string{}, common...), "entry_args", "nulls")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := emitHealthOmics(t, tc.fixture, tc.native)
			for _, junk := range []string{".nextflow.log", "params.json", ".env", "work/ab/secret", "results/x", "notes.txt"} {
				writeTreeFile(t, filepath.Join(dir, junk), "junk")
			}

			path, argsFile := fakeZip(t)
			cmd := exec.Command("bash", "package.sh")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+path)

			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("package.sh: %v\n%s", err, out)
			}

			args := strings.Fields(readFile(t, argsFile))
			if len(args) < 2 || args[0] != "-r" || args[1] != "workflow.zip" {
				t.Fatalf("zip args = %q, want -r workflow.zip <files>", args)
			}

			if diff := cmp.Diff(tc.want, args[2:]); diff != "" {
				t.Errorf("packaged paths (-want +got):\n%s", diff)
			}
		})
	}
}

// TestPackageScriptFailsOnMissingEntry checks package.sh refuses to build a
// zip missing a file the workflow reads instead of registering a broken one.
func TestPackageScriptFailsOnMissingEntry(t *testing.T) {
	dir := emitHealthOmics(t, "entry_file", false)
	if err := os.RemoveAll(filepath.Join(dir, "entry_args")); err != nil {
		t.Fatal(err)
	}

	path, _ := fakeZip(t)
	cmd := exec.Command("bash", "package.sh")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+path)

	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "entry_args is missing") {
		t.Errorf("package.sh without entry_args: err = %v, output:\n%s", err, out)
	}
}

// TestEmitDockerignore checks a container target emits a .dockerignore
// beside the Dockerfile that sends only runtime/ to the daemon and keeps the
// vendoring junk names out of it.
func TestEmitDockerignore(t *testing.T) {
	dir := emitHealthOmics(t, "entry_file", false)

	var rules []string

	for line := range strings.SplitSeq(readFile(t, filepath.Join(dir, ".dockerignore")), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			rules = append(rules, line)
		}
	}

	if len(rules) < 2 || rules[0] != "*" || rules[1] != "!runtime" {
		t.Fatalf(".dockerignore must exclude everything but runtime/ first; rules = %q", rules)
	}

	for _, want := range []string{"**/.git", "**/.env*", "**/__pycache__", "**/*.pyc", "**/.DS_Store", "**/*.swp"} {
		if !strings.Contains(strings.Join(rules[2:], "\n")+"\n", want+"\n") {
			t.Errorf(".dockerignore missing %q; rules = %q", want, rules)
		}
	}
}

// TestEmitConfigToolsMustResolveInRoots checks a container target bakes a tool
// set by a config file only when it, and the adapter module beside a
// config-set -shell, resolves inside the roots.
func TestEmitConfigToolsMustResolveInRoots(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tool    string
		setup   func(t *testing.T, tr hygieneTree) string
		allow   bool
		wantErr bool
	}{
		{"mrjob_outside", "mrjob", func(_ *testing.T, tr hygieneTree) string {
			return filepath.Join(tr.outside, "secret.txt")
		}, false, true},
		{"mrjob_outside_allowed", "mrjob", func(_ *testing.T, tr hygieneTree) string {
			return filepath.Join(tr.outside, "secret.txt")
		}, true, false},
		{"mrjob_in_tree", "mrjob", func(t *testing.T, tr hygieneTree) string {
			t.Helper()
			writeTreeFile(t, filepath.Join(tr.root, "bin", "mrjob"), "#!/bin/sh\n")

			return filepath.Join(tr.root, "bin", "mrjob")
		}, false, false},
		{"shell_module_link_escaping", "shell", func(t *testing.T, tr hygieneTree) string {
			t.Helper()
			writeTreeFile(t, filepath.Join(tr.root, "bin", "martian_shell.py"), "# adapter\n")
			symlinkT(t, filepath.Join(tr.outside, "secret.txt"), filepath.Join(tr.root, "bin", "martian.py"))

			return filepath.Join(tr.root, "bin", "martian_shell.py")
		}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newHygieneTree(t)
			mro := filepath.Join(tr.root, "pipeline.mro")
			writeTreeFile(t, mro, fmt.Sprintf(hygieneMRO, "input/reads.txt"))

			opts := emit.Options{
				OutDir: t.TempDir(), Mre: tr.mre, Shell: tr.shell, MROFile: "pipeline.mro",
				MRODir: tr.root, Target: emit.TargetAWSBatch, AllowExternalFiles: tc.allow,
				StageCode: map[string]string{"COUNT": inTreeCode(tr)}, ConfigTools: map[string]bool{tc.tool: true},
			}
			if p := tc.setup(t, tr); tc.tool == "shell" {
				opts.Shell = p
			} else {
				opts.Mrjob = p
			}

			err := emit.Emit(parseLowered(t, mro, tr.root), opts)
			if got := errors.Is(err, emit.ErrExternalFile); got != tc.wantErr {
				t.Errorf("Emit: want ErrExternalFile %v, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestEmitContainerResetsBuildContext checks a re-transpile into an existing
// project leaves nothing an earlier run vendored in runtime/: a dropped stage,
// a whole -shell directory, or a stale symlink. The reads default does not
// exist, so it is not staged into entry_args/.
func TestEmitContainerResetsBuildContext(t *testing.T) {
	tr := newHygieneTree(t)
	mro := filepath.Join(tr.root, "pipeline.mro")
	writeTreeFile(t, mro, fmt.Sprintf(hygieneMRO, "input/absent.txt"))

	opts := emit.Options{
		OutDir: t.TempDir(), Mre: tr.mre, Shell: tr.shell, MROFile: "pipeline.mro",
		MRODir: tr.root, Target: emit.TargetAWSBatch,
		StageCode: map[string]string{"COUNT": inTreeCode(tr), "ALIAS": inTreeCode(tr)},
	}
	if err := emit.Emit(parseLowered(t, mro, tr.root), opts); err != nil {
		t.Fatalf("first Emit: %v", err)
	}

	rt := filepath.Join(opts.OutDir, "runtime")
	writeTreeFile(t, filepath.Join(rt, "stages", "GONE", "secret.txt"), "SECRET")
	writeTreeFile(t, filepath.Join(rt, "adapters", "tester", "notes.txt"), "old")

	if err := emit.Emit(parseLowered(t, mro, tr.root), opts); err != nil {
		t.Fatalf("re-transpile: %v", err)
	}

	want := []string{
		"adapters", "adapters/martian_shell.py", "mre",
		"stages", "stages/ALIAS", "stages/ALIAS/__init__.py", "stages/COUNT",
	}
	if diff := cmp.Diff(want, relEntries(t, rt)); diff != "" {
		t.Errorf("build context after re-transpile (-want +got):\n%s", diff)
	}
}

// TestEmitRefusesSourceInBuildContext checks a container target refuses a
// tool or stage src that lies in, or holds, the runtime/ it empties, instead
// of deleting the user's only copy before vendoring it.
func TestEmitRefusesSourceInBuildContext(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, tr hygieneTree, opts *emit.Options) string
	}{
		{"mre_vendored_earlier", func(_ *testing.T, _ hygieneTree, opts *emit.Options) string {
			opts.Mre = filepath.Join(opts.OutDir, "runtime", "mre")

			return opts.Mre
		}},
		{"stage_src_under_runtime", func(t *testing.T, tr hygieneTree, opts *emit.Options) string {
			t.Helper()
			opts.OutDir = tr.root
			src := filepath.Join(tr.root, "runtime", "count")
			writeTreeFile(t, filepath.Join(src, "__init__.py"), "")
			opts.StageCode = map[string]string{"COUNT": src}

			return filepath.Join(src, "__init__.py")
		}},
		{"stage_src_holds_runtime", func(t *testing.T, tr hygieneTree, opts *emit.Options) string {
			t.Helper()
			opts.OutDir = filepath.Join(tr.root, "stages", "count", "out")
			opts.StageCode = map[string]string{"COUNT": inTreeCode(tr)}
			keep := filepath.Join(opts.OutDir, "runtime", "keep.txt")
			writeTreeFile(t, keep, "keep")

			return keep
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newHygieneTree(t)
			mro := filepath.Join(tr.root, "pipeline.mro")
			writeTreeFile(t, mro, fmt.Sprintf(hygieneMRO, "input/absent.txt"))

			opts := emit.Options{
				OutDir: t.TempDir(), Mre: tr.mre, Shell: tr.shell, MROFile: "pipeline.mro",
				MRODir: tr.root, Target: emit.TargetAWSBatch,
				StageCode: map[string]string{"COUNT": inTreeCode(tr)},
			}
			if err := emit.Emit(parseLowered(t, mro, tr.root), opts); err != nil {
				t.Fatalf("first Emit: %v", err)
			}

			kept := tc.setup(t, tr, &opts)

			err := emit.Emit(parseLowered(t, mro, tr.root), opts)
			if !errors.Is(err, emit.ErrSourceInBuildContext) {
				t.Errorf("Emit: want ErrSourceInBuildContext, got %v", err)
			}

			if _, statErr := os.Stat(kept); statErr != nil {
				t.Errorf("source %s was deleted: %v", kept, statErr)
			}
		})
	}
}
