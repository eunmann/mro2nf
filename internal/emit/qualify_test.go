package emit

import "testing"

// TestQualifyCollisionFree checks that distinct (pipeline, call) pairs whose
// names contain the "__" separator do not collapse to the same qualified name.
func TestQualifyCollisionFree(t *testing.T) {
	a := qualify("A", "B__C")
	b := qualify("A__B", "C")

	if a == b {
		t.Errorf("qualify collision: %q == %q", a, b)
	}

	// Sanity: the common case is stable and readable.
	if got := qualify("P", "S"); got != "1_P__S" {
		t.Errorf("qualify(P,S) = %q, want 1_P__S", got)
	}
}
