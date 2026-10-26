package cg

import (
	"encoding/json"
	"maps"
	"path/filepath"
	"testing"

	"github.com/martian-lang/martian/martian/core"
)

// TestWitnessCoversLaunch (#621): the project transpiled from a call's
// witness runs the invocations of the call mrp runs, so PLAN finds each
// launch's call graph within the witness's, whatever widths Martian expands
// the launch's entry collections to.
func TestWitnessCoversLaunch(t *testing.T) {
	for _, c := range []struct {
		name, fixture, input, value string
	}{
		{name: "merged literal map call at width 1", fixture: "witness_merged_literal", input: "names", value: `["a"]`},
		{name: "merged literal map call at width 4", fixture: "witness_merged_literal", input: "names", value: `["a","b","c","d"]`},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join("..", "..", "testdata", c.fixture)

			src, err := ReadSource(filepath.Join(dir, "pipeline.mro"), []string{dir})
			if err != nil {
				t.Fatal(err)
			}

			w, err := src.Witness([]string{dir})
			if err != nil {
				t.Fatal(err)
			}

			wt, err := w.Topology()
			if err != nil {
				t.Fatal(err)
			}

			args := core.LazyArgumentMap{}
			maps.Copy(args, src.Invocation.Args)
			args[c.input] = json.RawMessage(c.value)

			launch := src
			launch.Invocation = &core.InvocationData{Call: src.Invocation.Call, Args: args, SplitArgs: src.Invocation.SplitArgs}

			if err := launch.CoversLiteral(wt, []string{dir}); err != nil {
				t.Error(err)
			}
		})
	}
}
