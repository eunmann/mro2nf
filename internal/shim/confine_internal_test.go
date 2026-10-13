package shim

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/syntax"
)

// confineEnv lays out a task work dir the way a stage phase sees it: the
// stage's own phase dir, inputs Nextflow staged as symlinks to another task's
// leaf dirs (one holding a file, one a directory), and host files outside the
// work dir.
type confineEnv struct {
	work, files, host, hostDir, upstream, stagedFile, stagedDir string
}

func newConfineEnv(t *testing.T) confineEnv {
	t.Helper()

	root := t.TempDir()
	e := confineEnv{
		work:     filepath.Join(root, "work"),
		host:     filepath.Join(root, "host", "outside.txt"),
		hostDir:  filepath.Join(root, "host", "refdir"),
		upstream: filepath.Join(root, "upstream", "L0000", "real.txt"),
	}
	e.files = filepath.Join(e.work, "main", "files")
	e.stagedFile = filepath.Join(e.work, "args", "f", "L0000", "real.txt")
	e.stagedDir = filepath.Join(e.work, "args", "f", "L0001", "dir")

	upDir := filepath.Join(root, "upstream", "L0001", "dir")
	mkdirs(t, e.files, filepath.Join(e.work, "args", "f"), filepath.Dir(e.upstream), e.hostDir, upDir)
	writeTestFile(t, e.host, "host")
	writeTestFile(t, filepath.Join(e.hostDir, "x.txt"), "host dir")
	writeTestFile(t, e.upstream, "upstream")
	writeTestFile(t, filepath.Join(upDir, "y.txt"), "upstream dir")
	symlink(t, filepath.Join(upDir, "host"), e.host)
	symlink(t, filepath.Dir(e.stagedFile), filepath.Dir(e.upstream))
	symlink(t, filepath.Dir(e.stagedDir), filepath.Dir(upDir))

	return e
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()

	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, link, target string) {
	t.Helper()

	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// sameFile reports whether a and b are the same inode.
func sameFile(t *testing.T, a, b string) bool {
	t.Helper()

	ai, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}

	bi, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}

	return os.SameFile(ai, bi)
}

// markOut stages value as the stage's file-typed output `out` under the main
// phase's confinement and returns the staged leaf's path in the bundle.
func (e confineEnv) markOut(t *testing.T, value string, strict bool) (string, error) {
	t.Helper()

	conf, err := ConfineStage(e.work, "main", strict, Roots{})
	if err != nil {
		t.Fatal(err)
	}

	bundle := t.TempDir()
	params := []*syntax.StructMember{{Id: "out", Tname: syntax.TypeId{Tname: syntax.KindFile}}}

	if _, err := WriteItem(bundle, map[string]any{"out": value}, params, types.NewTable(syntax.NewTypeLookup()), Opts{Confine: conf}, Staged{}); err != nil {
		return "", err
	}

	raw, err := os.ReadFile(filepath.Join(bundle, bundleData))
	if err != nil {
		t.Fatal(err)
	}

	var marked map[string]string
	if err := json.Unmarshal(raw, &marked); err != nil {
		t.Fatal(err)
	}

	s := marked["out"]

	rel, _, ok := CutMarker(s)
	if !ok {
		t.Fatalf("marked out = %q, want a marker", s)
	}

	return filepath.Join(bundle, rel), nil
}

// outOfTreeCase is a stage output that is, or reaches, a host file outside the
// task work dir: the leaf value, and the staged file below the leaf (rel) that
// must hold host's content.
type outOfTreeCase struct {
	name, value, rel, host string
}

// outOfTreeCases lays out the out-of-tree output shapes in e's phase dir.
func (e confineEnv) outOfTreeCases(t *testing.T) []outOfTreeCase {
	t.Helper()

	ownDir := filepath.Join(e.files, "own_dir")
	mkdirs(t, ownDir)
	symlink(t, filepath.Join(ownDir, "child"), e.host)
	symlink(t, filepath.Join(e.files, "hop2"), e.host)
	symlink(t, filepath.Join(e.files, "hop1"), "hop2")
	symlink(t, filepath.Join(e.files, "to_host"), e.host)
	symlink(t, filepath.Join(e.files, "to_host_dir"), e.hostDir)

	return []outOfTreeCase{
		{"link to a host file", filepath.Join(e.files, "to_host"), "", e.host},
		{"link to a host dir", filepath.Join(e.files, "to_host_dir"), "x.txt", filepath.Join(e.hostDir, "x.txt")},
		{"chain to a host file", filepath.Join(e.files, "hop1"), "", e.host},
		{"absolute host path", e.host, "", e.host},
		{"own dir with a host child", ownDir, "child", e.host},
	}
}

