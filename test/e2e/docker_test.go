//go:build e2e

package e2e

// Container-isolation e2e: run transpiled pipelines under the generated
// project's docker profile, where every task runs in a container of the
// images the project ships, the stage image its Dockerfile builds FROM the
// runtime image deploy/runtime/Dockerfile builds, and the runtime image for
// PLAN and OUTS. A container sees its code below /opt/mro2nf/code, not at its
// host path, and an entry file only if the task staged it, as on AWS Batch
// and HealthOmics; a task that reads a host path the run did not stage fails
// here while the local suite still passes.

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// requireDocker gates a test on docker being present AND usable (`docker
// info`), plus nextflow and java. Like requireTools, a missing prerequisite
// skips locally but FAILS under E2E_REQUIRE=1, so a runner-image regression
// cannot turn the suite into a silent green skip.
func requireDocker(t *testing.T) {
	t.Helper()
	requireTools(t, "docker", "nextflow", "java")

	if err := exec.Command("docker", "info").Run(); err != nil {
		missingPrereqf(t, "docker not usable: %v", err)
	}
}

var (
	isoCfgOnce    sync.Once
	isoCfgContent string
)

// isoConfig returns the Nextflow config lines for isolation runs, detected
// once per test process. On rootful Docker (GitHub Actions) containers default
// to root, so task outputs come back root-owned and work-dir cleanup fails
// with "Permission denied". Mapping the container to the runner's uid/gid
// (with a writable HOME) keeps files owned by the runner. On ROOTLESS Docker
// the container's root already maps to the host user, and forcing -u maps to
// an unprivileged subuid that cannot write the bind-mounted work dir, so the
// mapping applies only when rootful.
func isoConfig() string {
	isoCfgOnce.Do(func() {
		out, err := exec.Command("docker", "info", "-f", "{{.SecurityOptions}}").Output()
		if err == nil && strings.Contains(string(out), "rootless") {
			return
		}

		isoCfgContent = fmt.Sprintf("docker.runOptions = '-u %d:%d -e HOME=/tmp'\n", os.Getuid(), os.Getgid())
	})

	return isoCfgContent
}

// checkoutImage tags a test image per checkout, so a concurrent run in
// another worktree cannot replace it mid-suite.
func checkoutImage(name string) string {
	sum := sha256.Sum256([]byte(root))

	return name + ":" + hex.EncodeToString(sum[:6])
}

var (
	runtimeImgOnce sync.Once
	errRuntimeImg  error
)

// runtimeImage builds, once per test process, the runtime image from
// deploy/runtime/Dockerfile, the image a release publishes, and returns its
// tag.
func runtimeImage(t *testing.T) string {
	t.Helper()

	tag := checkoutImage("mro2nf-runtime")

	runtimeImgOnce.Do(func() {
		cmd := exec.Command("docker", "build", "-q", "-t", tag, "-f", filepath.Join("deploy", "runtime", "Dockerfile"), ".")
		cmd.Dir = root

		if out, err := cmd.CombinedOutput(); err != nil {
			errRuntimeImg = fmt.Errorf("docker build %s: %w\n%s", tag, err, out)
		}
	})

	if errRuntimeImg != nil {
		t.Fatal(errRuntimeImg)
	}

	return tag
}

// stageImage builds proj's stage image with its build.sh, FROM the runtime
// image, under a tag named for name, and returns the tag.
func stageImage(t *testing.T, proj, name string) string {
	t.Helper()

	tag := checkoutImage("mro2nf-stage-" + strings.ToLower(name))

	cmd := exec.Command("bash", "build.sh", tag, "-q", "--build-arg", "RUNTIME="+runtimeImage(t))
	cmd.Dir = proj

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build.sh %s: %v\n%s", tag, err, out)
	}

	return tag
}

// runDocker runs proj under its docker profile in the stage image built
// for name and the runtime image.
func runDocker(t *testing.T, proj, name string, args ...string) error {
	t.Helper()

	writeFileT(t, filepath.Join(proj, "iso.config"), isoConfig())

	return runNextflow(t, proj, append([]string{
		"-profile", "docker", "-c", "iso.config",
		"--mro2nf_image", stageImage(t, proj, name), "--mro2nf_runtime_image", runtimeImage(t),
	}, args...)...)
}

