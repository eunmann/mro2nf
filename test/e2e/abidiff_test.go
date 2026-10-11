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

	// ruleIgnoreKey drops the top-level key the divergence names from the file
	// on both sides.
	ruleIgnoreKey = "ignore-key"

	// ruleCeilKey replaces a fractional value of the numeric top-level key the
	// divergence names with its ceiling, so it equals the whole number it rounds
	// up to; integral tokens are kept, so 1 and 1.0 still differ.
	ruleCeilKey = "ceil-number"
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

// divergence is one known difference between mrp and mro2nf in the stage ABI.
// Each names the open issue that tracks it; fixing the issue means deleting the
// entry, after which the differential checks the field again.
type divergence struct {
	Issue   int    `json:"issue"`
	Summary string `json:"summary"`
	File    string `json:"file"`
	Rule    string `json:"rule"`
	Key     string `json:"key,omitempty"`
}

// loadDivergences reads and validates the checked-in divergence list.
func loadDivergences(path string) ([]divergence, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read divergences: %w", err)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var doc struct {
		Divergences []divergence `json:"divergences"`
	}
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse divergences: %w", err)
	}

	for i, d := range doc.Divergences {
		if err := d.validate(); err != nil {
			return nil, fmt.Errorf("divergence %d: %w", i, err)
		}
	}

	return doc.Divergences, nil
}

func (d divergence) validate() error {
	switch {
	case d.Issue <= 0:
		return fmt.Errorf("%w: no tracking issue", errABIMeta)
	case d.Summary == "":
		return fmt.Errorf("%w: #%d has no summary", errABIMeta, d.Issue)
	case d.File != argsFile && d.File != outsFile:
		return fmt.Errorf("%w: #%d file %q is not %s or %s", errABIMeta, d.Issue, d.File, argsFile, outsFile)
	case d.Rule != ruleIgnoreKey && d.Rule != ruleCeilKey:
		return fmt.Errorf("%w: #%d unknown rule %q", errABIMeta, d.Issue, d.Rule)
	case d.Key == "":
		return fmt.Errorf("%w: #%d rule %q needs a key", errABIMeta, d.Issue, d.Rule)
	default:
		return nil
	}
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
}

func newABINormalizer(ownFiles []string, file string, divs []divergence) abiNormalizer {
	n := abiNormalizer{ownFiles: ownFiles}

	for _, d := range divs {
		if d.File != file {
			continue
		}

		switch d.Rule {
		case ruleIgnoreKey:
			n.ignoreKeys = append(n.ignoreKeys, d.Key)
		case ruleCeilKey:
			n.ceilKeys = append(n.ceilKeys, d.Key)
		}
	}

	return n
}

// topLevelKeys applies the key-scoped rules to a decoded document's top-level
// object; nested keys of the same name are left alone.
func (n abiNormalizer) topLevelKeys(v any) (any, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return v, nil
	}

	for _, k := range n.ignoreKeys {
		delete(obj, k)
	}

	for _, k := range n.ceilKeys {
		raw, present := obj[k]
		if !present {
			continue
		}

		num, isNum := raw.(json.Number)
		if !isNum {
			return nil, fmt.Errorf("%w: %s is %v, not a number", errABIMeta, k, raw)
		}

		f, err := num.Float64()
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", errABIMeta, k, err)
		}

		if f != math.Trunc(f) {
			obj[k] = json.Number(strconv.FormatFloat(math.Ceil(f), 'f', -1, 64))
		}
	}

	return obj, nil
}

// normalize decodes raw with numbers kept as their source token (an int-vs-float
// change is exactly what the differential must catch), rewrites path strings, and
// re-encodes with sorted keys.
func (n abiNormalizer) normalize(raw []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}

	if dec.More() {
		return "", fmt.Errorf("%w: trailing data after JSON value", errABIMeta)
	}

	v, err := n.topLevelKeys(v)
	if err != nil {
		return "", err
	}

	v, err = n.value(v)
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	if err := enc.Encode(v); err != nil {
		return "", fmt.Errorf("encode: %w", err)
	}

	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// value normalizes one JSON value. The recursion is bounded by the document's
// nesting depth, which encoding/json caps at 10000.
func (n abiNormalizer) value(v any) (any, error) {
	switch x := v.(type) {
	case string:
		return n.path(x)
	case []any:
		for i := range x {
			nv, err := n.value(x[i])
			if err != nil {
				return nil, err
			}

			x[i] = nv
		}

		return x, nil
	case map[string]any:
		for k := range x {
			nv, err := n.value(x[k])
			if err != nil {
				return nil, err
			}

			x[k] = nv
		}

		return x, nil
	default:
		return v, nil
	}
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
func readABIInvocation(stage, phase, meta string, ownFiles []string, divs []divergence) (abiInvocation, error) {
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

		norm, err := newABINormalizer(ownFiles, f.name, divs).normalize(raw)
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
func collectMrpInvocations(ps string, divs []divergence) ([]abiInvocation, error) {
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

		inv, err := readABIInvocation(stage, phase, p, own, divs)
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
func collectNextflowInvocations(proj string, divs []divergence) ([]abiInvocation, error) {
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

		inv, err := readABIInvocation(string(stage), phase, meta, []string{filepath.Join(meta, "files")}, divs)
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
