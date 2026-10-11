package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
)

// pyStageFlags returns the adapter and invocation flags the generated process
// passes a py stage phase for the stage code at testdata/<fixture>/stages/<dir>.
func pyStageFlags(t *testing.T, fixture, dir, call string) []string {
	t.Helper()

	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	shell, err := filepath.Abs(filepath.Join("..", "..", "vendor-martian", "python", "martian_shell.py"))
	if err != nil {
		t.Fatal(err)
	}

	code, err := filepath.Abs(filepath.Join("..", "..", "testdata", fixture, "stages", dir))
	if err != nil {
		t.Fatal(err)
	}

	return []string{"-shell", shell, "-stagecode", code, "-lang", "py", "-call", call, "-mro", "pipeline.mro"}
}

// writePhaseManifest writes man as the type manifest under dir and returns the
// -types and -callable flags naming it and callable.
func writePhaseManifest(t *testing.T, dir, callable string, man types.Manifest) []string {
	t.Helper()

	path := writeTestManifest(t, filepath.Join(dir, "types.json"), man)

	return []string{"-types", path, "-callable", callable}
}

// runPhase runs one mre phase subcommand and fails the test on its error.
func runPhase(t *testing.T, phase string, flags ...[]string) {
	t.Helper()

	if err := run(t.Context(), append([]string{phase}, slices.Concat(flags...)...)); err != nil {
		t.Fatalf("mre %s: %v", phase, err)
	}
}

// readAllocation reads the threads/memGB/vmemGB a phase's _jobinfo reported.
func readAllocation(t *testing.T, meta string) shim.Resources {
	t.Helper()

	var info struct {
		Threads float64 `json:"threads"`
		MemGB   float64 `json:"memGB"`
		VMemGB  float64 `json:"vmemGB"`
	}

	readJSONFile(t, filepath.Join(meta, "_jobinfo"), &info)

	return shim.Resources{Threads: info.Threads, MemGB: info.MemGB, VMemGB: info.VMemGB}
}

// chunkFileArg returns the file a chunk bundle's def passes as its arg name.
func chunkFileArg(t *testing.T, bundle, name string) string {
	t.Helper()

	raw, err := readBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}

	def, err := decodeChunk(raw)
	if err != nil {
		t.Fatal(err)
	}

	var path string
	if err := json.Unmarshal(def.Args[name], &path); err != nil {
		t.Fatalf("chunk arg %s = %s: %v", name, def.Args[name], err)
	}

	return path
}

// TestRunSplitMainJoin drives a split stage's three phases through the mre
// subcommands as the generated processes call them. The stage is
// split_chunk_file's MAKE_REFS, whose split creates one bin file per chunk in
// its own files dir and returns it as a chunk arg that main and join open. The
// split work dir is deleted before main and join run, as on an isolated
// worker, so both can only open the files writeChunkBundles staged into each
// chunk bundle.
func TestRunSplitMainJoin(t *testing.T) {
	stage := pyStageFlags(t, "split_chunk_file", "make_refs", "SPLIT_CHUNK_FILE")
	tmp := t.TempDir()
	typed := writePhaseManifest(t, tmp, "MAKE_REFS", types.Manifest{Callables: map[string]types.Callable{"MAKE_REFS": {
		In:       []ir.Param{{Name: "n", BaseType: "int"}},
		Out:      []ir.Param{{Name: "total", BaseType: "int"}},
		ChunkIn:  []ir.Param{{Name: "chunk_id", BaseType: "int"}, {Name: "ref", BaseType: "bin", IsFile: true}},
		ChunkOut: []ir.Param{{Name: "partial", BaseType: "int"}},
	}}})
	args, chunks, staged := filepath.Join(tmp, "args"), filepath.Join(tmp, "chunks.json"), filepath.Join(tmp, "staged")
	splitWork := filepath.Join(tmp, "split")

	writeTestBundle(t, args, map[string]any{"n": 2})
	runPhase(t, "split", stage, typed, []string{
		"-role", types.RoleChunkIn, "-threads", "0", "-memgb", "1",
		"-args", args, "-work", splitWork, "-o", chunks, "-joinres", filepath.Join(tmp, "joinres.json"), "-chunkdir", staged,
	})

	var defs []shim.ChunkDef
	readJSONFile(t, chunks, &defs)

	if len(defs) != 2 {
		t.Fatalf("chunks.json lists %d chunks, want 2", len(defs))
	}

	if err := os.RemoveAll(splitWork); err != nil {
		t.Fatal(err)
	}

	for i := range defs {
		chunk := filepath.Join(staged, fmt.Sprintf("chunk_%05d", i))
		if ref := chunkFileArg(t, chunk, "ref"); !strings.HasPrefix(ref, chunk+string(filepath.Separator)) {
			t.Errorf("chunk %d ref = %s, want a copy inside its bundle %s", i, ref, chunk)
		}

		work := filepath.Join(tmp, fmt.Sprintf("main%d", i))
		out := filepath.Join(staged, fmt.Sprintf("out_chunk_%05d", i))
		runPhase(t, "main", stage, typed, []string{
			"-role", types.RoleMainOut, "-threads", "0", "-memgb", "1",
			"-args", args, "-chunk", chunk, "-work", work, "-o", out,
		})

		if got, want := readTestBundle(t, out)["partial"], float64(10*(i+1)); got != want {
			t.Errorf("chunk %d partial = %v, want %v", i, got, want)
		}

		if got, want := readAllocation(t, filepath.Join(work, "main")), (shim.Resources{Threads: 1, MemGB: 1, VMemGB: 4}); got != want {
			t.Errorf("chunk %d _jobinfo allocation = %+v, want %+v", i, got, want)
		}
	}

	outs := filepath.Join(tmp, "outs")
	runPhase(t, "join", stage, typed, []string{
		"-role", types.RoleOut, "-threads", "0", "-memgb", "1",
		"-args", args, "-chunksfile", chunks, "-chunkdir", staged, "-joinres", filepath.Join(tmp, "joinres.json"),
		"-work", filepath.Join(tmp, "join"), "-o", outs,
	})

	if diff := cmp.Diff(map[string]any{"total": 30.0}, readTestBundle(t, outs)); diff != "" {
		t.Errorf("join outs (-want +got):\n%s", diff)
	}
}

