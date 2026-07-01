package emit

import (
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
)

// ref builds a name-preserving whole-field call reference binding (`param = id.param`).
func ref(param, id string) ir.Binding {
	return ir.Binding{Param: param, Value: ir.Value{Ref: &ir.Ref{Kind: refKindCall, ID: id, Output: param}}}
}

// TestForwardProducerExactCoverage pins the emit-once routing condition: a call is
// routed straight through only when its bindings forward EXACTLY one plain
// producer's declared outputs. A subset forward must keep its BIND, or routing the
// producer's whole bundle would leak the producer's extra outputs (and their file
// leaves) into the consumer's args — a data-movement regression the projecting
// BIND avoids.
func TestForwardProducerExactCoverage(t *testing.T) {
	prog := &ir.Program{Stages: map[string]*ir.Stage{
		"ONE": {Name: "ONE", Out: []ir.Param{{Name: "f"}}},
		"TWO": {Name: "TWO", Out: []ir.Param{{Name: "f"}, {Name: "g"}}},
	}}

	tests := []struct {
		name     string
		prodCall ir.Call
		bindings []ir.Binding
		wantID   string
		wantOK   bool
	}{
		{
			name:     "exact single-output forward routes",
			prodCall: ir.Call{Name: "P", Callable: "ONE"},
			bindings: []ir.Binding{ref("f", "P")},
			wantID:   "P",
			wantOK:   true,
		},
		{
			name:     "subset forward (producer has an extra output) does not route",
			prodCall: ir.Call{Name: "P", Callable: "TWO"},
			bindings: []ir.Binding{ref("f", "P")},
			wantOK:   false,
		},
		{
			name:     "mapped producer does not route",
			prodCall: ir.Call{Name: "P", Callable: "ONE", Mapped: true},
			bindings: []ir.Binding{ref("f", "P")},
			wantOK:   false,
		},
		{
			name:     "disabled producer does not route",
			prodCall: ir.Call{Name: "P", Callable: "ONE", Disabled: &ir.Ref{Kind: "self", ID: "off"}},
			bindings: []ir.Binding{ref("f", "P")},
			wantOK:   false,
		},
		{
			name:     "renaming binding does not route",
			prodCall: ir.Call{Name: "P", Callable: "ONE"},
			bindings: []ir.Binding{{Param: "y", Value: ir.Value{Ref: &ir.Ref{Kind: refKindCall, ID: "P", Output: "f"}}}},
			wantOK:   false,
		},
		{
			name:     "self/pipeargs binding does not route",
			prodCall: ir.Call{Name: "P", Callable: "ONE"},
			bindings: []ir.Binding{{Param: "f", Value: ir.Value{Ref: &ir.Ref{Kind: "self", ID: "f"}}}},
			wantOK:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &ir.Pipeline{Name: "PIPE", Calls: []ir.Call{tc.prodCall}}
			id, ok := forwardProducer(tc.bindings, p, prog)
			if ok != tc.wantOK || (ok && id != tc.wantID) {
				t.Errorf("forwardProducer = (%q, %v), want (%q, %v)", id, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}
