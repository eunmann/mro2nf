//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eunmann/mro2nf/test/scale"
)

// scaleShape is one bench fixture the scale tier runs at a narrow and a wide
// width. Its invocation, its mrp job and probe producer counts and its gather
// check are functions of the width.
type scaleShape struct {
	name   string
	probe  string
	narrow int
	wide   int // the default SCALE_WIDTH
	// chunked is set when the width is a split's chunk count, so the split's
	// join gathers one chunk output per unit of width.
	chunked bool
	call    func(width int) string
	jobs    func(width int) int
	prods   func(width int) int
	check   func(t *testing.T, outs string, width int)
}

// scaleShapes are the scale tier's shapes, keyed by bench fixture dir. Each
// default wide width runs about 10,000 tasks per mode, 20,000 per shape: the
// nightly runner ran the 1,000-fork forks shape, 2,020 tasks over both modes
// at once, in 315 s (6.4 tasks/s), and a 4,000-fork run's submit rate held
// steady, so 20,000 tasks take about 52 minutes of scaleShapeTimeout.
var scaleShapes = map[string]scaleShape{
	"forks": {
		name: "forks", probe: "MRE_BENCH_FORKS_PROBE", narrow: 4, wide: 10000,
		call:  func(w int) string { return fmt.Sprintf("FANOUT(\n    vs = [%s],\n    kb = 256,\n)", seq(w)) },
		jobs:  func(w int) int { return 1 + w },
		prods: func(int) int { return 1 },
		check: checkForksGather,
	},
	"split": {
		name: "split", probe: "MRE_BENCH_SPLIT_PROBE", narrow: 4, wide: 10000, chunked: true,
		call: func(w int) string {
			return fmt.Sprintf("WIDE(\n    nchunks = %d,\n    kb      = 512,\n)", w)
		},
		jobs:  func(w int) int { return w + 3 },
		prods: func(int) int { return 1 },
		check: checkSplitJoin,
	},
	"mapsplit": {
		name: "mapsplit", probe: "MRE_BENCH_MAPSPLIT_PROBE", narrow: 4, wide: 2000,
		call:  func(w int) string { return fmt.Sprintf("COHORT(\n    samples = [%s],\n)", seq(w)) },
		jobs:  func(w int) int { return 4 * w },
		prods: func(w int) int { return w },
		check: checkOutsLen("bams", 1),
	},
	"mapnested": {
		name: "mapnested", probe: "MRE_BENCH_MAPNESTED_PROBE", narrow: 2, wide: 100,
		call: func(w int) string {
			return fmt.Sprintf("OUTER(\n    ss = [%s],\n    ls = [%s],\n)", seq(w), seq(w))
		},
		jobs:  func(w int) int { return w * w },
		prods: func(w int) int { return w * w },
		check: checkOutsLen("fss", 2),
	},
	"mapdisabled": {
		name: "mapdisabled", probe: "MRE_BENCH_MAPDISABLED_PROBE", narrow: 4, wide: 2500,
		call:  func(w int) string { return fmt.Sprintf("OUTER(\n    xs = [%s],\n)", seq(w)) },
		jobs:  func(w int) int { return w },
		prods: func(w int) int { return w },
		check: checkOutsLen("fs", 1),
	},
}

// The open issues behind the known failures, beside issuePerForkPlumbing.
const (
	// A task stages the whole pipeline-args bundle, whatever it binds.
	issueOverStaging = 285
	// The default FORK writes each broadcast file into every fork bundle.
	issueBroadcastCopies = 325
	// An element-scatter instance stages the whole collection it scatters.
	issueScatterStaging = 326
)