// TestRunJoinUsesSplitJoinResources checks the join resources a split returns
// reach joinres.json and, through runJoin, the join's _args and _jobinfo, over
// the stage's own request, as mrp's getJobReqs overlays the join def; and that
// an --overrides memory beats both.
func TestRunJoinUsesSplitJoinResources(t *testing.T) {
	stage := pyStageFlags(t, "join_resources", "sum_squares", "SUM_SQUARE_PIPELINE")
	tmp := t.TempDir()
	args, chunks, joinRes := filepath.Join(tmp, "args"), filepath.Join(tmp, "chunks.json"), filepath.Join(tmp, "joinres.json")
	request := append([]string{"-threads", "1", "-memgb", "1"}, writePhaseManifest(t, tmp, "SUM_SQUARES", types.Manifest{
		Callables: map[string]types.Callable{"SUM_SQUARES": {
			In:       []ir.Param{{Name: "values", BaseType: "float", ArrayDim: 1}},
			Out:      []ir.Param{{Name: "sum", BaseType: "float"}},
			ChunkIn:  []ir.Param{{Name: "value", BaseType: "float"}},
			ChunkOut: []ir.Param{{Name: "square", BaseType: "float"}},
		}},
	})...)

	writeTestBundle(t, args, map[string]any{"values": []any{1.0, 2.0}})
	runPhase(t, "split", stage, request, []string{
		"-args", args, "-work", filepath.Join(tmp, "split"), "-o", chunks,
		"-joinres", joinRes, "-chunkdir", tmp,
	})

	var res shim.Resources
	readJSONFile(t, joinRes, &res)

	if want := (shim.Resources{Threads: 2, MemGB: 3}); res != want {
		t.Errorf("joinres.json = %+v, want %+v", res, want)
	}

	for i := range 2 {
		runPhase(t, "main", stage, request, []string{
			"-args", args, "-chunk", filepath.Join(tmp, fmt.Sprintf("chunk_%05d", i)),
			"-work", filepath.Join(tmp, fmt.Sprintf("main%d", i)), "-o", filepath.Join(tmp, fmt.Sprintf("out_chunk_%05d", i)),
		})
	}

	cases := []struct {
		name     string
		override []string
		want     shim.Resources
		argMem   string
	}{
		{"join_def", nil, shim.Resources{Threads: 2, MemGB: 3, VMemGB: 6}, "3"},
		{"override", []string{"-overridememgb", "5"}, shim.Resources{Threads: 2, MemGB: 5, VMemGB: 8}, "5"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			work, outs := filepath.Join(tmp, "join_"+tc.name), filepath.Join(tmp, "outs_"+tc.name)
			runPhase(t, "join", stage, request, tc.override, []string{
				"-args", args, "-chunksfile", chunks, "-chunkdir", tmp,
				"-joinres", joinRes, "-work", work, "-o", outs,
			})

			if got := readAllocation(t, filepath.Join(work, "join")); got != tc.want {
				t.Errorf("join _jobinfo allocation = %+v, want %+v", got, tc.want)
			}

			var joinArgs map[string]json.RawMessage
			readJSONFile(t, filepath.Join(work, "join", "_args"), &joinArgs)

			if got := string(joinArgs["__mem_gb"]); got != tc.argMem {
				t.Errorf("join __mem_gb = %s, want %s", got, tc.argMem)
			}

			if got := readTestBundle(t, outs)["sum"]; got != 5.0 {
				t.Errorf("join sum = %v, want 5", got)
			}
		})
	}
}

