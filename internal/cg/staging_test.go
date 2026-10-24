package cg

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

// standInSkipped are the fixtures the stand-in cannot run, by why.
func standInSkipped() map[string]string {
	return map[string]string{
		"split_gate_entry": "it zips entry collections with SRC's outs, whose keys and lengths no stand-in knows",
		"forkdisabled":     "GATED zips the entry's xs with FLAGS's skips, whose length no stand-in knows",
	}
}

// TestStandInStagesOnlyWhatItReads holds every instance of every fixture to
// mrp's data movement: an mrp job reads, of each node its bindings read,
// the outs of the forks its resolution reads, so an instance may be given
// no more instances of a node than its resolution reads, or one where it
// reads none, the instance a value the gate holds disabled would read. It
// runs each fixture's literal invocation and its witness's, whose
// collections all fork, and of different lengths where nested.
func TestStandInStagesOnlyWhatItReads(t *testing.T) {
	var found []string

	for _, mro := range entryFiles(t) {
		dir := filepath.Dir(mro)
		fixture := filepath.Base(dir)

		if _, skip := standInSkipped()[fixture]; skip {
			continue
		}

		src, err := ReadSource(mro, []string{dir})
		if err != nil {
			t.Fatalf("%s: %v", mro, err)
		}

		witness, err := src.Witness([]string{dir})
		if err != nil {
			t.Fatalf("%s: witness: %v", mro, err)
		}

		literal, err := Load(mro, "", []string{dir})
		if err != nil {
			t.Fatalf("%s: %v", mro, err)
		}

		for kind, g := range map[string]*Graph{"literal": literal, "witness": witness} {
			name := fmt.Sprintf("%s/%s %s", fixture, filepath.Base(mro), kind)

			t.Run(name, func(t *testing.T) {
				s := standInOf(t, g)
				s.overstaged = func(node, fork, read string, staged, resolved int) {
					found = append(found, fmt.Sprintf("%s: %s at %s is given %d instances of %s and reads %d", name, node, fork, staged, read, resolved))
				}

				s.stages()
			})
		}
	}

	slices.Sort(found)

	for _, f := range found {
		t.Error(f)
	}
}
