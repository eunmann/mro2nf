package scale

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// flat is a forks-like run whose wide width keeps every gate passing.
func flat() Run {
	narrow := Metrics{
		PlumbingTasks: 4, StageTasks: 5, MrpJobs: 5, StagedBytes: 1 << 20,
		StagedBytesMax:  map[string]int64{"MKPAY": 2500, "FUSE": 270000, "JOIN": 1000},
		CarriedBytesMax: map[string]int64{"MKPAY": 0, "FUSE": 120, "JOIN": 300},
		ProbeWrites:     2, Producers: 1, DriverHeapBytes: 80 << 20, MaxArgBytes: 74,
	}
	wide := Metrics{
		PlumbingTasks: 4, StageTasks: 1001, MrpJobs: 1001, StagedBytes: 256 << 20,
		StagedBytesMax:  map[string]int64{"MKPAY": 2510, "FUSE": 270001, "JOIN": 90000},
		CarriedBytesMax: map[string]int64{"MKPAY": 0, "FUSE": 132, "JOIN": 40000},
		ProbeWrites:     2, Producers: 1, DriverHeapBytes: 300 << 20, MaxArgBytes: 76,
	}

	return Run{Shape: "forks", Narrow: narrow, Wide: wide, Gathers: []string{"JOIN"}}
}

// failures returns the failing results of r, by id.
func failures(r Run) map[string]string {
	out := map[string]string{}

	for _, res := range Evaluate(r, Limits{StagedSlackBytes: 64, ArgSlackBytes: 64, HeapBytes: 512 << 20}) {
		if res.Failure != "" {
			out[res.id()] = res.Failure
		}
	}

	return out
}

func TestEvaluateFlatRunPasses(t *testing.T) {
	if got := failures(flat()); len(got) != 0 {
		t.Errorf("a width-flat run fails: %v", got)
	}
}

func TestEvaluateFailsEachGate(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*Run)
		id    string
		cause string
	}{
		{"plumbing", func(r *Run) { r.Wide.PlumbingTasks = 5 }, "forks plumbing-tasks", "4 narrow, 5 wide"},
		{"mrp jobs", func(r *Run) { r.Wide.StageTasks = 1002 }, "forks stage-tasks-equal-mrp-jobs", "1002 stage tasks, want 1001"},
		{"staged bytes", func(r *Run) { r.Wide.StagedBytesMax["MKPAY"] = 2565 }, "forks staged-bytes-per-stage-task MKPAY", "2500 narrow, 2565 wide"},
		{"gather is no exemption for another process", func(r *Run) { r.Wide.StagedBytesMax["FUSE"] = 280000 }, "forks staged-bytes-per-stage-task FUSE", "270000 narrow, 280000 wide"},
		{"process only wide", func(r *Run) { r.Wide.StagedBytesMax["NEW"] = 1 }, "forks staged-bytes-per-stage-task NEW", "only at the wide width"},
		{"nothing staged", func(r *Run) { r.Wide.StagedBytesMax = nil }, "forks staged-bytes-per-stage-task", "no stage task staged anything"},
		{"no task staged narrow", func(r *Run) { r.Narrow.StagedBytes = 0 }, "forks staged-bytes-per-stage-task", "no task staged anything"},
		{"no task staged wide", func(r *Run) { r.Wide.StagedBytes = 0 }, "forks staged-bytes-per-stage-task", "no task staged anything"},
		{"carried bytes", func(r *Run) { r.Wide.CarriedBytesMax["FUSE"] = 92720 }, "forks carried-bytes-per-stage-task FUSE", "120 narrow, 92720 wide"},
		{"carried only wide", func(r *Run) { r.Wide.CarriedBytesMax["NEW"] = 0 }, "forks carried-bytes-per-stage-task NEW", "only at the wide width"},
		{"carried unmeasured", func(r *Run) { r.Narrow.CarriedBytesMax = nil }, "forks carried-bytes-per-stage-task", "no stage process's carried bytes"},
		{"probe writes", func(r *Run) { r.Wide.ProbeWrites = 1002 }, "forks probe-writes-per-producer", "2/1 narrow, 1002/1 wide"},
		{"no producers", func(r *Run) { r.Narrow.Producers = 0 }, "forks probe-writes-per-producer", "no probe producers"},
		{"argument", func(r *Run) { r.Wide.MaxArgBytes = 131072 }, "forks max-arg-bytes", "74 narrow, 131072 wide"},
		{"heap", func(r *Run) { r.Wide.DriverHeapBytes = 600 << 20 }, "forks driver-heap", "600 MiB after GC exceeds the 512 MiB budget"},
		{"no gc log", func(r *Run) { r.Wide.DriverHeapBytes = 0 }, "forks driver-heap", "no driver GC log"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := flat()
			tc.edit(&r)

			got := failures(r)
			if len(got) != 1 || !strings.Contains(got[tc.id], tc.cause) {
				t.Errorf("failures = %v, want only %q naming %q", got, tc.id, tc.cause)
			}
		})
	}
}