// TestRunMainSingleChunk checks a non-split stage's main, which has no -chunk:
// its file output is staged into the output bundle, and its final outputs are
// validated as mrp validates a stage's _outs, so a struct missing a declared
// key fails the phase.
func TestRunMainSingleChunk(t *testing.T) {
	tmp := t.TempDir()
	args, outs := filepath.Join(tmp, "args"), filepath.Join(tmp, "outs")

	writeTestBundle(t, args, map[string]any{"x": 2.5})
	runPhase(t, "main", pyStageFlags(t, "file_min", "write", "FP"), writePhaseManifest(t, tmp, "WRITE", types.Manifest{
		Callables: map[string]types.Callable{"WRITE": {
			In:  []ir.Param{{Name: "x", BaseType: "float"}},
			Out: []ir.Param{{Name: "note", BaseType: "txt", IsFile: true}},
		}},
	}), []string{"-role", types.RoleMainOut, "-args", args, "-work", filepath.Join(tmp, "write"), "-o", outs})

	note, ok := readTestBundle(t, outs)["note"].(string)
	if !ok || !strings.HasPrefix(note, outs+string(filepath.Separator)) {
		t.Fatalf("note = %v, want a file staged inside the output bundle %s", note, outs)
	}

	if data, err := os.ReadFile(note); err != nil || string(data) != "x=2.5\n" {
		t.Errorf("staged note = %q (%v), want %q", data, err, "x=2.5\n")
	}

	upOuts := filepath.Join(tmp, "upouts")
	up := writePhaseManifest(t, t.TempDir(), "UP", types.Manifest{
		Structs:   map[string]*ir.StructType{"S": {Name: "S", Fields: []ir.Param{{Name: "a", BaseType: "int"}, {Name: "b", BaseType: "int"}}}},
		Callables: map[string]types.Callable{"UP": {Out: []ir.Param{{Name: "s", Type: "S", BaseType: "S"}}}},
	})

	err := run(t.Context(), slices.Concat([]string{"main"}, pyStageFlags(t, "missing_struct_key", "up", "P"), up,
		[]string{"-role", types.RoleMainOut, "-work", filepath.Join(tmp, "up"), "-o", upOuts}))
	if !errors.Is(err, types.ErrInvalidOutputs) || !strings.Contains(err.Error(), "missing key: b") {
		t.Errorf("main of a struct output missing b: err = %v, want ErrInvalidOutputs naming b", err)
	}

	if _, statErr := os.Stat(upOuts); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("main wrote an output bundle for outputs that failed validation (stat: %v)", statErr)
	}
}

// TestRunMainAssertExits42 checks a stage's martian.exit reaches the process
// as an ASSERT, which mre exits 42 on so the generated errorStrategy does not
// retry it.
func TestRunMainAssertExits42(t *testing.T) {
	tmp := t.TempDir()
	args := filepath.Join(tmp, "args")
	writeTestBundle(t, args, map[string]any{"x": 1.0})

	err := run(t.Context(), slices.Concat([]string{"main"}, pyStageFlags(t, "assert_min", "boom", "AP"),
		[]string{"-args", args, "-work", tmp, "-o", filepath.Join(tmp, "outs")}))

	if !errors.Is(err, shim.ErrStageAssert) {
		t.Fatalf("main of an asserting stage: err = %v, want ErrStageAssert", err)
	}

	if code := exitCode(err); code != shim.AssertExitCode {
		t.Errorf("exit code = %d, want %d", code, shim.AssertExitCode)
	}

	if !strings.Contains(err.Error(), "intentional assertion") {
		t.Errorf("err = %v, want the stage's assertion message", err)
	}
}

