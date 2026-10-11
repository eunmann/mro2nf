package e2e

// The per-stage ABI differential (#348): every stage phase that ran under mrp
// must have a twin under mro2nf whose _args and _outs are identical after
// normalization, and vice versa. Invocations are matched as a multiset per
// (stage, phase) rather than through the task graph, because orchestration
// between stages is not a contract. This file carries no build tag so its unit
// tests run under `make test`; mrpdiff_test.go drives it against live runs.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/eunmann/mro2nf/test/divergence"
	"github.com/google/go-cmp/cmp"
)

const (
	phaseSplit = "split"
	phaseMain  = "main"
	phaseJoin  = "join"

	argsFile = "_args"
	outsFile = "_outs"

	// mreStageFile is the stage name mre records beside the adapter metadata
	// (internal/shim stageNameFile).
	mreStageFile = "_mre_stage"

	// absentMeta stands in for a metadata file a phase did not write (a split
	// phase has no _outs on either runtime).
	absentMeta = "<absent>"
)

// errABIMeta reports metadata the differential cannot attribute or decode.
var errABIMeta = errors.New("stage metadata")

// abiInvocation is one stage phase as the stage saw it: its normalized _args
// and _outs in canonical JSON. Origin locates it for reports only.
type abiInvocation struct {
	Stage  string
	Phase  string
	Args   string
	Outs   string
	Origin string
}

func (inv abiInvocation) group() string { return inv.Stage + " " + inv.Phase }

func (inv abiInvocation) value() string { return inv.Args + "\x00" + inv.Outs }

// abiRules are the allowlist entries the ABI comparison of one fixture
// applies.
type abiRules struct {
	keys     []divergence.ABIMatcher
	sortKeys bool
}

// abiRulesFor collects the entries of allow that apply to fixture.
func abiRulesFor(allow divergence.Allowlist, fixture string) abiRules {
	r := abiRules{sortKeys: allow.Excuses(divergence.RuleABISortKeys)}

	for _, e := range allow.ABI(fixture) {
		r.keys = append(r.keys, *e.ABI)
	}

	return r
}

// abiNormalizer rewrites one metadata file's JSON into a form that is equal
// across runtimes exactly when the stage saw equal values.
type abiNormalizer struct {
	// ownFiles are the spellings of the invocation's own files dir. Each runtime
	// assigns that dir itself, so a path under it is host-specific by
	// construction; the rest of the path is what the stage chose.
	ownFiles   []string
	ignoreKeys []string
	ceilKeys   []string
	sortKeys   bool
}

func newABINormalizer(ownFiles []string, file string, rules abiRules) abiNormalizer {
	n := abiNormalizer{ownFiles: ownFiles, sortKeys: rules.sortKeys}

	for _, m := range rules.keys {
		if m.File != file {
			continue
		}

		switch m.Rule {
		case divergence.ABIIgnoreKey:
			n.ignoreKeys = append(n.ignoreKeys, m.Key)
		case divergence.ABICeilNumber:
			n.ceilKeys = append(n.ceilKeys, m.Key)
		}
	}

	return n
}

// topLevelKeys applies the key-scoped rules to a decoded document's top-level
// object; nested keys of the same name are left alone.
func (n abiNormalizer) topLevelKeys(v any) (any, error) {
	obj, ok := v.(jsonObject)
	if !ok {
		return v, nil
	}

	obj = slices.DeleteFunc(obj, func(m jsonMember) bool { return slices.Contains(n.ignoreKeys, m.Key) })

	for i, m := range obj {
		if !slices.Contains(n.ceilKeys, m.Key) {
			continue
		}

		num, isNum := m.Value.(json.Number)
		if !isNum {
			return nil, fmt.Errorf("%w: %s is %v, not a number", errABIMeta, m.Key, m.Value)
		}

		f, err := num.Float64()
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", errABIMeta, m.Key, err)
		}

		if f != math.Trunc(f) {
			obj[i].Value = json.Number(strconv.FormatFloat(math.Ceil(f), 'f', -1, 64))
		}
	}

	return obj, nil
}

