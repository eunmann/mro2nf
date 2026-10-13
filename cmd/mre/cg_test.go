package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/cg"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/google/go-cmp/cmp"
	"github.com/martian-lang/martian/martian/core"
)

// captureStdout redirects os.Stdout around fn and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	orig := os.Stdout
	os.Stdout = w

	fn()

	_ = w.Close()
	os.Stdout = orig

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	return string(data)
}

// runCG runs one call-graph subcommand against testdata/struct_proj and
// returns its stdout.
func runCG(t *testing.T, args ...string) string {
	t.Helper()

	gf := structProj()
	argv := append([]string{args[0], "-entry", gf.entry, "-mropath", gf.mropath}, args[1:]...)

	var err error

	out := captureStdout(t, func() { err = run(t.Context(), argv) })
	if err != nil {
		t.Fatalf("mre %s: %v", strings.Join(args, " "), err)
	}

	return out
}

func compactBundle(t *testing.T, dir string) string {
	t.Helper()

	raw, err := shim.ReadBundle(dir)
	if err != nil {
		t.Fatal(err)
	}

	return string(raw)
}

// structProj locates testdata/struct_proj's invocation.
func structProj() graphFlags {
	dir := filepath.Join("..", "..", "testdata", "struct_proj")

	return graphFlags{entry: filepath.Join(dir, "pipeline.mro"), mropath: dir}
}

// entryForks writes the fork elements of the entry-fed roots of the
// invocation gf locates into dir, as mre plan writes them.
func entryForks(t *testing.T, gf graphFlags, dir string) {
	t.Helper()

	g, err := gf.load()
	if err != nil {
		t.Fatal(err)
	}

	if err := g.PostEntry(dir); err != nil {
		t.Fatalf("entry forks: %v", err)
	}
}

// argsStep runs mre stage's args step against the invocation gf locates,
// linking the files of the args of the instance a names below out, and
// returns the instance and its args, compact.
func argsStep(gf graphFlags, a argsFlags, out string) (cg.Instance, string, error) {
	g, err := gf.load()
	if err != nil {
		return cg.Instance{}, "", err
	}

	inst, args, err := a.write(g, out)
	if err != nil || inst.Disabled {
		return inst, "", err
	}

	raw, err := json.Marshal(args)

	return inst, string(raw), err
}

// postStep runs mre stage's post step against the invocation gf locates,
// posting outs as the outs of the instance inst, written below dir, and
// returns the file of the value records it posted.
func postStep(gf graphFlags, inst cg.Instance, outs map[string]any, dir, gates string) (string, error) {
	g, err := gf.load()
	if err != nil {
		return "", err
	}

	m := core.LazyArgumentMap{}

	for k, v := range outs {
		if m[k], err = json.Marshal(v); err != nil {
			return "", err
		}
	}

	p := postFlags{
		forks: filepath.Join(dir, "forks"), forkValues: filepath.Join(dir, "fork_values"),
		fields: filepath.Join(dir, "outs"), values: filepath.Join(dir, "outs_values"), gates: gates,
	}

	return p.values, p.post(g, inst, finished{outs: m})
}

