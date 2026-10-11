package emit

import (
	"maps"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/google/go-cmp/cmp"
)

const liftSrc = `
stage ECHO(in int v, out int r, src py "s/echo",)
stage NOTE(out string n, src py "s/note",)
pipeline SUB(in int x, in int b, in bool skip, out int fx, out int fb,){
    call ECHO as DEP(v = self.x,)
    call ECHO as INDEP(v = self.b,)
    call NOTE()
    call ECHO as OWN(v = 1,) using (disabled = self.skip,)
    return (fx = DEP.r, fb = INDEP.r,)
}
pipeline PLAIN(in int[] xs, out SUB[] s,){
    map call SUB(x = split self.xs, b = 2, skip = true,)
    return (s = SUB,)
}
pipeline TOP(in int[] xs, in bool flag, out SUB[] ent, out SUB[] lit, out SUB[] p,){
    map call SUB as ENT(x = split self.xs, b = 1, skip = self.flag,)
    map call SUB as LIT(x = split [1, 2], b = 1, skip = false,)
    call PLAIN(xs = self.xs,)
    return (ent = ENT, lit = LIT, p = PLAIN.s,)
}
call TOP(xs = [1, 2], flag = false,)
`

// liftedCall is the part of a call the lift tests pin.
type liftedCall struct {
	Name, Callable string
	Gate           bool
	Disabled       string
}

func liftedCalls(p *ir.Pipeline) []liftedCall {
	out := make([]liftedCall, 0, len(p.Calls))

	for _, c := range p.Calls {
		lc := liftedCall{Name: c.Name, Callable: c.Callable, Gate: c.Gate}
		if c.Disabled != nil {
			lc.Disabled = c.Disabled.ID + "." + c.Disabled.Output
		}

		out = append(out, lc)
	}

	return out
}

// TestLiftForkIndependent pins #433's lift: the calls of SUB that read no
// split input leave each map call over it, under a gate on the map call's
// splits unless they are literals, combined with a lifted call's own disable;
// a map call inside a plain sub-pipeline lifts into that sub-pipeline; the
// residual copy keeps only the fork-dependent call and reads the lifted
// INDEP through an input.
func TestLiftForkIndependent(t *testing.T) {
	prog := lowerMRO(t, liftSrc)
	LiftForkIndependent(prog)

	want := []liftedCall{
		{Name: "ENT_gate", Gate: true},
		{Name: "ENT_INDEP", Callable: "ECHO", Disabled: "ENT_gate.off"},
		{Name: "ENT_NOTE", Callable: "NOTE", Disabled: "ENT_gate.off"},
		{Name: "ENT_OWN_gate", Gate: true},
		{Name: "ENT_OWN", Callable: "ECHO", Disabled: "ENT_OWN_gate.off"},
		{Name: "ENT", Callable: "SUB_lift1"},
		{Name: "LIT_INDEP", Callable: "ECHO"},
		{Name: "LIT_NOTE", Callable: "NOTE"},
		{Name: "LIT_OWN", Callable: "ECHO"},
		{Name: "LIT", Callable: "SUB_lift2"},
		{Name: "PLAIN", Callable: "PLAIN"},
	}
	if diff := cmp.Diff(want, liftedCalls(prog.Pipelines["TOP"])); diff != "" {
		t.Errorf("TOP calls mismatch (-want +got):\n%s", diff)
	}

	// PLAIN binds skip = true: its OWN folds away as a literal-true gate.
	plain := []liftedCall{
		{Name: "SUB_gate", Gate: true},
		{Name: "SUB_INDEP", Callable: "ECHO", Disabled: "SUB_gate.off"},
		{Name: "SUB_NOTE", Callable: "NOTE", Disabled: "SUB_gate.off"},
		{Name: "SUB", Callable: "SUB_lift3"},
	}
	if diff := cmp.Diff(plain, liftedCalls(prog.Pipelines["PLAIN"])); diff != "" {
		t.Errorf("PLAIN calls mismatch (-want +got):\n%s", diff)
	}

	residual := prog.Pipelines["SUB_lift1"]
	if diff := cmp.Diff([]liftedCall{{Name: "DEP", Callable: "ECHO"}}, liftedCalls(residual)); diff != "" {
		t.Errorf("residual calls mismatch (-want +got):\n%s", diff)
	}

	fb := residual.Returns[1].Value.Ref
	if fb == nil || fb.Kind != ir.RefKindSelf || fb.ID != "__h_INDEP" || fb.Output != "r" {
		t.Errorf("residual fb = %+v, want self.__h_INDEP.r", residual.Returns[1].Value)
	}

	ent, _ := callByName(prog.Pipelines["TOP"], "ENT")
	if b, ok := bindingFor(ent, "__h_INDEP"); !ok || b.Value.Ref == nil || b.Value.Ref.ID != "ENT_INDEP" {
		t.Errorf("ENT must bind __h_INDEP to ENT_INDEP, got %+v", ent.Bindings)
	}
}

// TestLiftLeavesInputProgram pins that the lift replaces pipelines rather
// than mutating them, so a shallow copy (internal/overrides) leaves the input
// program as it was.
func TestLiftLeavesInputProgram(t *testing.T) {
	prog := lowerMRO(t, liftSrc)
	before := liftedCalls(prog.Pipelines["TOP"])

	cp := *prog
	cp.Pipelines = maps.Clone(prog.Pipelines)
	LiftForkIndependent(&cp)

	if diff := cmp.Diff(before, liftedCalls(prog.Pipelines["TOP"])); diff != "" {
		t.Errorf("input TOP changed (-before +after):\n%s", diff)
	}

	if _, ok := prog.Pipelines["SUB_lift1"]; ok {
		t.Error("the input program gained a residual copy")
	}
}

