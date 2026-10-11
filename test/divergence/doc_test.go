package divergence

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

const featureCoverage = "../../docs/FEATURE_COVERAGE.md"

// TestFeatureCoverageIsCurrent fails when the checked-in divergence table of
// docs/FEATURE_COVERAGE.md is not what the allowlist renders.
func TestFeatureCoverageIsCurrent(t *testing.T) {
	allow, err := Load(checkedIn)
	if err != nil {
		t.Fatal(err)
	}

	doc, err := os.ReadFile(featureCoverage)
	if err != nil {
		t.Fatal(err)
	}

	want, err := Splice(string(doc), Markdown(allow))
	if err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff(want, string(doc)); diff != "" {
		t.Errorf("docs/FEATURE_COVERAGE.md's divergence table is stale; run make docs-divergences (-want +got):\n%s", diff)
	}
}

// TestFeatureCoverageWarningsAreGenerated fails on a ⚠️ table row written by
// hand outside the generated section.
func TestFeatureCoverageWarningsAreGenerated(t *testing.T) {
	doc, err := os.ReadFile(featureCoverage)
	if err != nil {
		t.Fatal(err)
	}

	before, rest, _ := strings.Cut(string(doc), BeginMarker)
	_, after, _ := strings.Cut(rest, EndMarker)

	for line := range strings.Lines(before + after) {
		if strings.HasPrefix(line, "|") && strings.Contains(line, "⚠️") {
			t.Errorf("hand-written ⚠️ row; make it an allowlist entry: %s", strings.TrimSpace(line))
		}
	}
}

func TestMarkdown(t *testing.T) {
	const ok = `"summary": "s | t", "justification": "j", "evidence": ["TestX", "TestY/sub"]`

	allow, err := Parse([]byte(`{"divergences": [
		{"id": "how", "kind": "mechanism", "feature": "f", ` + ok + `},
		{"id": "nf-fails", "kind": "outcome", "feature": "g", "issue": 9, ` + ok + `, "outcome": {"failed": true}},
		{"id": "b-order", "kind": "abi", "feature": "h", ` + ok + `, "comparator": {"rule": "abi-sort-object-keys"}},
		{"id": "a-key", "kind": "abi", "feature": "i", ` + ok + `, "abi": {"fixtures": ["fx"], "file": "_args", "rule": "ignore-key", "key": "k"}}]}`))
	if err != nil {
		t.Fatal(err)
	}

	want := "| Feature | Status | Divergence | Test |\n|---|---|---|---|\n" +
		"| i | ⚠️ abi, permanent | **`a-key`**: s \\| t j | `TestX`, `TestY/sub` |\n" +
		"| h | ⚠️ abi, permanent | **`b-order`**: s \\| t j | `TestX`, `TestY/sub` |\n" +
		"| g | ⚠️ outcome, #9 | **`nf-fails`**: s \\| t j | `TestX`, `TestY/sub` |\n" +
		"| f | ⚠️ mechanism, permanent | **`how`**: s \\| t j | `TestX`, `TestY/sub` |\n"

	if diff := cmp.Diff(want, Markdown(allow)); diff != "" {
		t.Errorf("Markdown (-want +got):\n%s", diff)
	}
}

func TestSplice(t *testing.T) {
	doc := "intro\n" + BeginMarker + "\nold\n" + EndMarker + "\noutro\n"

	got, err := Splice(doc, "new\n")
	if err != nil {
		t.Fatal(err)
	}

	if want := "intro\n" + BeginMarker + "\nnew\n" + EndMarker + "\noutro\n"; got != want {
		t.Errorf("Splice = %q, want %q", got, want)
	}

	for name, bad := range map[string]string{
		"no markers":   "intro\n",
		"no end":       BeginMarker + "\nold\n",
		"end first":    EndMarker + "\n" + BeginMarker + "\n",
		"two sections": doc + doc,
	} {
		if _, err := Splice(bad, "new\n"); !errors.Is(err, ErrNoSection) {
			t.Errorf("%s: err = %v, want ErrNoSection", name, err)
		}
	}
}

// TestWriteDoc checks the generator rewrites only the generated section and
// keeps the document's mode.
func TestWriteDoc(t *testing.T) {
	dir := t.TempDir()
	allowPath := filepath.Join(dir, "divergences.json")
	docPath := filepath.Join(dir, "doc.md")

	const allowDoc = `{"divergences": [{"id": "how", "kind": "mechanism", "feature": "f", "summary": "s",
		"justification": "j", "evidence": ["TestX"]}]}`
	if err := os.WriteFile(allowPath, []byte(allowDoc), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(docPath, []byte("intro\n"+BeginMarker+"\nold\n"+EndMarker+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := WriteDoc(allowPath, docPath); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}

	allow, err := Parse([]byte(allowDoc))
	if err != nil {
		t.Fatal(err)
	}

	if want := "intro\n" + BeginMarker + "\n" + Markdown(allow) + EndMarker + "\n"; string(got) != want {
		t.Errorf("doc = %q, want %q", got, want)
	}

	if info, err := os.Stat(docPath); err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("doc mode = %v (err %v), want 0640", info.Mode().Perm(), err)
	}

	if err := WriteDoc(allowPath, filepath.Join(dir, "missing.md")); err == nil {
		t.Error("WriteDoc of a missing doc succeeded")
	}
}

func TestParseRejectsHTMLInText(t *testing.T) {
	for _, summary := range []string{"see <ps>/outs", "an `unclosed span"} {
		doc := `{"divergences": [{"id": "a", "kind": "mechanism", "feature": "f", "summary": "` + summary +
			`", "justification": "j", "evidence": ["TestX"]}]}`
		if _, err := Parse([]byte(doc)); !errors.Is(err, ErrInvalid) {
			t.Errorf("summary %q: err = %v, want ErrInvalid", summary, err)
		}
	}

	if _, err := Parse([]byte(`{"divergences": [{"id": "a", "kind": "mechanism", "feature": "f",
		"summary": "see ` + "`<ps>/outs`" + `", "justification": "j", "evidence": ["TestX"]}]}`)); err != nil {
		t.Errorf("a < inside a code span: %v", err)
	}
}
