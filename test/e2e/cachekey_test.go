//go:build e2e

package e2e

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestResumeKeysOnTheBuild (#449): a -resume re-run of an unchanged project
// is fully cached, but once the project is re-transpiled by another mro2nf
// build, no task is: Nextflow keys a task on its script text and inputs, not
// on the mre binary, so a task cached by an older build would otherwise hand
// on outputs shaped by that build's mre (fork element dirs without a record
// the new mre args reads). The fixtures cover a map call's forks and the
// split triad.
func TestResumeKeysOnTheBuild(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")
	buildBinaries(t)

	other := toolchain{mro2nf: buildStampedMro2nf(t, "e2e-another-build"), mre: repoTools().mre}

	for _, fixture := range []string{"map_fork", "split_test"} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()

			proj := transpile(t, fixture)

			if err := runNextflow(t, proj); err != nil {
				t.Fatalf("first run: %v", err)
			}

			if err := runNextflow(t, proj, "-resume", "-with-trace", "same.txt"); err != nil {
				t.Fatalf("-resume rerun: %v", err)
			}

			assertAllCached(t, readTrace(t, filepath.Join(proj, "same.txt")))

			if err := transpileInto(other, filepath.Join(root, "testdata", fixture), "pipeline.mro", proj); err != nil {
				t.Fatalf("re-transpile with another build: %v", err)
			}

			if err := runNextflow(t, proj, "-resume", "-with-trace", "other.txt"); err != nil {
				t.Fatalf("-resume after re-transpile: %v", err)
			}

			assertNoneCached(t, readTrace(t, filepath.Join(proj, "other.txt")))
		})
	}
}

// TestResumeKeysOnlyOnATasksOwnTypes (#286): after one stage's signature
// changes, a -resume re-run of the re-transpiled project re-runs that stage
// but keeps the unrelated stage cached. Every task used to stage the one
// shared types.json, so any type edit re-ran the whole pipeline.
func TestResumeKeysOnlyOnATasksOwnTypes(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	fixture := t.TempDir()
	if err := os.CopyFS(fixture, os.DirFS(filepath.Join(root, "testdata", "diamond_min"))); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}

	proj := transpileDir(t, fixture)

	if err := runNextflow(t, proj); err != nil {
		t.Fatalf("first run: %v", err)
	}

	mro := filepath.Join(fixture, "pipeline.mro")
	editMRO(t, mro,
		"    in  float b,\n    out float c,", "    in  float b,\n    in  int   unused,\n    out float c,",
		"        b = GEN.y,\n", "        b = GEN.y,\n        unused = 1,\n")

	if err := transpileInto(repoTools(), fixture, "pipeline.mro", proj); err != nil {
		t.Fatalf("re-transpile after the ADD edit: %v", err)
	}

	if err := runNextflow(t, proj, "-resume", "-with-trace", "edited.txt"); err != nil {
		t.Fatalf("-resume after the ADD edit: %v", err)
	}

	status := map[string]string{}
	for _, row := range readTrace(t, filepath.Join(proj, "edited.txt")) {
		status[row.Name] = row.Status
	}

	for name, want := range map[string]bool{"N0_GEN (D.GEN)": true, "N1_ADD (D.ADD)": false} {
		got, ok := status[name]
		if !ok {
			t.Errorf("task %s missing from the trace %v", name, status)

			continue
		}

		if (got == "CACHED") != want {
			t.Errorf("task %s: status %s, want cached=%v", name, got, want)
		}
	}
}

// TestResumeKeysOnANodesOverrides (#301): a -resume re-run under an edited
// overrides file re-runs PLAN, which reads it, the tasks of the one node
// whose resources it changes, and OUTS, which reads that node's outs, and
// keeps every other task cached: each task keys on its own node's overrides
// alone.
func TestResumeKeysOnANodesOverrides(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	fixture := filepath.Join(root, "testdata", "overrides_resources")
	proj := transpile(t, "overrides_resources")

	if err := runNextflow(t, proj, "--mro2nf_overrides", filepath.Join(fixture, "overrides.json")); err != nil {
		t.Fatalf("first run: %v", err)
	}

	over, err := os.ReadFile(filepath.Join(fixture, "overrides.json"))
	if err != nil {
		t.Fatal(err)
	}

	edited := filepath.Join(t.TempDir(), "overrides.json")
	writeFileT(t, edited, strings.Replace(string(over), `"chunk.mem_gb": 2.25`, `"chunk.mem_gb": 2`, 1))

	if err := runNextflow(t, proj, "-resume", "-with-trace", "edited.txt", "--mro2nf_overrides", edited); err != nil {
		t.Fatalf("-resume under the edited overrides: %v", err)
	}

	rows := readTrace(t, filepath.Join(proj, "edited.txt"))
	for _, row := range rows {
		rerun := row.Name == "PLAN" || row.Name == "OUTS" || strings.HasSuffix(row.Name, "(OVR.OTHER)")
		if (row.Status == "CACHED") == rerun {
			t.Errorf("task %s: status %s, want cached=%v", row.Name, row.Status, !rerun)
		}
	}

	var outs map[string]any

	readJSON(t, filepath.Join(proj, "results", "pipeline_outs.json"), &outs)

	if got, want := outs["other_res"], "threads=2 mem=2 args=2/2/5"; got != want {
		t.Errorf("OTHER's resources after the edit %v, want %s", got, want)
	}
}

