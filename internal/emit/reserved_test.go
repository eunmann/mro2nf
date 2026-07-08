package emit

import (
	"errors"
	"testing"

	"github.com/eunmann/mro2nf/internal/apperror"
	"github.com/eunmann/mro2nf/internal/ir"
)

// TestCheckReservedEntryNames guards #171: an entry input whose name collides
// with a Nextflow param the emitter defines for the target must be rejected (it
// would otherwise be silently overridden by the config declaration). The reserved
// set is target-dependent, so a name reserved on one target may be free on
// another.
func TestCheckReservedEntryNames(t *testing.T) {
	mk := func(inName string) *ir.Program {
		return &ir.Program{
			Entry: &ir.EntryCall{Callable: "TOP"},
			Pipelines: map[string]*ir.Pipeline{
				"TOP": {Name: "TOP", In: []ir.Param{{Name: inName, BaseType: "int"}}},
			},
		}
	}

	cases := []struct {
		name    string
		input   string
		target  Target
		wantErr bool
	}{
		{"outdir reserved everywhere", "outdir", TargetLocal, true},
		{"job_resources reserved everywhere", "job_resources", TargetHealthOmics, true},
		{"aws_queue reserved on local", "aws_queue", TargetLocal, true},
		{"aws_outdir reserved on awsbatch", "aws_outdir", TargetAWSBatch, true},
		{"aws_outdir free on local", "aws_outdir", TargetLocal, false},
		{"container reserved on container target", "container", TargetAWSBatch, true},
		{"container free on local", "container", TargetLocal, false},
		{"container_dataplane reserved on container target", "container_dataplane", TargetHealthOmics, true},
		{"container_dataplane free on local", "container_dataplane", TargetLocal, false},
		{"aws_queue free on healthomics", "aws_queue", TargetHealthOmics, false},
		{"ordinary name free", "samples", TargetAWSBatch, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkReservedEntryNames(mk(tc.input), tc.target)

			switch {
			case tc.wantErr && !errors.Is(err, apperror.ErrUnsupported):
				t.Errorf("input %q on %q: err = %v, want an unsupported error", tc.input, tc.target, err)
			case !tc.wantErr && err != nil:
				t.Errorf("input %q on %q: err = %v, want nil", tc.input, tc.target, err)
			}
		})
	}
}
