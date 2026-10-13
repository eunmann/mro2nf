package shim

import (
	"errors"
	"fmt"
	"path/filepath"
)

// errRelativeMount reports a mount root that is not an absolute path.
var errRelativeMount = errors.New("the S3 mount root must be an absolute path")

// Mount is the root of the S3 mount, a read-only filesystem every task of a
// run sees at the same path, under which Mountpoint for S3 shows each input
// bucket on each instance, <mount>/<bucket>. It is one of the run's Roots.
// The zero Mount holds nothing.
type Mount string

// ParseMount is the mount rooted at root, "" for none.
func ParseMount(root string) (Mount, error) {
	if root != "" && !filepath.IsAbs(root) {
		return "", fmt.Errorf("%w: %q", errRelativeMount, root)
	}

	return Mount(root), nil
}

// Holds reports whether the absolute path p, cleaned, is the mount root or
// lies under it.
func (m Mount) Holds(p string) bool {
	return m != "" && filepath.IsAbs(p) && within(filepath.Clean(p), string(m))
}
