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
	elems := elementItems(t, f.post(gen), "MFU.DBL")

	if inst, _ := f.args("MFU.DBL", elems[1]); f.forkID(inst) != "fork_beta" {
		t.Errorf("fork id %q, mrp's fork dir is fork_beta", f.forkID(inst))
	}
}

func (f *fixture) forkID(in Instance) string {
	f.t.Helper()

	id, err := f.g.ForkID(in.Node, in.Fork, in.Lens)
	if err != nil {
		f.t.Fatal(err)
	}

	return id
}

// TestForkIDAfterAnOuterPart pins the fork dirs upstream mrp (50ad6c0)
// names under an outer array fork: a map fork writes its key after the
// outer index, and a fork whose inner part is empty is the outer fork's.
func TestForkIDAfterAnOuterPart(t *testing.T) {
	f := loadSource(t, `stage MK(in int n, out map<int> km, src py "stages/mk",)
stage SQ(in int x, out int y, src py "stages/sq",)

pipeline SUB(in int n, out map<int> ys,)
{
    call MK(n = self.n,)
    map call SQ(x = split MK.km,)
    return (ys = SQ.y,)
}

pipeline TOP(out SUB[] subs,)
{
    map call SUB(n = split [2, 0, 3],)
    return (subs = SUB,)
}

call TOP()
`)

	sub := func(i int) Part { return f.part("TOP.SUB", ArrayIndex(i)) }
	sq := f.part("TOP.SUB.SQ", MapKey("x"))

	cases := []struct {
		fork Fork
		lens []int
		want string
	}{
		{Fork{sub(0), sq}, []int{3, 2}, "fork0/fork_x"},
		{Fork{sub(2), sq}, []int{3, 2}, "fork2/fork_x"},
		{Fork{sub(1), sq}, []int{3, 0}, "fork1"},
	}

	for _, tc := range cases {
		got, err := f.g.ForkID("TOP.SUB.SQ", tc.fork, tc.lens)
		if err != nil || got != tc.want {
			t.Errorf("ForkID of %s = %q, %v; mrp's fork dir is %q", tc.fork, got, err, tc.want)
		}
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
		ids = append(ids, f.forkID(inst))
	}

	return ids
}
