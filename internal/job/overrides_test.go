package job

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/martian-lang/martian/martian/core"
)

// fixtureOverrides is testdata/overrides_resources's overrides file, read
// as mrp reads it.
func fixtureOverrides(t *testing.T) *core.PipestanceOverrides {
	t.Helper()

	over, err := core.ReadOverrides(filepath.Join("..", "..", "testdata", "overrides_resources", "overrides.json"))
	if err != nil {
		t.Fatal(err)
	}

	return over
}

// overrideCase is one job of overrides_resources and the threads, memGB
// and vmemGB of the _jobinfo mrp wrote for it under the fixture's
// overrides (mrp --overrides, run as the e2e harness runs mrp).
type overrideCase struct {
	name  string
	st    Stage
	phase string
	def   *core.JobResources
	want  core.JobResources
}

func overrideCases() []overrideCase {
	prep := Stage{Fqid: "OVR.INNER.PREP", Using: &core.JobResources{Threads: 1, MemGB: 1}}
	squares := Stage{Fqid: "OVR.INNER.SQUARES", Split: true, Using: &core.JobResources{MemGB: 1}}

	return []overrideCase{
		{"stage key over a pipeline's", prep, PhaseMain, nil, core.JobResources{Threads: 0.5, MemGB: 1.5, VMemGB: 4.5}},
		{"split", squares, PhaseSplit, nil, core.JobResources{Threads: 1, MemGB: 2.5, VMemGB: 6}},
		{"over a chunk's request", squares, PhaseMain, &core.JobResources{Threads: 2, MemGB: 2}, core.JobResources{Threads: 1.25, MemGB: 3, VMemGB: 6}},
		{"chunk", squares, PhaseMain, &core.JobResources{}, core.JobResources{Threads: 1.25, MemGB: 3, VMemGB: 6}},
		{"join beside its request", squares, PhaseJoin, &core.JobResources{MemGB: 2}, core.JobResources{Threads: 1.5, MemGB: 2, VMemGB: 7}},
		{"top-level pipeline key, never the empty key", Stage{Fqid: "OVR.REPORT"}, PhaseMain, nil, core.JobResources{Threads: 1, MemGB: 1.5, VMemGB: 4.5}},
		{"aliased call", Stage{Fqid: "OVR.OTHER"}, PhaseMain, nil, core.JobResources{Threads: 2, MemGB: 2.25, VMemGB: 5}},
		{"map call", Stage{Fqid: "OVR.EACH"}, PhaseMain, nil, core.JobResources{Threads: 1, MemGB: 0.75, VMemGB: 3.75}},
	}
}

// TestResourcesUnderOverrides: a job's resources under an overrides file
// are mrp's: the nearest key on its fqname sets each resource of its
// phase, over the stage's using() and over the def its split returned, and
// the empty key sets none.
func TestResourcesUnderOverrides(t *testing.T) {
	r := Runtime{PSID: "mrp", Overrides: fixtureOverrides(t)}

	for _, tc := range overrideCases() {
		if got := r.Resources(tc.st, tc.phase, tc.def); got != tc.want {
			t.Errorf("%s: %s %s resources %+v, mrp's %+v", tc.name, tc.st.Fqid, tc.phase, got, tc.want)
		}
	}
}

// TestOverrideGivesTheNodeItsResources: the overrides file Override writes
// for a node gives each of its jobs, read back as mrp reads a file, the
// resources the run's whole file gives it.
func TestOverrideGivesTheNodeItsResources(t *testing.T) {
	full := Runtime{PSID: "mrp", Overrides: fixtureOverrides(t)}

	for _, tc := range overrideCases() {
		raw, err := full.Override(tc.st)
		if err != nil {
			t.Fatal(err)
		}

		path := filepath.Join(t.TempDir(), "overrides.json")
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}

		own, err := core.ReadOverrides(path)
		if err != nil {
			t.Fatalf("%s: read %s: %v", tc.name, raw, err)
		}

		if got := (Runtime{PSID: "other", Overrides: own}).Resources(tc.st, tc.phase, tc.def); got != tc.want {
			t.Errorf("%s: under its own overrides %s, %s %s resources %+v, want %+v", tc.name, raw, tc.st.Fqid, tc.phase, got, tc.want)
		}
	}
}

// TestOverrideHoldsWhatAppliesToTheNode: a node's overrides file holds,
// under its own fqid, only the resources a key sets of the phases it runs,
// so it changes only with those, and is nil where no key applies.
func TestOverrideHoldsWhatAppliesToTheNode(t *testing.T) {
	full := Runtime{PSID: "mrp", Overrides: fixtureOverrides(t)}

	cases := []struct {
		st   Stage
		want string
	}{
		{Stage{Fqid: "OVR.INNER.PREP"}, `{"OVR.INNER.PREP":{"chunk.threads":0.5,"chunk.mem_gb":1.5}}`},
		{Stage{Fqid: "OVR.INNER.SQUARES", Split: true}, `{"OVR.INNER.SQUARES":{"join.threads":1.5,"join.vmem_gb":7,"chunk.threads":1.25,"chunk.mem_gb":3,"split.mem_gb":2.5,"split.vmem_gb":6}}`},
		{Stage{Fqid: "ELSE.S"}, ""},
	}

	for _, tc := range cases {
		raw, err := full.Override(tc.st)
		if err != nil {
			t.Fatal(err)
		}

		if string(raw) != tc.want {
			t.Errorf("%s: overrides %s, want %s", tc.st.Fqid, raw, tc.want)
		}
	}

	if raw, err := (Runtime{}).Override(Stage{Fqid: "OVR.INNER.PREP"}); raw != nil || err != nil {
		t.Errorf("without overrides: %s, %v, want none", raw, err)
	}
}
