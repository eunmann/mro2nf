package divergence

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const (
	checkedIn = "../e2e/divergences.json"
	repoRoot  = "../.."
)

func TestCheckedInAllowlistLoads(t *testing.T) {
	if _, err := Load(checkedIn); err != nil {
		t.Fatal(err)
	}
}

// TestEveryComparatorRuleHasAnEntry fails on a comparator rule the
// differential applies with no allowlist entry to excuse it.
func TestEveryComparatorRuleHasAnEntry(t *testing.T) {
	allow, err := Load(checkedIn)
	if err != nil {
		t.Fatal(err)
	}

	for rule := range ComparatorRules() {
		if !slices.ContainsFunc(allow.Entries(), func(e Entry) bool { return e.Comparator != nil && e.Comparator.Rule == rule }) {
			t.Errorf("comparator rule %q has no allowlist entry", rule)
		}
	}
}

var (
	testFunc   = regexp.MustCompile(`(?m)^func (Test\w+)\(`)
	stringLits = regexp.MustCompile(`"([^"\\\n]*)"`)
)

// TestEvidenceNamesTests fails on evidence naming a test function that does
// not exist, or a subtest whose name no test source spells as a string.
func TestEvidenceNamesTests(t *testing.T) {
	allow, err := Load(checkedIn)
	if err != nil {
		t.Fatal(err)
	}

	funcs, lits := testSources(t)

	for _, e := range allow.Entries() {
		for _, ev := range e.Evidence {
			parts := strings.Split(ev, "/")
			if !funcs[parts[0]] {
				t.Errorf("%s cites %s, but no test function %s exists", e.ID, ev, parts[0])
			}

			for _, sub := range parts[1:] {
				if !lits[sub] {
					t.Errorf("%s cites %s, but no test source names subtest %q", e.ID, ev, sub)
				}
			}
		}
	}
}

