//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// requiredContexts is the branch-protection required status-check set. Every one
// must be a job name in BOTH pr-validation.yml (the real gate) and
// pr-validation-skip.yml (the docs-only companion): a context present in only one
// leaves a PR stuck on an "Expected — Waiting for status" required check that
// never reports (issue #182). GitHub matches contexts by exact name, so a rename
// in one file (or here) without the other silently reintroduces that deadlock.
// This test is the consistency guard the companion-workflow pattern otherwise
// lacks — it cannot check the external branch-protection config, but it pins the
// two workflow files (and this list) to each other.
var requiredContexts = map[string]bool{
	"Lint":                          true,
	"Build":                         true,
	"Unit tests":                    true,
	"E2E (Nextflow vs mrp goldens)": true,
	"E2E (container isolation)":     true,
	"Lint generated Nextflow":       true,
	"Bench (data movement gate)":    true,
}

// jobNameRE matches a job-level `name:` — four-space indent, no leading dash
// (step names use `- name:`, the workflow name sits at column 0) — capturing the
// value with optional surrounding quotes.
var jobNameRE = regexp.MustCompile(`(?m)^    name: "?([^"\n]+?)"?\s*$`)

func workflowJobNames(t *testing.T, file string) map[string]bool {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(root, ".github", "workflows", file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}

	names := make(map[string]bool)
	for _, m := range jobNameRE.FindAllStringSubmatch(string(raw), -1) {
		names[m[1]] = true
	}

	return names
}

// TestRequiredContextsMirrored fails if the real and companion validation
// workflows disagree on the required status-check names, which would deadlock
// either docs-only or code PRs (issue #182).
func TestRequiredContextsMirrored(t *testing.T) {
	real := workflowJobNames(t, "pr-validation.yml")
	skip := workflowJobNames(t, "pr-validation-skip.yml")

	// Every required context is an actual job in the real gate.
	for ctx := range requiredContexts {
		if !real[ctx] {
			t.Errorf("required context %q is not a job name in pr-validation.yml", ctx)
		}
	}

	// The companion mirrors EXACTLY the required set — nothing missing (would
	// deadlock docs PRs) and nothing extra (a non-gate name reported for free).
	for ctx := range requiredContexts {
		if !skip[ctx] {
			t.Errorf("required context %q missing from pr-validation-skip.yml (docs-only PRs would deadlock)", ctx)
		}
	}

	for name := range skip {
		if !requiredContexts[name] {
			t.Errorf("pr-validation-skip.yml has job %q that is not a required context; remove it or add it to requiredContexts", name)
		}
	}
}
