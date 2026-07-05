//go:build e2e

package e2e

import (
	"bytes"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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