// testSources returns the test function names and the string literals of
// every _test.go file in the repo.
func testSources(t *testing.T) (map[string]bool, map[string]bool) {
	t.Helper()

	funcs, lits := map[string]bool{}, map[string]bool{}

	err := filepath.WalkDir(repoRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() && slices.Contains([]string{".git", "deploy", "vendor-martian"}, d.Name()) {
			return filepath.SkipDir
		}

		if d.IsDir() || !strings.HasSuffix(p, "_test.go") {
			return nil
		}

		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}

		for _, m := range testFunc.FindAllSubmatch(src, -1) {
			funcs[string(m[1])] = true
		}

		for _, m := range stringLits.FindAllSubmatch(src, -1) {
			lits[string(m[1])] = true
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return funcs, lits
}

func TestParseRejectsInvalid(t *testing.T) {
	const ok = `"feature": "f", "summary": "s", "justification": "j", "evidence": ["TestX"]`

	cases := map[string]string{
		"malformed":             `{"divergences": [`,
		"trailing data":         `{"divergences": []} {}`,
		"unknown field":         `{"divergences": [{"id": "a", "kind": "mechanism", ` + ok + `, "stage": "X"}]}`,
		"bad id":                `{"divergences": [{"id": "A_b", "kind": "mechanism", ` + ok + `}]}`,
		"duplicate id":          `{"divergences": [{"id": "a", "kind": "mechanism", ` + ok + `}, {"id": "a", "kind": "mechanism", ` + ok + `}]}`,
		"unknown kind":          `{"divergences": [{"id": "a", "kind": "other", ` + ok + `, "comparator": {"rule": "outs-json-strip-outs-prefix"}}]}`,
		"no summary":            `{"divergences": [{"id": "a", "kind": "mechanism", "feature": "f", "justification": "j", "evidence": ["TestX"]}]}`,
		"line break":            `{"divergences": [{"id": "a", "kind": "mechanism", "feature": "f\n", "summary": "s", "justification": "j", "evidence": ["TestX"]}]}`,
		"negative issue":        `{"divergences": [{"id": "a", "kind": "mechanism", "issue": -1, ` + ok + `}]}`,
		"unknown emitter":       `{"divergences": [{"id": "a", "kind": "mechanism", "emitter": "v3", ` + ok + `}]}`,
		"no evidence":           `{"divergences": [{"id": "a", "kind": "mechanism", "feature": "f", "summary": "s", "justification": "j"}]}`,
		"evidence not a test":   `{"divergences": [{"id": "a", "kind": "mechanism", "feature": "f", "summary": "s", "justification": "j", "evidence": ["kitchen_sink"]}]}`,
		"mechanism matcher":     `{"divergences": [{"id": "a", "kind": "mechanism", ` + ok + `, "outcome": {"failed": true}}]}`,
		"no matcher":            `{"divergences": [{"id": "a", "kind": "abi", ` + ok + `}]}`,
		"two matchers":          `{"divergences": [{"id": "a", "kind": "outcome", ` + ok + `, "outcome": {"failed": true}, "comparator": {"rule": "collapse-retried-attempts"}}]}`,
		"matcher of other kind": `{"divergences": [{"id": "a", "kind": "outcome", ` + ok + `, "abi": {"fixtures": ["f"], "file": "_args", "rule": "ignore-key", "key": "k"}}]}`,
		"unknown rule":          `{"divergences": [{"id": "a", "kind": "abi", ` + ok + `, "comparator": {"rule": "sort"}}]}`,
		"rule of other kind":    `{"divergences": [{"id": "a", "kind": "abi", ` + ok + `, "comparator": {"rule": "collapse-retried-attempts"}}]}`,
		"rule matched twice":    `{"divergences": [{"id": "a", "kind": "published-tree", ` + ok + `, "comparator": {"rule": "outs-json-strip-outs-prefix"}}, {"id": "b", "kind": "published-tree", ` + ok + `, "comparator": {"rule": "outs-json-strip-outs-prefix"}}]}`,
		"abi no fixture":        `{"divergences": [{"id": "a", "kind": "abi", ` + ok + `, "abi": {"fixtures": [], "file": "_args", "rule": "ignore-key", "key": "k"}}]}`,
		"abi bad file":          `{"divergences": [{"id": "a", "kind": "abi", ` + ok + `, "abi": {"fixtures": ["f"], "file": "_stdout", "rule": "ignore-key", "key": "k"}}]}`,
		"abi unknown rule":      `{"divergences": [{"id": "a", "kind": "abi", ` + ok + `, "abi": {"fixtures": ["f"], "file": "_args", "rule": "ignore", "key": "k"}}]}`,
		"abi no key":            `{"divergences": [{"id": "a", "kind": "abi", ` + ok + `, "abi": {"fixtures": ["f"], "file": "_args", "rule": "ceil-number", "key": ""}}]}`,
		"success with failure":  `{"divergences": [{"id": "a", "kind": "outcome", ` + ok + `, "outcome": {"failed": false, "failures": [{"kind": "error", "stage": "S"}]}}]}`,
		"bad failure kind":      `{"divergences": [{"id": "a", "kind": "outcome", ` + ok + `, "outcome": {"failed": true, "failures": [{"kind": "oom", "stage": "S"}]}}]}`,
		"tracked rule unscoped": `{"divergences": [{"id": "a", "kind": "published-tree", "issue": 1, ` + ok + `, "comparator": {"rule": "outs-json-strip-outs-prefix"}}]}`,
		"tracked rule blank":    `{"divergences": [{"id": "a", "kind": "published-tree", "issue": 1, ` + ok + `, "comparator": {"rule": "outs-json-strip-outs-prefix", "fixtures": [""]}}]}`,
		"permanent rule scoped": `{"divergences": [{"id": "a", "kind": "published-tree", ` + ok + `, "comparator": {"rule": "outs-json-strip-outs-prefix", "fixtures": ["fx"]}}]}`,
	}

	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(doc)); err == nil {
				t.Errorf("Parse accepted %s", doc)
			}
		})
	}
}

// lookupAllowlist holds one entry of each matcher shape: a key-scoped ABI
// entry, an issue-tracked rule scoped to fx, a permanent rule, an outcome
// and a mechanism.
func lookupAllowlist(t *testing.T) Allowlist {
	t.Helper()

	const ok = `"feature": "f", "summary": "s", "justification": "j", "evidence": ["TestX"]`

	allow, err := Parse([]byte(`{"divergences": [
		{"id": "key", "kind": "abi", "issue": 7, ` + ok + `, "abi": {"fixtures": ["fx"], "file": "_args", "rule": "ignore-key", "key": "k"}},
		{"id": "paths", "kind": "published-tree", "issue": 8, ` + ok + `, "comparator": {"rule": "outs-json-strip-outs-prefix", "fixtures": ["fx"]}},
		{"id": "by-stage", "kind": "outcome", ` + ok + `, "comparator": {"rule": "compare-failure-stage-and-kind"}},
		{"id": "nf-fails", "kind": "outcome", ` + ok + `, "outcome": {"failed": true}},
		{"id": "how", "kind": "mechanism", ` + ok + `}]}`))
	if err != nil {
		t.Fatal(err)
	}

	return allow
}

