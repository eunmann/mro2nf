package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/cg"
)

// shipMRO's stage returns a file in its own code dir and one in the
// include path, both of which the pipeline ships with its code.
const shipMRO = `filetype txt;

stage SHIP(
    out txt code_file,
    out txt mro_file,
    src exec "bin/ship.sh",
)

pipeline P(
    out txt code_file,
    out txt mro_file,
)
{
    call SHIP()

    return (
        code_file = SHIP.code_file,
        mro_file  = SHIP.mro_file,
    )
}

call P()
`

const shipScript = `#!/bin/sh
meta=$2
bin=$(cd "$(dirname "$0")" && pwd)
echo "{\"code_file\":\"$bin/shipped.txt\",\"mro_file\":\"$(dirname "$bin")/ref.txt\"}" > "$meta/_outs"
touch "$meta/_complete"
`

// writeShipPipeline writes shipMRO, its stage code in bin/ and the files it
// returns, into a new dir, and returns the dir.
func writeShipPipeline(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	writeTestFile(t, filepath.Join(dir, "pipeline.mro"), shipMRO)
	writeTestFile(t, filepath.Join(dir, "bin", "shipped.txt"), "shipped\n")
	writeTestFile(t, filepath.Join(dir, "ref.txt"), "ref\n")

	if err := os.WriteFile(filepath.Join(dir, "bin", "ship.sh"), []byte(shipScript), 0o700); err != nil {
		t.Fatal(err)
	}

	return dir
}

// TestStageKeepsPathsIntoTheCode (#625): the pipeline's code is at the same
// path in every task, on the host or in the image, so a stage that returns
// a file in its code dir or in the include path, under a strict
// confinement, posts the path as it is, as mrp keeps the path the stage
// returned, with no copy of the file in any bundle, where the confinement
// refused it as a file outside the task.
func TestStageKeepsPathsIntoTheCode(t *testing.T) {
	dir := writeShipPipeline(t)
	mro := filepath.Join(dir, "pipeline.mro")

	t.Chdir(t.TempDir())

	if _, err := mreIn(t, mro, "plan", "-build", testBuild(), "-psid", "mrp", "-work", "plan",
		"-call", "plan/call", "-nodes", "plan/nodes", "-roots", "plan/roots", "-leaves", "plan/leaves", "-files", "plan/files"); err != nil {
		t.Fatalf("plan: %v", err)
	}

	if _, err := mreIn(t, mro, append([]string{"stage", "-call", "plan/call", "-confine"}, unsplitTask("P.SHIP", ".")[1:]...)...); err != nil {
		t.Fatalf("stage: %v", err)
	}

	for out, path := range map[string]string{"code_file": filepath.Join(dir, "bin", "shipped.txt"), "mro_file": filepath.Join(dir, "ref.txt")} {
		if got, want := compactBundle(t, filepath.Join("outs", out)), `{"`+out+`":"`+path+`"}`; got != want {
			t.Errorf("outs/%s holds %s, want %s", out, got, want)
		}

		if _, err := os.Lstat(filepath.Join("outs", out, "f")); !os.IsNotExist(err) {
			t.Errorf("outs/%s has a files dir (%v), want none", out, err)
		}
	}
}

// TestCodeRootsHoldNoTaskOfTheRun (#625): an include path may hold the
// run's work dir, as a project transpiled from its pipeline's own dir is
// run from there, so no path in the work dir is under a root: a stage's
// output there is a task's, which its reader stages, not a path every task
// reads in place.
func TestCodeRootsHoldNoTaskOfTheRun(t *testing.T) {
	dir := writeShipPipeline(t)
	runWork := filepath.Join(dir, "work")
	task := filepath.Join(runWork, "ab", "cdef")

	if err := os.MkdirAll(task, 0o755); err != nil {
		t.Fatal(err)
	}

	g, err := (&graphFlags{entry: filepath.Join(dir, "pipeline.mro")}).load()
	if err != nil {
		t.Fatal(err)
	}

	roots, err := taskRoots(g, cg.ReadInPlace{}, task, runWork)
	if err != nil {
		t.Fatal(err)
	}

	for path, want := range map[string]bool{
		filepath.Join(dir, "ref.txt"):                 true,
		filepath.Join(dir, "bin", "shipped.txt"):      true,
		filepath.Join(runWork, "01", "2345", "x.txt"): false,
		filepath.Join(task, "x.txt"):                  false,
	} {
		if got, err := roots.Holds(path); err != nil || got != want {
			t.Errorf("the roots hold %s: %t (%v), want %t", path, got, err, want)
		}
	}
}
