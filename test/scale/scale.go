// Package scale judges the nightly scale tier (make test-scale). Each bench
// shape runs at a narrow and a wide width, and its
// orchestration overhead must not grow with the width (the CLAUDE.md overhead
// rule). A gate that an open issue still fails is a known failure keyed to
// that issue. Once the gate passes, the known failure fails the tier until it
// is removed: the staleness rule test/e2e/divergences.json applies to its
// issue-tracked entries.
package scale

import (
	"fmt"
	"maps"
	"slices"
)

// Metrics is the part of a bench_metrics.py line the gates and the summary
// read.
type Metrics struct {
	Name          string `json:"name"`
	Tasks         int    `json:"tasks"`
	PlumbingTasks int    `json:"plumbing_tasks"`
	StageTasks    int    `json:"stage_tasks"`
	MrpJobs       int    `json:"mrp_jobs"`
	StagedBytes   int64  `json:"staged_bytes"`
	UploadedBytes int64  `json:"uploaded_bytes"`
	// StagedBytesMax is, per stage process, the most bytes staged into one
	// of its tasks.
	StagedBytesMax  map[string]int64 `json:"stage_staged_bytes_max"`
	ProbeWrites     int              `json:"probe_writes"`
	Producers       int              `json:"producers"`
	DriverHeapBytes int64            `json:"driver_heap_bytes"`
	MaxArgBytes     int              `json:"max_arg_bytes"`
	WallSeconds     float64          `json:"wall_seconds"`
}

// Summary is m on one line, for the tier's report.
func (m Metrics) Summary() string {
	return fmt.Sprintf("%s: %d tasks (%d stage, %d plumbing), %d MiB staged, %d MiB uploaded, "+
		"driver heap %d MiB, longest argument %d B, %.0f s",
		m.Name, m.Tasks, m.StageTasks, m.PlumbingTasks, mib(m.StagedBytes), mib(m.UploadedBytes),
		mib(m.DriverHeapBytes), m.MaxArgBytes, m.WallSeconds)
}

const mibShift = 20

func mib(b int64) int64 { return b >> mibShift }

// Run is one shape, measured at both widths.
type Run struct {
	Shape  string
	Narrow Metrics
	Wide   Metrics
	// Gathers are the stage processes that read one input per fork or chunk
	// of the width, such as the join of a split as wide as the width, so
	// their staged bytes grow with it as the data does.
	Gathers []string
}

// Gate names one check of a Run.
type Gate string

// The gates.
const (
	// GatePlumbingTasks requires the same plumbing task count at both widths.
	GatePlumbingTasks Gate = "plumbing-tasks"
	// GateMrpJobs requires one stage task per mrp job at both widths.
	GateMrpJobs Gate = "stage-tasks-equal-mrp-jobs"
	// GateStagedBytes requires, per stage process, the most bytes staged into
	// one of its tasks to stay within Limits.StagedSlackBytes of the narrow
	// width's. It is judged per process because the mix of processes among
	// the stage tasks changes with the width.
	GateStagedBytes Gate = "staged-bytes-per-stage-task"
	// GateProbeWrites requires the same number of copies of the probe file
	// per producer at both widths: a task that copies a broadcast file into
	// every fork writes one more per fork.
	GateProbeWrites Gate = "probe-writes-per-producer"
	// GateArgBytes requires the longest command-line argument of any task to
	// stay within Limits.ArgSlackBytes of the narrow width's: an argument
	// that lists every fork or chunk hits Linux's 128 KiB MAX_ARG_STRLEN.
	GateArgBytes Gate = "max-arg-bytes"
	// GateDriverHeap requires the wide run's driver heap left after a GC to
	// stay under Limits.HeapBytes.
	GateDriverHeap Gate = "driver-heap"
)

// Limits are the slack and budgets the gates allow.
type Limits struct {
	StagedSlackBytes int64
	ArgSlackBytes    int64
	HeapBytes        int64
}

// Result is one gate's outcome for one Run, for one stage process of
// GateStagedBytes; Failure is "" when it passes.
type Result struct {
	Shape   string
	Gate    Gate
	Process string
	Failure string
}

func (r Result) id() string {
	id := fmt.Sprintf("%s %s", r.Shape, r.Gate)
	if r.Process != "" {
		id += " " + r.Process
	}

	return id
}

// Evaluate applies every gate to r.
func Evaluate(r Run, lim Limits) []Result {
	n, w := r.Narrow, r.Wide

	fixed := []Result{
		{Gate: GatePlumbingTasks, Failure: unequal("plumbing tasks", n.PlumbingTasks, w.PlumbingTasks)},
		{Gate: GateMrpJobs, Failure: mrpJobs(n, w)},
		{Gate: GateProbeWrites, Failure: probeWrites(n, w)},
		{Gate: GateArgBytes, Failure: grows("longest argument bytes", int64(n.MaxArgBytes), int64(w.MaxArgBytes), lim.ArgSlackBytes)},
		{Gate: GateDriverHeap, Failure: heap(w.DriverHeapBytes, lim.HeapBytes)},
	}

	out := slices.Concat(fixed, stagedBytes(r, lim.StagedSlackBytes))

	for i := range out {
		out[i].Shape = r.Shape
	}

	return out
}

