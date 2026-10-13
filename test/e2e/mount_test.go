//go:build e2e

package e2e

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/google/go-cmp/cmp"
)

// mountedFixture records an entry input's path in a JSON output that
// another task opens, the shape that failed on AWS Batch.
const mountedFixture = "mounted_paths"

// TestDockerMountedS3Inputs simulates the S3 mount of the awsbatch profile with a
// local dir laid out as Mountpoint for S3 shows a bucket, bound read-only
// into every container, under copy-staging into per-task scratch dirs. With
// the inputs staged into each task, as on any isolated executor, MAKE_SHARD
// opens the path RECORD_SAMPLE recorded, which was in RECORD_SAMPLE's own
// scratch, and fails, though mrp, given the same host paths, succeeds. With
// s3:// values under the mount, every stage receives the mounted path, as
// mrp passes the path it was given, and the run gives mrp's outs, on the
// docker profile and on the awsbatch profile itself, and no stage task
// holds a copy of the mounted reference.
func TestDockerMountedS3Inputs(t *testing.T) {
	t.Parallel()

	requireDocker(t)

	fixtureDir := filepath.Join(root, "testdata", mountedFixture)
	golden := filepath.Join(fixtureDir, "expected", "override_outs.json")
	s3 := simulatedS3(t, fixtureDir)
	image := pipelineImage(t, fixtureDir, mountedFixture)
	mounted := map[string]any{"fastqs": "s3://simbucket/fastqs", "reference_path": "s3://simbucket/ref"}

	t.Run("staged", func(t *testing.T) {
		t.Parallel()

		proj := transpile(t, mountedFixture)
		writeParams(t, proj, map[string]any{
			"fastqs":         filepath.Join(s3, "simbucket", "fastqs"),
			"reference_path": filepath.Join(s3, "simbucket", "ref"),
		})

		if err := runNextflowAllowingFailedAttempts(t, proj, append(dockerMountArgs(t, proj, image, s3), "-with-trace", "trace.txt")...); err == nil {
			t.Fatal("the run with staged inputs succeeded")
		}

		assertFailedInRecordedPath(t, proj)
	})

	t.Run("mounted", func(t *testing.T) {
		t.Parallel()

		proj := transpile(t, mountedFixture)
		writeParams(t, proj, mounted)

		if err := runNextflow(t, proj, append(dockerMountArgs(t, proj, image, s3), "--mro2nf_s3_mount", s3, "-with-trace", "trace.txt")...); err != nil {
			t.Fatalf("nextflow (docker profile, mounted): %v", err)
		}

		goldenJSON(t, mountedFixture, filepath.Join(proj, "results", "pipeline_outs.json"), golden)
		assertReadInPlace(t, proj, s3)
	})

	t.Run("awsbatch", func(t *testing.T) {
		t.Parallel()

		requireAmazonPlugin(t)

		proj := transpile(t, mountedFixture)
		writeParams(t, proj, mounted)

		err := runAWSBatchLocally(t, proj, image, []string{readOnlyBind(s3)},
			"--mro2nf_s3_mount", s3, "-params-file", "params.json", "-with-trace", "trace.txt")
		if err != nil {
			t.Fatalf("nextflow (awsbatch profile, local docker, mounted): %v", err)
		}

		goldenJSON(t, mountedFixture, filepath.Join(proj, "results", "pipeline_outs.json"), golden)
		assertReadInPlace(t, proj, s3)
	})
}