// scaleKnown are the gates open issues fail at the default widths, each
// removed once its issue is fixed: scale.Judge fails a known failure whose
// gate passes. A byte gate's growth is linear in the width, so a SCALE_WIDTH
// small enough to keep it within the slack reports its entry stale.
var scaleKnown = []scale.Known{
	{Shape: "forks", Mode: "default", Gate: scale.GateProbeWrites, Issue: issueBroadcastCopies},
	{Shape: "forks", Mode: "default", Gate: scale.GateStagedBytes, Process: "STAGE_6_FANOUT__MKPAY", Issue: issueOverStaging},
	{Shape: "forks", Mode: "native", Gate: scale.GateStagedBytes, Process: "STAGE_6_FANOUT__MKPAY", Issue: issueOverStaging},
	{Shape: "forks", Mode: "native", Gate: scale.GateStagedBytes, Process: "STAGE_6_FANOUT__FUSE", Issue: issueScatterStaging},
	{Shape: "mapsplit", Mode: "default", Gate: scale.GatePlumbingTasks, Issue: issuePerForkPlumbing},
	{Shape: "mapsplit", Mode: "native", Gate: scale.GatePlumbingTasks, Issue: issuePerForkPlumbing},
	{Shape: "mapnested", Mode: "default", Gate: scale.GatePlumbingTasks, Issue: issuePerForkPlumbing},
	{Shape: "mapnested", Mode: "native", Gate: scale.GatePlumbingTasks, Issue: issuePerForkPlumbing},
	{Shape: "mapnested", Mode: "default", Gate: scale.GateStagedBytes, Process: "FORK_5_INNER__CELL_KS", Issue: issueScatterStaging},
	{Shape: "mapnested", Mode: "native", Gate: scale.GateStagedBytes, Process: "FORK_5_INNER__CELL_KS", Issue: issueScatterStaging},
	{Shape: "mapdisabled", Mode: "default", Gate: scale.GatePlumbingTasks, Issue: issuePerForkPlumbing},
	{Shape: "mapdisabled", Mode: "native", Gate: scale.GatePlumbingTasks, Issue: issuePerForkPlumbing},
}

// scaleLimits are the slack and budgets of the scale gates.
var scaleLimits = scale.Limits{
	StagedSlackBytes: 64,
	ArgSlackBytes:    64,
	HeapBytes:        768 << 20,
}

// scaleJVM caps the driver heap above scaleLimits.HeapBytes, so a heap over
// budget fails the heap gate with its size rather than the run with an
// OutOfMemoryError.
const scaleJVM = "-Xmx2g -Xlog:gc:file=%s/gc-%%p.log"

// scaleShapeTimeout is the go test timeout one shape at its default widths
// runs under. The shapes run one after another.
const scaleShapeTimeout = 100 * time.Minute

// TestMakeScaleTimeoutFitsEveryShape requires make test-scale's default
// SCALE_TIMEOUT, which its default SCALE_RUN spends on every shape, to give
// each shape scaleShapeTimeout. Its name keeps it out of ^TestScale.
func TestMakeScaleTimeoutFitsEveryShape(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}

	m := regexp.MustCompile(`(?m)^SCALE_TIMEOUT \?= (\S+)$`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("Makefile sets no default SCALE_TIMEOUT")
	}

	got, err := time.ParseDuration(string(m[1]))
	if want := time.Duration(len(scaleShapes)) * scaleShapeTimeout; err != nil || got < want {
		t.Errorf("Makefile SCALE_TIMEOUT ?= %s (%v), want at least %v for %d shapes", m[1], err, want, len(scaleShapes))
	}
}

func TestScaleForks(t *testing.T)       { runScaleShape(t, scaleShapes["forks"]) }
func TestScaleSplit(t *testing.T)       { runScaleShape(t, scaleShapes["split"]) }
func TestScaleMapSplit(t *testing.T)    { runScaleShape(t, scaleShapes["mapsplit"]) }
func TestScaleMapNested(t *testing.T)   { runScaleShape(t, scaleShapes["mapnested"]) }
func TestScaleMapDisabled(t *testing.T) { runScaleShape(t, scaleShapes["mapdisabled"]) }

