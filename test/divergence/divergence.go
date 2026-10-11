// Package divergence is the checked-in allowlist of every known divergence
// between mrp and a transpiled pipeline (test/e2e/divergences.json). The
// differential excuses a difference only when an entry here matches it, and
// the divergence section of docs/FEATURE_COVERAGE.md is rendered from it.
package divergence

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
)

// The entry kinds, each naming the comparison its matcher applies to.
const (
	// KindABI excuses a difference in a stage invocation's _args, _outs or
	// _jobinfo allocation.
	KindABI = "abi"
	// KindOutcome excuses a difference in how a run ends.
	KindOutcome = "outcome"
	// KindPublishedTree excuses a difference in the published outputs: the
	// results/ tree and the pipeline outs JSON published with it.
	KindPublishedTree = "published-tree"
	// KindMechanism records a difference in how mro2nf reaches mrp's outputs
	// that leaves them byte-identical, so it has no matcher.
	KindMechanism = "mechanism"
)

// The files and rules of an ABI matcher.
const (
	FileArgs    = "_args"
	FileOuts    = "_outs"
	FileJobInfo = "_jobinfo"

	// ABIIgnoreKey drops the matcher's top-level key from the file on both
	// sides.
	ABIIgnoreKey = "ignore-key"
	// ABICeilNumber replaces a fractional value of the matcher's numeric
	// top-level key with its ceiling; integral tokens are kept, so 1 and 1.0
	// still differ.
	ABICeilNumber = "ceil-number"
)

// The comparator rules: normalizations built into the differential's
// comparators that excuse a real difference. A comparator applies one only
// while an entry matches it.
const (
	// RuleOutsRelativePaths strips mrp's outs-dir prefix from the paths in its
	// outs JSON.
	RuleOutsRelativePaths = "outs-json-strip-outs-prefix"
	// RuleResultsMetadata drops mro2nf's own metadata files from the root of
	// the published results/ tree.
	RuleResultsMetadata = "drop-results-metadata"
	// RuleMrpPublishedSymlinks compares a symlink mrp published by its target's
	// content, and the target path mrp's outs JSON names for it as the link's
	// path.
	RuleMrpPublishedSymlinks = "follow-mrp-published-symlinks"
	// RuleUnpublishedPaths reads the path of a file left unpublished as one
	// placeholder on both sides: in mrp's outs JSON a path inside the
	// pipestance but outside its outs dir, in pipeline_outs.json a path inside
	// a Nextflow task work dir.
	RuleUnpublishedPaths = "placeholder-unpublished-paths"
	// RuleRetriedAttempts lets a stage task fail more than once before its
	// last failure counts.
	RuleRetriedAttempts = "collapse-retried-attempts"
	// RuleFailureStageAndKind compares a failed run by the stages it blamed
	// and the failure kind of each, not by its messages or where they were
	// recorded.
	RuleFailureStageAndKind = "compare-failure-stage-and-kind"
)

// ComparatorRules lists every comparator rule with the kind of the entry that
// must match it.
func ComparatorRules() map[string]string {
	return map[string]string{
		RuleOutsRelativePaths:    KindPublishedTree,
		RuleResultsMetadata:      KindPublishedTree,
		RuleMrpPublishedSymlinks: KindPublishedTree,
		RuleUnpublishedPaths:     KindPublishedTree,
		RuleRetriedAttempts:      KindOutcome,
		RuleFailureStageAndKind:  KindOutcome,
	}
}

// Failure kinds an outcome matcher records.
const (
	FailAssert = "assert"
	FailError  = "error"
)

// Entry is one divergence. An entry with no issue is intentional and
// permanent; one with an issue lasts until the issue is fixed. A fixture-scoped
// matcher (an ABI matcher, or the comparator matcher of an issue-tracked entry)
// fails the comparison of each fixture it names once it excuses nothing
// there; an outcome matcher fails its case once mro2nf matches mrp.
type Entry struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	Feature       string `json:"feature"`
	Summary       string `json:"summary"`
	Justification string `json:"justification"`
	Issue         int    `json:"issue,omitempty"`
	// Evidence names the tests that apply the matcher, or for a mechanism
	// entry the tests that prove the outputs identical.
	Evidence   []string           `json:"evidence"`
	ABI        *ABIMatcher        `json:"abi,omitempty"`
	Outcome    *OutcomeMatcher    `json:"outcome,omitempty"`
	Comparator *ComparatorMatcher `json:"comparator,omitempty"`
}

