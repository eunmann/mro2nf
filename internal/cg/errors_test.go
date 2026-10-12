package cg

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/core"
)

// TestPostRejectsInvalidOuts: BAD's 1.5 for its out int fails its post, as
// mrp fails BAD at output validation (bad_out_type); an int passes.
func TestPostRejectsInvalidOuts(t *testing.T) {
	f := loadFixture(t, "bad_out_type")

	bad := f.outs("BOT.BAD", nil, `{"i":1.5}`)
	if err := f.g.Post(PostSpec{Args: bad, Outs: bad, Forks: f.next("forks")}); !errors.Is(err, errInvalidOuts) {
		t.Errorf("post of BAD's 1.5: %v, want %v", err, errInvalidOuts)
	}

	good := f.outs("BOT.BAD", nil, `{"i":1}`)
	f.post(good, good)
}

// TestWriteOutsRejectsInvalidOuts: a pipeline out of the wrong type fails
// the pipeline outs; an int passes. Resolution already filters each ref to
// its declared type, so the struct check is pinned on its own.
func TestWriteOutsRejectsInvalidOuts(t *testing.T) {
	f := loadFixture(t, "bad_out_type")

	use := f.outs("BOT.USE", nil, `{"j":1.5}`)
	if _, err := f.writeOuts(types.Manifest{}, use); err == nil {
		t.Error("pipeline outs j = 1.5 were written")
	}

	use = f.outs("BOT.USE", nil, `{"j":2}`)
	if got, err := f.writeOuts(types.Manifest{}, use); err != nil || got != `{"j":2}` {
		t.Errorf("pipeline outs j = 2: %s, %v", got, err)
	}

	bad := core.MarshalerMap{"j": json.RawMessage(`1.5`)}
	if err := f.g.verifyPipelineOutputs(bad, f.g.root.ResolvedOutputs().Type); !errors.Is(err, errInvalidOuts) {
		t.Errorf("check of pipeline outs j = 1.5: %v, want %v", err, errInvalidOuts)
	}
}

// TestInstanceForkErrors: a forked instance staging no element of its root,
// or elements that disagree on its index, fails rather than guess a fork.
func TestInstanceForkErrors(t *testing.T) {
	f := loadFixture(t, "map_fork_upstream")

	gen := f.outs("MFU.GEN", nil, `{"m":{"alpha":1,"beta":2}}`)
	elems := elementDirs(t, f.post(gen, gen), "MFU.DBL")

	for _, tc := range []struct {
		name string
		in   []string
		want error
	}{
		{"no element", []string{gen}, errNoForkPart},
		{"two elements", append([]string{gen}, elems...), errAmbiguous},
	} {
		_, err := f.g.WriteArgs(ArgsSpec{Node: "MFU.DBL", In: tc.in, Out: f.next("args")})
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
}
