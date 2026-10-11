//go:build e2e

package e2e

import (
	"strconv"
	"strings"
	"testing"
)

const (
	javaPin      = "${{ env.JAVA_VERSION }}"
	floorJavaPin = "${{ matrix.nextflow == 'latest-stable' && env.JAVA_VERSION || env.FLOOR_JAVA_VERSION }}"

	// floorJavaMax is the newest Java the Nextflow 24.04.0 launcher starts on:
	// its version check rejects anything else with "Java 8 or later (up to 21)".
	floorJavaMax = 21
	// nextflowJavaMin is the oldest Java Nextflow 26.04 supports.
	nextflowJavaMin = 17
)

// TestWorkflowJavaVersions holds every setup-java step to its workflow's one
// JAVA_VERSION (#237), so the copies cannot drift, and every job whose matrix
// includes the Nextflow floor to FLOOR_JAVA_VERSION on that lane: the floor
// engine does not start on the LTS the other lanes run.
func TestWorkflowJavaVersions(t *testing.T) {
	for _, file := range []string{gateWorkflow, nightlyWorkflow} {
		doc := readWorkflow(t, file)

		if got := scalarOf(doc.get("env").get("JAVA_VERSION")); got != "25" {
			t.Errorf("%s env JAVA_VERSION = %q, want the Temurin LTS \"25\"", file, got)
		}

		steps := 0
		jobs := doc.get("jobs")

		for _, id := range jobs.keys {
			job := jobs.get(id)

			want := javaPin
			if job.get("strategy").get("matrix").get("nextflow") != nil {
				want = floorJavaPin
			}

			for _, step := range job.get("steps").items {
				if !strings.HasPrefix(scalarOf(step.get("uses")), "actions/setup-java@") {
					continue
				}

				steps++

				if got := scalarOf(step.get("with").get("java-version")); got != want {
					t.Errorf("%s %s setup-java java-version = %q, want %q", file, id, got, want)
				}
			}
		}

		if steps == 0 {
			t.Errorf("%s: no setup-java step found", file)
		}
	}

	floor := scalarOf(readWorkflow(t, nightlyWorkflow).get("env").get("FLOOR_JAVA_VERSION"))
	if n, err := strconv.Atoi(floor); err != nil || n < nextflowJavaMin || n > floorJavaMax {
		t.Errorf("%s env FLOOR_JAVA_VERSION = %q, want %d..%d", nightlyWorkflow, floor, nextflowJavaMin, floorJavaMax)
	}
}