// TestCallGraphSteps drives struct_proj through its entry forks, mre
// stage's args and post steps and mre outs as a run's tasks would, each
// stage's outs standing in for the stage, and requires the _args and
// pipeline outs of a real mrp run of struct_proj: PP/MAKE/fork<i> gets
// n = i+1, PP/SUMX gets xs = [1,2,3], and PP/fork0/_outs is {"total":6}.
func TestCallGraphSteps(t *testing.T) {
	work, gf := t.TempDir(), structProj()
	forks := filepath.Join(work, "entry")

	entryForks(t, gf, forks)

	elems, err := filepath.Glob(filepath.Join(forks, "PP.MAKE", "*"))
	if err != nil || len(elems) != 3 {
		t.Fatalf("entry forks of MAKE: %v %v", elems, err)
	}

	made := make([]string, 0, len(elems))

	for i, el := range elems {
		dir := filepath.Join(work, strconv.Itoa(i))

		inst, args, err := argsStep(gf, argsFlags{node: "PP.MAKE", in: repeatedFlag{el}}, filepath.Join(dir, "args"))
		if err != nil || inst.Disabled {
			t.Fatalf("MAKE fork %d: %+v, %v", i, inst, err)
		}

		if d := cmp.Diff(`{"n":`+strconv.Itoa(i+1)+`}`, args); d != "" {
			t.Errorf("MAKE fork %d _args (-mrp +mre):\n%s", i, d)
		}

		n := float64(i + 1)

		values, err := postStep(gf, inst, map[string]any{"p": map[string]any{"x": n, "y": 10 * n}}, dir, "")
		if err != nil {
			t.Fatal(err)
		}

		made = append(made, readText(t, values))
	}

	sumxValues := writeTestFile(t, filepath.Join(work, "sumx_values"), strings.Join(made, "\n"))

	inst, args, err := argsStep(gf, argsFlags{node: "PP.SUMX", values: sumxValues}, filepath.Join(work, "sumx", "args"))
	if err != nil {
		t.Fatal(err)
	}

	if d := cmp.Diff(`{"xs":[1,2,3]}`, args); d != "" {
		t.Errorf("SUMX _args (-mrp +mre):\n%s", d)
	}

	values, err := postStep(gf, inst, map[string]any{"total": 6}, filepath.Join(work, "sumx"), "")
	if err != nil {
		t.Fatal(err)
	}

	pipeline := filepath.Join(work, "pipeline_outs.json")
	runCG(t, "outs", "-values", values, "-work", filepath.Join(work, "ps"), "-o", filepath.Join(work, "results"), "-json", pipeline)

	got, err := os.ReadFile(pipeline)
	if err != nil {
		t.Fatal(err)
	}

	if d := cmp.Diff("{\n    \"total\": 6\n}", string(got)); d != "" {
		t.Errorf("pipeline outs (-mrp +mre):\n%s", d)
	}
}

// TestReadLocations: each line of a locations file maps a staged bundle, by
// its absolute path, to its location, and a line without both fails.
func TestReadLocations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locations")
	if err := os.WriteFile(path, []byte("in/u1\ts3://bucket/work/ab/cd/outs/x\n\n/abs/u2\t/work/ef/01/outs/y\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := readLocations(path)
	if err != nil {
		t.Fatal(err)
	}

	u1, err := filepath.Abs("in/u1")
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{u1: "s3://bucket/work/ab/cd/outs/x", "/abs/u2": "/work/ef/01/outs/y"}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("locations (-want +got):\n%s", d)
	}

	if err := os.WriteFile(path, []byte("in/u1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := readLocations(path); !errors.Is(err, errBadLocation) {
		t.Errorf("a line without a location: %v, want errBadLocation", err)
	}
}

// TestArgsForkRecord: an entry fork passed to the args step as a value,
// the base64 of its fork record, gives the instance the _args staging the
// fork element does (struct_proj: PP/MAKE/fork<i> gets n = i+1).
func TestArgsForkRecord(t *testing.T) {
	work, gf := t.TempDir(), structProj()
	forks := filepath.Join(work, "entry")

	entryForks(t, gf, forks)

	elems, err := filepath.Glob(filepath.Join(forks, "PP.MAKE", "*"))
	if err != nil || len(elems) != 3 {
		t.Fatalf("entry forks of MAKE: %v %v", elems, err)
	}

	for i, el := range elems {
		rec, err := os.ReadFile(filepath.Join(el, "fork.json"))
		if err != nil {
			t.Fatal(err)
		}

		_, args, err := argsStep(gf, argsFlags{node: "PP.MAKE", forks: repeatedFlag{base64.StdEncoding.EncodeToString(rec)}}, filepath.Join(work, strconv.Itoa(i)))
		if err != nil {
			t.Fatal(err)
		}

		if d := cmp.Diff(`{"n":`+strconv.Itoa(i+1)+`}`, args); d != "" {
			t.Errorf("MAKE fork %d _args (-mrp +mre):\n%s", i, d)
		}
	}
}

// TestCallGraphNeedsEntry: the call-graph subcommands and plan refuse to
// run without the entry they resolve against.
func TestCallGraphNeedsEntry(t *testing.T) {
	for _, sub := range []string{"outs", "stage", "plan"} {
		if err := run(t.Context(), []string{sub}); !errors.Is(err, errNoEntry) {
			t.Errorf("mre %s without -entry: %v, want errNoEntry", sub, err)
		}
	}
}
