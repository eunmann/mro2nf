//go:build e2e

package e2e

// Container-isolation e2e (port of docker_iso.sh): run transpiled pipelines
// under the Nextflow `docker` executor, where each task runs in a container
// that mounts ONLY its work dir — not the project directory. This reproduces
// the AWS Batch + S3 / HealthOmics model (isolated workers, no shared
// filesystem) that the plain local-executor suite cannot: a task can read a
// file only if it was staged in as a declared `path` input.
//
// It is the regression guard for the _assets staging fix — if types.json or a
// bindspec is ever referenced via ${projectDir} in a script again, these runs
// fail (the file is invisible inside the container) while the local suite
// still passes.

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// requireDocker gates a test on docker being present AND usable (`docker
// info`), plus nextflow and java — mirroring docker_iso.sh's prerequisite
// checks. Like requireTools, a missing prerequisite skips locally but FAILS
// under E2E_REQUIRE=1, so a runner-image regression cannot turn the suite
// into a silent green skip.
func requireDocker(t *testing.T) {
	t.Helper()
	requireTools(t, "docker")

	if err := exec.Command("docker", "info").Run(); err != nil {
		if os.Getenv("E2E_REQUIRE") == "1" {
			t.Fatal("docker not usable (required by E2E_REQUIRE=1)")
		}

		t.Skip("docker not usable")
	}

	requireTools(t, "nextflow", "java")
}

var (
	isoCfgOnce    sync.Once
	isoCfgContent string
)

// isoConfig returns the Nextflow config lines for isolation runs, detected
// once per test process. On rootful Docker (GitHub Actions) containers default
// to root, so task outputs come back root-owned: the head-node Groovy that
// reads a chunk's data.json can be denied, and work-dir cleanup fails with
// "Permission denied". Mapping the container to the runner's uid/gid (with a
// writable HOME) keeps files owned by the runner. But on ROOTLESS Docker the
// opposite holds — the container's root already maps to the host user, and
// forcing -u maps to an unprivileged subuid that cannot write the bind-mounted
// work dir — so the -u mapping applies only when rootful.
func isoConfig() string {
	isoCfgOnce.Do(func() {
		out, err := exec.Command("docker", "info", "-f", "{{.SecurityOptions}}").Output()
		if err == nil && strings.Contains(string(out), "rootless") {
			return // rootless: default (container root -> host user) is correct
		}

		isoCfgContent = fmt.Sprintf("docker.runOptions = '-u %d:%d -e HOME=/tmp'\n",
			os.Getuid(), os.Getgid())
	})

	return isoCfgContent
}

const isoImage = "mro2nf-iso:test"

var (
	isoImgOnce sync.Once
	isoImgErr  error
)

// buildIsoImage builds (once per test process) an image that bakes mre, the
// Martian adapters, and the fixture stage code at the SAME absolute paths the
// transpiler bakes into the generated scripts, so they resolve inside the
// isolated task container. types.json and the bindspecs are deliberately NOT
// baked — they must arrive via the staged _assets input.
func buildIsoImage(t *testing.T) {
	t.Helper()
	buildBinaries(t)

	isoImgOnce.Do(func() {
		dockerfile := fmt.Sprintf(`FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends procps coreutils && rm -rf /var/lib/apt/lists/*
COPY mre %[1]s/mre
COPY vendor-martian/python %[1]s/vendor-martian/python
COPY testdata %[1]s/testdata
RUN chmod +x %[1]s/mre
`, root)

		cmd := exec.Command("docker", "build", "-q", "-t", isoImage, "-f", "-", root)
		cmd.Stdin = strings.NewReader(dockerfile)

		if out, err := cmd.CombinedOutput(); err != nil {
			isoImgErr = fmt.Errorf("docker build %s: %w\n%s", isoImage, err, out)
		}
	})

	if isoImgErr != nil {
		t.Fatal(isoImgErr)
	}
}