// dockerCases are the golden cases TestDockerIsolation runs isolated: every
// process kind (split, map call, keyed fork, disabled call, a split's join
// resources, the special scheduler key), each adapter (py, exec, comp), the
// null-bundle and zero-fork shapes, and file values of every shape an entry
// input stages.
var dockerCases = []string{
	"split_test", "map_pipe", "map_file_keyed", "disabled_map", "join_resources", "special_resource",
	"kitchen_sink", "file_chain", "map_split_file", "map_file_split", "struct_file_array", "exec_min",
	"mixed_adapters", "comp_split", "empty_fork_min", "map_null_map", "disabled_callref",
	"entry_file_override", "entry_filearr_override", "entry_struct_file_override", "entry_mapfile_override",
	"entry_dir_override", "entry_file_empty_override", "split_from_file_override",
}

// TestDockerIsolation runs each dockerCases golden under full container
// isolation and requires the outs and the published tree mrp's run gave.
func TestDockerIsolation(t *testing.T) {
	t.Parallel()

	requireDocker(t)

	for _, name := range dockerCases {
		i := slices.IndexFunc(goldenCases, func(tc goldenCase) bool { return tc.name == name })
		if i < 0 {
			t.Fatalf("dockerCases names %q, which is no goldenCases entry", name)
		}

		tc := goldenCases[i]

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fixtureDir := filepath.Join(root, "testdata", tc.fixture)
			proj := transpile(t, tc.fixture)

			var args []string

			if strings.HasSuffix(tc.name, "_override") {
				renderOverrideParams(t, fixtureDir, proj)
				args = append(args, "-params-file", "params.json")
			}

			if err := runDocker(t, proj, tc.name, args...); err != nil {
				t.Fatalf("nextflow (isolated): %v", err)
			}

			goldenJSON(t, tc.fixture, filepath.Join(proj, "results", "pipeline_outs.json"), filepath.Join(fixtureDir, tc.golden))
			assertGoldenTree(t, tc.name, tc.fixture, proj)
		})
	}
}