// runScaleShape runs s at its narrow and wide widths in default and -native
// mode, archives the metrics under SCALE_ARTIFACTS, and judges the gates
// against scaleKnown.
func runScaleShape(t *testing.T, s scaleShape) {
	t.Helper()

	requireTools(t, "nextflow", "java", "python3", "bash")

	width := scaleWidth(t, s)

	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join(root, "bench", s.name))); err != nil {
		t.Fatalf("copy bench/%s: %v", s.name, err)
	}

	mre := writeArgvProbe(t)
	mros := map[int]string{
		s.narrow: writeScaleInvocation(t, dir, s, s.narrow),
		width:    writeScaleInvocation(t, dir, s, width),
	}

	var gathers []string
	if s.chunked {
		gathers = joinProcesses(t, dir, mros[width])
	}

	var (
		mu    sync.Mutex
		runs  []scale.Run
		lines bytes.Buffer
	)

	ok := t.Run("lanes", func(t *testing.T) {
		for _, mode := range []string{"default", "native"} {
			t.Run(mode, func(t *testing.T) {
				t.Parallel()

				r, out := runScaleMode(t, s, scaleMode{dir: dir, mre: mre, mros: mros, mode: mode}, width)
				r.Gathers = gathers

				mu.Lock()
				defer mu.Unlock()

				runs = append(runs, r)
				lines.Write(out)
			})
		}
	})

	writeScaleArtifact(t, s.name+".jsonl", lines.Bytes())

	if !ok {
		t.Fatal("a scale lane did not complete; skipping the gates")
	}

	judgeScale(t, s.name, runs)
}

// scaleMode is where runScaleMode finds a shape's invocations, by width,
// the mre they call, and the transpile mode.
type scaleMode struct {
	dir  string
	mre  string
	mros map[int]string
	mode string
}

// runScaleMode runs s in m's mode at both widths and checks the wide run's
// gather, returning the Run and both metrics lines.
func runScaleMode(t *testing.T, s scaleShape, m scaleMode, width int) (scale.Run, []byte) {
	t.Helper()

	r := scale.Run{Shape: s.name, Mode: m.mode}

	var out []byte

	for _, w := range []int{s.narrow, width} {
		l := benchLane{
			key: fmt.Sprintf("%s_w%d_%s", s.name, w, m.mode), dir: s.name, mro: m.mros[w],
			probe: s.probe, producers: s.prods(w), mrpJobs: s.jobs(w), native: m.mode == "native",
		}

		gc := t.TempDir()

		line, proj := runLane(t, l, laneRun{dir: m.dir, mre: m.mre, jvm: fmt.Sprintf(scaleJVM, gc), gcLogs: gc})
		out = append(out, line...)

		var got scale.Metrics
		if err := json.Unmarshal(line, &got); err != nil {
			t.Fatalf("decode %s metrics %q: %v", l.key, line, err)
		}

		if w == s.narrow {
			r.Narrow = got
		} else {
			r.Wide = got

			s.check(t, filepath.Join(proj, "results", "pipeline_outs.json"), w)
		}
	}

	return r, out
}

// judgeScale applies the gates to runs, fails on every unrecorded failure
// and stale known failure, and archives the verdict.
func judgeScale(t *testing.T, shape string, runs []scale.Run) {
	t.Helper()

	slices.SortFunc(runs, func(a, b scale.Run) int { return strings.Compare(a.Mode, b.Mode) })

	results := make([]scale.Result, 0, len(runs))
	for _, r := range runs {
		results = append(results, scale.Evaluate(r, scaleLimits)...)
	}

	v := scale.Judge(shape, results, scaleKnown)

	var report strings.Builder

	for _, r := range runs {
		fmt.Fprintf(&report, "%s\n%s\n", r.Narrow.Summary(), r.Wide.Summary())
	}

	for _, n := range v.Notes {
		t.Log(n)
		fmt.Fprintf(&report, "KNOWN %s\n", n)
	}

	for _, f := range v.Failures {
		t.Error(f)
		fmt.Fprintf(&report, "FAIL %s\n", f)
	}

	writeScaleArtifact(t, shape+".txt", []byte(report.String()))
}

// scaleWidth reads SCALE_WIDTH, the wide width of every shape the run
// selects, which must exceed the shape's narrow width; unset, it is the
// shape's default.
func scaleWidth(t *testing.T, s scaleShape) int {
	t.Helper()

	v := os.Getenv("SCALE_WIDTH")
	if v == "" {
		return s.wide
	}

	w, err := strconv.Atoi(v)
	if err != nil || w <= s.narrow {
		t.Fatalf("SCALE_WIDTH=%q: want an integer above %s's narrow width %d", v, s.name, s.narrow)
	}

	return w
}

