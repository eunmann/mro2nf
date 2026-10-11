//go:build e2e

package e2e

import (
	"bytes"
	"go/scanner"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"testing"
)

// nonE2EFixtures are testdata fixtures deliberately NOT exercised by any e2e
// suite, each with the reason. A fixture here is exempt from the completeness
// check below; anything else under testdata/ must be named by some e2e test.
var nonE2EFixtures = map[string]string{
	// Runtime-wise it behaves like map_pipe_nested (which the golden + docker
	// suites run); only the -native diagnostic wording differs.
	"map_pipe_nested_file": "emit-only: keyed native-scatter FORK_K+MERGE_K diagnostic (internal/emit unit test)",
}

// TestEveryFixtureIsExercised fails when a testdata fixture (a directory with a
// pipeline.mro) is named by no e2e test and is not on the nonE2EFixtures
// allowlist. It is the guard the README's "every fixture" claim lacked: a new
// fixture that nobody wired into a suite (like entry_dir was) is caught here
// rather than shipping as silent dead weight. It also fails on a STALE allowlist
// entry, so the exemptions can't outlive their fixtures.
func TestEveryFixtureIsExercised(t *testing.T) {
	sources := readTestSources(t)
	present := pipelineFixtures(t)

	for _, name := range slices.Sorted(maps.Keys(present)) {
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

// mrpUnrunnable are the pipeline fixtures neither TestMrpDiff nor
// TestMrpDiffFailureParity can compare with real mrp, each with the reason:
// mrp cannot run them, or its result holds a host path no other run can give.
// A fixture mrp fails by design belongs in mrpOutcomeCases instead. The golden
// suite's *_override cases are no fixtures of their own: a launch-time
// -params-file override has no mrp invocation to diff against.
var mrpUnrunnable = map[string]string{
	"exec_min":             "mrp never completes it (killed after 180 s): the exec stub writes no journal entries",
	"mixed_adapters":       "mrp never completes it (killed after 180 s): its stubs speak mre's wrapped-adapter contract, not mrp's journal protocol",
	"map_pipe_nested":      "mrp rejects it: OUTER.INNER.DBL's merge could not match a reference to a specific fork",
	"map_pipe_nested_file": "emit-only fixture with no stage code: mrp cannot import stages/dbl",
	"symlink_escape":       mrpHostLinkOut,
	"entry_file":           mrpUnresolvedInput,
	"entry_filearr":        mrpUnresolvedInput,
	"entry_struct_file":    mrpUnresolvedInput,
	"entry_mapfile":        mrpUnresolvedInput,
	"entry_dir":            mrpUnresolvedInput,
	"entry_file_empty":     mrpUnresolvedInput,
	"split_from_file":      mrpUnresolvedInput,
}

const mrpUnresolvedInput = "mrp passes the invocation's relative input/ path through verbatim and the stage opens it " +
	"from its own job dir, where it does not resolve"

const mrpHostLinkOut = "mrp runs it, but its _outs value for the linked output is the link target's absolute host path; " +
	"the published tree and the stage ABI match"

// TestEveryFixtureMrpDiffed fails unless every pipeline fixture is in exactly
// one of mrpDiffCases (outputs diffed), mrpOutcomeCases (run outcome diffed)
// and mrpUnrunnable (with a reason), when a table names a fixture that does
// not exist, or when an mrpOutcomeCases entry is malformed: every fixture mrp
// can run faces live mrp.
func TestEveryFixtureMrpDiffed(t *testing.T) {
	present := pipelineFixtures(t)
	tables := map[string][]string{}

	for _, tc := range mrpDiffCases {
		tables[tc.name] = append(tables[tc.name], "mrpDiffCases")
	}

	for _, tc := range mrpOutcomeCases {
		tables[tc.name] = append(tables[tc.name], "mrpOutcomeCases")

		if err := tc.validate(); err != nil {
			t.Error(err)
		}
	}

	for name, reason := range mrpUnrunnable {
		tables[name] = append(tables[name], "mrpUnrunnable")

		if reason == "" {
			t.Errorf("mrpUnrunnable lists %q without a reason", name)
		}
	}

	for _, name := range slices.Sorted(maps.Keys(tables)) {
		if !present[name] {
			t.Errorf("%v list %q but testdata/%s/pipeline.mro does not exist", tables[name], name, name)
		}

		if len(tables[name]) > 1 {
			t.Errorf("%q is listed %d times, in %v; keep one entry", name, len(tables[name]), tables[name])
		}
	}

	for _, name := range slices.Sorted(maps.Keys(present)) {
		if len(tables[name]) == 0 {
			t.Errorf("fixture %q is not compared with mrp; add it to mrpDiffCases if mrp succeeds on it, "+
				"to mrpOutcomeCases if mrp fails on it by design, or to mrpUnrunnable with the reason mrp cannot run it", name)
		}
	}
}

// pipelineFixtures is the set of testdata fixtures: the directories holding a
// pipeline.mro.
func pipelineFixtures(t *testing.T) map[string]bool {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(root, "testdata"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}

	present := map[string]bool{}

	for _, e := range entries {
		if e.IsDir() && fileExists(filepath.Join(root, "testdata", e.Name(), "pipeline.mro")) {
			present[e.Name()] = true
		}
	}

	return present
}

// readTestSources extracts the STRING LITERALS (one per line) from every .go
// file in this package directory (the e2e suite source) so a fixture name can
// be matched against the case tables that reference it, wherever they live
// (exported slices or inline literals). What counts as coverage is exactly a
// WHOLE quoted string literal naming the fixture — a case-table entry or a
// path join. A name assembled by concatenation or held in a cross-package
// const is NOT seen, but that miss is loud (the check fails and demands the
// fixture be wired or allowlisted), never silent. Scanning tokens instead of
// raw bytes stops a COMMENT mention from counting as coverage (#127): a
// fixture that is merely discussed is still unexercised.
func readTestSources(t *testing.T) []byte {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(root, "test", "e2e", "*.go"))
	if err != nil {
		t.Fatalf("glob e2e sources: %v", err)
	}

	var all bytes.Buffer

	for _, p := range matches {
		if filepath.Base(p) == "coverage_test.go" {
			continue // don't let the allowlist's own names count as coverage
		}

		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}

		// A nil error handler is safe: these sources are this very package,
		// so they compiled before this test could run.
		fset := token.NewFileSet()

		var s scanner.Scanner
		s.Init(fset.AddFile(p, fset.Base(), len(b)), b, nil, 0)

		for {
			_, tok, lit := s.Scan()
			if tok == token.EOF {
				break
			}

			if tok != token.STRING {
				continue
			}

			val, err := strconv.Unquote(lit)
			if err != nil {
				val = lit
			}

			all.WriteString(val)
			all.WriteByte('\n')
		}
	}

	return all.Bytes()
}