// TestDockerEntryOverrides checks file-typed entry-input overrides under
// isolation, the strictest staging proof: every override file lives in a
// host temp dir, neither in the image nor below a code root, so the stage
// can read it only if the task staged it, the AWS Batch and HealthOmics S3
// path. It covers a scalar file, a file[], a file[] of one basename, a
// struct with a file, a map of files and a directory.
func TestDockerEntryOverrides(t *testing.T) {
	t.Parallel()

	requireDocker(t)

	work := t.TempDir()
	files := map[string]string{
		"o_scalar.txt":  "5\n7\n9\n",
		"o_arr1.txt":    "2\n3\n",
		"o_arr2.txt":    "10\n",
		"o_ref.txt":     "4\n4\n",
		"o_m1.txt":      "6\n",
		"o_m2.txt":      "14\n",
		"sb1/reads.txt": "2\n3\n",
		"sb2/reads.txt": "10\n",
		"odir/e.txt":    "10\n",
		"odir/f.txt":    "11\n12\n",
	}

	for name, content := range files {
		path := filepath.Join(work, name)

		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}

		writeFileT(t, path, content)
	}

	p := func(name string) string { return filepath.Join(work, name) }

	cases := []struct {
		name, fixture string
		params        map[string]any
		expect        string
	}{
		{"entry_file_override", "entry_file", map[string]any{"reads": p("o_scalar.txt")}, `{"total": 42.0}`},
		{"entry_filearr_override", "entry_filearr", map[string]any{"reads": []string{p("o_arr1.txt"), p("o_arr2.txt")}}, `{"total": 30.0}`},
		{"entry_filearr_samebasename", "entry_filearr", map[string]any{"reads": []string{p("sb1/reads.txt"), p("sb2/reads.txt")}}, `{"total": 30.0}`},
		{"entry_struct_file_override", "entry_struct_file", map[string]any{"cfg": map[string]any{"ref": p("o_ref.txt"), "n": 5}}, `{"total": 40.0}`},
		{"entry_mapfile_override", "entry_mapfile", map[string]any{"reads": map[string]any{"a": p("o_m1.txt"), "b": p("o_m2.txt")}}, `{"total": 40.0}`},
		{"entry_dir_override", "entry_dir", map[string]any{"fastqs": p("odir")}, `{"total": 66.0}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			proj := transpile(t, tc.fixture)

			params, err := json.Marshal(tc.params)
			if err != nil {
				t.Fatalf("marshal params: %v", err)
			}

			writeFileT(t, filepath.Join(proj, "params.json"), string(params))

			if err := runDocker(t, proj, tc.fixture, "-params-file", "params.json"); err != nil {
				t.Fatalf("nextflow (isolated): %v", err)
			}

			var got, want any

			readJSON(t, filepath.Join(proj, "results", "pipeline_outs.json"), &got)

			if err := decodeJSONNumber([]byte(tc.expect), &want); err != nil {
				t.Fatalf("parse expected %q: %v", tc.expect, err)
			}

			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("pipeline outs mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestGeneratedAWSBatchImage validates the awsbatch profile without a live
// account: its stage image built from the generated Dockerfile FROM the
// runtime image (#310), and a run of the awsbatch profile with only the
// executor swapped for local docker, so the profile's tools, code root,
// images and staging policy are the ones under test.
func TestGeneratedAWSBatchImage(t *testing.T) {
	t.Parallel()

	requireDocker(t)

	proj := transpile(t, "diamond_min")
	image := stageImage(t, proj, "awsbatch_diamond_min")

	writeFileT(t, filepath.Join(proj, "local.config"), "process.executor = 'local'\ndocker.enabled = true\n"+isoConfig())

	err := runNextflow(t, proj, "-profile", "awsbatch", "-c", "local.config",
		"--mro2nf_image", image, "--mro2nf_runtime_image", runtimeImage(t), "--outdir", filepath.Join(proj, "results"))
	if err != nil {
		t.Fatalf("nextflow (awsbatch profile, local docker): %v", err)
	}

	goldenJSON(t, "diamond_min", filepath.Join(proj, "results", "pipeline_outs.json"),
		filepath.Join(root, "testdata", "diamond_min", "expected", "outs.json"))
}

// TestGeneratedHealthOmicsPackage validates the HealthOmics package without
// a live account: package.sh refuses an image not pinned by digest (#334)
// and zips the files main.nf reads, with the healthomics profile the
// workflow's own config and the images pinned, so `nextflow config` of the
// unzipped workflow, given no -profile as HealthOmics gives none, resolves
// the pinned images and the HealthOmics pubdir (#309). Neither the build
// recipe nor any file of the pipeline's, an entry default included, is in it.
func TestGeneratedHealthOmicsPackage(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "zip")

	proj := transpile(t, "entry_file")

	pkg := func(images ...string) ([]byte, error) {
		cmd := exec.Command("bash", append([]string{"package.sh"}, images...)...)
		cmd.Dir = proj

		return cmd.CombinedOutput()
	}

	stage := "123456789012.dkr.ecr.us-east-1.amazonaws.com/stage@sha256:" + strings.Repeat("a", 64)
	runtime := "123456789012.dkr.ecr.us-east-1.amazonaws.com/runtime@sha256:" + strings.Repeat("b", 64)

	if out, err := pkg(strings.TrimSuffix(stage, "@sha256:"+strings.Repeat("a", 64))+":latest", runtime); err == nil {
		t.Errorf("package.sh packaged a stage image pinned by a tag:\n%s", out)
	}

	if out, err := pkg(stage, runtime); err != nil {
		t.Fatalf("package.sh: %v\n%s", err, out)
	}

	unzipped := unzipWorkflow(t, filepath.Join(proj, "workflow.zip"))

	cmd := exec.Command("nextflow", "config", "-flat", unzipped)
	cmd.Env = append(os.Environ(), "NXF_OFFLINE=true")

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("nextflow config of the package: %v\n%s", err, out)
	}

	for _, want := range []string{
		"process.container = '" + stage + "'",
		"process.'withName:PLAN|OUTS'.container = '" + runtime + "'",
		"params.outdir = '/mnt/workflow/pubdir'",
		"params.mro2nf_code = '/opt/mro2nf/code'",
		"params.mro2nf_host_inputs = false",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the package's config lacks %q:\n%s", want, out)
		}
	}
}

// unzipWorkflow extracts the zip at path into a temp dir, checking it holds
// the files main.nf reads and nothing else of the project's or the
// pipeline's, and returns the dir.
func unzipWorkflow(t *testing.T, path string) string {
	t.Helper()

	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open workflow.zip: %v", err)
	}
	defer func() { _ = zr.Close() }()

	dir := t.TempDir()

	var names []string

	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}

		names = append(names, f.Name)

		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}

		raw, err := io.ReadAll(rc)
		_ = rc.Close()

		if err != nil {
			t.Fatal(err)
		}

		dst := filepath.Join(dir, f.Name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}

		writeFileT(t, dst, string(raw))
	}

	entry := filepath.Join("_assets/mro", root, "testdata", "entry_file", "pipeline.mro")
	want := []string{entry, "_assets/types.json", "conf/container.config", "conf/healthomics.config", "main.nf", "nextflow.config", "parameter-template.json", "stage.nf"}

	slices.Sort(names)

	if diff := cmp.Diff(want, names); diff != "" {
		t.Errorf("workflow.zip files (-want +got):\n%s", diff)
	}

	return dir
}
