package shim

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// errSymlinkCycle is returned by walkTree when a directory reaches one of its
// own ancestors through a symlink, which would otherwise recurse unbounded.
var errSymlinkCycle = errors.New("directory symlink cycle")

// treeNode is a visitor at one path of the tree walkTree walks: a copy of
// the tree, its shape, or its bundle skeleton.
type treeNode interface {
	// admit is the node that visits src, which reaches something.
	admit(src string) (treeNode, error)
	// dangling visits src, a link that reaches nothing.
	dangling(src string) error
	// file visits the file at path, which src resolves to.
	file(path string, info fs.FileInfo) error
	// dir visits a dir before its entries are read.
	dir() error
	// child is the node of the dir's entry name, and false to leave it out.
	child(name string) (treeNode, bool)
	// leave visits the dir after its entries; empty when it has none.
	leave(info fs.FileInfo, empty bool) error
}

// walkTree walks the tree at src with n, following links to any target and
// handing n each link that reaches nothing. The identities of the dirs on
// the recursion stack bound it: a dir that (via a symlink) reaches one of
// its own ancestors would otherwise recurse until ENAMETOOLONG.
func walkTree(src string, n treeNode) error {
	return walkNode(src, n, map[dirID]struct{}{})
}

func walkNode(src string, n treeNode, ancestors map[dirID]struct{}) error {
	resolved, err := resolveSymlink(src)
	if err != nil {
		return err
	}

	if resolved == "" {
		return n.dangling(src)
	}

	if n, err = n.admit(src); err != nil {
		return err
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("stat %s: %w", resolved, err)
	}

	if !info.IsDir() {
		return n.file(resolved, info)
	}

	release, cycle := enterDir(info, ancestors)
	if cycle {
		return fmt.Errorf("walk %s: %w", resolved, errSymlinkCycle)
	}

	defer release()

	if err := n.dir(); err != nil {
		return err
	}

	entries, err := os.ReadDir(resolved)
	if err != nil {
		return fmt.Errorf("read dir %s: %w", resolved, err)
	}

	for _, e := range entries {
		c, ok := n.child(e.Name())
		if !ok {
			continue
		}

		if err := walkNode(filepath.Join(resolved, e.Name()), c, ancestors); err != nil {
			return err
		}
	}

	return n.leave(info, len(entries) == 0)
}

// resolveSymlink returns src, or its real target when src is a symlink, and ""
// for a dangling symlink. Nextflow stages inputs as symlinks, and link(2) does
// not follow them — hard-linking the symlink would leave a dangling link once
// the bundle is staged into another isolated task (AWS Batch + S3 /
// HealthOmics), where the original target is absent. A dangling symlink is
// left to the caller, which records it in a leaf's shape or leaves it out,
// rather than aborting the whole bundle.
func resolveSymlink(src string) (string, error) {
	lst, err := os.Lstat(src)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", src, err)
	}

	if lst.Mode()&os.ModeSymlink == 0 {
		return src, nil
	}

	resolved, err := filepath.EvalSymlinks(src)
	if os.IsNotExist(err) {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("resolve symlink %s: %w", src, err)
	}

	return resolved, nil
}

// dirID identifies a directory by (device, inode) — the same identity os.SameFile
// compares — so the walk detects a directory reached twice on one path
// regardless of the symlink route taken to it.
type dirID struct {
	dev, ino uint64
}

// enterDir records dir's (device, inode) in ancestors for cycle detection. The
// bool is true when it was already on the recursion stack (a cycle); the returned
// func pops it on unwind.
func enterDir(dir os.FileInfo, ancestors map[dirID]struct{}) (func(), bool) {
	st, ok := dir.Sys().(*syscall.Stat_t)
	if !ok {
		return func() {}, false
	}

	id := dirID{dev: st.Dev, ino: st.Ino}
	if _, seen := ancestors[id]; seen {
		return func() {}, true
	}

	ancestors[id] = struct{}{}

	return func() { delete(ancestors, id) }, false
}
