package shim

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// errCloseBoom is the failing-close sentinel for TestCloseChecked (package-level
// so the err113 linter does not flag a dynamic errors.New inside the test).
var errCloseBoom = errors.New("close boom")

// closerFunc adapts a func to io.Closer for closeChecked's test.
type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// TestCloseChecked verifies the close-error propagation helper: a Closer whose
// Close fails surfaces that error (the whole point of #174), and a clean close
// returns nil.
func TestCloseChecked(t *testing.T) {
	if err := closeChecked(closerFunc(func() error { return errCloseBoom }), "thing"); !errors.Is(err, errCloseBoom) {
		t.Errorf("closeChecked on a failing close: err = %v, want it to wrap the close error", err)
	}

	if err := closeChecked(closerFunc(func() error { return nil }), "thing"); err != nil {
		t.Errorf("closeChecked on a clean close: err = %v, want nil", err)
	}
}

// TestTreeWalksStopAtSymlinkCycle guards against unbounded recursion: a
// directory output containing a symlink back to one of its ancestors would
// otherwise recurse until ENAMETOOLONG. Every walk of a tree must stop with
// errSymlinkCycle instead. In-package so it can assert the sentinel via
// errors.Is rather than string-matching the message.
func TestTreeWalksStopAtSymlinkCycle(t *testing.T) {
	walks := map[string]func(src, dst string) error{
		"copy":     CopyTree,
		"shape":    CopyShape,
		"skeleton": func(src, _ string) error { return writeSkeleton(src) },
	}

	for name, walk := range walks {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()

			root := filepath.Join(dir, "out")
			if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
			// out/sub/loop -> out : recursing into loop re-enters out, an ancestor.
			if err := os.Symlink(root, filepath.Join(root, "sub", "loop")); err != nil {
				t.Fatal(err)
			}

			if err := walk(root, filepath.Join(dir, "copy")); !errors.Is(err, errSymlinkCycle) {
				t.Fatalf("%s over a symlink cycle: err = %v, want errSymlinkCycle", name, err)
			}
		})
	}
}
