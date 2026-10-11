//go:build e2e

package e2e

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/test/divergence"
)

// publishedPair lays out, as an mrp pipestance and an mro2nf results dir,
// what the two runtimes published for a pipeline with a plain file output, a
// file output the stage wrote as a symlink to a host file (as symlink_escape
// does), a file output the stage wrote as a relative symlink to a link to a
// host file, two file outputs bound to one stage file, which mrp publishes
// once and links to from the second, a 2-D file array both leave unpublished
// and name where their tasks wrote each file, and a struct output whose
// producer wrote its keys unsorted (as kitchen_sink's stats), which both
// runtimes publish in that order. It returns the pipestance and results dirs.
func publishedPair(t *testing.T) (string, string) {
	t.Helper()

	ps, results := t.TempDir(), t.TempDir()
	outs := filepath.Join(ps, "outs")
	host := writeABITestFile(t, filepath.Join(t.TempDir(), "host.py"), "print('linked')\n")

	writeABITestFile(t, filepath.Join(outs, "note.txt"), "21.0")
	shared := writeABITestFile(t, filepath.Join(outs, "shared.txt"), "shared")

	for _, link := range [][2]string{{host, "linked.txt"}, {"shared.txt", "again.txt"}} {
		if err := os.Symlink(link[0], filepath.Join(outs, link[1])); err != nil {
			t.Fatal(err)
		}
	}

	chained := mrpChainedOutput(t, ps)

	mrpOuts, err := json.Marshal(map[string]any{
		"again": shared, "chained": chained, "grid": pairMrpGrid(ps), "linked": host, "note": filepath.Join(outs, "note.txt"), "shared": shared,
	})
	if err != nil {
		t.Fatal(err)
	}

	writeABITestFile(t, filepath.Join(ps, "P", "fork0", "_outs"),
		string(mrpOuts[:len(mrpOuts)-1])+`, "stats": `+pairStats+`}`)

	writeABITestFile(t, filepath.Join(results, "note.txt"), "21.0")
	writeABITestFile(t, filepath.Join(results, "linked.txt"), "print('linked')\n")
	writeABITestFile(t, filepath.Join(results, "chained.txt"), "chained\n")
	writeABITestFile(t, filepath.Join(results, "shared.txt"), "shared")
	writeABITestFile(t, filepath.Join(results, "again.txt"), "shared")
	writeABITestFile(t, filepath.Join(results, "manifest.json.gz"), "index")
	writeABITestFile(t, filepath.Join(results, "pipeline_outs.json"), pairOutsJSON(pairStats))

	return ps, results
}

// pairStats is the stats output of publishedPair as its producer wrote it.
const pairStats = `{"count": 4, "total": 10.0, "mean": 2.5}`

// pairMrpGrid is the grid output mrp's outs JSON in the pipestance ps of
// publishedPair holds: each file where the stage wrote it, in its chunk's
// files dir.
func pairMrpGrid(ps string) []any {
	files := filepath.Join(ps, "P", "GRID", "fork0", "chnk0-u1a2b3c4d5", "files")

	return []any{[]any{filepath.Join(files, "cell.txt"), nil, filepath.Join(files, "edge.txt")}}
}

// pairOutsJSON is the pipeline_outs.json of publishedPair with stats as its
// stats value: its grid names each file where its task wrote it, one in a
// local work dir and one in an object-store work dir.
func pairOutsJSON(stats string) string {
	return pairOuts(`[["/nf/work/3f/0123456789abcdef0123456789abcd/main/files/cell.txt", null, `+
		`"s3://bucket/work/7a/0123456789abcdef0123456789abcd/main/files/edge.txt"]]`, stats)
}

// pairMrpRelativeJSON is the outs JSON of the pipestance ps of publishedPair
// with every published path relative to outs/, as pipeline_outs.json names
// it, so that the relative-paths entry excuses nothing there.
func pairMrpRelativeJSON(t *testing.T, ps string) string {
	t.Helper()

	grid, err := json.Marshal(pairMrpGrid(ps))
	if err != nil {
		t.Fatal(err)
	}

	return pairOuts(string(grid), pairStats)
}

// pairOuts is an outs JSON of publishedPair with the given grid and stats.
func pairOuts(grid, stats string) string {
	return `{"again": "shared.txt", "chained": "chained.txt", "grid": ` + grid +
		`, "linked": "linked.txt", "note": "note.txt", "shared": "shared.txt", "stats": ` + stats + `}`
}

// pairFixture is the fixture publishedPair stands in for.
const pairFixture = "kitchen_sink"

