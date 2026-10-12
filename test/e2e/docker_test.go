//go:build e2e

package e2e

// Container-isolation e2e (port of docker_iso.sh): run transpiled pipelines
// under the Nextflow `docker` executor, where each task runs in a container
// that mounts ONLY its work dir — not the project directory. This reproduces
// the AWS Batch + S3 / HealthOmics model (isolated workers, no shared
// filesystem) that the plain local-executor suite cannot: a task can read a
// file only if it was staged in as a declared `path` input. A script that
// reads a project file through ${projectDir} fails here (the file is
// invisible inside the container) while the local suite still passes.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// checkoutImage tags a test image per checkout. The images bake this
// checkout's absolute paths, so a tag shared across worktrees lets a
// concurrent run in another worktree replace the image mid-suite.
func checkoutImage(name string) string {
	sum := sha256.Sum256([]byte(root))

	return name + ":" + hex.EncodeToString(sum[:6])
}

func isoImage() string { return checkoutImage("mro2nf-iso") }

var (
	isoImgOnce sync.Once
	errIsoImg  error
)

var errNoRuntimeBase = errors.New("runtime Dockerfile: no FROM image")

// runtimeBase returns deploy/runtime/Dockerfile's final-stage base, the
// digest-pinned image the release runtime builds FROM, so the iso suite runs
// on what ships.
func runtimeBase() (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, "deploy", "runtime", "Dockerfile"))
	if err != nil {
		return "", fmt.Errorf("read runtime Dockerfile: %w", err)
	}

	base := ""

	for line := range strings.Lines(string(raw)) {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != "FROM" {
			continue
		}

		f = f[1:]
		if len(f) > 0 && strings.HasPrefix(f[0], "--platform=") {
			f = f[1:]
		}

		if len(f) > 0 {
			base = f[0]
		}
	}

	if base == "" {
		return "", errNoRuntimeBase
	}

	return base, nil
}

// buildIsoImage builds (once per test process) an image that bakes mre, the
// Martian adapters, and the fixture stage code at the SAME absolute paths the
// transpiler bakes into the generated scripts, so they resolve inside the
// isolated task container. The project's _assets are deliberately NOT baked:
// they must arrive as staged task inputs.
func buildIsoImage(t *testing.T) {
	t.Helper()
	buildBinaries(t)

	isoImgOnce.Do(func() {
		base, err := runtimeBase()
		if err != nil {
			errIsoImg = err

			return
		}

		dockerfile := fmt.Sprintf(`FROM %[2]s
RUN apt-get update && apt-get install -y --no-install-recommends procps coreutils && rm -rf /var/lib/apt/lists/*
COPY mre %[1]s/mre
COPY vendor-martian/python %[1]s/vendor-martian/python
COPY testdata %[1]s/testdata
RUN chmod +x %[1]s/mre
`, root, base)

		cmd := exec.Command("docker", "build", "-q", "-t", isoImage(), "-f", "-", root)
		cmd.Stdin = strings.NewReader(dockerfile)

		if out, err := cmd.CombinedOutput(); err != nil {
			errIsoImg = fmt.Errorf("docker build %s: %w\n%s", isoImage(), err, out)
		}
	})

	if errIsoImg != nil {
		t.Fatal(errIsoImg)
	}
}