// TestAllowlistRuleScope checks an issue-tracked rule applies only on its
// fixtures and a permanent one on every fixture, and that only the scoped one
// is a rule the comparison must find still excusing something.
func TestAllowlistRuleScope(t *testing.T) {
	allow := lookupAllowlist(t)

	excuses := []struct {
		allow   Allowlist
		rule    string
		fixture string
		want    bool
	}{
		{allow, RuleOutsRelativePaths, "fx", true},
		{allow, RuleOutsRelativePaths, "other", false},
		{allow.Without("paths"), RuleOutsRelativePaths, "fx", false},
		{allow, RuleFailureStageAndKind, "any", true},
		{allow, RuleRetriedAttempts, "fx", false},
	}

	for _, c := range excuses {
		if got := c.allow.Excuses(c.rule, c.fixture); got != c.want {
			t.Errorf("Excuses(%s, %s) = %v, want %v", c.rule, c.fixture, got, c.want)
		}
	}

	if got := allow.ScopedRules(KindPublishedTree, "fx"); len(got) != 1 || got[0].ID != "paths" {
		t.Errorf("ScopedRules(published-tree, fx) = %v, want the paths entry", got)
	}

	for _, c := range [][2]string{{KindPublishedTree, "other"}, {KindABI, "fx"}, {KindOutcome, "fx"}, {KindOutcome, "any"}} {
		if got := allow.ScopedRules(c[0], c[1]); len(got) != 0 {
			t.Errorf("ScopedRules(%s, %s) = %v, want none", c[0], c[1], got)
		}
	}
}

// TestAllowlistRulesEverywhere checks the allowlist for a program no fixture
// names applies every comparator rule on every fixture, the issue-tracked one
// with no fixture left to find it excusing nothing, keeps the mechanism, and
// drops the ABI and outcome matchers.
func TestAllowlistRulesEverywhere(t *testing.T) {
	allow := lookupAllowlist(t)
	everywhere := allow.RulesEverywhere()

	ids := make([]string, 0, len(everywhere.Entries()))
	for _, e := range everywhere.Entries() {
		ids = append(ids, e.ID)
	}

	if want := []string{"paths", "by-stage", "how"}; !slices.Equal(ids, want) {
		t.Errorf("RulesEverywhere kept %v, want %v", ids, want)
	}

	for _, rule := range []string{RuleOutsRelativePaths, RuleFailureStageAndKind} {
		if !everywhere.Excuses(rule, "other") {
			t.Errorf("RulesEverywhere does not apply %s on every fixture", rule)
		}
	}

	if got := slices.Concat(everywhere.ABI("fx"), everywhere.ScopedRules(KindPublishedTree, "fx")); len(got) != 0 {
		t.Errorf("RulesEverywhere keeps %v scoped to fx", got)
	}

	if allow.Excuses(RuleOutsRelativePaths, "other") || len(allow.ScopedRules(KindPublishedTree, "fx")) != 1 || len(allow.Entries()) != 5 {
		t.Error("RulesEverywhere changed the original allowlist")
	}
}

// TestAllowlistForEmitter: an entry scoped to one emitter holds only for
// it, and an unscoped one for both.
func TestAllowlistForEmitter(t *testing.T) {
	const ok = `"kind": "mechanism", "feature": "f", "summary": "s", "justification": "j", "evidence": ["TestX"]`

	allow, err := Parse([]byte(`{"divergences": [{"id": "both", ` + ok + `}, {"id": "old", "emitter": "v1", ` + ok + `}]}`))
	if err != nil {
		t.Fatal(err)
	}

	for emitter, want := range map[string]int{EmitterV1: 2, EmitterV2: 1} {
		if got := allow.ForEmitter(emitter).Entries(); len(got) != want || got[0].ID != "both" {
			t.Errorf("ForEmitter(%s) = %v, want %d entries", emitter, got, want)
		}
	}
}

func TestAllowlistLookups(t *testing.T) {
	allow := lookupAllowlist(t)

	if got := allow.ABI("fx"); len(got) != 1 || got[0].ID != "key" {
		t.Errorf("ABI(fx) = %v, want the key entry", got)
	}

	if got := allow.ABI("other"); len(got) != 0 {
		t.Errorf("ABI(other) = %v, want none", got)
	}

	if _, err := allow.Outcome("nf-fails"); err != nil {
		t.Errorf("Outcome(nf-fails): %v", err)
	}

	for _, id := range []string{"how", "missing"} {
		if _, err := allow.Outcome(id); !errors.Is(err, ErrInvalid) {
			t.Errorf("Outcome(%s) err = %v, want ErrInvalid", id, err)
		}
	}

	if _, ok := allow.Without("how").Entry("how"); ok {
		t.Error("Without kept the entry")
	}

	if len(allow.Entries()) != 5 {
		t.Errorf("Without changed the original allowlist: %d entries", len(allow.Entries()))
	}
}
