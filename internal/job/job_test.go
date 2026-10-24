package job

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/cg"
	"github.com/google/go-cmp/cmp"
	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
)

// The expected metadata below is what mrp wrote for the named fixture's
// job (run as the e2e harness runs mrp, psid "mrp"), with its pipestance
// paths replaced by the test's.

// stage loads node of the testdata fixture as its job runs it, its code
// replaced by an exec stage script with body.
func stage(t *testing.T, fixture, node, body string) (Stage, *core.InvocationData) {
	t.Helper()

	mro := filepath.Join("..", "..", "testdata", fixture, "pipeline.mro")

	g, err := cg.Load(mro, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	st, err := g.Stage(node)
	if err != nil {
		t.Fatal(err)
	}

	s, err := NewStage(st, g.MroPaths())
	if err != nil {
		t.Fatal(err)
	}

	inv, err := g.Invocation()
	if err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(t.TempDir(), "stage.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nmeta=$2\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	s.Code = Code{Type: syntax.ExecStage, Cmd: script}

	return s, inv
}

// mrpPool is the pool the e2e harness runs mrp with: --localcores=2
// --localmem=4.
func mrpPool() Pool { return Pool{Cores: 2, MemGB: 4} }

func runtime(t *testing.T, inv *core.InvocationData) Runtime {
	t.Helper()

	return Runtime{
		Adapters: "/opt/martian/adapters", Mrjob: "/opt/martian/bin/mrjob", PSID: "mrp",
		Invocation: inv, Version: core.VersionInfo{Martian: "v4", Pipelines: "v1"}, Work: t.TempDir(),
		Local: mrpPool(),
	}
}

func readJSON(t *testing.T, path string) any {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	return v
}

func decode(t *testing.T, s string) any {
	t.Helper()

	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}

	return v
}

// TestArgvIsMrps: py and comp stages run under mrjob, py through the
// adapters' martian_shell.py, and an exec stage directly, each with its
// src args then the phase, metadata, files and journal (core/node.go:1130).
func TestArgvIsMrps(t *testing.T) {
	r := runtime(t, nil)

	j, err := r.newJob(PhaseMain, "ID.mrp.P.S.fork1.chnk0", "chnk0")
	if err != nil {
		t.Fatal(err)
	}

	tail := []string{"main", j.dir, filepath.Join(j.dir, "files"), filepath.Join(r.Work, "journal", "P.S.fork1.chnk0")}

	cases := []struct {
		code Code
		want []string
	}{
		{Code{Type: syntax.PythonStage, Cmd: "/p/stages/s"}, append([]string{r.Mrjob, "/opt/martian/adapters/python/martian_shell.py", "/p/stages/s"}, tail...)},
		{Code{Type: syntax.CompiledStage, Cmd: "/bin/cr", Args: []string{"martian", "s"}}, append([]string{r.Mrjob, "/bin/cr", "martian", "s"}, tail...)},
		{Code{Type: syntax.ExecStage, Cmd: "/bin/x", Args: []string{"a"}}, append([]string{"/bin/x", "a"}, tail...)},
	}

	for _, tc := range cases {
		name, argv := r.argv(tc.code, j)
		if d := cmp.Diff(tc.want, append([]string{name}, argv...)); d != "" {
			t.Errorf("%v argv differs from mrp's (-mrp +got):\n%s", tc.code.Type, d)
		}
	}
}

// TestMainWritesMrpMetadata: a main job's _args carry its resolved
// resources, its _outs the default file path, and its _jobinfo every key
// the adapters read, named for the chunk with the top-level invocation
// (file_min, mrp's FP/WRITE/fork0/chnk0).
func TestMainWritesMrpMetadata(t *testing.T) {
	s, inv := stage(t, "file_min", "FP.WRITE", `touch "$meta/_complete"`)
	r := runtime(t, inv)

	args := core.LazyArgumentMap{"x": json.RawMessage("42")}

	outs, err := r.Main(context.Background(), Instance{Stage: s, ForkID: "fork0"}, args, Unsplit())
	if err != nil {
		t.Fatal(err)
	}

	meta := filepath.Join(r.Work, "chnk0")
	files := filepath.Join(meta, "files")

	if d := cmp.Diff(decode(t, `{"__mem_gb":1,"__threads":1,"__vmem_gb":4,"x":42}`), readJSON(t, filepath.Join(meta, "_args"))); d != "" {
		t.Errorf("_args differ from mrp's (-mrp +got):\n%s", d)
	}

	if d := cmp.Diff(map[string]any{"note": filepath.Join(files, "note.txt")}, decode(t, string(outs))); d != "" {
		t.Errorf("_outs differ from mrp's (-mrp +got):\n%s", d)
	}

	want := decode(t, `{"name":"ID.mrp.FP.WRITE.fork0.chnk0","type":"local","profile_mode":"disable",`+
		`"stackvars_flag":"disable","monitor_flag":"disable",`+
		`"invocation":{"call":"FP","args":{"x":42},"mro_file":"pipeline.mro"},`+
		`"version":{"martian":"v4","pipelines":"v1"},"threads":1,"memGB":1,"vmemGB":4}`)
	if d := cmp.Diff(want, readJSON(t, filepath.Join(meta, "_jobinfo"))); d != "" {
		t.Errorf("_jobinfo differs from mrp's (-mrp +got):\n%s", d)
	}

	raw, err := os.ReadFile(filepath.Join(meta, "_stdout"))
	if err != nil || string(raw) != "[stdout]\n" {
		t.Errorf("_stdout = %q, %v; mrp's starts [stdout]", raw, err)
	}
}

// TestMonitorFlag: -monitor reaches mrjob only as _jobinfo's monitor_flag,
// which mrjob enforces (cmd/mrjob/mrjob.go writeJobinfo).
func TestMonitorFlag(t *testing.T) {
	r := runtime(t, nil)
	r.Monitor = true

	j, err := r.newJob(PhaseSplit, "ID.mrp.P.S.fork0", PhaseSplit)
	if err != nil {
		t.Fatal(err)
	}

	if got := r.jobInfo(j).Monitor; got != "monitor" {
		t.Errorf("monitor_flag %q, mrp writes monitor under --monitor", got)
	}
}

// TestThreadEnvs: a job runs with each of mrp's thread_envs
// (jobmanagers/config.json) set to its granted threads rounded up, over the
// value it inherits, and TMPDIR its tmp dir (core/jobmanager_local.go:515
// Enqueue, core/jobmanager_remote.go:219 jobScript): from a local pool, an
// adaptive or oversized request is the pool's cores.
func TestThreadEnvs(t *testing.T) {
	t.Setenv("OMP_NUM_THREADS", "64")

	cases := []struct {
		name    string
		pool    Pool
		threads float64
		want    string
	}{
		{"local fraction", mrpPool(), 1.5, "2"},
		{"local adaptive", mrpPool(), -1, "2"},
		{"local oversized", mrpPool(), 64, "2"},
		{"cluster fraction", Pool{}, 2.5, "3"},
		{"cluster adaptive", Pool{}, -4, "4"},
		{"cluster oversized", Pool{}, 64, "64"},
	}

	for _, tc := range cases {
		got, meta := jobEnv(t, tc.pool, tc.threads)

		want := map[string]string{"TMPDIR": filepath.Join(meta, "tmp")}
		for _, k := range []string{
			"GOMAXPROCS", "MKL_NUM_THREADS", "NUMEXPR_MAX_THREADS", "NUMEXPR_NUM_THREADS", "OMP_NUM_THREADS",
			"OMP_THREAD_LIMIT", "POLARS_MAX_THREADS", "ROOT_MAX_THREADS", "TF_NUM_THREADS",
		} {
			want[k] = tc.want
		}

		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: %s = %q in the job's environment, mrp sets %q", tc.name, k, got[k], v)
			}
		}
	}
}