// TestMrpDiffHostEntryPaths (#543): given head-node paths, mounted_paths
// runs under mrp and on the local profile to the same outputs and stage
// ABI, and every path below the inputs that a stage's _args names is the
// same string under both: the entry paths mrp passes, the reference path
// SETUP_REF returns, and the read files MAKE_SHARD's split chooses.
func TestMrpDiffHostEntryPaths(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	bin := martianBin(t)

	shardSkip(t)

	dir := filepath.Join(t.TempDir(), mountedFixture)
	if err := shim.CopyTree(filepath.Join(root, "testdata", mountedFixture), dir); err != nil {
		t.Fatal(err)
	}

	input := filepath.Join(dir, "input")
	mro := filepath.Join(dir, "pipeline.mro")

	src, err := os.ReadFile(mro)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(mro); err != nil {
		t.Fatal(err)
	}

	writeFileT(t, mro, strings.ReplaceAll(string(src), `"input/`, `"`+input+`/`))

	tmp := t.TempDir()
	runMrpAt(t, bin, dir, "pipeline.mro", tmp)

	proj := transpileDir(t, dir)
	if err := runNextflow(t, proj, mrpPoolArgs(t, proj)...); err != nil {
		t.Fatalf("nextflow: %v", err)
	}

	compareMrpToNextflow(t, mountedFixture, tmp, proj)
	compareStageABI(t, mountedFixture, tmp, proj)

	mrpArgs, err := filepath.Glob(filepath.Join(tmp, "mrp", "MOUNTED_PATHS", "*", "fork0", "*", "_args"))
	if err != nil {
		t.Fatal(err)
	}

	nfArgs, err := filepath.Glob(filepath.Join(proj, "work", "*", "*", "*", "_args"))
	if err != nil {
		t.Fatal(err)
	}

	mrpPaths, nfPaths := pathsUnder(t, input, ran(mrpArgs, "_jobinfo")), pathsUnder(t, input, ran(nfArgs, mreStageFile))
	if len(mrpPaths) == 0 {
		t.Fatal("no _args of mrp's names a path below the inputs")
	}

	if d := cmp.Diff(mrpPaths, nfPaths); d != "" {
		t.Errorf("the paths below the inputs the stages' _args name (-mrp +mro2nf):\n%s", d)
	}
}

// ran are the _args files of args that a phase ran with: those beside a
// file named stamp, which only a phase that ran writes, each once, not
// again through mrp's link to its phase dir.
func ran(args []string, stamp string) []string {
	var out []string

	for _, a := range args {
		dir := filepath.Dir(a)

		info, err := os.Lstat(dir)
		if err == nil && info.Mode()&fs.ModeSymlink == 0 && regularFileExists(filepath.Join(dir, stamp)) {
			out = append(out, a)
		}
	}

	return out
}

// pathsUnder is every string at or below dir that the JSON files at paths
// hold, once per occurrence, sorted.
func pathsUnder(t *testing.T, dir string, paths []string) []string {
	t.Helper()

	var out []string

	for _, p := range paths {
		var v any

		readJSON(t, p, &v)
		walkJSONStrings(v, func(s string) {
			if s == dir || strings.HasPrefix(s, dir+"/") {
				out = append(out, s)
			}
		})
	}

	slices.Sort(out)

	return out
}

func walkJSONStrings(v any, visit func(string)) {
	switch x := v.(type) {
	case string:
		visit(x)
	case []any:
		for _, e := range x {
			walkJSONStrings(e, visit)
		}
	case map[string]any:
		for _, e := range x {
			walkJSONStrings(e, visit)
		}
	}
}

// TestResumeRechecksTheS3Mount: PLAN reads an s3:// input in place only
// when its instance mounts the input's bucket, which no cache key holds, so
// a -resume re-runs PLAN, which sees the mounts the instances have now, and
// keeps every other task cached, as PLAN's records are unchanged.
func TestResumeRechecksTheS3Mount(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	s3 := simulatedS3(t, filepath.Join(root, "testdata", mountedFixture))
	proj := transpile(t, mountedFixture)
	writeParams(t, proj, map[string]any{"fastqs": "s3://simbucket/fastqs", "reference_path": "s3://simbucket/ref"})

	if err := runNextflow(t, proj, "--mro2nf_s3_mount", s3, "-params-file", "params.json"); err != nil {
		t.Fatalf("first run: %v", err)
	}

	if err := runNextflow(t, proj, "--mro2nf_s3_mount", s3, "-params-file", "params.json", "-resume", "-with-trace", "resume.txt"); err != nil {
		t.Fatalf("-resume rerun: %v", err)
	}

	for _, row := range readTrace(t, filepath.Join(proj, "resume.txt")) {
		if rerun := row.Name == "PLAN"; (row.Status == "CACHED") == rerun {
			t.Errorf("task %s: status %s, want cached=%v", row.Name, row.Status, !rerun)
		}
	}
}