// ABIMatcher excuses one top-level key of one metadata file, for every
// differential leg of the named fixtures.
type ABIMatcher struct {
	Fixtures []string `json:"fixtures"`
	File     string   `json:"file"`
	Rule     string   `json:"rule"`
	Key      string   `json:"key"`
}

// OutcomeMatcher is the outcome mro2nf gives instead of mrp's on the fixture
// whose outcome case names the entry.
type OutcomeMatcher struct {
	Failed   bool      `json:"failed"`
	Failures []Failure `json:"failures,omitempty"`
}

// Failure is one stage a failed run blamed; an empty Stage is a failure no
// stage task owns.
type Failure struct {
	Kind  string `json:"kind"`
	Stage string `json:"stage"`
}

// ComparatorMatcher names the comparator rule the entry lets a comparator
// apply. A permanent entry applies the rule on every fixture. An issue-tracked
// entry applies it only on Fixtures, and the comparison of each of them fails
// once the rule excuses nothing there, so the entry cannot outlive its bug.
type ComparatorMatcher struct {
	Rule     string   `json:"rule"`
	Fixtures []string `json:"fixtures,omitempty"`
}

// covers reports whether the matcher applies on fixture.
func (m ComparatorMatcher) covers(fixture string) bool {
	return len(m.Fixtures) == 0 || slices.Contains(m.Fixtures, fixture)
}

// Allowlist is a validated list of divergences.
type Allowlist struct {
	entries []Entry
}

// ErrInvalid reports an allowlist entry that is malformed or ambiguous.
var ErrInvalid = errors.New("invalid divergence")

// Load reads and validates the allowlist at path.
func Load(path string) (Allowlist, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Allowlist{}, fmt.Errorf("read allowlist: %w", err)
	}

	return Parse(data)
}

// Parse decodes and validates an allowlist document.
func Parse(data []byte) (Allowlist, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var doc struct {
		Divergences []Entry `json:"divergences"`
	}
	if err := dec.Decode(&doc); err != nil {
		return Allowlist{}, fmt.Errorf("parse allowlist: %w", err)
	}

	if dec.More() {
		return Allowlist{}, fmt.Errorf("parse allowlist: %w: trailing data", ErrInvalid)
	}

	a := Allowlist{entries: doc.Divergences}
	if err := a.validate(); err != nil {
		return Allowlist{}, err
	}

	return a, nil
}

// Entries returns every entry in file order.
func (a Allowlist) Entries() []Entry { return slices.Clone(a.entries) }

// Entry returns the entry with the given id.
func (a Allowlist) Entry(id string) (Entry, bool) {
	i := slices.IndexFunc(a.entries, func(e Entry) bool { return e.ID == id })
	if i < 0 {
		return Entry{}, false
	}

	return a.entries[i], true
}

// Without returns the allowlist less the entry with the given id, so a test
// can show the differential fails once the entry is gone.
func (a Allowlist) Without(id string) Allowlist {
	return Allowlist{entries: slices.DeleteFunc(slices.Clone(a.entries), func(e Entry) bool { return e.ID == id })}
}

// RulesEverywhere returns the allowlist a program no fixture names, such as a
// generated one, is compared under: every comparator rule, an issue-tracked
// one unscoped so that it applies there and is never found excusing nothing,
// and no ABI or outcome matcher, since each applies only to the fixtures or
// the outcome case that name it.
func (a Allowlist) RulesEverywhere() Allowlist {
	out := make([]Entry, 0, len(a.entries))

	for _, e := range a.entries {
		switch {
		case e.ABI != nil || e.Outcome != nil:
			continue
		case e.Comparator != nil:
			e.Comparator = &ComparatorMatcher{Rule: e.Comparator.Rule}
		}

		out = append(out, e)
	}

	return Allowlist{entries: out}
}

