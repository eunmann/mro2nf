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
// the launch's entry collections to, whichever input is null, and whether
// nested collections are of one length.
func TestWitnessCoversLaunch(t *testing.T) {
	for _, c := range []struct {
		name, mro, input, value string
	}{
		{name: "merged literal map call at width 1", mro: "testdata/witness_merged_literal/pipeline.mro", input: "names", value: `["a"]`},
		{name: "merged literal map call at width 4", mro: "testdata/witness_merged_literal/pipeline.mro", input: "names", value: `["a","b","c","d"]`},
		{name: "one-element nested collection", mro: "testdata/map_chain_partial/pipeline.mro", input: "xss", value: "[[5]]"},
		{name: "entry flag disables a read stage", mro: "testdata/witness_flag_disables/pipeline.mro", input: "xs", value: "[1]"},
		{name: "entry flag drops an outer root", mro: "testdata/witness_drops_outer/pipeline.mro", input: "xs", value: "[1,2,3]"},
		{name: "null source of a runtime-gated split", mro: "testdata/rt_gate_split/pipeline.mro", input: "some", value: "null"},
		{name: "null source of an on-gated split", mro: "testdata/rt_gate_split_on/pipeline.mro", input: "some", value: "null"},
		{name: "null source of gated empty forks", mro: "testdata/empty_fork_gated/pipeline.mro", input: "none", value: "null"},
		{name: "null source of a gated map chain", mro: "bench/mapchain/pipeline_w4.mro", input: "xs", value: "null"},
		{name: "uniform nested collections", mro: "testdata/merge_unforked_pipeline/pipeline.mro", input: "xss", value: `[["a","b","c"],["d","e","f"]]`},
	} {
		t.Run(c.name, func(t *testing.T) {
			mro := filepath.Join("..", "..", c.mro)
			dir := filepath.Dir(mro)

			src, err := ReadSource(mro, []string{dir})
			if err != nil {
				t.Fatal(err)
			}

			_, wt, err := src.Witness([]string{dir})
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
