//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// scaleDefaultWidth is the nightly scale smoke's fork width: wide enough that
// per-fork plumbing or a dropped gather element is unmistakable, small enough
// (about 1,000 CPU-seconds for both modes) for one CI runner. #352 grows this
// into the 10k-fork tier.
const scaleDefaultWidth = 1000

// scaleNarrowMRO is the bench/forks invocation the wide run is held against.
const scaleNarrowMRO = "pipeline_w4.mro"

// TestScaleForks is the nightly scale smoke (`make test-scale`): the bench
// forks fixture at SCALE_WIDTH forks, in default and -native mode, must run
// one stage task per mrp job, the same plumbing task count as at width 4,
// and gather every fork's output in order.
func TestScaleForks(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	width := scaleWidth(t)

	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join(root, "bench", "forks"))); err != nil {
		t.Fatalf("copy bench/forks: %v", err)
	}

	wideMRO := writeForksInvocation(t, dir, width)

	for _, native := range []bool{false, true} {
		name := "default"
		if native {
			name = "native"
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			narrow := forksScaleLane(4, scaleNarrowMRO, native)
			wide := forksScaleLane(width, wideMRO, native)

			narrowM, _ := runScaleLane(t, narrow, dir)
			wideM, proj := runScaleLane(t, wide, dir)

			if wideM.StageTasks != wide.mrpJobs {
				t.Errorf("%s: %d stage tasks, want %d (mrp's jobs)", wide.key, wideM.StageTasks, wide.mrpJobs)
			}

			if wideM.PlumbingTasks != narrowM.PlumbingTasks {
				t.Errorf("plumbing tasks grow with fork width: %d at width 4, %d at width %d",
					narrowM.PlumbingTasks, wideM.PlumbingTasks, width)
			}

			checkForksGather(t, proj, width)
		})
	}
}

// scaleMetrics is the part of a bench_metrics.py line the scale smoke gates.
type scaleMetrics struct {
	PlumbingTasks int `json:"plumbing_tasks"`
	StageTasks    int `json:"stage_tasks"`
}

// forksScaleLane is the bench forks lane for the invocation mro of width forks:
// MKPAY plus one FUSE job per fork under mrp.
func forksScaleLane(width int, mro string, native bool) benchLane {
	return benchLane{
		key: fmt.Sprintf("forks_w%d", width), dir: "forks", mro: mro,
		probe: "MRE_BENCH_FORKS_PROBE", producers: 1, mrpJobs: 1 + width, native: native,
	}
}

func runScaleLane(t *testing.T, l benchLane, dir string) (scaleMetrics, string) {
	t.Helper()

	line, proj := runBenchLaneIn(t, l, dir, "")

	var m scaleMetrics
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("decode %s metrics %q: %v", l.key, line, err)
	}

	return m, proj
}

// scaleWidth reads SCALE_WIDTH, which must exceed the narrow width 4 for the
// comparison to mean anything.
func scaleWidth(t *testing.T) int {
	t.Helper()

	s := os.Getenv("SCALE_WIDTH")
	if s == "" {
		return scaleDefaultWidth
	}

	w, err := strconv.Atoi(s)
	if err != nil || w <= 4 {
		t.Fatalf("SCALE_WIDTH=%q: want an integer above 4", s)
	}

	return w
}

// writeForksInvocation writes a bench/forks invocation over [1..width] into
// dir and returns its file name.
func writeForksInvocation(t *testing.T, dir string, width int) string {
	t.Helper()

	vs := make([]string, width)
	for i := range vs {
		vs[i] = strconv.Itoa(i + 1)
	}

	src := fmt.Sprintf("@include \"common.mro\"\n\ncall FANOUT(\n    vs = [%s],\n    kb = 256,\n)\n",
		strings.Join(vs, ", "))

	const name = "pipeline_scale.mro"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}

	return name
}

// checkForksGather requires FANOUT's ws to be every fork's v*v in fork order.
func checkForksGather(t *testing.T, proj string, width int) {
	t.Helper()

	var outs struct {
		Ws []int `json:"ws"`
	}

	readJSON(t, filepath.Join(proj, "results", "pipeline_outs.json"), &outs)

	want := make([]int, width)
	for i := range want {
		want[i] = (i + 1) * (i + 1)
	}

	if !slices.Equal(outs.Ws, want) {
		t.Errorf("ws has %d elements, want the %d fork outputs v*v in order:\n got %v", len(outs.Ws), width, outs.Ws)
	}
}