func TestEvaluateProbeWritesPerProducer(t *testing.T) {
	r := flat()
	r.Narrow.ProbeWrites, r.Narrow.Producers = 28, 4
	r.Wide.ProbeWrites, r.Wide.Producers = 7000, 1000

	if got := failures(r); len(got) != 0 {
		t.Errorf("7 copies per producer at both widths fails: %v", got)
	}
}

func TestJudge(t *testing.T) {
	results := []Result{
		{Shape: "forks", Gate: GatePlumbingTasks},
		{Shape: "forks", Gate: GateProbeWrites, Failure: "copies grow"},
		{Shape: "forks", Gate: GateStagedBytes, Process: "FUSE", Failure: "bytes grow"},
		{Shape: "forks", Gate: GateStagedBytes, Process: "MKPAY"},
	}

	// recorded are the known failures of the two failing results, and noted
	// what Judge reports for them.
	recorded := []Known{
		{Shape: "forks", Gate: GateProbeWrites, Issue: 325},
		{Shape: "forks", Gate: GateStagedBytes, Process: "FUSE", Issue: 326},
	}
	noted := []string{
		"forks probe-writes-per-producer: known failure #325: copies grow",
		"forks staged-bytes-per-stage-task FUSE: known failure #326: bytes grow",
	}

	cases := []struct {
		name  string
		known []Known
		want  Verdict
	}{
		{
			name: "unrecorded failures fail",
			want: Verdict{Failures: []string{
				"forks probe-writes-per-producer: copies grow",
				"forks staged-bytes-per-stage-task FUSE: bytes grow",
			}},
		},
		{name: "known failures that still fail pass", known: recorded, want: Verdict{Notes: noted}},
		{
			name:  "a known failure whose gate passes is stale",
			known: append(slices.Clone(recorded), Known{Shape: "forks", Gate: GateStagedBytes, Process: "MKPAY", Issue: 285}),
			want: Verdict{
				Failures: []string{"forks staged-bytes-per-stage-task MKPAY: stale known failure, remove it: the gate passes, so #285 no longer fails it"},
				Notes:    noted,
			},
		},
		{
			name: "a known failure matching no result fails; another shape's is ignored",
			known: append(slices.Clone(recorded),
				Known{Shape: "forks", Gate: GateStagedBytes, Process: "GONE", Issue: 285},
				Known{Shape: "split", Gate: GatePlumbingTasks, Issue: 255}),
			want: Verdict{
				Failures: []string{"forks staged-bytes-per-stage-task GONE: known failure #285 matches no result of the tier"},
				Notes:    noted,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, Judge("forks", results, tc.known)); diff != "" {
				t.Errorf("Judge (-want +got):\n%s", diff)
			}
		})
	}
}
