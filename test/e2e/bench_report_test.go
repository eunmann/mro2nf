//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// reportLane is the subset of a bench_metrics.py line the report gates read.
type reportLane struct {
	Name          string `json:"name"`
	Tasks         int    `json:"tasks"`
	PlumbingTasks int    `json:"plumbing_tasks"`
	StageTasks    int    `json:"stage_tasks"`
	MrpJobs       int    `json:"mrp_jobs"`
	Processes     int    `json:"processes"`
	Refs          int    `json:"refs"`
	Multiplier    int    `json:"multiplier"`
	WcharBytes    int    `json:"wchar_bytes"`
	KnownScaling  int    `json:"known_scaling,omitempty"`
	FlatStaging   bool   `json:"flat_staging,omitempty"`
	// StagedMax is the most bytes one task of each stage process staged.
	StagedMax map[string]int `json:"stage_staged_bytes_max,omitempty"`
	Producers int            `json:"producers"`
	Restaged  int            `json:"restaged_bytes"`
}

// restagedPair is widthPair(2, 2, 0) with producers p4 and p8 restaging r4
// and r8 bytes.
func restagedPair(p4, r4, p8, r8 int) []reportLane {
	lanes := widthPair(2, 2, 0)
	lanes[0].Producers, lanes[0].Restaged = p4, r4
	lanes[1].Producers, lanes[1].Restaged = p8, r8

	return lanes
}

// stagingPair is widthPair(2, 2, 0) with flat_staging set, its widest
// stage task staging s4 and s8 bytes.
func stagingPair(s4, s8 int) []reportLane {
	lanes := widthPair(2, 2, 0)
	for i, s := range []int{s4, s8} {
		lanes[i].FlatStaging = true
		lanes[i].StagedMax = map[string]int{"N0_SRC": 0, "N1_USE": s}
	}

	return lanes
}

// widthPair is one fixture's lanes at widths 4 and 8, with the given plumbing
// counts and known_scaling marker; stage tasks equal mrp jobs.
func widthPair(p4, p8, known int) []reportLane {
	return []reportLane{
		{Name: "x_w4", Tasks: 4 + p4, PlumbingTasks: p4, StageTasks: 4, MrpJobs: 4, KnownScaling: known, Producers: 1},
		{Name: "x_w8", Tasks: 8 + p8, PlumbingTasks: p8, StageTasks: 8, MrpJobs: 8, KnownScaling: known, Producers: 1},
	}
}

// TestBenchReportGates drives bench_report.py on synthetic metrics: the
// width-flatness gate, its known_scaling exemption (which may not get worse
// and must be dropped once flat), the flat per-task staging gate, the flat
// restaged-bytes-per-producer gate, and the stage-tasks-equal-mrp-jobs gate,
// gating and under BENCH_UPDATE=1.
func TestBenchReportGates(t *testing.T) {
	requireTools(t, "python3")

	offMrp := widthPair(2, 2, 0)
	offMrp[1].StageTasks = 9

	cases := []struct {
		name     string
		update   bool
		cur      []reportLane
		base     []reportLane
		wantFail string // "" = the report must pass
	}{
		{"flat passes", false, widthPair(2, 2, 0), widthPair(2, 2, 0), ""},
		{"unmarked scaling fails", false, widthPair(2, 3, 0), widthPair(2, 3, 0), "must be identical"},
		{"known scaling passes", false, widthPair(2, 3, 255), widthPair(2, 3, 255), ""},
		{"known scaling now flat fails", false, widthPair(2, 2, 255), widthPair(2, 3, 255), "now width-flat"},
		{"known scaling worse fails", false, widthPair(2, 4, 255), widthPair(2, 3, 255), "REGRESSION[x_w8]: plumbing_tasks"},
		{"stage tasks off mrp fails", false, offMrp, offMrp, "MRP-JOBS[x_w8]"},
		{"flat staging passes", false, stagingPair(65600, 65610), stagingPair(65600, 65610), ""},
		{"staging grown with width fails", false, stagingPair(4*65600, 8*65600), stagingPair(4*65600, 8*65600), "STAGING[x]"},
		{"restaged per producer flat passes", false, restagedPair(4, 4*100, 8, 8*100), restagedPair(4, 4*100, 8, 8*100), ""},
		{"no restaged bytes passes", false, restagedPair(1, 0, 1, 0), restagedPair(1, 0, 1, 0), ""},
		{"broadcast copied per fork fails", false, restagedPair(1, 4*65600, 1, 8*65600), restagedPair(1, 4*65600, 1, 8*65600), "RESTAGED[x]"},
		{"restaged where none were fails", false, restagedPair(1, 0, 1, 100), restagedPair(1, 0, 1, 100), "RESTAGED[x]"},
		{"update records known scaling", true, widthPair(2, 3, 255), widthPair(2, 3, 255), ""},
		{"update refuses worse known scaling", true, widthPair(2, 4, 255), widthPair(2, 3, 255), "may not get worse"},
		{"update refuses stage tasks off mrp", true, offMrp, offMrp, "MRP-JOBS[x_w8]"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, failed := runReportOn(t, tc.cur, tc.base, tc.update)

			switch {
			case tc.wantFail == "" && failed:
				t.Errorf("report failed, want pass:\n%s", out)
			case tc.wantFail != "" && !failed:
				t.Errorf("report passed, want a failure naming %q:\n%s", tc.wantFail, out)
			case tc.wantFail != "" && !strings.Contains(out, tc.wantFail):
				t.Errorf("report output does not name %q:\n%s", tc.wantFail, out)
			}
		})
	}
}

// runReportOn writes cur as metrics and base as the baseline, runs
// bench_report.py, and returns its output and whether it exited non-zero. An
// update run that passes must have recorded cur's known_scaling markers.
func runReportOn(t *testing.T, cur, base []reportLane, update bool) (string, bool) {
	t.Helper()

	dir := t.TempDir()
	metrics, baseline := filepath.Join(dir, "metrics.jsonl"), filepath.Join(dir, "baseline.json")

	lines := make([]string, 0, len(cur))

	for _, l := range cur {
		b, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}

		lines = append(lines, string(b))
	}

	baseMap := map[string]reportLane{}
	for _, l := range base {
		baseMap[l.Name] = l
	}

	b, err := json.Marshal(baseMap)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(metrics, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(baseline, b, 0o644); err != nil {
		t.Fatal(err)
	}

	env := "BENCH_UPDATE=0"
	if update {
		env = "BENCH_UPDATE=1"
	}

	cmd := exec.Command("python3", filepath.Join(root, "test", "e2e", "bench_report.py"), metrics, baseline)
	cmd.Env = append(os.Environ(), env)
	out, err := cmd.CombinedOutput()

	if update && err == nil {
		var got map[string]reportLane

		readJSON(t, baseline, &got)

		for _, l := range cur {
			if got[l.Name].KnownScaling != l.KnownScaling {
				t.Errorf("recorded %s known_scaling = %d, want %d", l.Name, got[l.Name].KnownScaling, l.KnownScaling)
			}
		}
	}

	return string(out), err != nil
}
