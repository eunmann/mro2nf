package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/cg"
	"github.com/google/go-cmp/cmp"
)

// TestWitnessSource: a fixture's witness source binds every entry
// collection non-empty, nested ones of different lengths, and is no second
// invocation of a fixture whose call binds no input.
func TestWitnessSource(t *testing.T) {
	fixture := func(name string) string { return filepath.Join("..", "..", "testdata", name) }

	if _, differs, err := witnessSource(fixture("dir_hidden")); err != nil || differs {
		t.Errorf("dir_hidden: differs %v, err %v; want its own invocation", differs, err)
	}

	dir := fixture("merge_unforked_pipeline")

	text, differs, err := witnessSource(dir)
	if err != nil || !differs {
		t.Fatalf("merge_unforked_pipeline: differs %v, err %v; want a witness invocation", differs, err)
	}

	out := filepath.Join(t.TempDir(), "fixture")
	if err := writeWitnessFixture(dir, out, text); err != nil {
		t.Fatal(err)
	}

	src, err := cg.ReadSource(filepath.Join(out, "pipeline.mro"), []string{out})
	if err != nil {
		t.Fatal(err)
	}

	var xss [][]string
	if err := json.Unmarshal(src.Invocation.Args["xss"], &xss); err != nil {
		t.Fatal(err)
	}

	if d := cmp.Diff([][]string{{""}, {"", ""}}, xss); d != "" {
		t.Errorf("witness xss (-want +got):\n%s", d)
	}

	if _, err := os.Stat(filepath.Join(out, "stages", "count", "__init__.py")); err != nil {
		t.Errorf("witness fixture lacks the stages: %v", err)
	}
}

// TestJoinVerdicts: the witness run's findings join under witness legs, a
// program is no test only where neither run is one, and its fixture is
// written at the witness invocation only when that alone diverged.
func TestJoinVerdicts(t *testing.T) {
	lit := genFinding{leg: "default", key: "outs"}
	wit := genFinding{leg: "default", key: "abi"}

	both := joinVerdicts(genVerdict{findings: []genFinding{lit}}, genVerdict{findings: []genFinding{wit}})
	if d := cmp.Diff([]string{"default outs", "default@witness abi"}, findingIDs(both)); d != "" {
		t.Errorf("finding ids (-want +got):\n%s", d)
	}

	if both.atWitness() {
		t.Error("a literal finding's fixture written at the witness invocation")
	}

	if v := joinVerdicts(genVerdict{skip: "mrp refused"}, genVerdict{findings: []genFinding{wit}}); v.skip != "" || !v.atWitness() || len(v.notes) != 1 {
		t.Errorf("literal no test, witness diverges: skip %q, atWitness %v, notes %q", v.skip, v.atWitness(), v.notes)
	}

	if v := joinVerdicts(genVerdict{skip: "a"}, genVerdict{skip: "b"}); v.skip == "" {
		t.Error("a program neither run tests is a test")
	}
}

func findingIDs(v genVerdict) []string {
	ids := make([]string, len(v.findings))
	for i, f := range v.findings {
		ids[i] = f.id()
	}

	return ids
}

// TestMrpRanUnresolved: a run whose log shows mrp ran a job on bindings it
// failed to resolve (#606) is told apart from a clean one.
func TestMrpRanUnresolved(t *testing.T) {
	dir := t.TempDir()

	for name, tc := range map[string]struct {
		log  string
		want bool
	}{
		"clean":      {"Pipestance completed successfully!\n", false},
		"unresolved": {"[runtime] Error resolving input argument bindings for ID.mrp.TOP.C1.ST0.fork1\n", true},
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(tc.log), 0o600); err != nil {
			t.Fatal(err)
		}

		if got, err := mrpRanUnresolved(path); err != nil || got != tc.want {
			t.Errorf("%s: mrpRanUnresolved = %v, %v; want %v", name, got, err, tc.want)
		}
	}
}