// writeFileT writes content to path or fails the test.
func writeFileT(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestDockerIsolation runs a representative slice across process kinds under
// full container isolation: single-stage, split, map call (fork/merge +
// keyed), disabled call (null-bundle staging), split-returned join override,
// and the `special` scheduler key — plus adapter coverage (exec, comp via
// -mrjob) and the null-bundle / zero-chunk shapes most likely to break on
// isolated workers.
func TestDockerIsolation(t *testing.T) {
	requireDocker(t)
	buildIsoImage(t)

	cases := []struct{ name, golden string }{
		{"split_test", "expected/SUM_SQUARE_PIPELINE/fork0/_outs"},
		{"map_pipe", "expected/outs.json"},
		{"disabled_map", "expected/outs.json"},
		{"join_resources", "expected/outs.json"},
		{"special_resource", "expected/outs.json"},
		{"map_pipe_nested", "expected/outs.json"},
		{"kitchen_sink", "expected/main_outs.json"},
		{"entry_file", "expected/ep_outs.json"},
		{"entry_filearr", "expected/epa_outs.json"},
		{"entry_struct_file", "expected/eps_outs.json"},
		{"entry_mapfile", "expected/epm_outs.json"},
		{"split_from_file", "expected/sp_outs.json"},
		{"map_split_file", "expected/outs.json"},
		{"struct_file_array", "expected/outs.json"},
		// Adapter coverage under isolation: exec and comp (comp needs -mrjob,
		// which transpile() adds automatically; the fixture's mrjob.sh is
		// baked into the image with testdata/).
		{"exec_min", "expected/ep_outs.json"},
		{"mixed_adapters", "expected/outs.json"},
		// Null-bundle / zero-chunk staging — the shapes most likely to break
		// on isolated workers.
		{"empty_fork_min", "expected/outs.json"},
		{"map_null_map", "expected/outs.json"},
		{"disabled_callref", "expected/outs.json"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			proj := transpile(t, tc.name)
			writeFileT(t, filepath.Join(proj, "iso.config"), isoConfig())

			if err := runNextflow(t, proj, "-c", "iso.config", "-with-docker", isoImage); err != nil {
				t.Fatalf("nextflow (isolated): %v", err)
			}

			goldenJSON(t, filepath.Join(proj, "results", "pipeline_outs.json"),
				filepath.Join(root, "testdata", tc.name, tc.golden))
		})
	}
}