// readersMRO maps PAIR over the forks of A.xs, beside a second SRC, B, that
// nothing reads. Each readersEdits case changes only how PAIR reads A or B.
const readersMRO = `stage SRC(
    in  int   n,
    out int[] xs,
    out int[] ys,
    out bool  off,
    src py    "stages/src",
)

stage PAIR(
    in  int    a,
    in  int    b,
    out string s,
    src py     "stages/pair",
)

pipeline READERS(
    in  bool     skip,
    out string[] s,
)
{
    call SRC as A(
        n = 1,
    )

    call SRC as B(
        n = 2,
    ) using (
        disabled = self.skip,
    )

    map call PAIR(
        a = split A.xs,
        b = 0,
    ) using (
        disabled = self.skip,
    )

    return (
        s = PAIR.s,
    )
}

call READERS(
    skip = false,
)
`

const readersSrc = `def main(args, outs):
    outs.xs = [args.n, args.n + 1, args.n + 2]
    outs.ys = [10 * args.n, 10 * args.n + 1, 10 * args.n + 2]
    outs.off = args.n == 2
`

const readersPair = `def main(args, outs):
    outs.s = "%s,%s" % (args.a, args.b)
`

// readersEdits are edits of readersMRO that change only what a producer's
// post writes for PAIR, by the producer's task: a fork element member of
// A.ys zipped with A.xs, B's partner elements of B.ys, and the gate B posts.
var readersEdits = map[string]struct {
	producer string
	edit     []string
}{
	"zip":     {"(READERS.A)", []string{"        b = 0,\n", "        b = split A.ys,\n"}},
	"partner": {"(READERS.B)", []string{"        b = 0,\n", "        b = split B.ys,\n"}},
	"gate":    {"(READERS.B)", []string{"        b = 0,\n    ) using (\n        disabled = self.skip,\n", "        b = 0,\n    ) using (\n        disabled = B.off,\n"}},
}

// TestResumeKeysOnTheReadersAPostWritesFor (#499): a stage's post writes
// fork elements, partner elements and gates for the stages that read its
// outs, so after a re-transpile changes only how a consumer splits or gates
// on a producer, a -resume re-run must re-run the producer, and publish what
// a fresh run of the edited pipeline publishes. A producer cached with its
// old post's elements left the consumer reading stale or missing ones.
func TestResumeKeysOnTheReadersAPostWritesFor(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	for _, name := range slices.Sorted(maps.Keys(readersEdits)) {
		tc := readersEdits[name]

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fixture := readersFixture(t)
			proj := transpileDir(t, fixture)

			if err := runNextflow(t, proj); err != nil {
				t.Fatalf("first run: %v", err)
			}

			editMRO(t, filepath.Join(fixture, "pipeline.mro"), tc.edit...)

			if err := transpileInto(repoTools(), fixture, "pipeline.mro", proj); err != nil {
				t.Fatalf("re-transpile after the edit: %v", err)
			}

			if err := runNextflow(t, proj, "-resume", "-with-trace", "edited.txt"); err != nil {
				t.Fatalf("-resume after the edit: %v", err)
			}

			assertRan(t, readTrace(t, filepath.Join(proj, "edited.txt")), tc.producer)

			fresh := transpileDir(t, fixture)
			if err := runNextflow(t, fresh); err != nil {
				t.Fatalf("fresh run of the edited pipeline: %v", err)
			}

			if diff := cmp.Diff(publishedOutputs(t, fresh), publishedOutputs(t, proj)); diff != "" {
				t.Errorf("-resume after the edit published other outputs than a fresh run (-fresh +resumed):\n%s", diff)
			}
		})
	}
}

// readersFixture writes readersMRO and its stage code into a temp dir.
func readersFixture(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	for _, stage := range []string{"src", "pair"} {
		if err := os.MkdirAll(filepath.Join(dir, "stages", stage), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	writeFileT(t, filepath.Join(dir, "pipeline.mro"), readersMRO)
	writeFileT(t, filepath.Join(dir, "stages", "src", "__init__.py"), readersSrc)
	writeFileT(t, filepath.Join(dir, "stages", "pair", "__init__.py"), readersPair)

	return dir
}

// assertRan fails unless the trace holds a task named with suffix, and
// none of them was CACHED.
func assertRan(t *testing.T, rows []traceRow, suffix string) {
	t.Helper()

	found := false

	for _, row := range rows {
		if !strings.HasSuffix(row.Name, suffix) {
			continue
		}

		found = true

		if row.Status == "CACHED" {
			t.Errorf("task %s was reused from the cache, want it re-run", row.Name)
		}
	}

	if !found {
		t.Errorf("no task %s in the trace %v", suffix, rows)
	}
}

// editMRO applies each old->new replacement pair to the .mro at path,
// failing when a pair's old text is absent.
func editMRO(t *testing.T, path string, pairs ...string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	src := string(data)

	for i := 0; i+1 < len(pairs); i += 2 {
		if !strings.Contains(src, pairs[i]) {
			t.Fatalf("%s has no %q to edit", path, pairs[i])
		}

		src = strings.Replace(src, pairs[i], pairs[i+1], 1)
	}

	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

// assertNoneCached fails on an empty trace or any CACHED row: a reused task
// has a cache key that misses what changed.
func assertNoneCached(t *testing.T, rows []traceRow) {
	t.Helper()

	if len(rows) == 0 {
		t.Fatal("empty trace, want >0 rows")
	}

	for _, row := range rows {
		if row.Status == "CACHED" {
			t.Errorf("task %s was reused from the cache, want it re-run", row.Name)
		}
	}
}

// buildStampedMro2nf builds mro2nf as `make build` does, stamped with
// version, into a temp dir and returns its path.
func buildStampedMro2nf(t *testing.T, version string) string {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "mro2nf")
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-X main.version="+version, "-o", bin, "./cmd/mro2nf")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build stamped mro2nf: %v\n%s", err, out)
	}

	return bin
}