// normalize decodes raw with numbers kept as their source token (an int-vs-float
// change is exactly what the differential must catch) and objects in member
// order, rewrites path strings, and re-encodes it, sorting object keys only
// under the allowlist's key-order rule.
func (n abiNormalizer) normalize(raw []byte) (string, error) {
	v, err := decodeOrderedJSON(raw)
	if err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}

	v, err = n.topLevelKeys(v)
	if err != nil {
		return "", err
	}

	v, err = mapJSONStrings(v, n.path)
	if err != nil {
		return "", err
	}

	if n.sortKeys {
		v = sortJSONKeys(v)
	}

	return encodeOrderedJSON(v)
}

// path replaces an absolute path that exists with its content identity plus the
// basename the stage sees, and an absent one under the invocation's own files
// dir with its files-relative remainder. Every other string is kept verbatim.
func (n abiNormalizer) path(s string) (string, error) {
	if !filepath.IsAbs(s) {
		return s, nil
	}

	info, err := os.Stat(s)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return n.ownFilesRel(s), nil
	case err != nil:
		return "", fmt.Errorf("stat %s: %w", s, err)
	case info.IsDir():
		sum, err := hashDirTree(s)
		if err != nil {
			return "", err
		}

		return "dir:" + sum + ":" + filepath.Base(s), nil
	default:
		sum, err := hashFileContent(s)
		if err != nil {
			return "", err
		}

		return "file:" + sum + ":" + filepath.Base(s), nil
	}
}

func (n abiNormalizer) ownFilesRel(s string) string {
	rest, own := n.ownFilesRest(s)

	switch {
	case !own:
		return s
	case rest == "":
		return "<files>"
	default:
		return "<files>/" + rest
	}
}

// ownFilesRest reports whether s lies in the invocation's own files dir, and
// its path below that dir.
func (n abiNormalizer) ownFilesRest(s string) (string, bool) {
	for _, own := range n.ownFiles {
		if s == own {
			return "", true
		}

		if rest, ok := strings.CutPrefix(s, own+"/"); ok {
			return rest, true
		}
	}

	return "", false
}

func hashFileContent(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}

	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:]), nil
}

