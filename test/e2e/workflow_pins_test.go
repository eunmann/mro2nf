//go:build e2e

package e2e

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	shaPinned   = regexp.MustCompile(`^[\w.-]+/[\w./-]+@[0-9a-f]{40}$`)
	usesLine    = regexp.MustCompile(`^\s*(?:-\s+)?uses:\s*(\S+)(.*)$`)
	versionNote = regexp.MustCompile(`^\s+# v\d+\.\d+\.\d+\s*$`)
	errUnpinned = errors.New("action not pinned to a full commit SHA")
	errNoNote   = errors.New("SHA pin lacks a trailing `# vX.Y.Z` comment")
)

// checkActionRef accepts a local action or one pinned to a full commit SHA. A
// tag or branch is mutable: whoever controls the action's repo can repoint it
// at code that then runs with the job's token.
func checkActionRef(ref string) error {
	if strings.HasPrefix(ref, "./") || shaPinned.MatchString(ref) {
		return nil
	}

	return fmt.Errorf("%w: %s", errUnpinned, ref)
}

// checkUsesLine requires the version comment Dependabot rewrites alongside
// the SHA, so a reader can tell which release a pin is.
func checkUsesLine(line string) error {
	m := usesLine.FindStringSubmatch(line)
	if m == nil || strings.HasPrefix(m[1], "./") || versionNote.MatchString(m[2]) {
		return nil
	}

	return fmt.Errorf("%w: %s", errNoNote, strings.TrimSpace(line))
}

// collectUses returns every `uses:` value in n, at any depth: steps, composite
// action steps and job-level reusable workflow calls alike.
func collectUses(n *yamlNode) []string {
	if n == nil {
		return nil
	}

	var out []string

	for _, k := range n.keys {
		if k == "uses" && n.fields[k].scalar != "" {
			out = append(out, n.fields[k].scalar)

			continue
		}

		out = append(out, collectUses(n.fields[k])...)
	}

	for _, it := range n.items {
		out = append(out, collectUses(it)...)
	}

	return out
}

func actionFiles(t *testing.T) []string {
	t.Helper()

	var files []string

	for _, pattern := range []string{"workflows/*.yml", "workflows/*.yaml", "actions/*/action.yml", "actions/*/action.yaml"} {
		m, err := filepath.Glob(filepath.Join(root, ".github", pattern))
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}

		files = append(files, m...)
	}

	return files
}

// TestActionsPinnedBySHA fails on any workflow or composite action that runs
// a third-party action by tag or branch.
func TestActionsPinnedBySHA(t *testing.T) {
	files := actionFiles(t)
	refs := 0

	for _, path := range files {
		rel, _ := filepath.Rel(root, path)

		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}

		doc, err := parseYAML(string(raw))
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}

		for _, ref := range collectUses(doc) {
			refs++

			if err := checkActionRef(ref); err != nil {
				t.Errorf("%s: %v", rel, err)
			}
		}

		for i, line := range strings.Split(string(raw), "\n") {
			if err := checkUsesLine(line); err != nil {
				t.Errorf("%s:%d: %v", rel, i+1, err)
			}
		}
	}

	if len(files) < 4 || refs < 20 {
		t.Fatalf("found %d files and %d uses: refs; the globs or the walk are wrong", len(files), refs)
	}
}

func TestCheckActionRef(t *testing.T) {
	cases := map[string]error{
		"actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1":          nil,
		"github/codeql-action/init@3d3c42e5aac5ba805825da76410c181273ba90b1": nil,
		"./.github/actions/setup-martian":                                    nil,
		"actions/checkout@v7":                                                errUnpinned,
		"actions/checkout@main":                                              errUnpinned,
		"actions/checkout@3d3c42e":                                           errUnpinned,
		"actions/checkout@3D3C42E5AAC5BA805825DA76410C181273BA90B1":          errUnpinned,
		"docker://alpine:3":                                                  errUnpinned,
	}

	for ref, want := range cases {
		if err := checkActionRef(ref); !errors.Is(err, want) {
			t.Errorf("checkActionRef(%q) = %v, want %v", ref, err, want)
		}
	}
}

func TestCheckUsesLine(t *testing.T) {
	cases := map[string]error{
		"      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1": nil,
		"      uses: ./.github/actions/setup-martian":                                      nil,
		"        run: echo uses: nothing":                                                  nil,
		"      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1":          errNoNote,
		"      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7":     errNoNote,
		"      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # main":   errNoNote,
	}

	for line, want := range cases {
		if err := checkUsesLine(line); !errors.Is(err, want) {
			t.Errorf("checkUsesLine(%q) = %v, want %v", line, err, want)
		}
	}
}
