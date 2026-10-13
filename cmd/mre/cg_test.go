package main

import (
	"encoding/base64"
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
// writing the args bundle of the instance a names into out.
func argsStep(gf graphFlags, a argsFlags, out string) (cg.Instance, error) {
	g, err := gf.load()
	if err != nil {
		return cg.Instance{}, err
	}

	return a.write(g, out)
}

// postStep runs mre stage's post step against the invocation gf locates,
// posting the outs bundle outs of the instance the args bundle args stamps.
func postStep(gf graphFlags, p postFlags, args, outs string) error {
	g, err := gf.load()
	if err != nil {
		return err
	}

	return p.post(g, args, outs)
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

	sumx := argsFlags{node: "PP.SUMX"}

	for i, el := range elems {
		args, outs := filepath.Join(work, strconv.Itoa(i), "args"), filepath.Join(work, strconv.Itoa(i), "outs")

		if inst, err := argsStep(gf, argsFlags{node: "PP.MAKE", in: repeatedFlag{el}}, args); err != nil || inst.Disabled {
			t.Fatalf("MAKE fork %d: %+v, %v", i, inst, err)
		}

		if d := cmp.Diff(`{"n":`+strconv.Itoa(i+1)+`}`, compactBundle(t, args)); d != "" {
			t.Errorf("MAKE fork %d _args (-mrp +mre):\n%s", i, d)
		}

		n := float64(i + 1)
		writeTestBundle(t, outs, map[string]any{"p": map[string]any{"x": n, "y": 10 * n}})

		if err := postStep(gf, postFlags{forks: filepath.Join(work, strconv.Itoa(i), "forks"), forkValues: filepath.Join(work, strconv.Itoa(i), "fork_values")}, args, outs); err != nil {
			t.Fatal(err)
		}

		sumx.in = append(sumx.in, outs)
	}

	if _, err := argsStep(gf, sumx, filepath.Join(work, "sumx", "args")); err != nil {
		t.Fatal(err)
	}

	if d := cmp.Diff(`{"xs":[1,2,3]}`, compactBundle(t, filepath.Join(work, "sumx", "args"))); d != "" {
		t.Errorf("SUMX _args (-mrp +mre):\n%s", d)
	}

	outs := filepath.Join(work, "sumx", "outs")
	writeTestBundle(t, outs, map[string]any{"total": 6})

	if err := postStep(gf, postFlags{forks: filepath.Join(work, "sumx", "forks"), forkValues: filepath.Join(work, "sumx", "fork_values")}, filepath.Join(work, "sumx", "args"), outs); err != nil {
		t.Fatal(err)
	}

	pipeline := filepath.Join(work, "pipeline_outs.json")
	runCG(t, "outs", "-in", outs, "-work", filepath.Join(work, "ps"), "-o", filepath.Join(work, "results"), "-json", pipeline)

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

		args := filepath.Join(work, strconv.Itoa(i))
		if _, err := argsStep(gf, argsFlags{node: "PP.MAKE", forks: repeatedFlag{base64.StdEncoding.EncodeToString(rec)}}, args); err != nil {
			t.Fatal(err)
		}

		if d := cmp.Diff(`{"n":`+strconv.Itoa(i+1)+`}`, compactBundle(t, args)); d != "" {
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