// mrpChainedOutput lays out under the pipestance ps what mrp publishes for a
// stage output that is a relative symlink to a link to a host file: outs/
// links to the intermediate link and the outs JSON names the host file
// (martian/core/post_process.go copyOutSymlink). It returns the host file.
func mrpChainedOutput(t *testing.T, ps string) string {
	t.Helper()

	host := writeABITestFile(t, filepath.Join(t.TempDir(), "chained.txt"), "chained\n")
	files := filepath.Join(ps, "P", "CHAIN", "fork0", "files")
	staged := filepath.Join(files, "staged.txt")

	if err := os.MkdirAll(files, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, link := range [][2]string{
		{host, staged},
		{"staged.txt", filepath.Join(files, "chained.txt")},
		{staged, filepath.Join(ps, "outs", "chained.txt")},
	} {
		if err := os.Symlink(link[0], link[1]); err != nil {
			t.Fatal(err)
		}
	}

	return host
}

// TestRemovingAPublishedTreeEntryFailsTheComparison runs the published-output
// comparison on publishedPair: the checked-in allowlist excuses every
// difference, and removing any published-tree entry makes it fail.
func TestRemovingAPublishedTreeEntryFailsTheComparison(t *testing.T) {
	allow := mustAllowlist(t)
	ps, results := publishedPair(t)

	if reports, err := diffPublished(allow, pairFixture, ps, results); err != nil || len(reports) > 0 {
		t.Fatalf("the checked-in allowlist leaves reports %q (err %v)", reports, err)
	}

	for _, e := range allow.Entries() {
		if e.Kind != divergence.KindPublishedTree {
			continue
		}

		t.Run(e.ID, func(t *testing.T) {
			reports, err := diffPublished(allow.Without(e.ID), pairFixture, ps, results)
			if err != nil {
				t.Fatal(err)
			}

			if len(reports) == 0 {
				t.Errorf("the comparison passes without entry %s", e.ID)
			}
		})
	}
}

// TestScopedRuleEntryFailsWhereItExcusesNothing runs the published-output
// comparison with the checked-in relative-paths entry tracked by an issue and
// scoped to pairFixture: on a fixture outside its scope mrp's absolute paths
// are a difference, and on its own fixture an mrp outs JSON that names no path
// under outs/ fails as an entry that excuses nothing.
func TestScopedRuleEntryFailsWhereItExcusesNothing(t *testing.T) {
	allow := allowlistScoping(t, "outs-json-relative-paths", pairFixture)
	ps, results := publishedPair(t)
	mrpJSON := filepath.Join(ps, "P", "fork0", "_outs")
	relative := []byte(pairMrpRelativeJSON(t, ps))

	cases := []struct {
		name, fixture, want string
		mrpOuts             []byte
	}{
		{"out_of_scope", "file_min", "_outs json mismatch", nil},
		{
			"excuses_nothing", pairFixture,
			"allowlist entry outs-json-relative-paths excuses nothing in " + pairFixture, relative,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.mrpOuts != nil {
				writeABITestFile(t, mrpJSON, string(tc.mrpOuts))
			}

			reports, err := diffPublished(allow, tc.fixture, ps, results)
			if err != nil {
				t.Fatal(err)
			}

			if got := strings.Join(reports, "\n"); len(reports) != 1 || !strings.Contains(got, tc.want) {
				t.Errorf("reports %q, want one containing %q", got, tc.want)
			}
		})
	}
}

// TestNormcmpAppliesTheAllowlist runs normcmp.py, the AWS lanes' outs JSON
// comparator, on the outs JSON of publishedPair: it matches under the
// checked-in allowlist, fails without any of its entries, and fails on a key
// order or number token change the allowlist does not excuse.
func TestNormcmpAppliesTheAllowlist(t *testing.T) {
	requireTools(t, "python3")

	ps, results := publishedPair(t)
	mrpOuts := filepath.Join(ps, "outs")
	mrpJSON := filepath.Join(ps, "P", "fork0", "_outs")
	nfJSON := filepath.Join(results, "pipeline_outs.json")
	sortedJSON := writeABITestFile(t, filepath.Join(t.TempDir(), "nf_sorted.json"),
		pairOutsJSON(`{"count": 4, "mean": 2.5, "total": 10.0}`))
	intJSON := writeABITestFile(t, filepath.Join(t.TempDir(), "nf_int.json"),
		pairOutsJSON(`{"count": 4, "total": 10, "mean": 2.5}`))

	checkedIn := filepath.Join(root, "test", "e2e", allowlistFile)

	cases := []struct {
		name, fixture, nf, allowlist string
		match                        bool
	}{
		{"checked_in", pairFixture, nfJSON, checkedIn, true},
		{"key_order", pairFixture, sortedJSON, checkedIn, false},
		{"number_token", pairFixture, intJSON, checkedIn, false},
		{"without_relative_paths", pairFixture, nfJSON, allowlistWithout(t, checkedIn, "outs-json-relative-paths"), false},
		{"without_published_symlinks", pairFixture, nfJSON, allowlistWithout(t, checkedIn, "published-symlinks-copied"), false},
		{"without_unpublished_paths", pairFixture, nfJSON, allowlistWithout(t, checkedIn, "unpublished-file-paths"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("python3", filepath.Join(root, "test", "e2e", "normcmp.py"), mrpJSON, mrpOuts, tc.nf, tc.fixture, tc.allowlist)
			out, err := cmd.CombinedOutput()

			var exit *exec.ExitError
			if err != nil && !errors.As(err, &exit) {
				t.Fatalf("normcmp.py: %v\n%s", err, out)
			}

			if match := err == nil; match != tc.match {
				t.Errorf("normcmp.py match = %v, want %v:\n%s", match, tc.match, out)
			}
		})
	}
}

