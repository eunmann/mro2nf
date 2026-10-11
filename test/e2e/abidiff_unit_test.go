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

	"github.com/eunmann/mro2nf/test/divergence"
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

// TestABINormalizeCanonicalJSON checks the compact form keeps key order and
// every number's source token, so key order, int-vs-float and big-int
// precision stay visible.
func TestABINormalizeCanonicalJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "key_order_kept", in: `{"b": 1, "a": {"y": true, "x": null}}`, want: `{"b":1,"a":{"y":true,"x":null}}`},
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
	for _, in := range []string{`{"x": `, `{"x": 1} {"y": 2}`, `{"x": 1}]`, `[1, 2`} {
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

	link := filepath.Join(dir, "staged", "link.txt")
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
		name string
		in   string
		want string
	}{
		{name: "file", in: file, want: "file:" + sha256Hex("ACGT") + ":reads.txt"},
		{name: "symlink_keeps_link_basename", in: link, want: "file:" + sha256Hex("ACGT") + ":link.txt"},
		{name: "own_file", in: ownFile, want: "file:" + sha256Hex("BAM") + ":out.bam"},
		{name: "dir", in: filepath.Join(dir, "tree"), want: "dir:" + treeSum + ":tree"},
		{name: "absent_under_own_files", in: filepath.Join(own, "sub", "gone.bam"), want: "<files>/sub/gone.bam"},
		{name: "absent_elsewhere_verbatim", in: filepath.Join(dir, "gone.txt"), want: filepath.Join(dir, "gone.txt")},
		{name: "own_prefix_needs_separator", in: own + "x/y", want: own + "x/y"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := abiNormalizer{ownFiles: []string{own}}.path(tc.in)
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

// TestABINormalizeKeyRules checks ignore-key and ceil-number touch only the
// named top-level key of the named file.
func TestABINormalizeKeyRules(t *testing.T) {
	divs := abiRules{keys: []divergence.ABIMatcher{
		{Fixtures: []string{"f"}, File: argsFile, Rule: divergence.ABIIgnoreKey, Key: "__special"},
		{Fixtures: []string{"f"}, File: argsFile, Rule: divergence.ABICeilNumber, Key: "__threads"},
	}}

	const doc = `{"__special":"gpu","__threads":0.5,"__mem_gb":0.5,"x":{"__special":"gpu","__threads":0.5}}`

	cases := []struct {
		file string
		want string
	}{
		{argsFile, `{"__threads":1,"__mem_gb":0.5,"x":{"__special":"gpu","__threads":0.5}}`},
		{outsFile, doc},
	}

	for _, tc := range cases {
		got, err := newABINormalizer(nil, tc.file, divs).normalize([]byte(doc))
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}

		if got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.file, got, tc.want)
		}
	}

	pairs := []struct {
		mrp, nf string
		equal   bool
	}{
		{"0.5", "1", true},
		{"0.5", "2", false},
		{"0.5", "1.0", false},
		{"1", "1.0", false},
		{"1.0", "1", false},
		{"2", "2", true},
	}

	for _, p := range pairs {
		mrp, err := newABINormalizer(nil, argsFile, divs).normalize([]byte(`{"__threads":` + p.mrp + `}`))
		if err != nil {
			t.Fatal(err)
		}

		nf, err := newABINormalizer(nil, argsFile, divs).normalize([]byte(`{"__threads":` + p.nf + `}`))
		if err != nil {
			t.Fatal(err)
		}

		if equal := mrp == nf; equal != p.equal {
			t.Errorf("__threads %s vs %s: equal=%v, want %v", p.mrp, p.nf, equal, p.equal)
		}
	}

	if _, err := newABINormalizer(nil, argsFile, divs).normalize([]byte(`{"__threads":"1"}`)); !errors.Is(err, errABIMeta) {
		t.Errorf("ceil-number on a string: err=%v, want errABIMeta", err)
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
	invs, err := collectMrpInvocations(filepath.Join("..", "..", "testdata", "split_test", "expected"), abiRules{})
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

	invs, err := collectNextflowInvocations(proj, abiRules{})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	want := []abiGroupCount{{"SUM join", 1}, {"SUM main", 1}}
	if diff := cmp.Diff(want, groupCounts(invs)); diff != "" {
		t.Errorf("invocation groups (-want +got):\n%s", diff)
	}

	task("anon", "0", phaseMain, "")

	if _, err := collectNextflowInvocations(proj, abiRules{}); !errors.Is(err, errABIMeta) {
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

// abiPair is one CONS main invocation as mrp leaves it in a pipestance and as
// mre leaves it in a Nextflow project.
type abiPair struct {
	ps, proj string
}

func writeABIPair(t *testing.T, mrpArgs, nfArgs string) abiPair {
	t.Helper()

	p := abiPair{ps: t.TempDir(), proj: t.TempDir()}
	fork := filepath.Join(p.ps, "P", "CONS", "fork0")
	chunk := filepath.Join(fork, "chnk0-u1c28cb18e6")
	task := filepath.Join(p.proj, "work", "ab", "cdef1234")

	writeABITestFile(t, filepath.Join(fork, "_invocation"), "call CONS(\n    s = PROD.s,\n)\n")
	writeABITestFile(t, filepath.Join(chunk, "_jobinfo"), "{}")
	writeABITestFile(t, filepath.Join(chunk, argsFile), mrpArgs)
	writeABITestFile(t, filepath.Join(chunk, outsFile), `{"keys": ["b", "a"]}`)
	writeABITestFile(t, filepath.Join(task, ".exitcode"), "0")
	writeABITestFile(t, filepath.Join(task, phaseMain, mreStageFile), "CONS")
	writeABITestFile(t, filepath.Join(task, phaseMain, argsFile), nfArgs)
	writeABITestFile(t, filepath.Join(task, phaseMain, outsFile), `{"keys":["b","a"]}`)

	return p
}

func (p abiPair) diff(t *testing.T, allow divergence.Allowlist, fixture string) string {
	t.Helper()

	reports, err := diffStageABI(allow, fixture, p.ps, p.proj)
	if err != nil {
		t.Fatal(err)
	}

	return strings.Join(reports, "\n")
}

// TestABIComparesKeyOrder compares the consumer _args a real mrp 4a558e7 run
// gave a stage whose struct input's producer wrote {"b": 1, "a": 2}, which mrp
// passes in that order, with _args holding the same values: under the
// checked-in allowlist the comparison fails on sorted keys, on any fixture, and
// passes on the producer's order (#366).
func TestABIComparesKeyOrder(t *testing.T) {
	const mrpArgs = `{"__mem_gb": 1, "__threads": 1, "__vmem_gb": 4, "m": {"y": 2, "z": 1}, "s": {"b": 1, "a": 2}}`

	sorted := writeABIPair(t, mrpArgs, `{"__mem_gb":1,"__threads":1,"__vmem_gb":4,"m":{"y":2,"z":1},"s":{"a":2,"b":1}}`)
	agreeing := writeABIPair(t, mrpArgs, `{"__mem_gb":1,"__threads":1,"__vmem_gb":4,"m":{"y":2,"z":1},"s":{"b":1,"a":2}}`)
	allow := mustAllowlist(t)

	cases := []struct {
		name    string
		pair    abiPair
		fixture string
		want    string
	}{
		{"sorted", sorted, "nested_struct", "CONS main"},
		{"sorted_other_fixture", sorted, "file_min", "CONS main"},
		{"producer_order", agreeing, "nested_struct", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.pair.diff(t, allow, tc.fixture)
			if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
				t.Errorf("reports %q, want %q", got, tc.want)
			}
		})
	}
}

