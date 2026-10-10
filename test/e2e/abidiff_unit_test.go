package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func writeABITestFile(t *testing.T, path, content string) string {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))

	return hex.EncodeToString(sum[:])
}

// TestABINormalizeCanonicalJSON checks key order is canonicalized while every
// number keeps its source token, so int-vs-float and big-int precision stay
// visible.
func TestABINormalizeCanonicalJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "sorted_keys", in: `{"b": 1, "a": {"y": true, "x": null}}`, want: `{"a":{"x":null,"y":true},"b":1}`},
		{name: "float_token_kept", in: `{"x": 1.0}`, want: `{"x":1.0}`},
		{name: "exponent_token_kept", in: `{"x": 1e6}`, want: `{"x":1e6}`},
		{name: "big_int_exact", in: `{"x": 9007199254740993}`, want: `{"x":9007199254740993}`},
		{name: "html_not_escaped", in: `{"x": "<a&b>"}`, want: `{"x":"<a&b>"}`},
		{name: "relative_path_verbatim", in: `{"x": "input/a.txt"}`, want: `{"x":"input/a.txt"}`},
		{name: "array_order_kept", in: `[3, 1, 2]`, want: `[3,1,2]`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := abiNormalizer{}.normalize([]byte(tc.in))
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}

			if got != tc.want {
				t.Errorf("normalize(%s) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// TestABINormalizeIntVsFloatDiffer guards the class of bug the differential
// exists for (#238, #245): an int where mrp passes a float must not normalize
// away.
func TestABINormalizeIntVsFloatDiffer(t *testing.T) {
	asInt, err := abiNormalizer{}.normalize([]byte(`{"x": 1000000}`))
	if err != nil {
		t.Fatal(err)
	}

	asFloat, err := abiNormalizer{}.normalize([]byte(`{"x": 1000000.0}`))
	if err != nil {
		t.Fatal(err)
	}

	if asInt == asFloat {
		t.Errorf("1000000 and 1000000.0 both normalize to %s", asInt)
	}
}

func TestABINormalizeRejectsMalformed(t *testing.T) {
	for _, in := range []string{`{"x": `, `{"x": 1} {"y": 2}`} {
		if _, err := (abiNormalizer{}).normalize([]byte(in)); err == nil {
			t.Errorf("normalize(%q) succeeded, want an error", in)
		}
	}
}

// TestABINormalizePaths checks each path rewrite: existing files and dirs
// become content identity plus the basename the stage sees, absent paths under
// the invocation's own files dir lose only that host prefix, and anything else
// stays verbatim.
func TestABINormalizePaths(t *testing.T) {
	dir := t.TempDir()
	own := filepath.Join(dir, "chnk0", "files")
	file := writeABITestFile(t, filepath.Join(dir, "up", "reads.txt"), "ACGT")
	writeABITestFile(t, filepath.Join(dir, "tree", "a.txt"), "x")
	ownFile := writeABITestFile(t, filepath.Join(own, "out.bam"), "BAM")

	link := filepath.Join(dir, "staged", "L0000.txt")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}

	treeSum, err := hashDirTree(filepath.Join(dir, "tree"))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		in      string
		extOnly bool
		want    string
	}{
		{name: "file", in: file, want: "file:" + sha256Hex("ACGT") + ":reads.txt"},
		{name: "symlink_keeps_link_basename", in: link, want: "file:" + sha256Hex("ACGT") + ":L0000.txt"},
		{name: "ext_only_received", in: link, extOnly: true, want: "file:" + sha256Hex("ACGT") + ":*.txt"},
		{name: "ext_only_spares_own_files", in: ownFile, extOnly: true, want: "file:" + sha256Hex("BAM") + ":out.bam"},
		{name: "dir", in: filepath.Join(dir, "tree"), want: "dir:" + treeSum + ":tree"},
		{name: "absent_under_own_files", in: filepath.Join(own, "sub", "gone.bam"), want: "<files>/sub/gone.bam"},
		{name: "absent_elsewhere_verbatim", in: filepath.Join(dir, "gone.txt"), want: filepath.Join(dir, "gone.txt")},
		{name: "own_prefix_needs_separator", in: own + "x/y", want: own + "x/y"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := abiNormalizer{ownFiles: []string{own}, extOnly: tc.extOnly}.path(tc.in)
			if err != nil {
				t.Fatalf("path: %v", err)
			}

			if got != tc.want {
				t.Errorf("path(%s) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// TestABINormalizeSameContentAcrossHosts checks two runtimes that hand a stage
// the same bytes under different host dirs normalize equal, and that a content
// difference does not.
func TestABINormalizeSameContentAcrossHosts(t *testing.T) {
	mrpFile := writeABITestFile(t, filepath.Join(t.TempDir(), "ps", "files", "ref.bin"), "data")
	nfFile := writeABITestFile(t, filepath.Join(t.TempDir(), "work", "ab", "ref.bin"), "data")
	other := writeABITestFile(t, filepath.Join(t.TempDir(), "ref.bin"), "different")

	norm := func(p string) string {
		got, err := abiNormalizer{}.normalize([]byte(`{"ref": "` + p + `"}`))
		if err != nil {
			t.Fatal(err)
		}

		return got
	}

	if a, b := norm(mrpFile), norm(nfFile); a != b {
		t.Errorf("same content normalized differently:\n%s\n%s", a, b)
	}

	if norm(mrpFile) == norm(other) {
		t.Errorf("different content normalized equal: %s", norm(other))
	}
}

func TestHashDirTree(t *testing.T) {
	build := func(t *testing.T, files map[string]string, emptyDirs ...string) string {
		t.Helper()

		dir := t.TempDir()
		for rel, content := range files {
			writeABITestFile(t, filepath.Join(dir, rel), content)
		}

		for _, d := range emptyDirs {
			if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}

		return dir
	}

	sum := func(t *testing.T, dir string) string {
		t.Helper()

		s, err := hashDirTree(dir)
		if err != nil {
			t.Fatal(err)
		}

		return s
	}

	base := sum(t, build(t, map[string]string{"a.txt": "x", "sub/b.txt": "y"}))

	if got := sum(t, build(t, map[string]string{"a.txt": "x", "sub/b.txt": "y"})); got != base {
		t.Errorf("identical trees hash differently")
	}

	variants := map[string]string{
		"content":   sum(t, build(t, map[string]string{"a.txt": "x", "sub/b.txt": "z"})),
		"name":      sum(t, build(t, map[string]string{"a.txt": "x", "sub/c.txt": "y"})),
		"empty_dir": sum(t, build(t, map[string]string{"a.txt": "x", "sub/b.txt": "y"}, "empty")),
	}
	for name, got := range variants {
		if got == base {
			t.Errorf("%s change did not change the tree hash", name)
		}
	}

	target := build(t, map[string]string{"a.txt": "x", "sub/b.txt": "y"})
	link := filepath.Join(t.TempDir(), "link")

	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if got := sum(t, link); got != base {
		t.Errorf("symlinked root hashes differently from the tree it points at")
	}
}

func TestLoadDivergencesCheckedIn(t *testing.T) {
	divs, err := loadDivergences("divergences.json")
	if err != nil {
		t.Fatalf("load checked-in divergences: %v", err)
	}

	if len(divs) == 0 {
		t.Fatal("divergences.json lists no entries")
	}
}

func TestLoadDivergencesRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{name: "no_issue", doc: `{"divergences":[{"summary":"s","file":"_args","rule":"basename-extension-only"}]}`},
		{name: "no_summary", doc: `{"divergences":[{"issue":1,"file":"_args","rule":"basename-extension-only"}]}`},
		{name: "bad_file", doc: `{"divergences":[{"issue":1,"summary":"s","file":"_jobinfo","rule":"basename-extension-only"}]}`},
		{name: "unknown_rule", doc: `{"divergences":[{"issue":1,"summary":"s","file":"_args","rule":"ignore"}]}`},
		{name: "unknown_field", doc: `{"divergences":[{"issue":1,"summary":"s","file":"_args","rule":"basename-extension-only","stage":"X"}]}`},
		{name: "malformed", doc: `{"divergences":[`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeABITestFile(t, filepath.Join(t.TempDir(), "d.json"), tc.doc)
			if _, err := loadDivergences(path); err == nil {
				t.Errorf("loadDivergences accepted %s", tc.doc)
			}
		})
	}
}

// TestNewABINormalizerScopesRuleToFile checks a divergence applies only to the
// metadata file it names.
func TestNewABINormalizerScopesRuleToFile(t *testing.T) {
	divs := []divergence{{Issue: 284, Summary: "s", File: argsFile, Rule: ruleBasenameExt}}

	if !newABINormalizer(nil, argsFile, divs).extOnly {
		t.Errorf("rule for %s not applied to %s", argsFile, argsFile)
	}

	if newABINormalizer(nil, outsFile, divs).extOnly {
		t.Errorf("rule for %s leaked into %s", argsFile, outsFile)
	}
}

type abiGroupCount struct {
	Group string
	N     int
}

func groupCounts(invs []abiInvocation) []abiGroupCount {
	counts := map[string]int{}
	for _, inv := range invs {
		counts[inv.group()]++
	}

	out := make([]abiGroupCount, 0, len(counts))
	for g, n := range counts {
		out = append(out, abiGroupCount{g, n})
	}

	slices.SortFunc(out, func(a, b abiGroupCount) int { return strings.Compare(a.Group, b.Group) })

	return out
}

// TestCollectMrpInvocations reads a real mrp pipestance: a split stage's split,
// three chunks and join all ran, while the non-split REPORT ran only chnk0 (its
// split/ and join/ dirs hold bookkeeping no stage code saw).
func TestCollectMrpInvocations(t *testing.T) {
	invs, err := collectMrpInvocations(filepath.Join("..", "..", "testdata", "split_test", "expected"), nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	want := []abiGroupCount{{"REPORT main", 1}, {"SUM_SQUARES join", 1}, {"SUM_SQUARES main", 3}, {"SUM_SQUARES split", 1}}
	if diff := cmp.Diff(want, groupCounts(invs)); diff != "" {
		t.Errorf("invocation groups (-want +got):\n%s", diff)
	}

	for _, inv := range invs {
		if inv.Stage == "SUM_SQUARES" && inv.Phase == phaseSplit {
			if inv.Args != `{"values":[1,2,3]}` || inv.Outs != absentMeta {
				t.Errorf("split invocation = args %s outs %s, want {\"values\":[1,2,3]} and absent outs", inv.Args, inv.Outs)
			}
		}
	}
}

func TestMrpStageName(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{name: "plain", src: "@include \"p.mro\"\n\ncall WRITE_BAM(\n    x = 1,\n)\n", want: "WRITE_BAM"},
		{name: "alias", src: "@include \"p.mro\"\n\ncall WRITE_BAM as MULTI_WRITE_BAM(\n    x = 1,\n)\n", want: "WRITE_BAM"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fork := t.TempDir()
			writeABITestFile(t, filepath.Join(fork, "_invocation"), tc.src)

			got, err := mrpStageName(fork)
			if err != nil {
				t.Fatalf("mrpStageName: %v", err)
			}

			if got != tc.want {
				t.Errorf("mrpStageName = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCollectNextflowInvocations reads mre's per-task metadata dirs, counting
// only tasks that exited 0.
func TestCollectNextflowInvocations(t *testing.T) {
	proj := t.TempDir()
	task := func(hash, exit, phase, stage string) {
		dir := filepath.Join(proj, "work", "ab", hash)
		writeABITestFile(t, filepath.Join(dir, ".exitcode"), exit)
		writeABITestFile(t, filepath.Join(dir, phase, mreStageFile), stage)
		writeABITestFile(t, filepath.Join(dir, phase, argsFile), `{"n": 3}`)
		writeABITestFile(t, filepath.Join(dir, phase, outsFile), `{"total": 6}`)
	}

	task("ok1", "0", phaseMain, "SUM")
	task("ok2", "0\n", phaseJoin, "SUM")
	task("failed", "1", phaseMain, "SUM")

	invs, err := collectNextflowInvocations(proj, nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	want := []abiGroupCount{{"SUM join", 1}, {"SUM main", 1}}
	if diff := cmp.Diff(want, groupCounts(invs)); diff != "" {
		t.Errorf("invocation groups (-want +got):\n%s", diff)
	}

	task("anon", "0", phaseMain, "")

	if _, err := collectNextflowInvocations(proj, nil); !errors.Is(err, errABIMeta) {
		t.Errorf("empty stage name: err = %v, want errABIMeta", err)
	}
}

func TestDiffABI(t *testing.T) {
	inv := func(stage, phase, args, origin string) abiInvocation {
		return abiInvocation{Stage: stage, Phase: phase, Args: args, Outs: absentMeta, Origin: origin}
	}

	mrp := []abiInvocation{
		inv("S", phaseMain, `{"i":0}`, "mrp/chnk0"),
		inv("S", phaseMain, `{"i":1}`, "mrp/chnk1"),
		inv("S", phaseMain, `{"i":1}`, "mrp/chnk2"),
	}

	cases := []struct {
		name    string
		nf      []abiInvocation
		reports []string
	}{
		{
			name: "same_multiset_any_order",
			nf: []abiInvocation{
				inv("S", phaseMain, `{"i":1}`, "nf/c"), inv("S", phaseMain, `{"i":0}`, "nf/a"),
				inv("S", phaseMain, `{"i":1}`, "nf/b"),
			},
		},
		{
			name:    "duplicate_missing",
			nf:      []abiInvocation{inv("S", phaseMain, `{"i":0}`, "nf/a"), inv("S", phaseMain, `{"i":1}`, "nf/b")},
			reports: []string{"S main: 3 mrp vs 2 nextflow", "mrp      mrp/chnk2"},
		},
		{
			name: "value_differs",
			nf: []abiInvocation{
				inv("S", phaseMain, `{"i":0}`, "nf/a"), inv("S", phaseMain, `{"i":1}`, "nf/b"),
				inv("S", phaseMain, `{"i":1.0}`, "nf/c"),
			},
			reports: []string{"mrp      mrp/chnk2", "nextflow nf/c", "_args (-mrp +nextflow)", "1.0"},
		},
		{
			name: "extra_phase_on_one_side",
			nf: []abiInvocation{
				inv("S", phaseMain, `{"i":0}`, "nf/a"), inv("S", phaseMain, `{"i":1}`, "nf/b"),
				inv("S", phaseMain, `{"i":1}`, "nf/c"), inv("S", phaseJoin, `{}`, "nf/j"),
			},
			reports: []string{"S join: 0 mrp vs 1 nextflow"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(diffABI(mrp, tc.nf), "\n")

			if len(tc.reports) == 0 && got != "" {
				t.Fatalf("unexpected report:\n%s", got)
			}

			for _, want := range tc.reports {
				if !strings.Contains(got, want) {
					t.Errorf("report lacks %q:\n%s", want, got)
				}
			}
		})
	}
}