// allowlistDoc is the checked-in allowlist's entries as generic objects, for
// tests that derive a variant of it.
type allowlistDoc struct {
	Divergences []map[string]any `json:"divergences"`
}

func readAllowlistDoc(t *testing.T, path string) allowlistDoc {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var doc allowlistDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}

	return doc
}

// allowlistScoping returns the checked-in allowlist with the permanent rule
// entry id turned into one tracked by an issue and scoped to fixture.
func allowlistScoping(t *testing.T, id, fixture string) divergence.Allowlist {
	t.Helper()

	doc := readAllowlistDoc(t, filepath.Join(root, "test", "e2e", allowlistFile))

	i := slices.IndexFunc(doc.Divergences, func(e map[string]any) bool { return e["id"] == id })
	if i < 0 {
		t.Fatalf("checked-in allowlist has no entry %s", id)
	}

	comparator, ok := doc.Divergences[i]["comparator"].(map[string]any)
	if !ok {
		t.Fatalf("entry %s has no comparator rule", id)
	}

	doc.Divergences[i]["issue"] = 1
	comparator["fixtures"] = []string{fixture}

	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	allow, err := divergence.Parse(data)
	if err != nil {
		t.Fatal(err)
	}

	return allow
}

// allowlistWithout writes a copy of the allowlist at path less the entry id
// and returns the copy's path.
func allowlistWithout(t *testing.T, path, id string) string {
	t.Helper()

	doc := readAllowlistDoc(t, path)

	n := len(doc.Divergences)
	doc.Divergences = slices.DeleteFunc(doc.Divergences, func(e map[string]any) bool { return e["id"] == id })

	if len(doc.Divergences) != n-1 {
		t.Fatalf("allowlist %s has no entry %s", path, id)
	}

	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	return writeABITestFile(t, filepath.Join(t.TempDir(), allowlistFile), string(out))
}

// TestRemovingAnOutcomeEntryFailsItsCase replays each mrpOutcomeCases entry
// that names a divergence on its recorded outcomes, mrp's from the case and
// mro2nf's from the allowlist entry: the case holds with the entry, and fails
// once the entry is removed, whether or not the case still names it. A
// synthetic case keeps the check live while no checked-in case diverges.
func TestRemovingAnOutcomeEntryFailsItsCase(t *testing.T) {
	t.Run("synthetic", func(t *testing.T) {
		replayOutcomeDivergence(t, outcomeTestAllowlist(t), mrpOutcomeCase{name: "f", divergence: "nf-fails"})
	})

	allow := mustAllowlist(t)

	for _, tc := range mrpOutcomeCases {
		if tc.divergence == "" {
			continue
		}

		t.Run(tc.name, func(t *testing.T) {
			replayOutcomeDivergence(t, allow, tc)
		})
	}
}

func replayOutcomeDivergence(t *testing.T, allow divergence.Allowlist, tc mrpOutcomeCase) {
	t.Helper()

	d, err := tc.validate(allow)
	if err != nil {
		t.Fatal(err)
	}

	if bad := tc.check(d, tc.mrp, d.nf); len(bad) > 0 {
		t.Errorf("the recorded outcomes fail the case: %q", bad)
	}

	if _, err := tc.validate(allow.Without(tc.divergence)); err == nil {
		t.Errorf("the case still validates without entry %s", tc.divergence)
	}

	if bad := tc.check(nil, tc.mrp, d.nf); len(bad) == 0 {
		t.Errorf("with no divergence the recorded outcomes still pass the case")
	}
}
