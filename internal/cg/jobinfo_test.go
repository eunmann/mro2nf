package cg

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestInvocationIsTheTopLevelCall: every job's _jobinfo invocation is the
// top-level call's, whatever stage the job runs (fork_mid, mrp's
// MID/SCALE/fork1/chnk0/_jobinfo).
func TestInvocationIsTheTopLevelCall(t *testing.T) {
	f := loadFixture(t, "fork_mid")

	inv, err := f.g.Invocation()
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(inv)
	if err != nil {
		t.Fatal(err)
	}

	if d := cmp.Diff(`{"call":"MID","args":{"factor":10,"values":[1,2,3]},"mro_file":"pipeline.mro"}`, string(raw)); d != "" {
		t.Errorf("invocation differs from mrp's (-mrp +got):\n%s", d)
	}
}

// TestForkIDNamesMrpForks: an instance's fork id is the name of mrp's fork
// dir: one index per fork, a mixed-radix index over parts of known length,
// '_' before a part of variable length, and a map key (fork_mid
// MID/SCALE/fork*, zip_literal ZIP_LITERAL/INNER_SHORT/PAIR/fork*,
// map_fork_upstream MFU/DBL/fork_beta).
func TestForkIDNamesMrpForks(t *testing.T) {
	cases := []struct {
		fixture, node string
		up, upOuts    string
		want          []string
	}{
		{"fork_mid", "MID.SCALE", "", "", []string{"fork0", "fork1", "fork2"}},
		{
			"zip_literal", "ZIP_LITERAL.INNER_SHORT.PAIR", "ZIP_LITERAL.TWO", `{"xs":[1,2]}`,
			[]string{"fork0_fork0", "fork0_fork1", "fork1", "fork2_fork0", "fork2_fork1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			f := loadFixture(t, tc.fixture)

			entry := f.next("entry")
			if err := f.g.PostEntry(entry); err != nil {
				t.Fatal(err)
			}

			st, err := f.g.Stage(tc.node)
			if err != nil {
				t.Fatal(err)
			}

			var ups []string
			if tc.up != "" {
				ups = append(ups, f.outs(tc.up, nil, tc.upOuts))
			}

			got := forkIDs(t, f, tc.node, entry, st.ForkRoots()[len(st.ForkRoots())-1].Fqid, ups)
			if d := cmp.Diff(tc.want, got); d != "" {
				t.Errorf("fork ids differ from mrp's fork dirs (-mrp +got):\n%s", d)
			}
		})
	}

	f := loadFixture(t, "map_fork_upstream")
	gen := f.outs("MFU.GEN", nil, `{"m":{"alpha":1,"beta":2,"gamma":3}}`)
	elems := elementItems(t, f.post(gen, gen), "MFU.DBL")

	if inst, _ := f.args("MFU.DBL", elems[1]); inst.ForkID() != "fork_beta" {
		t.Errorf("fork id %q, mrp's fork dir is fork_beta", inst.ForkID())
	}
}

// forkIDs are the fork ids of node's instances, one per element of its last
// fork root, each staged with ups and the element of its outer root it
// names.
func forkIDs(t *testing.T, f *fixture, node, entry, last string, ups []string) []string {
	t.Helper()

	elems := elementItems(t, entry, last)
	ids := make([]string, 0, len(elems))

	for _, el := range elems {
		in := append([]string{el}, ups...)

		rec, err := f.g.ReadElement(el)
		if err != nil {
			t.Fatal(err)
		}

		if len(rec.Fork) > 1 {
			outer := elementItems(t, entry, rec.Fork[0].Root.Fqid)
			in = append(in, outer[rec.Fork[0].Index.ArrayIndex()])
		}

		inst, _ := f.args(node, in...)
		ids = append(ids, inst.ForkID())
	}

	return ids
}