// hashDirTree hashes a directory's relative entry names (empty subdirs
// included) and file contents in lexical walk order, through a symlinked root.
func hashDirTree(dir string) (string, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", dir, err)
	}

	h := sha256.New()

	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}

		// hash.Hash.Write never returns an error.
		if d.IsDir() {
			_, _ = fmt.Fprintf(h, "d %s\n", rel)

			return nil
		}

		sum, err := hashFileContent(p)
		if err != nil {
			return err
		}

		_, _ = fmt.Fprintf(h, "f %s %s\n", rel, sum)

		return nil
	})
	if err != nil {
		return "", fmt.Errorf("hash tree %s: %w", dir, err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// readABIInvocation reads and normalizes one phase metadata dir. _args is
// required (both runtimes write it before the stage runs); a missing _outs is
// recorded as absent so a phase that writes one on only one side still differs.
func readABIInvocation(stage, phase, meta string, ownFiles []string, rules abiRules) (abiInvocation, error) {
	inv := abiInvocation{Stage: stage, Phase: phase, Origin: meta}

	for _, f := range []struct {
		name string
		dst  *string
	}{{argsFile, &inv.Args}, {outsFile, &inv.Outs}} {
		raw, err := os.ReadFile(filepath.Join(meta, f.name))
		if f.name == outsFile && errors.Is(err, fs.ErrNotExist) {
			*f.dst = absentMeta

			continue
		}

		if err != nil {
			return abiInvocation{}, fmt.Errorf("read %s: %w", f.name, err)
		}

		norm, err := newABINormalizer(ownFiles, f.name, rules).normalize(raw)
		if err != nil {
			return abiInvocation{}, fmt.Errorf("normalize %s/%s: %w", meta, f.name, err)
		}

		*f.dst = norm
	}

	return inv, nil
}

var (
	// mrpPhaseDir matches mrp's phase dirs, which carry a -u<uuid> suffix behind a
	// plain-named symlink on current mrp and none on older pipestances.
	mrpPhaseDir = regexp.MustCompile(`^(split|join|chnk[0-9]+)(-u[0-9a-f]+)?$`)
	// mrpInvocationCall reads the stage from a fork's _invocation, which mrp
	// writes as `call <STAGE>(` or `call <STAGE> as <ALIAS>(`.
	mrpInvocationCall = regexp.MustCompile(`(?m)^call\s+(\w+)(?:\s+as\s+\w+)?\s*\(`)
)

// collectMrpInvocations walks an mrp pipestance for every phase that ran. A
// phase ran when it has a _jobinfo: mrp also lays out split/ and join/ dirs for
// a non-split stage, holding bookkeeping _args/_outs no stage code saw.
func collectMrpInvocations(ps string, rules abiRules) ([]abiInvocation, error) {
	var invs []abiInvocation

	err := filepath.WalkDir(ps, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		m := mrpPhaseDir.FindStringSubmatch(d.Name())
		if !d.IsDir() || m == nil || !regularFileExists(filepath.Join(p, "_jobinfo")) {
			return nil
		}

		fork := filepath.Dir(p)

		stage, err := mrpStageName(fork)
		if err != nil {
			return err
		}

		phase := m[1]
		if strings.HasPrefix(phase, "chnk") {
			phase = phaseMain
		}

		own := []string{filepath.Join(p, "files"), filepath.Join(fork, m[1], "files")}

		inv, err := readABIInvocation(stage, phase, p, own, rules)
		if err != nil {
			return err
		}

		invs = append(invs, inv)

		return filepath.SkipDir
	})
	if err != nil {
		return nil, fmt.Errorf("collect mrp invocations: %w", err)
	}

	return invs, nil
}

func mrpStageName(fork string) (string, error) {
	src, err := os.ReadFile(filepath.Join(fork, "_invocation"))
	if err != nil {
		return "", fmt.Errorf("read stage invocation: %w", err)
	}

	m := mrpInvocationCall.FindSubmatch(src)
	if m == nil {
		return "", fmt.Errorf("%w: no call in %s/_invocation", errABIMeta, fork)
	}

	return string(m[1]), nil
}

// collectNextflowInvocations reads every phase mre ran in a Nextflow work dir
// (<work>/<xx>/<hash>/<phase>/). Only tasks that exited 0 count: a retried
// attempt leaves its failed task dir behind, and that run's outputs never
// reached the pipeline.
func collectNextflowInvocations(proj string, rules abiRules) ([]abiInvocation, error) {
	stamps, err := filepath.Glob(filepath.Join(proj, "work", "*", "*", "*", mreStageFile))
	if err != nil {
		return nil, fmt.Errorf("glob stage stamps: %w", err)
	}

	invs := make([]abiInvocation, 0, len(stamps))

	for _, stamp := range stamps {
		meta := filepath.Dir(stamp)

		exit, err := os.ReadFile(filepath.Join(filepath.Dir(meta), ".exitcode"))
		if err != nil || strings.TrimSpace(string(exit)) != "0" {
			continue
		}

		phase := filepath.Base(meta)
		if !slices.Contains([]string{phaseSplit, phaseMain, phaseJoin}, phase) {
			return nil, fmt.Errorf("%w: %s is not a phase dir", errABIMeta, meta)
		}

		stage, err := os.ReadFile(stamp)
		if err != nil {
			return nil, fmt.Errorf("read stage name: %w", err)
		}

		if len(stage) == 0 {
			return nil, fmt.Errorf("%w: empty stage name in %s", errABIMeta, stamp)
		}

		inv, err := readABIInvocation(string(stage), phase, meta, []string{filepath.Join(meta, "files")}, rules)
		if err != nil {
			return nil, err
		}

		invs = append(invs, inv)
	}

	return invs, nil
}

func regularFileExists(path string) bool {
	info, err := os.Stat(path)

	return err == nil && info.Mode().IsRegular()
}

// diffStageABI compares every stage phase of the mrp pipestance ps with the
// Nextflow project proj, applying the allowlist entries that cover fixture. It
// also reports each fixture-scoped entry whose removal would change nothing, so
// an entry cannot outlive the divergence it excuses.
func diffStageABI(allow divergence.Allowlist, fixture, ps, proj string) ([]string, error) {
	reports, err := diffStageABIUnder(abiRulesFor(allow, fixture), ps, proj)
	if err != nil {
		return nil, err
	}

	for _, e := range allow.ABI(fixture) {
		without, err := diffStageABIUnder(abiRulesFor(allow.Without(e.ID), fixture), ps, proj)
		if err != nil {
			return nil, err
		}

		if slices.Equal(without, reports) {
			reports = append(reports, fmt.Sprintf("allowlist entry %s excuses nothing in %s; delete it or narrow its fixtures", e.ID, fixture))
		}
	}

	return reports, nil
}

func diffStageABIUnder(rules abiRules, ps, proj string) ([]string, error) {
	mrp, err := collectMrpInvocations(ps, rules)
	if err != nil {
		return nil, err
	}

	nf, err := collectNextflowInvocations(proj, rules)
	if err != nil {
		return nil, err
	}

	return diffABI(mrp, nf), nil
}

// diffABI reports each (stage, phase) group whose invocation multisets differ.
func diffABI(mrp, nf []abiInvocation) []string {
	groups := map[string]*[2][]abiInvocation{}

	for side, invs := range [][]abiInvocation{mrp, nf} {
		for _, inv := range invs {
			g := groups[inv.group()]
			if g == nil {
				g = &[2][]abiInvocation{}
				groups[inv.group()] = g
			}

			g[side] = append(g[side], inv)
		}
	}

	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	var reports []string

	for _, k := range keys {
		if r := diffABIGroup(k, groups[k][0], groups[k][1]); r != "" {
			reports = append(reports, r)
		}
	}

	return reports
}

// diffABIGroup lists the invocations without a twin on the other side, then
// diffs the first unmatched pair (in value order, which lines chunks up).
func diffABIGroup(key string, mrp, nf []abiInvocation) string {
	onlyMrp, onlyNF := unmatchedInvocations(mrp, nf), unmatchedInvocations(nf, mrp)
	if len(onlyMrp) == 0 && len(onlyNF) == 0 {
		return ""
	}

	var b strings.Builder

	fmt.Fprintf(&b, "%s: %d mrp vs %d nextflow invocations; without a twin:\n", key, len(mrp), len(nf))

	for _, inv := range onlyMrp {
		fmt.Fprintf(&b, "  mrp      %s\n", inv.Origin)
	}

	for _, inv := range onlyNF {
		fmt.Fprintf(&b, "  nextflow %s\n", inv.Origin)
	}

	if len(onlyMrp) > 0 && len(onlyNF) > 0 {
		for _, f := range []struct{ name, mrp, nf string }{
			{argsFile, onlyMrp[0].Args, onlyNF[0].Args},
			{outsFile, onlyMrp[0].Outs, onlyNF[0].Outs},
		} {
			if d := cmp.Diff(indentJSON(f.mrp), indentJSON(f.nf)); d != "" {
				fmt.Fprintf(&b, "  %s (-mrp +nextflow):\n%s", f.name, d)
			}
		}
	}

	return b.String()
}

// unmatchedInvocations returns the members of a with no twin left in b, in
// value order.
func unmatchedInvocations(a, b []abiInvocation) []abiInvocation {
	left := make(map[string]int, len(b))
	for _, inv := range b {
		left[inv.value()]++
	}

	sorted := slices.Clone(a)
	slices.SortFunc(sorted, func(x, y abiInvocation) int {
		return strings.Compare(x.value()+x.Origin, y.value()+y.Origin)
	})

	var out []abiInvocation

	for _, inv := range sorted {
		if left[inv.value()] > 0 {
			left[inv.value()]--

			continue
		}

		out = append(out, inv)
	}

	return out
}

// indentJSON pretty-prints a canonical value so a diff shows the differing
// fields line by line; the absent marker is not JSON and passes through.
func indentJSON(s string) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(s), "", "  "); err != nil {
		return s
	}

	return buf.String()
}