func unequal(what string, narrow, wide int) string {
	if narrow == wide {
		return ""
	}

	return fmt.Sprintf("%s grow with width: %d narrow, %d wide", what, narrow, wide)
}

func mrpJobs(narrow, wide Metrics) string {
	for _, m := range []Metrics{narrow, wide} {
		if m.StageTasks != m.MrpJobs {
			return fmt.Sprintf("%d stage tasks, want %d (mrp's jobs)", m.StageTasks, m.MrpJobs)
		}
	}

	return ""
}

// stagedBytes is GateStagedBytes's result for each stage process of the
// wide run that is not a gather. Every shape stages files between its tasks,
// so a width at which no task staged a byte was not measured: the metrics no
// longer read the staging Nextflow writes.
func stagedBytes(r Run, slack int64) []Result {
	if r.Narrow.StagedBytes == 0 || r.Wide.StagedBytes == 0 {
		return []Result{{Gate: GateStagedBytes, Failure: fmt.Sprintf(
			"no task staged anything: %d bytes narrow, %d wide", r.Narrow.StagedBytes, r.Wide.StagedBytes)}}
	}

	if len(r.Wide.StagedBytesMax) == 0 {
		return []Result{{Gate: GateStagedBytes, Failure: "no stage task staged anything"}}
	}

	var out []Result

	for _, p := range slices.Sorted(maps.Keys(r.Wide.StagedBytesMax)) {
		if slices.Contains(r.Gathers, p) {
			continue
		}

		res := Result{Gate: GateStagedBytes, Process: p}

		if n, ok := r.Narrow.StagedBytesMax[p]; ok {
			res.Failure = grows("bytes staged into one task", n, r.Wide.StagedBytesMax[p], slack)
		} else {
			res.Failure = "the process runs only at the wide width"
		}

		out = append(out, res)
	}

	return out
}

func grows(what string, narrow, wide, slack int64) string {
	if wide <= narrow+slack {
		return ""
	}

	return fmt.Sprintf("%s grow with width: %d narrow, %d wide (slack %d)", what, narrow, wide, slack)
}

// probeWrites compares copies per producer exactly, as a ratio of integers,
// so a shape with one producer per fork is held to the same ratio.
func probeWrites(narrow, wide Metrics) string {
	if narrow.Producers == 0 || wide.Producers == 0 {
		return fmt.Sprintf("no probe producers: %d narrow, %d wide", narrow.Producers, wide.Producers)
	}

	if narrow.ProbeWrites*wide.Producers == wide.ProbeWrites*narrow.Producers {
		return ""
	}

	return fmt.Sprintf("probe file copies per producer grow with width: %d/%d narrow, %d/%d wide",
		narrow.ProbeWrites, narrow.Producers, wide.ProbeWrites, wide.Producers)
}

func heap(got, budget int64) string {
	switch {
	case got == 0:
		return "no driver GC log was read"
	case got > budget:
		return fmt.Sprintf("driver heap %d MiB after GC exceeds the %d MiB budget", mib(got), mib(budget))
	default:
		return ""
	}
}

// Known is a gate an open issue makes fail for one shape, and for one stage
// process of GateStagedBytes.
type Known struct {
	Shape   string
	Gate    Gate
	Process string
	Issue   int
}

func (k Known) matches(r Result) bool {
	return k.Shape == r.Shape && k.Gate == r.Gate && k.Process == r.Process
}

// Verdict is the tier's judgment of a shape's results: Failures fail it,
// Notes are known failures that still fail as recorded.
type Verdict struct {
	Failures []string
	Notes    []string
}

// Judge holds the results of one shape to the known failures recorded for
// it. An unrecorded failure fails the tier, as does a known failure whose
// gate passes or that matches no result.
func Judge(shape string, results []Result, known []Known) Verdict {
	var v Verdict

	for _, r := range results {
		i := slices.IndexFunc(known, func(k Known) bool { return k.matches(r) })

		switch {
		case i < 0 && r.Failure != "":
			v.Failures = append(v.Failures, fmt.Sprintf("%s: %s", r.id(), r.Failure))
		case i >= 0 && r.Failure == "":
			v.Failures = append(v.Failures, fmt.Sprintf("%s: stale known failure, remove it: the gate passes, so #%d no longer fails it", r.id(), known[i].Issue))
		case i >= 0:
			v.Notes = append(v.Notes, fmt.Sprintf("%s: known failure #%d: %s", r.id(), known[i].Issue, r.Failure))
		}
	}

	for _, k := range known {
		if k.Shape == shape && !slices.ContainsFunc(results, k.matches) {
			r := Result{Shape: k.Shape, Gate: k.Gate, Process: k.Process}
			v.Failures = append(v.Failures, fmt.Sprintf("%s: known failure #%d matches no result of the tier", r.id(), k.Issue))
		}
	}

	return v
}
