//go:build e2e

package e2e

// The corpus stub differential (#654): every corpus invocation Martian
// accepts (test/corpus) runs under mrp and under mro2nf + Nextflow with its
// topology verbatim and every stage's code the generative differential's
// stub (gen.WriteStubFixture), under a few stub seeds, which draw different
// disabled branches and split widths. Each run faces TestGenDiff's checks:
// at its literal and its witness invocation, the published outputs, the
// stage ABI, the stage tasks against mrp's jobs and, where either run fails,
// the run outcome, with every allowlist rule applied as to a generated
// program. A divergence's fixture is kept beside the run's logs under
// artifacts/e2e/.
//
// Environment:
//
//	CORPUS_SEEDS   stub seeds per invocation, 1 to n (default 2).
//	MRO2NF_CORPUS  an MROPATH whose pipelines run beside the vendored
//	               corpus (test/corpus), as make test-corpus sets it.

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/eunmann/mro2nf/test/corpus"
	"github.com/eunmann/mro2nf/test/gen"
)

// corpusStubExcluded are the corpus entries the differential leaves out,
// by why: each a known mro2nf defect.
func corpusStubExcluded() map[string]string {
	return map[string]string{
		"martian/test/fork_test/pipeline.mro": "where a fork of the top-level map call publishes no file, mrp publishes " +
			"an empty outs/<key>/ and mro2nf nothing (seed 1: english)",
	}
}

// TestCorpusStubDiff runs every corpus invocation Martian accepts, its
// stages stubbed, under mrp and mro2nf and fails on any divergence.
func TestCorpusStubDiff(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	env := loadGenEnv(t)
	seeds := envInt(t, "CORPUS_SEEDS", 2)

	srcs, err := corpus.Sources(filepath.Join(root, "testdata", "corpus"))
	if err != nil {
		t.Fatal(err)
	}

	entries, err := corpus.Load(t.TempDir(), srcs)
	if err != nil {
		t.Fatal(err)
	}

	accepted := corpus.Accepted(entries)

	for name := range corpusStubExcluded() {
		if !slices.ContainsFunc(accepted, func(e corpus.Entry) bool { return e.Name == name }) {
			t.Errorf("corpusStubExcluded names %s, which is no corpus entry Martian accepts", name)
		}
	}

	for _, e := range accepted {
		t.Run(corpus.Slug(e.Name), func(t *testing.T) {
			t.Parallel()
			shardSkip(t)

			if reason, ok := corpusStubExcluded()[e.Name]; ok {
				t.Skipf("excluded: %s", reason)
			}

			for seed := range uint64(seeds) {
				t.Run("seed"+strconv.FormatUint(seed+1, 10), func(t *testing.T) {
					t.Parallel()
					runCorpusStub(t, env, e, seed+1)
				})
			}
		})
	}
}

// runCorpusStub runs the stub fixture of the corpus entry e under seed.
func runCorpusStub(t *testing.T, env genEnv, e corpus.Entry, seed uint64) {
	t.Helper()

	work := t.TempDir()
	dir := filepath.Join(work, "fixture")

	if err := gen.WriteStubFixture(dir, e.Mro, e.MroPath, seed); err != nil {
		t.Fatal(err)
	}

	v, err := evalGenInvocations(t, env, dir, work)
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range v.notes {
		t.Log(n)
	}

	switch {
	case v.skip != "":
		t.Skipf("%s seed %d is no test: %s", e.Name, seed, v.skip)
	case len(v.findings) > 0:
		repro, err := keepCorpusStubRepro(t.Name(), work, v)
		if err != nil {
			t.Fatal(err)
		}

		t.Errorf("%s seed %d diverges from mrp (repro %s):\n%s", e.Name, seed, repro, v)
	}
}

// keepCorpusStubRepro copies the fixture in work that reproduces v's
// findings, its witness invocation's when only that one diverged, beside
// the run logs of the test named test in the artifacts dir
// (keepRunLogsOnFailure), with the findings in divergence.txt, and returns
// the copy's dir.
func keepCorpusStubRepro(test, work string, v genVerdict) (string, error) {
	src := filepath.Join(work, "fixture")
	if v.atWitness() {
		src = filepath.Join(work, "witness", "fixture")
	}

	dir := filepath.Join(rootPath(envOr("E2E_ARTIFACTS", e2eArtifactsDefault)), artifactPath(test), "fixture")

	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("clear %s: %w", dir, err)
	}

	if err := os.CopyFS(dir, os.DirFS(src)); err != nil {
		return "", fmt.Errorf("keep repro: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "divergence.txt"), []byte(v.String()), 0o644); err != nil {
		return "", fmt.Errorf("write divergence report: %w", err)
	}

	return dir, removePycache(dir)
}