// Excuses reports whether an entry lets the comparison of fixture apply the
// comparator rule.
func (a Allowlist) Excuses(rule, fixture string) bool {
	return slices.ContainsFunc(a.entries, func(e Entry) bool {
		return e.Comparator != nil && e.Comparator.Rule == rule && e.Comparator.covers(fixture)
	})
}

// ScopedRules returns the issue-tracked comparator-rule entries of kind
// that name fixture: those its comparison must find still excusing something.
func (a Allowlist) ScopedRules(kind, fixture string) []Entry {
	var out []Entry

	for _, e := range a.entries {
		if e.Kind == kind && e.Comparator != nil && slices.Contains(e.Comparator.Fixtures, fixture) {
			out = append(out, e)
		}
	}

	return out
}

// ABI returns the ABI-matcher entries scoped to fixture.
func (a Allowlist) ABI(fixture string) []Entry {
	var out []Entry

	for _, e := range a.entries {
		if e.ABI != nil && slices.Contains(e.ABI.Fixtures, fixture) {
			out = append(out, e)
		}
	}

	return out
}

// Outcome returns the outcome-matcher entry with the given id.
func (a Allowlist) Outcome(id string) (Entry, error) {
	e, ok := a.Entry(id)

	switch {
	case !ok:
		return Entry{}, fmt.Errorf("%w: no entry %q", ErrInvalid, id)
	case e.Outcome == nil:
		return Entry{}, fmt.Errorf("%w: entry %q has no outcome matcher", ErrInvalid, id)
	default:
		return e, nil
	}
}

var (
	entryID      = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	evidenceName = regexp.MustCompile(`^Test\w+(/[\w.-]+)*$`)
)

func (a Allowlist) validate() error {
	ids := map[string]bool{}
	rules := map[string]string{}

	for i, e := range a.entries {
		if err := e.validate(); err != nil {
			return fmt.Errorf("divergence %d: %w", i, err)
		}

		if ids[e.ID] {
			return fmt.Errorf("%w: id %q is used twice", ErrInvalid, e.ID)
		}

		ids[e.ID] = true

		if e.Comparator == nil {
			continue
		}

		if other, dup := rules[e.Comparator.Rule]; dup {
			return fmt.Errorf("%w: %q and %q both match rule %q", ErrInvalid, other, e.ID, e.Comparator.Rule)
		}

		rules[e.Comparator.Rule] = e.ID
	}

	return nil
}

func (e Entry) validate() error {
	switch {
	case !entryID.MatchString(e.ID):
		return fmt.Errorf("%w: id %q is not lower-case words joined by hyphens", ErrInvalid, e.ID)
	case !slices.Contains([]string{KindABI, KindOutcome, KindPublishedTree, KindMechanism}, e.Kind):
		return fmt.Errorf("%w: %q has unknown kind %q", ErrInvalid, e.ID, e.Kind)
	case e.Feature == "" || e.Summary == "" || e.Justification == "":
		return fmt.Errorf("%w: %q needs a feature, a summary and a justification", ErrInvalid, e.ID)
	case strings.ContainsAny(e.Feature+e.Summary+e.Justification, "\n\r"):
		return fmt.Errorf("%w: %q has a line break, which the generated table cannot hold", ErrInvalid, e.ID)
	case !markdownSafe(e.Feature) || !markdownSafe(e.Summary) || !markdownSafe(e.Justification):
		return fmt.Errorf("%w: %q has an unclosed code span or a < outside one, which Markdown renders as HTML", ErrInvalid, e.ID)
	case e.Issue < 0:
		return fmt.Errorf("%w: %q has issue %d", ErrInvalid, e.ID, e.Issue)
	case len(e.Evidence) == 0:
		return fmt.Errorf("%w: %q cites no evidence", ErrInvalid, e.ID)
	}

	for _, ev := range e.Evidence {
		if !evidenceName.MatchString(ev) {
			return fmt.Errorf("%w: %q evidence %q is not a test name", ErrInvalid, e.ID, ev)
		}
	}

	return e.validateMatcher()
}

