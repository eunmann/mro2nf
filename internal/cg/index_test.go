package cg

import (
	"slices"
	"testing"

	"github.com/martian-lang/martian/martian/syntax"
)

// TestIndexOrder: concrete indices order as mrp orders forks, an array call's
// by position and a map call's by key bytes, and each answers only for its
// own mode: an array index has no key and a map key no position.
func TestIndexOrder(t *testing.T) {
	arr := []syntax.CollectionIndex{ArrayIndex(2), ArrayIndex(0), ArrayIndex(10), ArrayIndex(1)}
	slices.SortFunc(arr, compareIndex)

	if got := []int{arr[0].ArrayIndex(), arr[1].ArrayIndex(), arr[2].ArrayIndex(), arr[3].ArrayIndex()}; !slices.Equal(got, []int{0, 1, 2, 10}) {
		t.Errorf("array order %v, want 0 1 2 10", got)
	}

	keys := []syntax.CollectionIndex{MapKey("b"), MapKey("B"), MapKey("a"), MapKey("b"), MapKey("é")}
	slices.SortFunc(keys, compareIndex)

	got := make([]string, 0, len(keys))
	for _, k := range keys {
		got = append(got, k.MapKey())
	}

	if !slices.Equal(got, []string{"B", "a", "b", "b", "é"}) {
		t.Errorf("map key order %q, want byte order", got)
	}

	if ArrayIndex(3).MapKey() != "" || MapKey("k").ArrayIndex() != -1 {
		t.Error("an index answers for the other mode")
	}
}