// TestLiftOuterFork pins the nested case: a call that reads an outer map
// call's fork element stays under it, gated by the inner map call's splits,
// while a call that reads neither fork leaves both, without the inner gate,
// whose splits depend on the outer fork (mrp does not fork a call over the
// source of a map call it sits in).
func TestLiftOuterFork(t *testing.T) {
	prog := lowerMRO(t, `
stage ECHO(in int v, out int r, src py "s/echo",)
stage NOTE(out string n, src py "s/note",)
pipeline SUB(in int x, in int b, out int fx,){
    call ECHO as DEP(v = self.x,)
    call ECHO as INDEP(v = self.b,)
    call NOTE()
    return (fx = DEP.r,)
}
pipeline OUTER(in int o, in int[] xs, out SUB[] s,){
    map call SUB(x = split self.xs, b = self.o,)
    return (s = SUB,)
}
pipeline TOP(out SUB[][] s,){
    map call OUTER(o = split [1, 2], xs = split [[1], [2]],)
    return (s = OUTER.s,)
}
call TOP()
`)
	LiftForkIndependent(prog)

	top := []liftedCall{{Name: "OUTER_SUB_NOTE", Callable: "NOTE"}, {Name: "OUTER", Callable: "OUTER_lift1"}}
	if diff := cmp.Diff(top, liftedCalls(prog.Pipelines["TOP"])); diff != "" {
		t.Errorf("TOP calls mismatch (-want +got):\n%s", diff)
	}

	outer := []liftedCall{
		{Name: "SUB_gate", Gate: true},
		{Name: "SUB_INDEP", Callable: "ECHO", Disabled: "SUB_gate.off"},
		{Name: "SUB", Callable: "SUB_lift1_lift1"},
	}
	if diff := cmp.Diff(outer, liftedCalls(prog.Pipelines["OUTER_lift1"])); diff != "" {
		t.Errorf("OUTER calls mismatch (-want +got):\n%s", diff)
	}
}

// TestLiftLiftedMapCall pins that a map call over a sub-pipeline that itself
// leaves an enclosing map call gets its own fork-independent calls lifted:
// mrp runs NOTE once, not once per EACH fork.
func TestLiftLiftedMapCall(t *testing.T) {
	prog := lowerMRO(t, `
stage ECHO(in int v, out int r, src py "s/echo",)
stage NOTE(out string n, src py "s/note",)
pipeline Q(in int v, out int r,){
    call ECHO(v = self.v,)
    call NOTE()
    return (r = ECHO.r,)
}
pipeline SUB(in int x, in int[] ys, out int fx, out Q[] q,){
    call ECHO as DEP(v = self.x,)
    map call Q as EACH(v = split self.ys,)
    return (fx = DEP.r, q = EACH,)
}
pipeline TOP(in int[] xs, out SUB[] s,){
    map call SUB(x = split self.xs, ys = [5, 6],)
    return (s = SUB,)
}
call TOP(xs = [1, 2],)
`)
	LiftForkIndependent(prog)

	want := []liftedCall{
		{Name: "SUB_gate", Gate: true},
		{Name: "SUB_EACH_gate", Gate: true},
		{Name: "SUB_EACH_NOTE", Callable: "NOTE", Disabled: "SUB_EACH_gate.off"},
		{Name: "SUB_EACH", Callable: "Q_lift1", Disabled: "SUB_gate.off"},
		{Name: "SUB", Callable: "SUB_lift1"},
	}
	if diff := cmp.Diff(want, liftedCalls(prog.Pipelines["TOP"])); diff != "" {
		t.Errorf("TOP calls mismatch (-want +got):\n%s", diff)
	}
}

// TestLiftCopyCalleeStruct pins the residual input for a lifted call to a
// pipeline copy (here a SplitKnownContexts-style Q_ctx2): it is typed by a
// struct of the copy's outputs, so the file-leaf walk sees the lifted file,
// and the struct is added to the lifted program only.
func TestLiftCopyCalleeStruct(t *testing.T) {
	prog := lowerMRO(t, `
stage MAKE(out file f, src py "s/make",)
stage TAKE(in int v, in file f, out int r, src py "s/take",)
pipeline Q(out file f,){
    call MAKE()
    return (f = MAKE.f,)
}
pipeline SUB(in int x, out int r,){
    call Q as MADE()
    call TAKE(v = self.x, f = MADE.f,)
    return (r = TAKE.r,)
}
pipeline TOP(in int[] xs, out SUB[] s,){
    map call SUB(x = split self.xs,)
    return (s = SUB,)
}
call TOP(xs = [1, 2],)
`)
	q := *prog.Pipelines["Q"]
	q.Name = "Q_ctx2"
	prog.Pipelines["Q_ctx2"] = &q

	sub := clonePipeline(prog.Pipelines["SUB"], "SUB")
	sub.Calls[0].Callable = "Q_ctx2"
	prog.Pipelines["SUB"] = sub

	lifted := *prog
	lifted.Pipelines = maps.Clone(prog.Pipelines)
	LiftForkIndependent(&lifted)

	residual := lifted.Pipelines["SUB_lift1"]
	if residual == nil {
		t.Fatal("no residual SUB_lift1")
	}

	in := residual.In[len(residual.In)-1]
	if in.Name != "__h_MADE" || in.BaseType != "Q_ctx2" {
		t.Fatalf("residual input = %+v, want __h_MADE typed Q_ctx2", in)
	}

	if !hasFileLeaf(in, lifted.Structs) {
		t.Errorf("__h_MADE has no file leaf under the lifted program's structs")
	}

	if _, ok := prog.Structs["Q_ctx2"]; ok {
		t.Error("the input program gained a struct for the copy")
	}
}
