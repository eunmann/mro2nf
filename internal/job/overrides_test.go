package job

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/martian-lang/martian/martian/core"
)

// fixtureOverrides is testdata/overrides_resources's overrides file, read
// as mrp reads it.
func fixtureOverrides(t *testing.T) *core.PipestanceOverrides {
	t.Helper()

	return readOverrides(t, filepath.Join("..", "..", "testdata", "overrides_resources", "overrides.json"))
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
	r := Runtime{PSID: "mrp", Overrides: fixtureOverrides(t), Local: mrpPool()}

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

		if got := (Runtime{PSID: "other", Overrides: own, Local: mrpPool()}).Resources(tc.st, tc.phase, tc.def); got != tc.want {
			t.Errorf("%s: under its own overrides %s, %s %s resources %+v, want %+v", tc.name, raw, tc.st.Fqid, tc.phase, got, tc.want)
		}
	}
}

// TestOverrideHoldsWhatAppliesToTheNode: a node's overrides file holds,
// under its own fqid, only the resources and profiles a key sets of the
// phases it runs, so it changes only with those, and is nil where no key
// applies.
func TestOverrideHoldsWhatAppliesToTheNode(t *testing.T) {
	full := Runtime{PSID: "mrp", Overrides: fixtureOverrides(t)}

	cases := []struct {
		st   Stage
		want string
	}{
		{Stage{Fqid: "OVR.INNER.PREP"}, `{"OVR.INNER.PREP":{"chunk.threads":0.5,"chunk.mem_gb":1.5,"chunk.profile":"disable"}}`},
		{Stage{Fqid: "OVR.INNER.SQUARES", Split: true}, `{"OVR.INNER.SQUARES":{"join.threads":1.5,"join.vmem_gb":7,"join.profile":"mem",` +
			`"chunk.threads":1.25,"chunk.mem_gb":3,"chunk.profile":"cpu","split.mem_gb":2.5,"split.vmem_gb":6,"split.profile":"mem"}}`},
		{Stage{Fqid: "OVR.EACH"}, `{"OVR.EACH":{"chunk.mem_gb":0.75,"chunk.profile":"no such mode"}}`},
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

// TestOverrideHoldsAProfileAlone: a profile is what applies to a node, as
// a resource is, only for a phase the node runs; an empty mode is one a
// key sets.
func TestOverrideHoldsAProfileAlone(t *testing.T) {
	cases := []struct {
		file, want string
	}{
		{`{"P": {"chunk.profile": "cpu"}}`, `{"P.S":{"chunk.profile":"cpu"}}`},
		{`{"P": {"chunk.profile": ""}}`, `{"P.S":{"chunk.profile":""}}`},
		{`{"P": {"join.profile": "cpu", "split.profile": "mem"}}`, ""},
	}

	for _, tc := range cases {
		path := filepath.Join(t.TempDir(), "overrides.json")
		if err := os.WriteFile(path, []byte(tc.file), 0o644); err != nil {
			t.Fatal(err)
		}

		raw, err := (Runtime{PSID: "mrp", Overrides: readOverrides(t, path)}).Override(Stage{Fqid: "P.S"})
		if err != nil {
			t.Fatal(err)
		}

		if string(raw) != tc.want {
			t.Errorf("%s: overrides %s, want %s", tc.file, raw, tc.want)
		}
	}
}

// memProfile is the profile_config mrp writes for the mem profile
// (jobmanagers/config.json).
const memProfile = `{"adapter":"mem","env":{"HEAPPROFILE":"${RAW_PERF_DEST}.tcmalloc.pprof",` +
	`"MALLOC_CONF":"prof:true,prof_prefix:${RAW_PERF_DEST}.jeprof,lg_prof_sample:21"}}`

// TestProfileUnderOverrides: each job's _jobinfo holds the profile_mode
// and profile_config mrp wrote for overrides_resources under the fixture's
// overrides, by the nearest key that sets its phase's profile, under the
// run's whole file and under its node's own (Override): an adapter mode,
// mem's environment, an explicit disable, and a mode no profile names,
// which mrp passes on with no config.
func TestProfileUnderOverrides(t *testing.T) {
	cases := []struct {
		fqid, phase, mode, config string
	}{
		{"OVR.INNER.PREP", PhaseMain, "disable", ""},
		{"OVR.INNER.SQUARES", PhaseSplit, "mem", memProfile},
		{"OVR.INNER.SQUARES", PhaseMain, "cpu", `{"adapter":"cpu"}`},
		{"OVR.INNER.SQUARES", PhaseJoin, "mem", memProfile},
		{"OVR.REPORT", PhaseMain, "cpu", `{"adapter":"cpu"}`},
		{"OVR.OTHER", PhaseMain, "cpu", `{"adapter":"cpu"}`},
		{"OVR.EACH", PhaseMain, "no such mode", ""},
	}

	full := fixtureOverrides(t)

	for _, tc := range cases {
		s, inv := stage(t, "overrides_resources", tc.fqid,
			`[ "$1" = split ] && echo '{"chunks":[]}' > "$meta/_stage_defs"; touch "$meta/_complete"`)

		over, err := (Runtime{PSID: "mrp", Overrides: full}).Override(s)
		if err != nil {
			t.Fatal(err)
		}

		own := filepath.Join(t.TempDir(), "overrides.json")
		if err := os.WriteFile(own, over, 0o644); err != nil {
			t.Fatal(err)
		}

		for name, file := range map[string]*core.PipestanceOverrides{"run's": full, "node's": readOverrides(t, own)} {
			r := runtime(t, inv)
			r.Overrides = file

			got := runPhase(t, r, s, tc.phase)

			want := map[string]any{"profile_mode": tc.mode}
			if tc.config != "" {
				want["profile_config"] = decode(t, tc.config)
			}

			if d := cmp.Diff(want, got); d != "" {
				t.Errorf("%s %s under the %s overrides: _jobinfo profile differs from mrp's (-mrp +got):\n%s", tc.fqid, tc.phase, name, d)
			}
		}
	}
}

// TestProfileOfAJob: a perf override gives the job mrp's perf config,
// whose cmd mrjob runs, under its own mode, and an empty mode leaves
// profile_mode out with no config, as mrp's _jobinfo omits it (its py
// adapter then fails the job on the missing key, as under mrp).
func TestProfileOfAJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overrides.json")
	if err := os.WriteFile(path, []byte(`{"P": {"chunk.profile": "perf", "split.profile": ""}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	r := runtime(t, nil)
	r.Overrides = readOverrides(t, path)

	perf := `{"cmd":"perf","args":["record","-g","-F","${MRO_PERF_FREQ}","-o","${RAW_PERF_DEST}","-e","${MRO_PERF_EVENTS}",` +
		`"-p","${STAGE_PID}","sleep","${MRO_PERF_DURATION}"],` +
		`"defaults":{"MRO_PERF_DURATION":"2400","MRO_PERF_EVENTS":"task-clock","MRO_PERF_FREQ":"200"}}`

	for _, tc := range []struct {
		phase, sub string
		want       map[string]any
	}{
		{PhaseMain, "chnk0", map[string]any{"profile_mode": "perf", "profile_config": decode(t, perf)}},
		{PhaseSplit, PhaseSplit, map[string]any{}},
	} {
		j, err := r.newJob(Instance{Stage: Stage{Fqid: "P.S"}, ForkID: "fork0"}, tc.phase, tc.sub)
		if err != nil {
			t.Fatal(err)
		}

		raw, err := json.Marshal(r.jobInfo(j))
		if err != nil {
			t.Fatal(err)
		}

		if d := cmp.Diff(tc.want, profileKeys(decode(t, string(raw)))); d != "" {
			t.Errorf("%s: _jobinfo profile differs from mrp's (-mrp +got):\n%s", tc.phase, d)
		}
	}
}

func readOverrides(t *testing.T, path string) *core.PipestanceOverrides {
	t.Helper()

	over, err := core.ReadOverrides(path)
	if err != nil {
		t.Fatal(err)
	}

	return over
}

// runPhase runs the phase job of s's fork0 and returns its _jobinfo's
// profile_mode and profile_config.
func runPhase(t *testing.T, r Runtime, s Stage, phase string) map[string]any {
	t.Helper()

	in, ctx := Instance{Stage: s, ForkID: "fork0"}, context.Background()

	var (
		err error
		sub = phase
	)

	switch phase {
	case PhaseSplit:
		_, err = r.Split(ctx, in, core.LazyArgumentMap{})
	case PhaseJoin:
		_, err = r.Join(ctx, in, core.LazyArgumentMap{}, &core.StageDefs{}, nil)
	default:
		sub = ChunkName(0, 1)
		_, err = r.Main(ctx, in, core.LazyArgumentMap{}, Unsplit())
	}

	if err != nil {
		t.Fatal(err)
	}

	return profileKeys(readJSON(t, filepath.Join(r.Work, sub, "_jobinfo")))
}

// profileKeys are the profile_mode and profile_config of a decoded
// _jobinfo, each only if it holds it.
func profileKeys(info any) map[string]any {
	m, _ := info.(map[string]any)
	got := map[string]any{}

	for _, k := range []string{"profile_mode", "profile_config"} {
		if v, ok := m[k]; ok {
			got[k] = v
		}
	}

	return got
}
