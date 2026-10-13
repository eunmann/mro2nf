package shim_test

import (
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
)

// TestRootsHolds: roots hold each root and what lies under it once cleaned,
// a root under another adding nothing, so neither a sibling sharing a
// root's prefix nor a path climbing out of one reads as held; a relative
// path is no root, and the zero Roots hold nothing.
func TestRootsHolds(t *testing.T) {
	r := shim.NewRoots("/mnt/s3", "/data/ref/", "/data/ref/genome.fa", "rel/x", "")

	for p, want := range map[string]bool{
		"/mnt/s3":              true,
		"/mnt/s3/b/k.txt":      true,
		"/data/ref":            true,
		"/data/ref/genome.fa":  true,
		"/data/ref/./sub/x":    true,
		"/data/reference/x":    false,
		"/data/ref/../other/x": false,
		"/data":                false,
		"/mnt/s3x/b/k":         false,
		"rel/x":                false,
		"/rel/x":               false,
	} {
		if got := r.Holds(p); got != want {
			t.Errorf("Holds(%q) = %t, want %t", p, got, want)
		}
	}

	if (shim.Roots{}).Holds("/mnt/s3/b") || shim.NewRoots().Holds("/") {
		t.Error("no roots hold a path")
	}
}
