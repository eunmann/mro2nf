package gen

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestTidyDropsSubsOnlyAnUncalledSubCalled checks dropping the entry's call
// of A, which calls the sub B, which calls the stage S, removes all three.
func TestTidyDropsSubsOnlyAnUncalledSubCalled(t *testing.T) {
	p := &Program{
		Stages: []*Stage{{Name: "S"}, {Name: "KEEP"}},
		Pipelines: []*Pipeline{
			{Name: "B", Calls: []*Call{{Callee: "S", ID: "S"}}},
			{Name: "A", Calls: []*Call{{Callee: "B", ID: "B"}}},
			{Name: "E", Calls: []*Call{{Callee: "A", ID: "A"}, {Callee: "KEEP", ID: "KEEP"}}},
		},
	}

	p.dropCall(2, 0)
	p.tidy()

	stages := make([]string, 0, len(p.Stages))
	for _, s := range p.Stages {
		stages = append(stages, s.Name)
	}

	pipelines := make([]string, 0, len(p.Pipelines))
	for _, pl := range p.Pipelines {
		pipelines = append(pipelines, pl.Name)
	}

	if diff := cmp.Diff([]string{"KEEP"}, stages); diff != "" {
		t.Errorf("stages (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff([]string{"E"}, pipelines); diff != "" {
		t.Errorf("pipelines (-want +got):\n%s", diff)
	}
}