// writeFileT writes content to path or fails the test.
func writeFileT(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestDockerIsolation runs a representative slice of shapes under full
// container isolation: single-stage, split, map call (array and keyed),
// disabled call, split-returned join override, and the `special` scheduler
// key — plus adapter coverage (exec, comp) and the null and zero-chunk shapes
// most likely to break on isolated workers.
func TestDockerIsolation(t *testing.T) {
	t.Parallel()

	requireDocker(t)
	buildIsoImage(t)

	cases := []struct{ name, golden string }{
		{"split_test", "expected/SUM_SQUARE_PIPELINE/fork0/_outs"},
		{"map_pipe", "expected/outs.json"},
		{"disabled_map", "expected/outs.json"},
		{"join_resources", "expected/outs.json"},
		{"special_resource", "expected/outs.json"},
		{"map_pipe_disabled_nested", "expected/outs.json"},
		{"kitchen_sink", "expected/main_outs.json"},
		{"map_split_file", "expected/outs.json"},
		{"struct_file_array", "expected/outs.json"},
		// Adapter coverage under isolation: exec and comp.
		{"exec_min", "expected/ep_outs.json"},
		{"mixed_adapters", "expected/outs.json"},
		// Null and zero-chunk staging — the shapes most likely to break on
		// isolated workers.
		{"empty_fork_min", "expected/outs.json"},
		{"map_null_map", "expected/outs.json"},
		{"disabled_callref", "expected/outs.json"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Isolated like awsbatch/healthomics, so confined like them too.
			proj := transpile(t, tc.name, "-confine-outputs")
			writeFileT(t, filepath.Join(proj, "iso.config"), isoConfig())

			if err := runNextflow(t, proj, "-c", "iso.config", "-with-docker", isoImage()); err != nil {
				t.Fatalf("nextflow (isolated): %v", err)
			}

			goldenJSON(t, tc.name, filepath.Join(proj, "results", "pipeline_outs.json"),
				filepath.Join(root, "testdata", tc.name, tc.golden))
			assertGoldenTree(t, tc.name, tc.name, proj)
		})
	}
}

// TestDockerEntryOverrides checks file-typed entry-input overrides under
// isolation: the strictest staging proof. Every override file lives in a host
// temp dir (NOT baked into the image), so the stage can only read its content
// if Nextflow staged file(params.<name>) into the container — exactly the AWS
// Batch / HealthOmics S3-localization path. Covers a scalar file, a file[]
// (list), a struct-with-file, a map of files and a directory.
func TestDockerEntryOverrides(t *testing.T) {
	t.Parallel()

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
		"odir/e.txt": "10\n",     // 10
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
		{
			"entry_file_override", "entry_file",
			map[string]any{"reads": p("o_scalar.txt")},
			`{"total": 42.0}`,
		},
		{
			"entry_filearr_override", "entry_filearr",
			map[string]any{"reads": []string{p("o_arr1.txt"), p("o_arr2.txt")}},
			`{"total": 30.0}`,
		},
		{
			"entry_filearr_samebasename", "entry_filearr",
			map[string]any{"reads": []string{p("sb1/reads.txt"), p("sb2/reads.txt")}},
			`{"total": 30.0}`,
		},
		{
			"entry_struct_file_override", "entry_struct_file",
			map[string]any{"cfg": map[string]any{"ref": p("o_ref.txt"), "n": 5}},
			`{"total": 40.0}`,
		},
		{
			"entry_mapfile_override", "entry_mapfile",
			map[string]any{"reads": map[string]any{"a": p("o_m1.txt"), "b": p("o_m2.txt")}},
			`{"total": 40.0}`,
		},
		{
			"entry_dir_override", "entry_dir",
			map[string]any{"fastqs": p("odir")},
			`{"total": 66.0}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			proj := transpile(t, tc.fixture, "-confine-outputs")

			params, err := json.Marshal(tc.params)
			if err != nil {
				t.Fatalf("marshal params: %v", err)
			}

			writeFileT(t, filepath.Join(proj, "params.json"), string(params))
			writeFileT(t, filepath.Join(proj, "iso.config"), isoConfig())

			err = runNextflow(t, proj,
				"-c", "iso.config", "-params-file", "params.json", "-with-docker", isoImage())
			if err != nil {
				t.Fatalf("nextflow (isolated): %v", err)
			}

			var got, want any

			readJSON(t, filepath.Join(proj, "results", "pipeline_outs.json"), &got)

			// Decode the expected value the same way (UseNumber) so both sides are
			// json.Number; a plain json.Unmarshal float64 would never match got.
			if err := decodeJSONNumber([]byte(tc.expect), &want); err != nil {
				t.Fatalf("parse expected %q: %v", tc.expect, err)
			}

			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("pipeline outs mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
