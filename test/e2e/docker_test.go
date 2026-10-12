//go:build e2e

package e2e

// Container-isolation e2e: run transpiled pipelines under the generated
// project's docker profile, where every task runs in a container of the one
// image a user builds FROM the runtime image deploy/runtime/Dockerfile
// builds, with the pipeline's code at the paths it was transpiled from. A
// container sees an entry file only if the task staged it, as on AWS Batch
// and HealthOmics; a task that reads a host path the run did not stage
// fails here while the local suite still passes.

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
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

// pipelineImage builds the image a user builds for the fixture at dir, the
// README's recipe: FROM the runtime image, with the fixture's code at the
// path the project was transpiled from, under a tag named for name, and
// returns the tag.
func pipelineImage(t *testing.T, dir, name string) string {
	t.Helper()

	dest, err := json.Marshal(dir)
	if err != nil {
		t.Fatal(err)
	}

	tag := checkoutImage("mro2nf-pipeline-" + strings.ToLower(name))

	cmd := exec.Command("docker", "build", "-q", "-t", tag, "-")
	cmd.Stdin = codeContext(t, dir, "FROM "+runtimeImage(t)+"\nCOPY [\"code\", "+string(dest)+"]\n")

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker build %s: %v\n%s", tag, err, out)
	}

	return tag
}

// fixtureDataDirs hold a fixture's entry files and goldens, which a run
// reads only through what its tasks stage, so the image must not hold them.
var fixtureDataDirs = []string{"input", "expected"}

// codeContext returns a docker build context, as a tar stream, of recipe as
// its Dockerfile and the fixture at dir under code/, less its data dirs.
func codeContext(t *testing.T, dir, recipe string) io.Reader {
	t.Helper()

	var buf bytes.Buffer

	tw := tar.NewWriter(&buf)

	if err := tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len(recipe))}); err != nil {
		t.Fatal(err)
	}

	if _, err := tw.Write([]byte(recipe)); err != nil {
		t.Fatal(err)
	}

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("relativize %s: %w", path, err)
		}

		if d.IsDir() && slices.Contains(fixtureDataDirs, rel) {
			return filepath.SkipDir
		}

		return addTarEntry(tw, path, filepath.ToSlash(filepath.Join("code", rel)), d)
	})
	if err != nil {
		t.Fatalf("tar the code of %s: %v", dir, err)
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	return &buf
}

// addTarEntry writes the file or directory at path to tw as name, keeping
// its mode, so a stage stays executable in the image.
func addTarEntry(tw *tar.Writer, path, name string, d fs.DirEntry) error {
	info, err := d.Info()
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return fmt.Errorf("tar header of %s: %w", path, err)
	}

	hdr.Name = name

	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write tar header of %s: %w", path, err)
	}

	if !info.Mode().IsRegular() {
		return nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	if _, err := tw.Write(raw); err != nil {
		return fmt.Errorf("tar %s: %w", path, err)
	}

	return nil
}

// runDocker runs proj, transpiled from the fixture at dir, under its docker
// profile in the image built for name.
func runDocker(t *testing.T, proj, dir, name string, args ...string) error {
	t.Helper()

	writeFileT(t, filepath.Join(proj, "iso.config"), isoConfig())

	return runNextflow(t, proj, append([]string{
		"-profile", "docker", "-c", "iso.config", "--mro2nf_image", pipelineImage(t, dir, name),
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

			if err := runDocker(t, proj, fixtureDir, tc.name, args...); err != nil {
				t.Fatalf("nextflow (isolated): %v", err)
			}

			goldenJSON(t, tc.fixture, filepath.Join(proj, "results", "pipeline_outs.json"), filepath.Join(fixtureDir, tc.golden))
			assertGoldenTree(t, tc.name, tc.fixture, proj)
		})
	}
}