// TestABIKeyEntryScopedAndStale checks a key-matcher entry excuses its key in
// the fixtures it names and nowhere else, that removing it fails the
// comparison, and that the comparison reports it once it excuses nothing.
func TestABIKeyEntryScopedAndStale(t *testing.T) {
	allow, err := divergence.Parse([]byte(`{"divergences": [{"id": "special-dropped", "kind": "abi",
		"feature": "f", "summary": "s", "justification": "j", "issue": 1, "evidence": ["TestMrpDiff"],
		"abi": {"fixtures": ["fx"], "file": "_args", "rule": "ignore-key", "key": "__special"}}]}`))
	if err != nil {
		t.Fatal(err)
	}

	differ := writeABIPair(t, `{"__special": "gpu", "n": 1}`, `{"n": 1}`)
	same := writeABIPair(t, `{"n": 1}`, `{"n": 1}`)

	cases := []struct {
		name    string
		pair    abiPair
		fixture string
		allow   divergence.Allowlist
		want    string
	}{
		{"excused", differ, "fx", allow, ""},
		{"other_fixture", differ, "other", allow, "CONS main"},
		{"entry_removed", differ, "fx", allow.Without("special-dropped"), "CONS main"},
		{"stale", same, "fx", allow, "special-dropped excuses nothing in fx"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.pair.diff(t, tc.allow, tc.fixture)
			if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
				t.Errorf("reports %q, want %q", got, tc.want)
			}
		})
	}
}
