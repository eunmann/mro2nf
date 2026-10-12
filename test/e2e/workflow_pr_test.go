//go:build e2e

package e2e

import (
	"slices"
	"strings"
	"testing"
)

// prSuiteJobs are the pr-validation.yml jobs that run Nextflow suites, each
// with the make target it runs.
var prSuiteJobs = map[string]string{"e2e-shard": "make test-e2e"}

const uploadArtifact = "actions/upload-artifact@"

// TestPRValidationUploadsRunLogsOnFailure requires each suite job to upload,
// after its suite and only when it failed, the dir the harness keeps a failed
// test's run logs in, under a name unique to the job and matrix shard, with
// the upload-artifact commit nightly.yml pins.
func TestPRValidationUploadsRunLogsOnFailure(t *testing.T) {
	pinned := uploadArtifactRefs(t, nightlyWorkflow)
	if len(pinned) != 1 {
		t.Fatalf("%s uses upload-artifact at %q, want one pinned commit", nightlyWorkflow, pinned)
	}

	jobs := readWorkflow(t, gateWorkflow).get("jobs")
	names := map[string]string{}

	for id, target := range prSuiteJobs {
		job := jobs.get(id)
		steps := job.get("steps").items

		suite := slices.IndexFunc(steps, func(s *yamlNode) bool { return scalarOf(s.get("run")) == target })
		upload := slices.IndexFunc(steps, func(s *yamlNode) bool { return strings.HasPrefix(scalarOf(s.get("uses")), uploadArtifact) })

		if suite < 0 || upload < suite {
			t.Errorf("%s job %q: %q at step %d, upload-artifact at step %d; want the upload after the suite", gateWorkflow, id, target, suite, upload)

			continue
		}

		step := steps[upload]
		if got := scalarOf(step.get("uses")); got != pinned[0] {
			t.Errorf("%s job %q uses %q, want %q as %s pins it", gateWorkflow, id, got, pinned[0], nightlyWorkflow)
		}

		if got := scalarOf(step.get("if")); got != "${{ failure() }}" {
			t.Errorf("%s job %q upload runs if %q, want only on failure", gateWorkflow, id, got)
		}

		if got := scalarOf(step.get("with").get("path")); got != e2eArtifactsDefault+"/" {
			t.Errorf("%s job %q uploads %q, want the harness's %s/", gateWorkflow, id, got, e2eArtifactsDefault)
		}

		name := scalarOf(step.get("with").get("name"))
		if job.get("strategy").get("matrix").get("shard") != nil && !strings.Contains(name, "${{ matrix.shard }}") {
			t.Errorf("%s job %q artifact %q does not name its shard, so the shards' uploads collide", gateWorkflow, id, name)
		}

		if other, dup := names[name]; dup || name == "" {
			t.Errorf("%s jobs %q and %q upload the same artifact name %q", gateWorkflow, other, id, name)
		}

		names[name] = id
	}
}

// uploadArtifactRefs is every distinct upload-artifact `uses:` in file.
func uploadArtifactRefs(t *testing.T, file string) []string {
	t.Helper()

	var refs []string

	jobs := readWorkflow(t, file).get("jobs")
	for _, id := range jobs.keys {
		for _, s := range jobs.get(id).get("steps").items {
			if uses := scalarOf(s.get("uses")); strings.HasPrefix(uses, uploadArtifact) && !slices.Contains(refs, uses) {
				refs = append(refs, uses)
			}
		}
	}

	return refs
}
