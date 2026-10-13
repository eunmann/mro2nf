package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/cg"
)

// s3Root makes a mount root holding bucket/reads.txt and bucket/ref/, the
// layout Mountpoint for S3 shows under <root>/<bucket>, and returns it.
func s3Root(t *testing.T) string {
	t.Helper()

	root := filepath.Join(t.TempDir(), "s3")
	if err := os.MkdirAll(filepath.Join(root, "bucket", "ref"), 0o755); err != nil {
		t.Fatal(err)
	}

	writeTestFile(t, filepath.Join(root, "bucket", "reads.txt"), "7\n")
	writeTestFile(t, filepath.Join(root, "bucket", "ref", "genome.fa"), ">g\n")

	return root
}

// TestPlanMountsS3EntryFiles: under -s3-mount an s3:// entry file value of
// a bucket the mount shows is its path under the mount in the run's call, on
// a profile that sees no head-node file too, and no task stages it, so the
// plan lists it among neither the staged values nor any instance's; a path
// given under the mount is mounted as well. A key no object of a mounted
// bucket has, a path given under the mount of a bucket it does not show, a
// path climbing out of the mount, which is a head-node path, and a relative
// root fail the plan.
func TestPlanMountsS3EntryFiles(t *testing.T) {
	root := s3Root(t)
	reads := filepath.Join(root, "bucket", "reads.txt")

	for _, value := range []string{"s3://bucket/reads.txt", reads} {
		work, err := planEntryFile(t, value, "-host-inputs=false", "-s3-mount", root)
		if err != nil {
			t.Fatalf("plan with reads %s: %v", value, err)
		}

		for _, record := range []string{"leaves", "files"} {
			if got := readText(t, filepath.Join(work, record)); got != "" {
				t.Errorf("reads %s: the %s record holds %q, want nothing staged", value, record, got)
			}
		}

		if call := planCall(t, work); !strings.Contains(call, `"`+reads+`"`) {
			t.Errorf("reads %s: the call names it as %s, want %s", value, call, reads)
		}
	}

	for _, value := range []string{"s3://bucket/none.txt", root + "/other/reads.txt", root + "/bucket/../../reads.txt"} {
		if _, err := planEntryFile(t, value, "-host-inputs=false", "-s3-mount", root); !errors.Is(err, cg.ErrEntryFile) {
			t.Errorf("plan with reads %s: %v, want ErrEntryFile", value, err)
		}
	}

	if _, err := planEntryFile(t, "s3://bucket/reads.txt", "-s3-mount", "s3"); err == nil {
		t.Error("plan under a relative mount root succeeded")
	}
}

// TestPlanStagesS3EntryFilesOfUnmountedBuckets: under -s3-mount an s3://
// entry file value of a bucket the mount does not show, on a profile that
// sees no head-node file, stays its URI in the run's call and is staged, as
// it is with no mount: the plan vets it as a staged value and lists it by
// its index among the run's entry file values, beside a mounted value of the
// same run, which it does not.
func TestPlanStagesS3EntryFilesOfUnmountedBuckets(t *testing.T) {
	root := s3Root(t)

	for _, flags := range [][]string{{"-s3-mount", root}, {"-s3-mount", filepath.Join(t.TempDir(), "none")}} {
		work, err := planEntryFile(t, "s3://other/reads.txt", append([]string{"-host-inputs=false"}, flags...)...)
		if err != nil {
			t.Fatalf("plan %v: %v", flags, err)
		}

		assertStaged(t, work, "EP.COUNT - 0\n", "s3://other/reads.txt")
	}

	if _, err := planEntryFile(t, "s3://other/r*.txt", "-host-inputs=false", "-s3-mount", root); !errors.Is(err, cg.ErrEntryFile) {
		t.Errorf("plan with a staged value Nextflow's stage-in would expand: %v, want ErrEntryFile", err)
	}

	reads := `["s3://other/a.txt", "s3://bucket/reads.txt", "s3://other/b.txt"]`

	work, err := planEntry(t, fixtureDir(t, "entry_filearr"), `{"reads": `+reads+`}`, "-host-inputs=false", "-s3-mount", root)
	if err != nil {
		t.Fatalf("plan with reads %s: %v", reads, err)
	}

	assertStaged(t, work, "EPA.COUNTARR - 0,2\n", "s3://other/a.txt", "", "s3://other/b.txt")

	if call := planCall(t, work); !strings.Contains(call, `"`+filepath.Join(root, "bucket", "reads.txt")+`"`) {
		t.Errorf("the call names s3://bucket/reads.txt as %s, want its mounted path", call)
	}
}