// TestPhaseInputErrors checks each phase fails, before running the stage, on
// input it cannot read, wrapping the cause.
func TestPhaseInputErrors(t *testing.T) {
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "absent")
	badJSON := writeTestFile(t, filepath.Join(tmp, "bad.json"), "{")
	typed := writePhaseManifest(t, tmp, "OTHER", types.Manifest{Callables: map[string]types.Callable{"OTHER": {}}})
	args := filepath.Join(tmp, "args")
	writeTestBundle(t, args, map[string]any{"x": 1.0})

	cases := []struct {
		name string
		argv []string
		want error
		msg  string
	}{
		{"split_bad_flag", []string{"split", "-nosuch"}, nil, "parse flags"},
		{"split_args_absent", []string{"split", "-args", missing}, os.ErrNotExist, "read input bundle"},
		{"split_unknown_callable", slices.Concat([]string{"split", "-args", args}, typed[:2], []string{"-callable", "S"}), types.ErrUnknownCallable, "coerce inputs"},
		{"main_bad_flag", []string{"main", "-nosuch"}, nil, "parse flags"},
		{"main_chunk_absent", []string{"main", "-args", args, "-chunk", missing}, os.ErrNotExist, "read input bundle"},
		{"main_types_absent", []string{"main", "-args", args, "-types", missing, "-callable", "S"}, os.ErrNotExist, "load type manifest"},
		{"join_bad_flag", []string{"join", "-nosuch"}, nil, "parse flags"},
		{"join_no_chunksfile", []string{"join", "-args", args}, errNoChunksFile, ""},
		{"join_chunk_count", []string{"join", "-args", args, "-chunksfile", writeTestFile(t, filepath.Join(tmp, "one.json"), "[{}]"), "-chunkdir", tmp}, errGatherCount, ""},
		{"join_bad_joinres", []string{"join", "-args", args, "-chunksfile", writeTestFile(t, filepath.Join(tmp, "none.json"), "[]"), "-chunkdir", tmp, "-joinres", badJSON}, nil, "parse join resources"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := run(t.Context(), append(tc.argv, "-work", t.TempDir()))
			if err == nil {
				t.Fatal("run succeeded, want an error")
			}

			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}

			if !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("err = %v, want it to say %q", err, tc.msg)
			}
		})
	}
}

// TestWriteChunkBundles checks the chunk bundles are named so a lexical sort
// is chunk order past ten chunks, each holding its def's args and resources,
// and that a callable the manifest lacks fails before any bundle is written.
func TestWriteChunkBundles(t *testing.T) {
	dir := t.TempDir()
	defs := make([]shim.ChunkDef, 11)

	for i := range defs {
		defs[i] = shim.ChunkDef{
			Args:      map[string]json.RawMessage{"i": json.RawMessage(strconv.Itoa(i))},
			Resources: shim.Resources{MemGB: float64(i)},
		}
	}

	if err := writeChunkBundles(dir, defs, &producer{}, nil); err != nil {
		t.Fatalf("writeChunkBundles: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != len(defs) {
		t.Fatalf("%d bundles, want %d", len(entries), len(defs))
	}

	for i, e := range entries {
		if want := fmt.Sprintf("chunk_%05d", i); e.Name() != want {
			t.Errorf("bundle %d is %s, want %s", i, e.Name(), want)
		}

		raw, err := readBundle(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}

		got, err := decodeChunk(raw)
		if err != nil {
			t.Fatal(err)
		}

		if string(got.Args["i"]) != strconv.Itoa(i) || got.Resources.MemGB != float64(i) {
			t.Errorf("bundle %s = %s / %+v, want chunk %d's def", e.Name(), raw, got.Resources, i)
		}
	}

	path := writePhaseManifest(t, t.TempDir(), "OTHER", types.Manifest{Callables: map[string]types.Callable{"OTHER": {}}})[1]
	empty := t.TempDir()

	if err := writeChunkBundles(empty, defs, &producer{types: path, callable: "S"}, nil); !errors.Is(err, types.ErrUnknownCallable) {
		t.Errorf("unknown callable: err = %v, want ErrUnknownCallable", err)
	}

	if entries, _ := os.ReadDir(empty); len(entries) != 0 {
		t.Errorf("unknown callable wrote %d bundles, want none", len(entries))
	}
}