// jobEnv runs a main job requesting threads, granted from pool, and
// returns the environment it ran with and its metadata dir.
func jobEnv(t *testing.T, pool Pool, threads float64) (map[string]string, string) {
	t.Helper()

	s, inv := stage(t, "file_min", "FP.WRITE", `env > "$meta/env"; touch "$meta/_complete"`)
	s.Using = &core.JobResources{Threads: threads}
	r := runtime(t, inv)
	r.Local = pool

	if _, err := r.Main(context.Background(), Instance{Stage: s, ForkID: "fork0"}, core.LazyArgumentMap{}, Unsplit()); err != nil {
		t.Fatal(err)
	}

	meta := filepath.Join(r.Work, "chnk0")

	raw, err := os.ReadFile(filepath.Join(meta, "env"))
	if err != nil {
		t.Fatal(err)
	}

	env := map[string]string{}

	for line := range strings.SplitSeq(string(raw), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}

	return env, meta
}

// TestOutcomes: mrjob exits 0 however the stage ends, so the job's state
// is read from its metadata as mrp reads it: an assertion, which no retry
// may rerun, an error, a non-zero exit, or no completion. An error is
// transient, which mrp retries, only when a line of its _errors matches a
// retry_on pattern: a killed job ("signal: killed", as mrp's local job
// manager records it), mrjob's caught signal, ENOMEM; never a stage
// exception or, without --auto-adjust-memory, the monitor's memory kill.
func TestOutcomes(t *testing.T) {
	cases := []struct {
		name, body string
		want       error
		transient  bool
	}{
		{"complete", `touch "$meta/_complete"`, nil, false},
		{"assert", `printf 'nope' > "$meta/_assert"; printf 'signal: killed' > "$meta/_errors"`, ErrAssert, false},
		{"errors", `printf 'Traceback' > "$meta/_errors"`, errFailed, false},
		{"exit", `exit 3`, errFailed, false},
		{"killed", `kill -9 $$`, errFailed, true},
		{"caught signal", `printf 'Job failed\n2026-10-11 21:00:00 Caught signal terminated\n' > "$meta/_errors"`, errFailed, true},
		{"enomem", `printf 'error: [Errno 12] Cannot allocate memory' > "$meta/_errors"`, errFailed, true},
		{"memory quota", `printf 'Job failed in stage code\n\nStage exceeded its memory quota (using 0.8, allowed 0.5G)\n' > "$meta/_errors"`, errFailed, false},
		{"incomplete", `true`, errIncomplete, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, inv := stage(t, "file_min", "FP.WRITE", tc.body)
			r := runtime(t, inv)

			_, err := r.Main(context.Background(), Instance{Stage: s, ForkID: "fork0"}, core.LazyArgumentMap{}, Unsplit())

			switch {
			case tc.want == nil && err != nil:
				t.Errorf("a complete job failed: %v", err)
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Errorf("outcome %v, want %v", err, tc.want)
			case errors.Is(err, ErrTransient) != tc.transient:
				t.Errorf("outcome %v: transient %t, mrp's retry_on says %t", err, !tc.transient, tc.transient)
			}
		})
	}
}

