package job

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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

	dir := filepath.Join("..", "..", "testdata", fixture)

	g, err := cg.Load(filepath.Join(dir, "pipeline.mro"), nil)
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

func runtime(t *testing.T, inv *core.InvocationData) Runtime {
	t.Helper()

	return Runtime{
		Adapters: "/opt/martian/adapters", Mrjob: "/opt/martian/bin/mrjob", PSID: "mrp",
		Invocation: inv, Version: core.VersionInfo{Martian: "v4", Pipelines: "v1"}, Work: t.TempDir(),
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
// src args then the phase, metadata, files and journal (core/node.go:1120).
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

// TestOutcomes: mrjob exits 0 however the stage ends, so the job's state
// is read from its metadata as mrp reads it: an assertion, which no retry
// may rerun, an error, a non-zero exit, or no completion.
func TestOutcomes(t *testing.T) {
	cases := []struct {
		name, body string
		want       error
	}{
		{"complete", `touch "$meta/_complete"`, nil},
		{"assert", `printf 'nope' > "$meta/_assert"; touch "$meta/_errors"`, ErrAssert},
		{"errors", `printf 'Traceback' > "$meta/_errors"`, errFailed},
		{"exit", `exit 3`, errFailed},
		{"incomplete", `true`, errIncomplete},
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
			}
		})
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

// TestReqs: a job's resources are the using() request with the def's over
// it, resolved as mrp's local job manager resolves them: whole centicores
// and MiB, one thread and 1 GB unset, vmem the memory plus 3 GB unset and
// never below the memory.
func TestReqs(t *testing.T) {
	cases := []struct {
		name       string
		using, def *core.JobResources
		want       core.JobResources
	}{
		{"defaults", nil, nil, core.JobResources{Threads: 1, MemGB: 1, VMemGB: 4}},
		{"fractions", &core.JobResources{Threads: 0.333, MemGB: 0.3}, nil, core.JobResources{Threads: 0.34, MemGB: 308.0 / 1024, VMemGB: (308.0 + 3072) / 1024}},
		{"def over using", &core.JobResources{Threads: 4, MemGB: 8, Special: "a"}, &core.JobResources{MemGB: 2, Special: "b"}, core.JobResources{Threads: 4, MemGB: 2, VMemGB: 5, Special: "b"}},
		{"adaptive", &core.JobResources{Threads: -2, MemGB: -6}, nil, core.JobResources{Threads: 2, MemGB: 6, VMemGB: 9}},
		{"vmem below mem", &core.JobResources{MemGB: 8, VMemGB: 2}, nil, core.JobResources{Threads: 1, MemGB: 8, VMemGB: 8}},
	}

	for _, tc := range cases {
		if got := reqs(tc.using, tc.def); got != tc.want {
			t.Errorf("%s: reqs = %+v, want %+v", tc.name, got, tc.want)
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
