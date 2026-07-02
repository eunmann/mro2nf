//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// nonE2EFixtures are testdata fixtures deliberately NOT exercised by any e2e
// suite, each with the reason. A fixture here is exempt from the completeness
// check below; anything else under testdata/ must be named by some e2e test.
var nonE2EFixtures = map[string]string{
	// GPU/vmem resolve to config-text directives (accelerator, the -monitor
	// vmem cap), not runtime behavior diffable against mrp, so they are pinned
	// by emit unit tests (TestEmitGPUAccelerator / TestEmitVmemFlag) instead.
	"gpu_stage":  "emit-only: accelerator directive text (internal/emit unit test)",
	"vmem_stage": "emit-only: -monitor vmem cap text (internal/emit unit test)",
	// Directory entry-input fixture with no committed golden yet. The directory
	// input shape IS validated live (LIVE_AWS_TEST, via an s3:// prefix); wiring
	// entry_dir into an e2e suite is tracked as a GitHub issue.
	"entry_dir": "no committed golden yet — tracked in issue #42",
}

// TestEveryFixtureIsExercised fails when a testdata fixture (a directory with a
// pipeline.mro) is named by no e2e test and is not on the nonE2EFixtures
// allowlist. It is the guard the README's "every fixture" claim lacked: a new
// fixture that nobody wired into a suite (like entry_dir was) is caught here
// rather than shipping as silent dead weight. It also fails on a STALE allowlist
// entry, so the exemptions can't outlive their fixtures.
func TestEveryFixtureIsExercised(t *testing.T) {
	sources := readTestSources(t)

	entries, err := os.ReadDir(filepath.Join(root, "testdata"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}

	present := map[string]bool{}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		name := e.Name()
		if !fileExists(filepath.Join(root, "testdata", name, "pipeline.mro")) {
			continue
		}

		present[name] = true

		if _, exempt := nonE2EFixtures[name]; exempt {
			continue
		}

		// Word-boundary match so "map_split" does not satisfy "map_split_file".
		ref := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
		if !ref.Match(sources) {
			t.Errorf("fixture %q is run by no e2e test and is not on the nonE2EFixtures allowlist; "+
				"add it to a suite or allowlist it with a reason", name)
		}
	}

	for name, reason := range nonE2EFixtures {
		if !present[name] {
			t.Errorf("nonE2EFixtures lists %q (%q) but testdata/%s/pipeline.mro does not exist; "+
				"drop the stale allowlist entry", name, reason, name)
		}
	}
}

// readTestSources concatenates every .go file in this package directory (the e2e
// suite source) so a fixture name can be matched against the case tables that
// reference it, wherever they live (exported slices or inline literals).
func readTestSources(t *testing.T) []byte {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(root, "test", "e2e", "*.go"))
	if err != nil {
		t.Fatalf("glob e2e sources: %v", err)
	}

	var all []byte

	for _, p := range matches {
		if filepath.Base(p) == "coverage_test.go" {
			continue // don't let the allowlist's own names count as coverage
		}

		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}

		all = append(all, b...)
	}

	return all
}