// markdownSafe reports whether s closes every backtick code span and has no <
// outside one.
func markdownSafe(s string) bool {
	spans := strings.Split(s, "`")
	if len(spans)%2 == 0 {
		return false
	}

	for i := 0; i < len(spans); i += 2 {
		if strings.Contains(spans[i], "<") {
			return false
		}
	}

	return true
}

// validateMatcher requires exactly the one matcher the entry's kind allows:
// none for a mechanism.
func (e Entry) validateMatcher() error {
	n := 0

	for _, set := range []bool{e.ABI != nil, e.Outcome != nil, e.Comparator != nil} {
		if set {
			n++
		}
	}

	switch {
	case e.Kind == KindMechanism && n == 0:
		return nil
	case e.Kind == KindMechanism:
		return fmt.Errorf("%w: mechanism %q excuses no diff, so it takes no matcher", ErrInvalid, e.ID)
	case n != 1:
		return fmt.Errorf("%w: %q needs exactly one matcher, has %d", ErrInvalid, e.ID, n)
	case e.Comparator != nil:
		return e.Comparator.validate(e)
	case e.ABI != nil && e.Kind == KindABI:
		return e.ABI.validate(e.ID)
	case e.Outcome != nil && e.Kind == KindOutcome:
		return e.Outcome.validate(e.ID)
	default:
		return fmt.Errorf("%w: %q of kind %q has a matcher of another kind", ErrInvalid, e.ID, e.Kind)
	}
}

func (m ComparatorMatcher) validate(e Entry) error {
	kind, ok := ComparatorRules()[m.Rule]

	switch {
	case !ok:
		return fmt.Errorf("%w: %q names unknown comparator rule %q", ErrInvalid, e.ID, m.Rule)
	case kind != e.Kind:
		return fmt.Errorf("%w: %q is of kind %q but rule %q belongs to kind %q", ErrInvalid, e.ID, e.Kind, m.Rule, kind)
	case e.Issue > 0 && (len(m.Fixtures) == 0 || slices.Contains(m.Fixtures, "")):
		return fmt.Errorf("%w: issue-tracked rule entry %q names no fixtures where it must excuse a difference", ErrInvalid, e.ID)
	case e.Issue == 0 && len(m.Fixtures) > 0:
		return fmt.Errorf("%w: permanent rule entry %q applies on every fixture, so it takes no fixtures", ErrInvalid, e.ID)
	default:
		return nil
	}
}

func (m ABIMatcher) validate(id string) error {
	switch {
	case len(m.Fixtures) == 0 || slices.Contains(m.Fixtures, ""):
		return fmt.Errorf("%w: %q names no fixture", ErrInvalid, id)
	case !slices.Contains([]string{FileArgs, FileOuts, FileJobInfo}, m.File):
		return fmt.Errorf("%w: %q file %q is not %s, %s or %s", ErrInvalid, id, m.File, FileArgs, FileOuts, FileJobInfo)
	case m.Rule != ABIIgnoreKey && m.Rule != ABICeilNumber:
		return fmt.Errorf("%w: %q has unknown ABI rule %q", ErrInvalid, id, m.Rule)
	case m.Key == "":
		return fmt.Errorf("%w: %q rule %q needs a key", ErrInvalid, id, m.Rule)
	default:
		return nil
	}
}

func (m OutcomeMatcher) validate(id string) error {
	if !m.Failed && len(m.Failures) > 0 {
		return fmt.Errorf("%w: %q records failures for a run that succeeds", ErrInvalid, id)
	}

	for _, f := range m.Failures {
		if f.Kind != FailAssert && f.Kind != FailError {
			return fmt.Errorf("%w: %q failure kind %q is not %s or %s", ErrInvalid, id, f.Kind, FailAssert, FailError)
		}
	}

	return nil
}