// assertStaged requires the plan in work to stage the entry file values
// staged, each at its index, "" for one it does not stage, by the instances
// files lists, each value named by its URI in the run's call.
func assertStaged(t *testing.T, work, files string, staged ...string) {
	t.Helper()

	var leaves strings.Builder

	for i, v := range staged {
		if v == "" {
			continue
		}

		fmt.Fprintf(&leaves, "%d %s\n", i, base64.StdEncoding.EncodeToString([]byte(v)))

		if call := planCall(t, work); !strings.Contains(call, `"`+v+`"`) {
			t.Errorf("the call does not name %s:\n%s", v, call)
		}
	}

	if got := readText(t, filepath.Join(work, "leaves")); got != leaves.String() {
		t.Errorf("the leaves record holds %q, want %q", got, leaves.String())
	}

	if got := readText(t, filepath.Join(work, "files")); got != files {
		t.Errorf("the files record holds %q, want %q", got, files)
	}
}

// echoMountMRO's stage returns the directory it was given, which its exec
// code reads from its _args.
const echoMountMRO = `stage ECHO(
    in  path ref,
    out path same,
    src exec "echo.sh",
)

pipeline P(
    in  path ref,
    out path same,
)
{
    call ECHO(
        ref = self.ref,
    )

    return (
        same = ECHO.same,
    )
}

call P(
    ref = "s3://bucket/ref",
)
`

const echoScript = `#!/bin/sh
meta=$2
ref=$(tr -d ' \n' < "$meta/_args" | sed 's/.*"ref":"\([^"]*\)".*/\1/')
echo "{\"same\":\"$ref\"}" > "$meta/_outs"
touch "$meta/_complete"
`

// TestStageKeepsPathsReadInPlace: a run hands its stage the path it reads
// in place, an s3:// value planned under -s3-mount at its mounted path and a
// head-node path as it is given, as mrp hands the path it was given, so a
// path the stage writes into an output names the same file in every task;
// and a stage that returns the path, under a strict confinement, posts it
// as it is, with no copy of the tree in any bundle. A head-node path is
// read in place under -host-in-place, as on the local and grid profiles.
func TestStageKeepsPathsReadInPlace(t *testing.T) {
	root := s3Root(t)
	host := filepath.Join(t.TempDir(), "ref")
	if err := os.MkdirAll(host, 0o755); err != nil {
		t.Fatal(err)
	}

	writeTestFile(t, filepath.Join(host, "genome.fa"), ">g\n")

	cases := []struct {
		name, value, ref string
		planFlags, flags []string
	}{
		{"mounted", "s3://bucket/ref", filepath.Join(root, "bucket", "ref"), []string{"-host-inputs=false"}, nil},
		{"host", host, host, []string{"-host-in-place"}, []string{"-host-in-place"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stageEcho(t, root, tc.value, tc.planFlags, tc.flags)

			for _, record := range []string{"leaves", "files"} {
				if got := readText(t, filepath.Join("plan", record)); got != "" {
					t.Errorf("the %s record holds %q, want nothing staged", record, got)
				}
			}

			assertEchoed(t, tc.ref)
		})
	}
}

// stageEcho plans echoMountMRO with ref given as value under -s3-mount
// root and planFlags, into plan/ of a new cwd, and runs its stage task there
// under a strict confinement and flags.
func stageEcho(t *testing.T, root, value string, planFlags, flags []string) {
	t.Helper()

	dir := t.TempDir()
	mro := writeTestFile(t, filepath.Join(dir, "pipeline.mro"), strings.Replace(echoMountMRO, "s3://bucket/ref", value, 1))

	if err := os.WriteFile(filepath.Join(dir, "echo.sh"), []byte(echoScript), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Chdir(t.TempDir())

	if _, err := mreIn(t, mro, append([]string{
		"plan", "-psid", "mrp", "-s3-mount", root, "-work", "plan",
		"-call", "plan/call", "-nodes", "plan/nodes", "-roots", "plan/roots", "-leaves", "plan/leaves", "-files", "plan/files",
	},
		planFlags...)...); err != nil {
		t.Fatalf("plan: %v", err)
	}

	if _, err := mreIn(t, mro, append(append([]string{"stage", "-call", "plan/call", "-s3-mount", root, "-confine"}, flags...),
		unsplitTask("P.ECHO", ".")[1:]...)...); err != nil {
		t.Fatalf("stage: %v", err)
	}
}

// assertEchoed requires the ECHO task in the cwd to have been handed ref
// and to have posted it as it is, with no files dir below its args or in
// the bundle it posted.
func assertEchoed(t *testing.T, ref string) {
	t.Helper()

	var args struct {
		Ref string `json:"ref"`
	}

	if err := json.Unmarshal([]byte(readText(t, filepath.Join("chnk0", "_args"))), &args); err != nil || args.Ref != ref {
		t.Errorf("ECHO's _args ref %q (%v), want %s", args.Ref, err, ref)
	}

	if got, want := compactBundle(t, filepath.Join("outs", "same")), `{"same":"`+ref+`"}`; got != want {
		t.Errorf("outs/same holds %s, want %s", got, want)
	}

	for _, dir := range []string{"args", filepath.Join("outs", "same")} {
		if _, err := os.Lstat(filepath.Join(dir, "f")); !os.IsNotExist(err) {
			t.Errorf("%s has a files dir (%v), want none", dir, err)
		}
	}
}
