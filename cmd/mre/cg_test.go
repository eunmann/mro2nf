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

	dir := filepath.Join("..", "..", "testdata", "struct_proj")
	argv := append([]string{args[0], "-mro", filepath.Join(dir, "pipeline.mro"), "-mropath", dir}, args[1:]...)

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

// TestCallGraphSubcommands drives struct_proj through post -entry, args,
// post and outs as a run's tasks would, each stage's outs standing in for
// the stage, and requires the _args and pipeline outs of a real mrp run of
// struct_proj: PP/MAKE/fork<i> gets n = i+1, PP/SUMX gets xs = [1,2,3], and
// PP/fork0/_outs is {"total":6}.
func TestCallGraphSubcommands(t *testing.T) {
	work := t.TempDir()
	forks := filepath.Join(work, "entry")

	runCG(t, "post", "-entry", "-forks", forks)

	elems, err := filepath.Glob(filepath.Join(forks, "PP.MAKE", "*"))
	if err != nil || len(elems) != 3 {
		t.Fatalf("entry forks of MAKE: %v %v", elems, err)
	}

	sumx := make([]string, 0, 5+2*len(elems))
	sumx = append(sumx, "args", "-node", "PP.SUMX", "-o", filepath.Join(work, "sumx", "args"))

	for i, el := range elems {
		args, outs := filepath.Join(work, strconv.Itoa(i), "args"), filepath.Join(work, strconv.Itoa(i), "outs")

		if state := runCG(t, "args", "-node", "PP.MAKE", "-in", el, "-o", args); state != "enabled\n" {
			t.Errorf("MAKE fork %d state %q", i, state)
		}

		if d := cmp.Diff(`{"n":`+strconv.Itoa(i+1)+`}`, compactBundle(t, args)); d != "" {
			t.Errorf("MAKE fork %d _args (-mrp +mre):\n%s", i, d)
		}

		n := float64(i + 1)
		writeTestBundle(t, outs, map[string]any{"p": map[string]any{"x": n, "y": 10 * n}})
		runCG(t, "post", "-args", args, "-outs", outs, "-forks", filepath.Join(work, strconv.Itoa(i), "forks"))

		sumx = append(sumx, "-in", outs)
	}

	runCG(t, sumx...)

	if d := cmp.Diff(`{"xs":[1,2,3]}`, compactBundle(t, filepath.Join(work, "sumx", "args"))); d != "" {
		t.Errorf("SUMX _args (-mrp +mre):\n%s", d)
	}

	outs := filepath.Join(work, "sumx", "outs")
	writeTestBundle(t, outs, map[string]any{"total": 6})
	runCG(t, "post", "-args", filepath.Join(work, "sumx", "args"), "-outs", outs)
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

// TestArgsForkRecord: an entry fork passed to mre args as a value, the
// base64 of its fork record, gives the instance the _args staging the fork
// element does (struct_proj: PP/MAKE/fork<i> gets n = i+1).
func TestArgsForkRecord(t *testing.T) {
	work := t.TempDir()
	forks := filepath.Join(work, "entry")

	runCG(t, "post", "-entry", "-forks", forks)

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
		runCG(t, "args", "-node", "PP.MAKE", "-fork", base64.StdEncoding.EncodeToString(rec), "-o", args)

		if d := cmp.Diff(`{"n":`+strconv.Itoa(i+1)+`}`, compactBundle(t, args)); d != "" {
			t.Errorf("MAKE fork %d _args (-mrp +mre):\n%s", i, d)
		}
	}
}

// TestCallGraphNeedsMRO: the call-graph subcommands refuse to run without
// the invocation they resolve against.
func TestCallGraphNeedsMRO(t *testing.T) {
	for _, sub := range []string{"args", "post", "outs"} {
		if err := run(t.Context(), []string{sub}); !errors.Is(err, errNoMRO) {
			t.Errorf("mre %s without -mro: %v, want errNoMRO", sub, err)
		}
	}
}