// TestDockerEntryOverrides checks file-typed entry-input overrides under
// isolation, the strictest staging proof: every override file lives in a
// host temp dir, neither in the image nor below the fixture, so the stage
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

			if err := runDocker(t, proj, filepath.Join(root, "testdata", tc.fixture), "overrides_"+tc.name, "-params-file", "params.json"); err != nil {
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

// TestGeneratedAWSBatchProfile validates the awsbatch profile without a
// live account: a run of it with only the executor swapped for local
// docker, in the one image built FROM the runtime image, so the profile's
// tools, image and staging policy are the ones under test.
func TestGeneratedAWSBatchProfile(t *testing.T) {
	t.Parallel()

	requireDocker(t)

	proj := transpile(t, "diamond_min")
	image := pipelineImage(t, filepath.Join(root, "testdata", "diamond_min"), "awsbatch_diamond_min")

	writeFileT(t, filepath.Join(proj, "local.config"), "process.executor = 'local'\ndocker.enabled = true\n"+isoConfig())

	err := runNextflow(t, proj, "-profile", "awsbatch", "-c", "local.config",
		"--mro2nf_image", image, "--outdir", filepath.Join(proj, "results"))
	if err != nil {
		t.Fatalf("nextflow (awsbatch profile, local docker): %v", err)
	}

	goldenJSON(t, "diamond_min", filepath.Join(proj, "results", "pipeline_outs.json"),
		filepath.Join(root, "testdata", "diamond_min", "expected", "outs.json"))
}

// TestGeneratedProfilesRequireImage: a container profile launched without
// --mro2nf_image fails before any task, saying which image it needs.
func TestGeneratedProfilesRequireImage(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java")

	for _, profile := range []string{"docker", "awsbatch"} {
		t.Run(profile, func(t *testing.T) {
			t.Parallel()

			proj := transpile(t, "diamond_min")

			if err := runNextflowAllowingFailedAttempts(t, proj, "-profile", profile); err == nil {
				t.Fatal("the run started without an image")
			}

			raw, err := os.ReadFile(filepath.Join(proj, "run.log"))
			if err != nil {
				t.Fatal(err)
			}

			want := "a container profile runs every task in params.mro2nf_image: pass --mro2nf_image <image>"
			if !strings.Contains(string(raw), want) {
				t.Errorf("run.log lacks %q:\n%s", want, raw)
			}

			if strings.Contains(string(raw), "PLAN") {
				t.Errorf("PLAN started:\n%s", raw)
			}
		})
	}
}

// TestGeneratedHealthOmicsPackage validates the HealthOmics package without
// a live account: package.sh refuses an image not pinned by digest (#334)
// and zips the files main.nf reads, with the healthomics profile the
// workflow's own config and the image pinned, so `nextflow config` of the
// unzipped workflow, given no -profile as HealthOmics gives none, resolves
// the pinned image, required, and the HealthOmics pubdir (#309). No file of
// the pipeline's, an entry default included, is in it.
func TestGeneratedHealthOmicsPackage(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "zip")

	proj := transpile(t, "entry_file")

	pkg := func(args ...string) ([]byte, error) {
		cmd := exec.Command("bash", append([]string{"package.sh"}, args...)...)
		cmd.Dir = proj

		return cmd.CombinedOutput()
	}

	repo := "123456789012.dkr.ecr.us-east-1.amazonaws.com/pipeline"
	image := repo + "@sha256:" + strings.Repeat("a", 64)

	for _, bad := range [][]string{{repo + ":latest"}, {image, image}, {}} {
		if out, err := pkg(bad...); err == nil {
			t.Errorf("package.sh %v packaged:\n%s", bad, out)
		}
	}

	if out, err := pkg(image); err != nil {
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
		"process.container = '" + image + "'",
		"params.outdir = '/mnt/workflow/pubdir'",
		"params.mro2nf_require_image = true",
		"params.mro2nf_host_inputs = false",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the package's config lacks %q:\n%s", want, out)
		}
	}

	if strings.Contains(string(out), "withName") {
		t.Errorf("the package's config gives a task an image of its own:\n%s", out)
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