// simulatedS3 lays the fixture's inputs out as Mountpoint for S3 shows
// bucket simbucket under a mount root, <root>/simbucket/{fastqs,ref}, and
// returns the root.
func simulatedS3(t *testing.T, fixtureDir string) string {
	t.Helper()

	s3 := filepath.Join(t.TempDir(), "s3")

	for _, d := range []string{"fastqs", "ref"} {
		dst := filepath.Join(s3, "simbucket", d)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := shim.CopyTree(filepath.Join(fixtureDir, "input", d), dst); err != nil {
			t.Fatal(err)
		}
	}

	return s3
}

func readOnlyBind(dir string) string {
	return "-v " + dir + ":" + dir + ":ro"
}

func writeParams(t *testing.T, proj string, params map[string]any) {
	t.Helper()

	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}

	writeFileT(t, filepath.Join(proj, "params.json"), string(raw))
}

// dockerMountArgs writes proj's configs for a run of its docker profile in
// image under copy-staging, s3 bound read-only into every container, and
// returns its launch args, the entry values params.json's.
func dockerMountArgs(t *testing.T, proj, image, s3 string) []string {
	t.Helper()

	writeCloudConfig(t, proj)
	writeFileT(t, filepath.Join(proj, "iso.config"), dockerProfileConfig()+"docker.runOptions = '"+readOnlyBind(s3)+"'\n")

	return []string{"-profile", "docker", "-c", "iso.config", "-c", "cloud.config", "--mro2nf_image", image, "-params-file", "params.json"}
}

// assertFailedInRecordedPath requires the failed run of proj to have failed
// in MAKE_SHARD's split alone, on the path RECORD_SAMPLE was handed in its
// own task. The task ran in a scratch dir, which keeps no stage stamp for
// the outcome comparison to read, so the trace and the run log tell.
func assertFailedInRecordedPath(t *testing.T, proj string) {
	t.Helper()

	var failed []string

	for _, r := range readTrace(t, filepath.Join(proj, "trace.txt")) {
		if r.Status == "FAILED" {
			failed = append(failed, r.Name)
		}
	}

	if len(failed) != 1 || !strings.Contains(failed[0], "(MOUNTED_PATHS.MAKE_SHARD)") || !strings.Contains(failed[0], "_SP ") {
		t.Errorf("failed tasks %q, want MAKE_SHARD's split alone", failed)
	}

	runLog, err := os.ReadFile(filepath.Join(proj, "run.log"))
	if err != nil {
		t.Fatal(err)
	}

	if want := "No such file or directory: "; !strings.Contains(string(runLog), want) || !strings.Contains(string(runLog), "/args/f/L0000/fastqs'") {
		t.Errorf("the run log does not show the split opening RECORD_SAMPLE's staged path:\n%s", runLog)
	}
}

// assertReadInPlace requires a run of proj under the mount s3 to have read
// the mounted inputs where they are: a stage's outs bundle names the mounted
// reference by its path, and no task's work dir holds a copy of it but the
// OUTS task's, which publishes it as mrp does.
func assertReadInPlace(t *testing.T, proj, s3 string) {
	t.Helper()

	var publish []string

	for _, r := range readTrace(t, filepath.Join(proj, "trace.txt")) {
		if strings.HasPrefix(r.Name, "OUTS") {
			publish = append(publish, r.Hash)
		}
	}

	if len(publish) == 0 {
		t.Fatal("the trace lists no OUTS task")
	}

	ref, named := `"`+filepath.Join(s3, "simbucket", "ref")+`"`, false
	work := filepath.Join(proj, "work")

	err := filepath.WalkDir(work, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(work, path)
		if err != nil {
			return err
		}

		if d.Name() == "genome.fa" && !hasAnyPrefix(filepath.ToSlash(rel), publish) {
			t.Errorf("%s holds a copy of the mounted reference", rel)
		}

		if d.Name() == "data.json" && !named {
			raw, err := os.ReadFile(path)
			named = err == nil && strings.Contains(string(raw), ref)

			return err
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk work/: %v", err)
	}

	if !named {
		t.Errorf("no bundle under work/ names the reference by its mounted path %s", ref)
	}
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}

	return false
}
