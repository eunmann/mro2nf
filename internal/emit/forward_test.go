package emit

import (
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
)

// ref builds a name-preserving whole-field call reference binding (`param = id.param`).
func ref(param, id string) ir.Binding {
	return ir.Binding{Param: param, Value: ir.Value{Ref: &ir.Ref{Kind: refKindCall, ID: id, Output: param}}}
}

// TestConsumerCount guards #59 Lever 4's fold-safety: consumerCount must count
// every dependent on a call's output — input bindings, pipeline returns, AND
// disable refs (which live on the call, not in its bindings). Missing a disable
// consumer would fold a producer whose ch_<producer> the gate still references.
func TestConsumerCount(t *testing.T) {
	p := &ir.Pipeline{
		Calls: []ir.Call{
			{Name: "SRC"},
			{Name: "USE", Bindings: []ir.Binding{ref("y", "SRC")}},
			{Name: "GATE", Disabled: &ir.Ref{Kind: refKindCall, ID: "SRC", Output: "flag"}},
		},
		Returns: []ir.Binding{ref("w", "GATE")},
	}

	if got := consumerCount("SRC", p); got != 2 {
		t.Errorf("consumerCount(SRC) = %d, want 2 (USE input + GATE disable ref)", got)
	}
	if got := consumerCount("GATE", p); got != 1 {
		t.Errorf("consumerCount(GATE) = %d, want 1 (return)", got)
	}
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

// TestFuseableStageCall pins which calls fold their BIND inline (#16): a plain,
// enabled call to a NON-SPLIT stage whose bind is a real transform (not a pure
// forward, which #14 routes with no process). Split stages, mapped/disabled
// calls, sub-pipeline callees, and pure forwards keep the separate BIND (or, for
// forwards, none). A preflight leaf stage fuses like any other leaf (#59).
func TestFuseableStageCall(t *testing.T) {
	prog := &ir.Program{
		Stages: map[string]*ir.Stage{
			"PLAIN": {Name: "PLAIN", Out: []ir.Param{{Name: "y"}}},
			"SPLIT": {Name: "SPLIT", Split: true, Out: []ir.Param{{Name: "y"}}},
		},
		Pipelines: map[string]*ir.Pipeline{"SUB": {Name: "SUB", Out: []ir.Param{{Name: "y"}}}},
	}

	self := []ir.Binding{{Param: "x", Value: ir.Value{Ref: &ir.Ref{Kind: "self", ID: "x"}}}}
	fwd := []ir.Binding{ref("y", "PROD")} // exact forward of PROD's {y}

	tests := []struct {
		name string
		c    ir.Call
		want bool
	}{
		{"non-split stage, transform bind", ir.Call{Name: "C", Callable: "PLAIN", Bindings: self}, true},
		{"split stage keeps BIND", ir.Call{Name: "C", Callable: "SPLIT", Bindings: self}, false},
		{"mapped keeps BIND", ir.Call{Name: "C", Callable: "PLAIN", Bindings: self, Mapped: true}, false},
		{"disabled keeps BIND", ir.Call{Name: "C", Callable: "PLAIN", Bindings: self, Disabled: &ir.Ref{Kind: "self", ID: "off"}}, false},
		// A preflight leaf stage fuses (#59): its output only signals completion to
		// the pa gate; preflight semantics live in the wiring, not the process.
		{"preflight leaf stage fuses", ir.Call{Name: "C", Callable: "PLAIN", Bindings: self, Preflight: true}, true},
		{"pure forward is routed, not fused", ir.Call{Name: "C", Callable: "PLAIN", Bindings: fwd}, false},
		{"sub-pipeline callee keeps BIND", ir.Call{Name: "C", Callable: "SUB", Bindings: self}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &ir.Pipeline{Name: "P", Calls: []ir.Call{{Name: "PROD", Callable: "PLAIN"}, tc.c}}
			_, ok := fuseableStageCall(tc.c, p, prog)
			if ok != tc.want {
				t.Errorf("fuseableStageCall(%s) = %v, want %v", tc.name, ok, tc.want)
			}
		})
	}
}