// TestAlarmsReachStderr: each alarm line a job writes reaches mre's stderr
// under the job's name, which mrp prints when the run ends.
func TestAlarmsReachStderr(t *testing.T) {
	s, inv := stage(t, "file_min", "FP.WRITE", `printf 'low disk\n\n  slow\n' > "$meta/_alarm"; touch "$meta/_complete"`)
	r := runtime(t, inv)

	rd, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	orig := os.Stderr
	os.Stderr = w

	_, err = r.Main(context.Background(), Instance{Stage: s, ForkID: "fork0"}, core.LazyArgumentMap{}, Unsplit())

	os.Stderr = orig
	_ = w.Close()

	if err != nil {
		t.Fatal(err)
	}

	got, err := io.ReadAll(rd)
	if err != nil {
		t.Fatal(err)
	}

	want := "mre: ALARM: ID.mrp.FP.WRITE.fork0.chnk0: low disk\nmre: ALARM: ID.mrp.FP.WRITE.fork0.chnk0: slow\n"
	if d := cmp.Diff(want, string(got)); d != "" {
		t.Errorf("stderr (-want +got):\n%s", d)
	}
}

// TestJoinWritesChunkFiles: the join's _args carry its resolved join def,
// its _chunk_defs each chunk's args without resources, and its _chunk_outs
// an empty object per chunk for a stage with no outputs
// (core/stage.go:1171-1246).
func TestJoinWritesChunkFiles(t *testing.T) {
	s, inv := stage(t, "file_min", "FP.WRITE", `touch "$meta/_complete"`)
	s.Split, s.Outs, s.ChunkOuts = true, nil, nil
	r := runtime(t, inv)

	defs := &core.StageDefs{
		JoinDef: &core.JobResources{Threads: 2},
		ChunkDefs: []*core.ChunkDef{
			{Args: core.LazyArgumentMap{"v": json.RawMessage("1")}, Resources: &core.JobResources{MemGB: 3}},
			{Args: core.LazyArgumentMap{"v": json.RawMessage("2")}},
		},
	}

	if _, err := r.Join(context.Background(), Instance{Stage: s, ForkID: "fork0"}, core.LazyArgumentMap{"x": json.RawMessage("1")},
		defs, []core.LazyArgumentMap{{}, {}}); err != nil {
		t.Fatal(err)
	}

	meta := filepath.Join(r.Work, PhaseJoin)

	for _, f := range []struct{ name, want string }{
		{"_args", `{"__mem_gb":1,"__threads":2,"__vmem_gb":4,"x":1}`},
		{"_chunk_defs", `[{"v":1},{"v":2}]`},
		{"_chunk_outs", `[{},{}]`},
		{"_outs", `{}`},
	} {
		if d := cmp.Diff(decode(t, f.want), readJSON(t, filepath.Join(meta, f.name))); d != "" {
			t.Errorf("%s differs from mrp's (-mrp +got):\n%s", f.name, d)
		}
	}
}

// TestChunkName: chunk dirs are padded to the width of the chunk count, as
// mrp names them (core/stage.go:98).
func TestChunkName(t *testing.T) {
	got := []string{ChunkName(0, 1), ChunkName(3, 9), ChunkName(0, 10), ChunkName(9, 10)}
	if want := []string{"chnk0", "chnk3", "chnk00", "chnk09"}; !slices.Equal(got, want) {
		t.Errorf("chunk names %v, mrp's %v", got, want)
	}
}
