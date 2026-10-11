package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/cg"
	"github.com/google/go-cmp/cmp"
)

// writeSource writes the transpiled source of fixture's pipeline.mro into
// dir and returns its path and the fixture's dir.
func writeSource(t *testing.T, fixture, dir string) (string, string) {
	t.Helper()

	fx, err := filepath.Abs(filepath.Join("..", "..", "testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}

	src, err := cg.ReadSource(filepath.Join(fx, "pipeline.mro"), []string{fx})
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "source.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	return path, fx
}

// plan runs mre plan on fixture with the entry values values, and returns
// the plan dir.
func plan(t *testing.T, fixture, values string) (string, error) {
	t.Helper()

	work := t.TempDir()
	source, fx := writeSource(t, fixture, work)

	vf := filepath.Join(work, "values.json")
	if err := os.WriteFile(vf, []byte(values), 0o644); err != nil {
		t.Fatal(err)
	}

	err := run(t.Context(), []string{
		"plan", "-source", source, "-values", vf, "-mropath", fx, "-work", work,
		"-o", filepath.Join(work, "plan"), "-nodes", filepath.Join(work, "nodes"), "-roots", filepath.Join(work, "roots"),
	})

	return work, err
}

func readText(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(raw)
}

// TestPlanNestedEntryForks: a run of map_pipe_nested with its sets widened
// at launch, which the witness transpile does not fix, keys the inner map
// call's forks by the outer fork and its own, three under the first set and
// one under the second, as mrp forks OUTER/INNER/DBL.
func TestPlanNestedEntryForks(t *testing.T) {
	work, err := plan(t, "map_pipe_nested", `{"sets":[[1,2,3],[4]]}`)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(readText(t, filepath.Join(work, "roots", "OUTER.INNER.DBL"))), "\n")
	keys := make([]string, len(lines))

	for i, line := range lines {
		keys[i] = strings.Fields(line)[0]
	}

	if d := cmp.Diff([]string{"0.0", "0.1", "0.2", "1.0"}, keys); d != "" {
		t.Errorf("DBL's fork keys (-mrp +plan):\n%s", d)
	}

	if inv := readText(t, filepath.Join(work, "plan", "pipeline.mro")); !strings.Contains(inv, "call OUTER(") || !strings.Contains(inv, "4") {
		t.Errorf("the invocation does not carry the launch values:\n%s", inv)
	}

	if nodes := readText(t, filepath.Join(work, "nodes")); !strings.HasPrefix(nodes, "OUTER.INNER.DBL ") || strings.Count(nodes, "\n") != 1 {
		t.Errorf("node records %q, want one for OUTER.INNER.DBL", nodes)
	}
}

// TestPlanDigestFollowsTheNode: a node's spec digest changes with the
// entry values it reads and with no other (chain_gate: SRC reads x, USE
// reads k).
func TestPlanDigestFollowsTheNode(t *testing.T) {
	digests := func(values string) map[string]string {
		work, err := plan(t, "chain_gate", values)
		if err != nil {
			t.Fatal(err)
		}

		out := map[string]string{}

		for line := range strings.SplitSeq(strings.TrimSpace(readText(t, filepath.Join(work, "nodes"))), "\n") {
			f := strings.Fields(line)
			out[f[0]] = f[1]
		}

		return out
	}

	base, k := digests(`{}`), digests(`{"k":"7"}`)

	if base["CH.USE"] == k["CH.USE"] || base["CH.SRC"] != k["CH.SRC"] || base["CH.GATE"] != k["CH.GATE"] {
		t.Errorf("digests %v with k unset, %v with k = 7: want only USE's to change", base, k)
	}
}

// TestPlanRefusesAsMrp: an invocation mrp refuses to instantiate fails the
// plan, so no stage runs (rt_gate_split_reject_pass: mrp refuses a split of
// a runtime-gated literal collection, #387), and a value that is not a
// literal of its input's type fails it too.
func TestPlanRefusesAsMrp(t *testing.T) {
	if _, err := plan(t, "chain_gate", `{"x":"four"}`); !errors.Is(err, errOverrideParse) {
		t.Errorf("plan with a non-int x: %v, want errOverrideParse", err)
	}

	if _, err := plan(t, "empty_fork_gated", `{"none":[1]}`); err == nil {
		t.Error("plan of a runtime-gated split of a non-empty literal succeeded; mrp refuses it")
	}
}