// TestDockerEntryOverrides checks file-typed entry-input overrides under
// isolation: the strictest staging proof. Every override file lives in a host
// temp dir (NOT baked into the image), so the stage can only read its content
// if Nextflow staged file(params.<name>) into the container — exactly the AWS
// Batch / HealthOmics S3-localization path. Covers a scalar file, a file[]
// (list), and a struct-with-file, each reconstructed by mre entryargs.
func TestDockerEntryOverrides(t *testing.T) {
	requireDocker(t)
	buildIsoImage(t)

	// The parent's TempDir outlives the parallel subtests, so it is a safe
	// shared home for the override inputs.
	work := t.TempDir()
	files := map[string]string{
		"o_scalar.txt": "5\n7\n9\n", // 21 * 2 == 42
		"o_arr1.txt":   "2\n3\n",    // 5
		"o_arr2.txt":   "10\n",      // 10 ; (5+10) * 2 == 30
		"o_ref.txt":    "4\n4\n",    // 8  ; * n(5) == 40
		"o_m1.txt":     "6\n",       // 6
		"o_m2.txt":     "14\n",      // 14 ; (6+14) * 2 == 40
		// Same-basename file[] leaves (sb1/reads.txt + sb2/reads.txt): the
		// regression guard for the entry-file staging collision. Both leaves
		// share the basename reads.txt; only the per-index
		// `stageAs: '<in>_?/*'` subdir keeps them from clobbering each other
		// in the isolated task work dir. Without it one file overwrites the
		// other and the total is wrong.
		"sb1/reads.txt": "2\n3\n", // 5
		"sb2/reads.txt": "10\n",   // 10 ; (5+10) * 2 == 30
		// A DIRECTORY entry input (`path fastqs`, the Cell Ranger --fastqs
		// shape): the whole dir is staged into the container, and the stage sums
		// every file in it. Lives outside the image, so a correct total proves
		// the directory arrived only via staging.
		"odir/e.txt": "10\n",    // 10
		"odir/f.txt": "11\n12\n", // 23 ; (10+11+12) * 2 == 66
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
		name    string
		fixture string
		params  map[string]any
		expect  string
	}{
		{"entry_file_override", "entry_file",
			map[string]any{"reads": p("o_scalar.txt")}, `{"total": 42.0}`},
		{"entry_filearr_override", "entry_filearr",
			map[string]any{"reads": []string{p("o_arr1.txt"), p("o_arr2.txt")}}, `{"total": 30.0}`},
		{"entry_filearr_samebasename", "entry_filearr",
			map[string]any{"reads": []string{p("sb1/reads.txt"), p("sb2/reads.txt")}}, `{"total": 30.0}`},
		{"entry_struct_file_override", "entry_struct_file",
			map[string]any{"cfg": map[string]any{"ref": p("o_ref.txt"), "n": 5}}, `{"total": 40.0}`},
		{"entry_mapfile_override", "entry_mapfile",
			map[string]any{"reads": map[string]any{"a": p("o_m1.txt"), "b": p("o_m2.txt")}}, `{"total": 40.0}`},
		{"entry_dir_override", "entry_dir",
			map[string]any{"fastqs": p("odir")}, `{"total": 66.0}`},
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
			writeFileT(t, filepath.Join(proj, "iso.config"), isoConfig())

			err = runNextflow(t, proj,
				"-c", "iso.config", "-params-file", "params.json", "-with-docker", isoImage)
			if err != nil {
				t.Fatalf("nextflow (isolated): %v", err)
			}

			var got, want any

			readJSON(t, filepath.Join(proj, "results", "pipeline_outs.json"), &got)

			if err := json.Unmarshal([]byte(tc.expect), &want); err != nil {
				t.Fatalf("parse expected %q: %v", tc.expect, err)
			}

			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("pipeline outs mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestGeneratedAWSBatchImage validates the -target awsbatch cloud artifacts
// without a live account: build the runtime image from the EMITTED Dockerfile
// (not this harness's inline one) and run the pipeline with only that image's
// baked /opt/mro2nf runtime, executor overridden to local+docker.
func TestGeneratedAWSBatchImage(t *testing.T) {
	requireDocker(t)

	const genImage = "mro2nf-gen:test"

	// transpile() always passes -mre/-shell host paths; that is correct here
	// too — container targets copy them into the project's runtime/ dir.
	proj := transpile(t, "diamond_min", "-target", "awsbatch", "-container", genImage)

	build := exec.Command("docker", "build", "-q", "-t", genImage, proj)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("docker build of the generated Dockerfile: %v\n%s", err, out)
	}

	cfg := "process.executor = 'local'\ndocker.enabled = true\n" + isoConfig()
	writeFileT(t, filepath.Join(proj, "local.config"), cfg)

	err := runNextflow(t, proj,
		"-c", "local.config", "--aws_outdir", filepath.Join(proj, "results"))
	if err != nil {
		t.Fatalf("nextflow (generated image): %v", err)
	}

	goldenJSON(t, filepath.Join(proj, "results", "pipeline_outs.json"),
		filepath.Join(root, "testdata", "diamond_min", "expected", "outs.json"))
}

// TestGeneratedAWSBatchImageNative validates -native + -native-runner on the
// awsbatch container target (#99): the emitted image bakes the Python runner at
// /opt/mro2nf/runner, the generated scripts exec that baked path (no mounted
// project dir), and the baked entry_resolved (a file-typed entry) stages into
// the isolated task — the same self-contained-image mechanism the default
// container path uses. Executor overridden to local+docker (no live account).
func TestGeneratedAWSBatchImageNative(t *testing.T) {
	requireDocker(t)

	const genImage = "mro2nf-gen-native:test"

	proj := transpile(t, "entry_file",
		"-native", "-native-runner", "-target", "awsbatch", "-container", genImage)

	build := exec.Command("docker", "build", "-q", "-t", genImage, proj)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("docker build of the generated native Dockerfile: %v\n%s", err, out)
	}

	cfg := "process.executor = 'local'\ndocker.enabled = true\n" + isoConfig()
	writeFileT(t, filepath.Join(proj, "local.config"), cfg)

	if err := runNextflow(t, proj,
		"-c", "local.config", "--aws_outdir", filepath.Join(proj, "results")); err != nil {
		t.Fatalf("nextflow (generated native image): %v", err)
	}

	goldenJSON(t, filepath.Join(proj, "results", "pipeline_outs.json"),
		filepath.Join(root, "testdata", "entry_file", "expected", "ep_outs.json"))
}

// TestGeneratedHealthOmicsPackage validates the -target healthomics packaging
// artifacts without a live account: package.sh must build a zip containing the
// workflow (not the docker build context), and parameter-template.json must
// parse with the container + entry params.
func TestGeneratedHealthOmicsPackage(t *testing.T) {
	requireDocker(t)

	// docker_iso.sh skips this section on a missing zip even under
	// E2E_REQUIRE=1 (only the top-level docker/nextflow/java gate hard-fails).
	if _, err := exec.LookPath("zip"); err != nil {
		t.Skip("zip not found")
	}

	proj := transpile(t, "entry_file", "-target", "healthomics", "-container", "mro2nf-gen:test")

	pkg := exec.Command("bash", "package.sh")
	pkg.Dir = proj

	if out, err := pkg.CombinedOutput(); err != nil {
		t.Fatalf("package.sh: %v\n%s", err, out)
	}

	zr, err := zip.OpenReader(filepath.Join(proj, "workflow.zip"))
	if err != nil {
		t.Fatalf("open workflow.zip: %v", err)
	}
	defer func() { _ = zr.Close() }()

	var hasMain bool

	for _, f := range zr.File {
		if f.Name == "main.nf" || strings.HasSuffix(f.Name, "/main.nf") {
			hasMain = true
		}

		if strings.Contains(f.Name, "runtime/mre") {
			t.Errorf("workflow.zip must exclude the docker build context; found %s", f.Name)
		}
	}

	if !hasMain {
		t.Error("workflow.zip missing main.nf")
	}

	var tpl map[string]any

	readJSON(t, filepath.Join(proj, "parameter-template.json"), &tpl)

	if _, ok := tpl["container"]; !ok {
		t.Error("parameter-template.json missing container entry")
	}

	reads, ok := tpl["reads"].(map[string]any)
	if !ok {
		t.Fatalf("parameter-template.json missing reads entry: %v", tpl)
	}

	if opt, _ := reads["optional"].(bool); !opt {
		t.Errorf("parameter-template.json reads must be optional: %v", reads)
	}
}