// writeScaleInvocation writes s's invocation at width into dir and returns
// its file name.
func writeScaleInvocation(t *testing.T, dir string, s scaleShape, width int) string {
	t.Helper()

	name := fmt.Sprintf("pipeline_scale_w%d.mro", width)
	src := "@include \"common.mro\"\n\ncall " + s.call(width) + "\n"

	if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}

	return name
}

// joinProcesses returns the join-phase process names, the STAGE_JOIN
// aliases, of the project transpiled from dir/mro.
func joinProcesses(t *testing.T, dir, mro string) []string {
	t.Helper()

	names, err := stageAliases(transpileMRO(t, dir, mro))
	if err != nil {
		t.Fatal(err)
	}

	return slices.DeleteFunc(names, func(name string) bool { return !strings.HasSuffix(name, "_JN") })
}

// seq is the MRO int array body 1, 2, ..., n.
func seq(n int) string {
	vs := make([]string, n)
	for i := range vs {
		vs[i] = strconv.Itoa(i + 1)
	}

	return strings.Join(vs, ", ")
}

// argvProbeScript wraps mre: inside a task (its cwd holds .command.sh) it
// appends the command line's bytes and its longest argument's bytes to
// .mre-argv, which bench_metrics.py reads.
const argvProbeScript = `#!/bin/bash
if [ -e .command.sh ]; then
  LC_ALL=C
  total=0 longest=0
  for a in %[1]q "$@"; do
    total=$((total + ${#a} + 1))
    if [ "${#a}" -gt "$longest" ]; then longest=${#a}; fi
  done
  echo "$total $longest" >> .mre-argv
fi
exec %[1]q "$@"
`

// writeArgvProbe writes the argv probe around the repo's mre and returns
// its path.
func writeArgvProbe(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "mre")
	src := fmt.Sprintf(argvProbeScript, filepath.Join(root, "mre"))

	if err := os.WriteFile(path, []byte(src), 0o755); err != nil {
		t.Fatalf("write argv probe: %v", err)
	}

	return path
}

// writeScaleArtifact writes data as name under SCALE_ARTIFACTS (default
// artifacts/scale), which the nightly uploads.
func writeScaleArtifact(t *testing.T, name string, data []byte) {
	t.Helper()

	dir := rootPath(envOr("SCALE_ARTIFACTS", filepath.Join("artifacts", "scale")))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}

	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// checkForksGather requires FANOUT's ws to be every fork's v*v in fork order.
func checkForksGather(t *testing.T, outs string, width int) {
	t.Helper()

	var got struct {
		Ws []int `json:"ws"`
	}

	readJSON(t, outs, &got)

	want := make([]int, width)
	for i := range want {
		want[i] = (i + 1) * (i + 1)
	}

	if !slices.Equal(got.Ws, want) {
		t.Errorf("ws has %d elements, want the %d fork outputs v*v in order:\n got %v", len(got.Ws), width, got.Ws)
	}
}

// checkSplitJoin requires WIDE's total to be the join of every chunk's i*i.
func checkSplitJoin(t *testing.T, outs string, width int) {
	t.Helper()

	var got struct {
		Total int `json:"total"`
	}

	readJSON(t, outs, &got)

	want := 0
	for i := range width {
		want += i * i
	}

	if got.Total != want {
		t.Errorf("total = %d, want %d, the sum of i*i over %d chunks", got.Total, want, width)
	}
}

// checkOutsLen requires the outs array key to hold width elements, and each
// of its depth-1 nested arrays too, so no fork's output was dropped.
func checkOutsLen(key string, depth int) func(*testing.T, string, int) {
	return func(t *testing.T, outs string, width int) {
		t.Helper()

		var got map[string]any

		readJSON(t, outs, &got)

		level := []any{got[key]}

		for d := range depth {
			var next []any

			for _, v := range level {
				arr, ok := v.([]any)
				if !ok || len(arr) != width {
					t.Fatalf("%s at depth %d is %v, want %d elements", key, d, v, width)
				}

				next = append(next, arr...)
			}

			level = next
		}
	}
}
