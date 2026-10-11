package divergence

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

// The markers around the generated divergence table in a document.
const (
	BeginMarker = "<!-- BEGIN GENERATED DIVERGENCES: `make docs-divergences` writes this table from test/e2e/divergences.json; edit that file, not the table. -->"
	EndMarker   = "<!-- END GENERATED DIVERGENCES -->"
)

// ErrNoSection reports a document without exactly one pair of markers, in
// order.
var ErrNoSection = errors.New("no generated divergence section")

// Markdown renders the allowlist as a table with one ⚠️ row per entry,
// grouped by kind in a fixed order and sorted by id within a kind.
func Markdown(a Allowlist) string {
	order := []string{KindABI, KindPublishedTree, KindOutcome, KindMechanism}

	entries := a.Entries()
	slices.SortFunc(entries, func(x, y Entry) int {
		if d := slices.Index(order, x.Kind) - slices.Index(order, y.Kind); d != 0 {
			return d
		}

		return strings.Compare(x.ID, y.ID)
	})

	var b strings.Builder

	b.WriteString("| Feature | Status | Divergence | Test |\n|---|---|---|---|\n")

	for _, e := range entries {
		tracking := "permanent"
		if e.Issue > 0 {
			tracking = fmt.Sprintf("#%d", e.Issue)
		}

		tests := make([]string, len(e.Evidence))
		for i, ev := range e.Evidence {
			tests[i] = "`" + ev + "`"
		}

		fmt.Fprintf(&b, "| %s | ⚠️ %s, %s | **`%s`**: %s %s | %s |\n",
			cell(e.Feature), e.Kind, tracking, e.ID, cell(e.Summary), cell(e.Justification), strings.Join(tests, ", "))
	}

	return b.String()
}

// cell escapes the one character that ends a table cell even inside a code
// span.
func cell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

// Splice returns doc with the text between its markers replaced by table.
func Splice(doc, table string) (string, error) {
	before, rest, ok := strings.Cut(doc, BeginMarker+"\n")
	if !ok || strings.Contains(rest, BeginMarker) {
		return "", fmt.Errorf("%w: want one line %q", ErrNoSection, BeginMarker)
	}

	_, after, ok := strings.Cut(rest, EndMarker)
	if !ok || strings.Contains(after, EndMarker) {
		return "", fmt.Errorf("%w: want one %q after the begin marker", ErrNoSection, EndMarker)
	}

	return before + BeginMarker + "\n" + table + EndMarker + after, nil
}

// WriteDoc rewrites the generated section of the document at docPath from
// the allowlist at allowPath.
func WriteDoc(allowPath, docPath string) error {
	allow, err := Load(allowPath)
	if err != nil {
		return err
	}

	info, err := os.Stat(docPath)
	if err != nil {
		return fmt.Errorf("stat doc: %w", err)
	}

	doc, err := os.ReadFile(docPath)
	if err != nil {
		return fmt.Errorf("read doc: %w", err)
	}

	out, err := Splice(string(doc), Markdown(allow))
	if err != nil {
		return fmt.Errorf("splice %s: %w", docPath, err)
	}

	if err := os.WriteFile(docPath, []byte(out), info.Mode().Perm()); err != nil {
		return fmt.Errorf("write doc: %w", err)
	}

	return nil
}
