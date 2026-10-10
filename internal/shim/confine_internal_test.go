package shim

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
)

// confineEnv lays out a task work dir the way a stage phase sees it: the
// stage's own phase dir, an input Nextflow staged as a symlink to another
// task's file and directory, and host files outside the work dir.
type confineEnv struct {
	work, files, host, hostDir, stagedFile, stagedDir string
}

func newConfineEnv(t *testing.T) confineEnv {
	t.Helper()

	root := t.TempDir()
	e := confineEnv{
		work:    filepath.Join(root, "work"),
		host:    filepath.Join(root, "host", "outside.txt"),
		hostDir: filepath.Join(root, "host", "refdir"),
	}
	e.files = filepath.Join(e.work, "main", "files")
	e.stagedFile = filepath.Join(e.work, "args", "f", "L0000.txt")
	e.stagedDir = filepath.Join(e.work, "args", "f", "L0001")

	upstream := filepath.Join(root, "upstream")
	mkdirs(t, e.files, filepath.Dir(e.stagedFile), e.hostDir, filepath.Join(upstream, "dir"))
	writeTestFile(t, e.host, "host")
	writeTestFile(t, filepath.Join(e.hostDir, "x.txt"), "host dir")
	writeTestFile(t, filepath.Join(upstream, "real.txt"), "upstream")
	writeTestFile(t, filepath.Join(upstream, "dir", "y.txt"), "upstream dir")
	symlink(t, filepath.Join(upstream, "dir", "host"), e.host)
	symlink(t, e.stagedFile, filepath.Join(upstream, "real.txt"))
	symlink(t, e.stagedDir, filepath.Join(upstream, "dir"))

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

// markOut stages value as the stage's file-typed output `out` under the main
// phase's confinement and returns the staged leaf's path in the bundle.
func (e confineEnv) markOut(t *testing.T, value string) (string, error) {
	t.Helper()

	conf, err := ConfineStage(e.work, "main")
	if err != nil {
		t.Fatal(err)
	}

	bundle := t.TempDir()
	params := []ir.Param{{Name: "out", BaseType: "txt", IsFile: true}}

	marked, err := MarkFiles(bundle, map[string]any{"out": value}, params, types.NewTable(nil), conf)
	if err != nil {
		return "", err
	}

	s, ok := marked["out"].(string)
	if !ok {
		t.Fatalf("marked out = %v, want a string", marked["out"])
	}

	rel, _, ok := CutMarker(s)
	if !ok {
		t.Fatalf("marked out = %q, want a marker", s)
	}

	return filepath.Join(bundle, rel), nil
}

// TestConfineRefusesOutOfTreeTargets guards #332: a stage output that is, or
// reaches through a symlink, a host file or directory outside the task work dir
// must fail the bundle write naming the output and the target. mrp publishes
// only a symlink there, so copying or hard-linking the host file into the
// bundle would publish and upload a file the stage was never given.
func TestConfineRefusesOutOfTreeTargets(t *testing.T) {
	e := newConfineEnv(t)

	ownDir := filepath.Join(e.files, "own_dir")
	mkdirs(t, ownDir)
	symlink(t, filepath.Join(ownDir, "child"), e.host)
	symlink(t, filepath.Join(e.files, "hop2"), e.host)
	symlink(t, filepath.Join(e.files, "hop1"), "hop2")
	symlink(t, filepath.Join(e.files, "to_host"), e.host)
	symlink(t, filepath.Join(e.files, "to_host_dir"), e.hostDir)

	cases := map[string]string{
		"link to a host file":       filepath.Join(e.files, "to_host"),
		"link to a host dir":        filepath.Join(e.files, "to_host_dir"),
		"chain to a host file":      filepath.Join(e.files, "hop1"),
		"absolute host path":        e.host,
		"own dir with a host child": ownDir,
	}

	hostRoot, err := filepath.EvalSymlinks(filepath.Dir(e.host))
	if err != nil {
		t.Fatal(err)
	}

	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := e.markOut(t, value)
			if !errors.Is(err, errOutsideWork) {
				t.Fatalf("MarkFiles err = %v, want errOutsideWork", err)
			}

			if msg := err.Error(); !strings.Contains(msg, "param out") || !strings.Contains(msg, hostRoot) {
				t.Errorf("error %q must name the output and the host target under %s", msg, hostRoot)
			}
		})
	}
}

// TestConfineAdmitsOwnAndStagedFiles checks the paths a stage legitimately
// outputs still stage: its own files and dirs, and a Nextflow-staged input
// (named directly or through the stage's own symlink, absolute or relative),
// which is dereferenced to the other task's file it points at. A dangling link
// in an output dir is still skipped.
func TestConfineAdmitsOwnAndStagedFiles(t *testing.T) {
	e := newConfineEnv(t)

	own := filepath.Join(e.files, "own.txt")
	writeTestFile(t, own, "own")

	ownDir := filepath.Join(e.files, "own_dir")
	mkdirs(t, ownDir)
	writeTestFile(t, filepath.Join(ownDir, "kept.txt"), "own")
	symlink(t, filepath.Join(ownDir, "dangling"), filepath.Join(e.files, "gone"))
	symlink(t, filepath.Join(e.files, "abs_fwd"), e.stagedFile)
	symlink(t, filepath.Join(e.files, "rel_fwd"), filepath.Join("..", "..", "args", "f", "L0000.txt"))

	cases := []struct {
		name, value, rel, want string
	}{
		{"own file", own, "", "own"},
		{"own dir", ownDir, "kept.txt", "own"},
		{"staged input", e.stagedFile, "", "upstream"},
		{"absolute link to a staged input", filepath.Join(e.files, "abs_fwd"), "", "upstream"},
		{"relative link to a staged input", filepath.Join(e.files, "rel_fwd"), "", "upstream"},
		{"staged input dir", e.stagedDir, "y.txt", "upstream dir"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			leaf, err := e.markOut(t, tc.value)
			if err != nil {
				t.Fatalf("MarkFiles: %v", err)
			}

			got, err := os.ReadFile(filepath.Join(leaf, tc.rel))
			if err != nil || string(got) != tc.want {
				t.Errorf("staged content = %q (err %v), want %q", got, err, tc.want)
			}
		})
	}

	leaf, err := e.markOut(t, ownDir)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(filepath.Join(leaf, "dangling")); !os.IsNotExist(err) {
		t.Errorf("dangling link in an output dir was reproduced: %v", err)
	}
}