// TestConfineStrictRefusesOutOfTreeTargets guards #332 on isolated container
// targets: a stage output that is, or reaches through a symlink, a host file
// or directory outside the task work dir must fail the bundle write naming the
// output and the target, since copying it would upload host data the stage was
// never given.
func TestConfineStrictRefusesOutOfTreeTargets(t *testing.T) {
	e := newConfineEnv(t)

	hostRoot, err := filepath.EvalSymlinks(filepath.Dir(e.host))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range e.outOfTreeCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.markOut(t, tc.value, true)
			if !errors.Is(err, errOutsideWork) {
				t.Fatalf("WriteItem err = %v, want errOutsideWork", err)
			}

			if msg := err.Error(); !strings.Contains(msg, "param out") || !strings.Contains(msg, hostRoot) {
				t.Errorf("error %q must name the output and the host target under %s", msg, hostRoot)
			}
		})
	}
}

// TestConfineLocalCopiesOutOfTreeTargets guards #332 on a shared filesystem,
// where mrp publishes a symlink to a host file and pipelines return paths into
// host reference dirs: the output stages, but as a byte copy, never a hard
// link, so no later write through the bundle can alias the host file.
func TestConfineLocalCopiesOutOfTreeTargets(t *testing.T) {
	e := newConfineEnv(t)

	for _, tc := range e.outOfTreeCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			leaf, err := e.markOut(t, tc.value, false)
			if err != nil {
				t.Fatalf("WriteItem: %v", err)
			}

			staged := filepath.Join(leaf, tc.rel)

			want, err := os.ReadFile(tc.host)
			if err != nil {
				t.Fatal(err)
			}

			if got, err := os.ReadFile(staged); err != nil || string(got) != string(want) {
				t.Errorf("staged content = %q (err %v), want %q", got, err, want)
			}

			if sameFile(t, staged, tc.host) {
				t.Errorf("%s is a hard link to the host file %s; it must be a copy", staged, tc.host)
			}
		})
	}
}

// TestConfineAdmitsOwnAndStagedFiles checks the paths a stage legitimately
// outputs stage the same under either mode, hard-linked: its own files and
// dirs, and a Nextflow-staged input (named directly, through the stage's own
// symlink, absolute or relative, or by the real path it resolves to outside
// the task), which is dereferenced to the other task's file it points at. A
// dangling link in an output dir is still skipped.
func TestConfineAdmitsOwnAndStagedFiles(t *testing.T) {
	e := newConfineEnv(t)

	own := filepath.Join(e.files, "own.txt")
	writeTestFile(t, own, "own")

	ownDir := filepath.Join(e.files, "own_dir")
	mkdirs(t, ownDir)
	writeTestFile(t, filepath.Join(ownDir, "kept.txt"), "own")
	symlink(t, filepath.Join(ownDir, "dangling"), filepath.Join(e.files, "gone"))
	symlink(t, filepath.Join(e.files, "abs_fwd"), e.stagedFile)
	symlink(t, filepath.Join(e.files, "rel_fwd"), filepath.Join("..", "..", "args", "f", "L0000", "real.txt"))
	symlink(t, filepath.Join(e.files, "real_fwd"), e.upstream)

	upDir, err := filepath.EvalSymlinks(e.stagedDir)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, value, rel, src string
	}{
		{"own file", own, "", own},
		{"own dir", ownDir, "kept.txt", filepath.Join(ownDir, "kept.txt")},
		{"staged input", e.stagedFile, "", e.upstream},
		{"absolute link to a staged input", filepath.Join(e.files, "abs_fwd"), "", e.upstream},
		{"relative link to a staged input", filepath.Join(e.files, "rel_fwd"), "", e.upstream},
		{"staged input dir", e.stagedDir, "y.txt", filepath.Join(e.stagedDir, "y.txt")},
		{"real path of a staged input", e.upstream, "", e.upstream},
		{"link to the real path of a staged input", filepath.Join(e.files, "real_fwd"), "", e.upstream},
		{"real path in a staged input dir", filepath.Join(upDir, "y.txt"), "", filepath.Join(upDir, "y.txt")},
	}

	for _, strict := range []bool{true, false} {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				leaf, err := e.markOut(t, tc.value, strict)
				if err != nil {
					t.Fatalf("WriteItem (strict %v): %v", strict, err)
				}

				if staged := filepath.Join(leaf, tc.rel); !sameFile(t, staged, tc.src) {
					t.Errorf("strict %v: %s is not a hard link to %s", strict, staged, tc.src)
				}
			})
		}

		leaf, err := e.markOut(t, ownDir, strict)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := os.Lstat(filepath.Join(leaf, "dangling")); !os.IsNotExist(err) {
			t.Errorf("strict %v: dangling link in an output dir was reproduced: %v", strict, err)
		}
	}
}
