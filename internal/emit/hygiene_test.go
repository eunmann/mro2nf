package emit_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/emit"
	"github.com/eunmann/mro2nf/internal/frontend"
	"github.com/eunmann/mro2nf/internal/ir"
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
