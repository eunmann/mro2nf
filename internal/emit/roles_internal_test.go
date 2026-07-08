package emit

import "testing"

// TestContainerRoleLabel pins the role→Nextflow-label mapping the config's
// withLabel: selectors and the process-body markers both depend on, and checks
// the dataplane marker line stays in sync with roleDataplane.label().
func TestContainerRoleLabel(t *testing.T) {
	if got := roleStage.label(); got != "role_stage" {
		t.Errorf("roleStage.label() = %q, want role_stage", got)
	}

	if got := roleDataplane.label(); got != "role_dataplane" {
		t.Errorf("roleDataplane.label() = %q, want role_dataplane", got)
	}

	want := "  label '" + roleDataplane.label() + "'\n"
	if dataplaneLabelLine != want {
		t.Errorf("dataplaneLabelLine = %q, want %q", dataplaneLabelLine, want)
	}
}
