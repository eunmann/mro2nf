package main

import (
	"encoding/base64"
	"errors"
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

// TestPlanMountsS3EntryFiles: under -s3-mount an s3:// entry file value is
// its path under the mount in the run's call, on a profile that sees no
// head-node file too, and no task stages it, so the plan lists it among
// neither the staged values nor any instance's; a path given under the mount
// is mounted as well. A bucket the mount does not show, a path climbing out
// of the mount, which is a head-node path, and a relative root fail the plan.
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

		call, err := base64.StdEncoding.DecodeString(readText(t, filepath.Join(work, "call")))
		if err != nil || !strings.Contains(string(call), `"`+reads+`"`) {
			t.Errorf("reads %s: the call names it as %s (%v), want %s", value, call, err, reads)
		}
	}

	for _, value := range []string{"s3://other/reads.txt", "s3://bucket/none.txt", root + "/bucket/../../reads.txt"} {
		if _, err := planEntryFile(t, value, "-host-inputs=false", "-s3-mount", root); !errors.Is(err, cg.ErrEntryFile) {
			t.Errorf("plan with reads %s: %v, want ErrEntryFile", value, err)
		}
	}

	if _, err := planEntryFile(t, "s3://bucket/reads.txt", "-s3-mount", "s3"); err == nil {
		t.Error("plan under a relative mount root succeeded")
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

// TestStageKeepsMountedPaths: a run planned under -s3-mount hands its stage
// the mounted path itself, as mrp hands the path it was given, so a path the
// stage writes into an output names the same file in every task; and a
// stage that returns the path, under a strict confinement, posts it as it
// is, with no copy of the mounted tree in any bundle.
func TestStageKeepsMountedPaths(t *testing.T) {
	root := s3Root(t)
	ref := filepath.Join(root, "bucket", "ref")

	dir := t.TempDir()
	mro := writeTestFile(t, filepath.Join(dir, "pipeline.mro"), echoMountMRO)

	if err := os.WriteFile(filepath.Join(dir, "echo.sh"), []byte(echoScript), 0o700); err != nil {
		t.Fatal(err)
	}

	plan := t.TempDir()
	if _, err := mreIn(t, mro, "plan", "-psid", "mrp", "-host-inputs=false", "-s3-mount", root, "-work", plan,
		"-call", filepath.Join(plan, "call"), "-nodes", filepath.Join(plan, "nodes"), "-roots", filepath.Join(plan, "roots"),
		"-counts", filepath.Join(plan, "counts"), "-leaves", filepath.Join(plan, "leaves"), "-files", filepath.Join(plan, "files")); err != nil {
		t.Fatalf("plan: %v", err)
	}

	t.Chdir(t.TempDir())

	if _, err := mreIn(t, mro, append([]string{"stage", "-call", filepath.Join(plan, "call"), "-s3-mount", root, "-confine"},
		unsplitTask("P.ECHO", ".")[1:]...)...); err != nil {
		t.Fatalf("stage: %v", err)
	}

	for bundle, want := range map[string]string{
		"args":       `{"ref":"` + ref + `"}`,
		"stage_outs": `{"same":"` + ref + `"}`,
		"outs/same":  `{"same":"` + ref + `"}`,
	} {
		if got := compactBundle(t, bundle); got != want {
			t.Errorf("%s holds %s, want %s", bundle, got, want)
		}

		if _, err := os.Lstat(filepath.Join(bundle, "f")); !os.IsNotExist(err) {
			t.Errorf("%s has a files dir (%v), want none", bundle, err)
		}
	}
}
