//go:build e2e

package e2e

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestFreshRunsAreIdentical (#270): jitter_wide's 64 forks and 64 chunks
// each sleep a random interval, so tasks complete in a different order on
// every run. Two runs from scratch, in separate projects and work dirs, must
// publish byte-identical trees and pipeline_outs.json, and a -resume of the
// first must re-execute no task and leave its published tree unchanged.
func TestFreshRunsAreIdentical(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	// The fresh runs' group must wait for them, so it cannot be parallel,
	// and a parallel test's direct subtests must be (tparallel): it nests
	// under the fixture's.
	t.Run("jitter_wide", func(t *testing.T) {
		t.Parallel()
		// Claim the shard here so both fresh runs and the resume run beside
		// each other (shardSkip's ancestor claim).
		shardSkip(t)

		projs := []string{transpile(t, "jitter_wide"), transpile(t, "jitter_wide")}

		t.Run("fresh", func(t *testing.T) {
			for i, proj := range projs {
				t.Run(strconv.Itoa(i+1), func(t *testing.T) {
					t.Parallel()

					if err := runNextflow(t, proj); err != nil {
						t.Fatal(err)
					}
				})
			}
		})

		if t.Failed() {
			return
		}

		first := publishedOutputs(t, projs[0])

		if diff := cmp.Diff(first, publishedOutputs(t, projs[1])); diff != "" {
			t.Errorf("two fresh runs published different outputs (-run1 +run2):\n%s", diff)
		}

		if err := runNextflow(t, projs[0], "-resume", "-with-trace", "resume.txt"); err != nil {
			t.Fatalf("-resume rerun: %v", err)
		}

		assertAllCached(t, readTrace(t, filepath.Join(projs[0], "resume.txt")))

		if diff := cmp.Diff(first, publishedOutputs(t, projs[0])); diff != "" {
			t.Errorf("-resume changed the published outputs (-before +after):\n%s", diff)
		}
	})
}

// publishedRun is what a run publishes: the results/ tree by content, and
// the decoded pipeline_outs.json.
type publishedRun struct {
	Tree map[string]string
	Outs any
}

func publishedOutputs(t *testing.T, proj string) publishedRun {
	t.Helper()

	results := filepath.Join(proj, "results")
	run := publishedRun{Tree: hashTree(t, results, nfResultsMeta...)}

	readJSON(t, filepath.Join(results, "pipeline_outs.json"), &run.Outs)

	return run
}

// assertAllCached fails on an empty -resume trace or any row that is not
// CACHED: a re-executed task has an unstable cache key.
func assertAllCached(t *testing.T, rows []traceRow) {
	t.Helper()

	if len(rows) == 0 {
		t.Fatal("empty trace on -resume rerun, want >0 rows")
	}

	ran := 0

	for _, row := range rows {
		if row.Status != "CACHED" {
			ran++

			t.Errorf("task %s re-executed under -resume (status %s), want CACHED", row.Name, row.Status)
		}
	}

	if ran > 0 {
		t.Errorf("%d of %d tasks re-executed under -resume", ran, len(rows))
	}
}
