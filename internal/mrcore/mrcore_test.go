package mrcore

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/martian-lang/martian/martian/syntax"
)

// fakeStore serves outs by stage and the indices of its fork, and the
// split elements a test stages.
type fakeStore struct {
	outs     map[string]string
	elements map[string]json.RawMessage
}

func storeKey(st *syntax.CallGraphStage, fork ForkId) string {
	var ix []string

	for _, r := range st.ForkRoots() {
		i, ok := fork.Index(r)
		if !ok {
			ix = append(ix, "?")

			continue
		}

		ix = append(ix, i.GoString())
	}

	return st.Fqid + "|" + strings.Join(ix, ",")
}

func (s fakeStore) Outs(st *syntax.CallGraphStage, fork ForkId) (LazyArgumentMap, error) {
	raw, ok := s.outs[storeKey(st, fork)]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotStaged, storeKey(st, fork))
	}

	var m LazyArgumentMap
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, err
	}

	return m, nil
}

func (s fakeStore) SplitElement(e *syntax.SplitExp, _ syntax.Type, _ ForkId, part *ForkSourcePart) (json.Marshaler, bool, error) {
	v, ok := s.elements[e.Call.Id+"|"+part.Id.GoString()]

	return v, ok, nil
}

const stages = `
stage MK(
    out int[]    xs,
    out map<int> m,
    out map<int> few,
    out int[]    short,
    src py       "s",
)

stage SQ(
    in  int x,
    in  int z,
    out int y,
    src py  "s",
)

stage SUM(
    in  int[]    ys,
    out int      s,
    src py       "s",
)

stage SUMM(
    in  map<int> ys,
    out int      s,
    src py       "s",
)
`

// compile is the call graph of the invocation src of TOP over stages.
func compile(t *testing.T, src string, store Store) *TopNode {
	t.Helper()

	var p syntax.Parser

	_, _, ast, err := p.ParseSourceBytes([]byte(stages+src), "pipeline.mro", nil, false)
	if err != nil {
		t.Fatal(err)
	}

	root, err := ast.MakePipelineCallGraph("", ast.Call)
	if err != nil {
		t.Fatal(err)
	}

	return NewTop(root, &ast.TypeTable, store)
}

func stage(t *testing.T, top *TopNode, fqid string) *syntax.CallGraphStage {
	t.Helper()

	n, err := top.node(fqid)
	if err != nil {
		t.Fatal(err)
	}

	st, ok := n.call.(*syntax.CallGraphStage)
	if !ok {
		t.Fatalf("%s is no stage", fqid)
	}

	return st
}

func compact(t *testing.T, v json.Marshaler) string {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	return string(raw)
}

const gather = `
pipeline TOP(
    out int s,
    out int sm,
)
{
    call MK()

    map call SQ(
        x = split MK.xs,
        z = split MK.short,
    )

    map call SQ as SQM(
        x = split MK.m,
        z = 0,
    )

    call SUM(
        ys = SQ.y,
    )

    call SUMM(
        ys = SQM.y,
    )

    return (
        s  = SUM.s,
        sm = SUMM.s,
    )
}

call TOP()
`

func gatherStore() fakeStore {
	return fakeStore{outs: map[string]string{
		"TOP.MK|":        `{"xs":[1,2,3],"m":{"b":2,"a":1},"short":[5,6]}`,
		"TOP.SQ|0":       `{"y":1}`,
		"TOP.SQ|fork1":   `{"y":4}`,
		"TOP.SQ|fork2":   `{"y":9}`,
		`TOP.SQM|fork_a`: `{"y":10}`,
		`TOP.SQM|fork_b`: `{"y":20}`,
	}}
}

// TestInputsGather: a gather of a map call over a stage output expands the
// call's forks from that output, an array by index and a map by key, as
// core's getParts and expandForkFromRef do.
func TestInputsGather(t *testing.T) {
	s := gatherStore()
	s.outs["TOP.SQ|fork0"] = s.outs["TOP.SQ|0"]
	top := compile(t, gather, s)

	for fqid, want := range map[string]string{
		"TOP.SUM":  `{"ys":[1,4,9]}`,
		"TOP.SUMM": `{"ys":{"a":10,"b":20}}`,
	} {
		args, missing, err := top.Inputs(fqid, nil)
		if err != nil || missing != nil {
			t.Fatalf("%s: %v, missing %v", fqid, err, missing)
		}

		if d := cmp.Diff(want, compact(t, args)); d != "" {
			t.Errorf("%s args (-want +got):\n%s", fqid, d)
		}
	}

	root := stage(t, top, "TOP.SQ")

	idx, err := top.Indices(root.ForkRoots()[0], nil, nil)
	if err != nil || len(idx) != 3 {
		t.Errorf("Indices of TOP.SQ = %v, %v; want 3", idx, err)
	}
}

// TestInputsMissingElement: a zipped split past its shorter collection's
// end runs with null, its error reported apart, as mrp runs it; a read the
// task did not stage fails.
func TestInputsMissingElement(t *testing.T) {
	s := gatherStore()
	s.outs["TOP.SQ|fork0"] = s.outs["TOP.SQ|0"]
	top := compile(t, gather, s)
	sq := stage(t, top, "TOP.SQ")

	args, missing, err := top.Inputs("TOP.SQ", ForkId{Part(sq.ForkRoots()[0], arrayIndexFork(2))})
	if err != nil || !errors.Is(missing, ErrNoElement) {
		t.Fatalf("SQ fork 2: %v, missing %v; want a missing element", err, missing)
	}

	if got := compact(t, args); got != `{"x":3,"z":null}` {
		t.Errorf("SQ fork 2 args = %s", got)
	}

	delete(s.outs, "TOP.SQ|fork1")

	if _, _, err := top.Inputs("TOP.SUM", nil); !errors.Is(err, ErrNotStaged) {
		t.Errorf("SUM without SQ fork 1 staged: %v, want ErrNotStaged", err)
	}
}

// TestSplitElementStaged: a split's element the task staged is used in
// place of its collection, which it did not stage.
func TestSplitElementStaged(t *testing.T) {
	s := fakeStore{outs: map[string]string{}, elements: map[string]json.RawMessage{
		"SQ|fork1":  json.RawMessage(`7`),
		"SQ|fork1z": json.RawMessage(`8`),
	}}
	top := compile(t, gather, s)
	sq := stage(t, top, "TOP.SQ")

	ready, v, err := top.resolve(sq.ResolvedInputs()["x"].Exp, top.types.Get(syntax.TypeId{Tname: syntax.KindInt}),
		ForkId{Part(sq.ForkRoots()[0], arrayIndexFork(1))}, 0)
	if err != nil || !ready || compact(t, v) != "7" {
		t.Errorf("x at SQ fork 1 = %v, %t, %v; want the staged 7", v, ready, err)
	}
}

const nested = `
pipeline INNER(
    in  int[] xs,
    out int[] ys,
)
{
    map call SQ(
        x = split self.xs,
        z = 0,
    )

    return (
        ys = SQ.y,
    )
}

pipeline TOP(
    out int[][] yss,
    out int     s,
)
{
    map call INNER(
        xs = split [[1, 2], [3]],
    )

    map call SQ as USE(
        x = split MK.xs,
        z = 0,
    )

    call MK()

    map call SQ as NEXT(
        x = split USE.y,
        z = 1,
    )

    call SUM(
        ys = NEXT.y,
    )

    return (
        yss = INNER.ys,
        s   = SUM.s,
    )
}

call TOP()
`

// TestInputsNestedGather: a gather of a map call inside another, each outer
// fork's inner forks expanded from its own element; and a map call over
// another's merged outs, each fork reading its own fork of the other.
func TestInputsNestedGather(t *testing.T) {
	s := fakeStore{outs: map[string]string{
		"TOP.MK|":                  `{"xs":[1,2],"m":{},"short":[]}`,
		"TOP.INNER.SQ|fork0,fork0": `{"y":1}`,
		"TOP.INNER.SQ|fork0,fork1": `{"y":4}`,
		"TOP.INNER.SQ|fork1,fork0": `{"y":9}`,
		"TOP.USE|fork0":            `{"y":10}`,
		"TOP.USE|fork1":            `{"y":20}`,
		"TOP.NEXT|fork0":           `{"y":11}`,
		"TOP.NEXT|fork1":           `{"y":21}`,
		"TOP.SUM|":                 `{"s":32}`,
	}}

	top := compile(t, nested, s)

	v, _, err := top.PipelineOutputs("TOP", nil)
	if err != nil {
		t.Fatal(err)
	}

	if got := compact(t, v); got != `{"s":32,"yss":[[1,4],[9]]}` {
		t.Errorf("TOP outs = %s", got)
	}

	next := stage(t, top, "TOP.NEXT")

	args, _, err := top.Inputs("TOP.NEXT", ForkId{Part(next.ForkRoots()[0], arrayIndexFork(1))})
	if err != nil || compact(t, args) != `{"x":20,"z":1}` {
		t.Errorf("NEXT fork 1 = %v, %v; want USE fork 1's y", args, err)
	}

	sum, _, err := top.Inputs("TOP.SUM", nil)
	if err != nil || compact(t, sum) != `{"ys":[11,21]}` {
		t.Errorf("SUM = %v, %v", sum, err)
	}
}

// TestForkID: a fork's id is core's ForkIdString of its counted parts: a
// part of variable length starts a new index after '_', one of a single
// fork folds into the index before it, and a map part writes its key.
func TestForkID(t *testing.T) {
	top := compile(t, nested, fakeStore{})
	sq := stage(t, top, "TOP.INNER.SQ").ForkRoots()
	gtop := compile(t, gather, fakeStore{})
	sqm := stage(t, gtop, "TOP.SQM").ForkRoots()[0]

	for _, tc := range []struct {
		node string
		top  *TopNode
		fork ForkId
		want string
	}{
		{"TOP.INNER.SQ", top, ForkId{CountedPart(sq[0], arrayIndexFork(0), 2), CountedPart(sq[1], arrayIndexFork(1), 2)}, "fork0_fork1"},
		{"TOP.INNER.SQ", top, ForkId{CountedPart(sq[0], arrayIndexFork(1), 2), CountedPart(sq[1], arrayIndexFork(0), 1)}, "fork1"},
		{"TOP.SQM", gtop, ForkId{CountedPart(sqm, mapKeyFork("a"), 2)}, "fork_a"},
		{"TOP.MK", top, nil, "fork0"},
	} {
		got, err := tc.top.ForkID(tc.node, tc.fork)
		if err != nil || got != tc.want {
			t.Errorf("ForkID of %s at %s = %q, %v; want %q", tc.node, tc.fork.GoString(), got, err, tc.want)
		}
	}
}

const knownInner = `
pipeline IN2(
    in  int   a,
    out int[] ys,
)
{
    map call SQ(
        x = split [1, 2],
        z = self.a,
    )

    return (
        ys = SQ.y,
    )
}

pipeline TOP(
    out int[][] yss,
)
{
    call MK()

    map call IN2(
        a = split MK.xs,
    )

    map call SQ as ZIP(
        x = split MK.m,
        z = split MK.few,
    )

    return (
        yss = IN2.ys,
    )
}

call TOP()
`

// TestInputsKnownInner: a gather of a map call of known length inside each
// fork of one of run-time length takes each outer fork's own (getParts over
// the inner call's static parts); a zipped split over a map lacking a key
// runs with null there.
func TestInputsKnownInner(t *testing.T) {
	s := fakeStore{outs: map[string]string{
		"TOP.MK|":                `{"xs":[5,6],"m":{"a":1,"b":2},"few":{"a":3},"short":[]}`,
		"TOP.IN2.SQ|fork0,fork0": `{"y":1}`,
		"TOP.IN2.SQ|fork0,fork1": `{"y":2}`,
		"TOP.IN2.SQ|fork1,fork0": `{"y":3}`,
		"TOP.IN2.SQ|fork1,fork1": `{"y":4}`,
	}}

	top := compile(t, knownInner, s)

	v, _, err := top.PipelineOutputs("TOP", nil)
	if err != nil || compact(t, v) != `{"yss":[[1,2],[3,4]]}` {
		t.Errorf("TOP outs = %v, %v", v, err)
	}

	zip := stage(t, top, "TOP.ZIP")

	args, missing, err := top.Inputs("TOP.ZIP", ForkId{Part(zip.ForkRoots()[0], mapKeyFork("b"))})
	if err != nil || !errors.Is(missing, ErrNoElement) || compact(t, args) != `{"x":2,"z":null}` {
		t.Errorf("ZIP key b = %v, missing %v, %v", args, missing, err)
	}
}

const gated = `
struct ITEM(
    int v,
)

stage FLAG(
    out bool        off,
    out map<ITEM>   items,
    src py          "s",
)

stage TAKE(
    in  map<int> vs,
    out int      n,
    src py       "s",
)

pipeline LIT(
    in  int   n,
    out int[] xs,
)
{
    call SQ(
        x = self.n,
        z = 0,
    )

    return (
        xs = [1, 2],
    )
}

pipeline TOP()
{
    call FLAG()

    call LIT(
        n = 0,
    ) using (
        disabled = FLAG.off,
    )

    map call SQ as EACH(
        x = split LIT.xs,
        z = 0,
    )

    call TAKE(
        vs = FLAG.items.v,
    )

    return ()
}

call TOP()
`

// TestInputsGatedLiteral: a split of a literal behind a gate takes the
// element alone where the gate holds it on, and null where it is off; a
// projection through a typed map of structs reads each member's field.
func TestInputsGatedLiteral(t *testing.T) {
	for off, want := range map[string]string{"false": `{"x":2,"z":0}`, "true": `{"x":null,"z":0}`} {
		s := fakeStore{outs: map[string]string{
			"TOP.FLAG|": `{"off":` + off + `,"items":{"a":{"v":1},"b":{"v":2}}}`,
		}}
		top := compile(t, gated, s)
		sq := stage(t, top, "TOP.EACH")

		args, _, err := top.Inputs("TOP.EACH", ForkId{Part(sq.ForkRoots()[0], arrayIndexFork(1))})
		if err != nil || compact(t, args) != want {
			t.Errorf("off=%s: SQ fork 1 = %v, %v; want %s", off, args, err, want)
		}

		take, _, err := top.Inputs("TOP.TAKE", nil)
		if err != nil || compact(t, take) != `{"vs":{"a":1,"b":2}}` {
			t.Errorf("TAKE = %v, %v", take, err)
		}
	}
}

// TestDisabled: a fork is disabled as core's Fork.disabled decides: by a
// gate that reads true, not by one that reads false or null, and by a part
// of no forks whatever its gates; a gate the task did not stage fails.
func TestDisabled(t *testing.T) {
	for off, want := range map[string]bool{"true": true, "false": false, "null": false} {
		top := compile(t, gated, fakeStore{outs: map[string]string{"TOP.FLAG|": `{"off":` + off + `,"items":{}}`}})
		sq := stage(t, top, "TOP.LIT.SQ")

		got, err := top.Disabled(sq.Fqid, nil)
		if err != nil || got != want {
			t.Errorf("off=%s: Disabled of %s = %t, %v; want %t", off, sq.Fqid, got, err, want)
		}

		got, err = top.Disables(sq.Disabled()[0], nil)
		if err != nil || got != want {
			t.Errorf("off=%s: Disables = %t, %v; want %t", off, got, err, want)
		}
	}

	top := compile(t, gated, fakeStore{outs: map[string]string{}})
	if _, err := top.Disabled("TOP.LIT.SQ", nil); !errors.Is(err, ErrNotStaged) {
		t.Errorf("Disabled with its gate unstaged: %v, want ErrNotStaged", err)
	}

	each := stage(t, top, "TOP.EACH")
	for n, want := range map[int]bool{0: true, 2: false} {
		got, err := top.Disabled(each.Fqid, ForkId{CountedPart(each.ForkRoots()[0], arrayIndexFork(0), n)})
		if err != nil || got != want {
			t.Errorf("Disabled of %s, a part of %d forks = %t, %v; want %t", each.Fqid, n, got, err, want)
		}
	}
}

// TestFailuresAreLoud: an unknown node, an unstaged read and a root whose
// count reads what the task did not stage each fail.
func TestFailuresAreLoud(t *testing.T) {
	top := compile(t, gather, fakeStore{outs: map[string]string{}})

	if _, _, err := top.Inputs("TOP.NOPE", nil); err == nil {
		t.Error("Inputs of an unknown node succeeded")
	}

	if _, _, err := top.Inputs("TOP.SUM", nil); !errors.Is(err, ErrNotStaged) {
		t.Errorf("Inputs of an unstaged gather: %v, want ErrNotStaged", err)
	}

	sq := stage(t, top, "TOP.SQ")
	if _, err := top.Indices(sq.ForkRoots()[0], nil, nil); !errors.Is(err, ErrUnresolved) {
		t.Errorf("Indices of an unstaged source: %v, want ErrUnresolved", err)
	}

	if _, ok := (ForkId{}).Index(sq.ForkRoots()[0]); ok {
		t.Error("Index of a root an empty fork lacks")
	}
}

func TestOnlyMissing(t *testing.T) {
	missing := &elementError{element: "array index 3", inner: ErrNoElement}
	other := &elementError{element: "stage X"}

	for _, tc := range []struct {
		err  error
		want bool
	}{
		{missing, true},
		{other, false},
		{syntax.ErrorList{missing, missing}, true},
		{syntax.ErrorList{missing, other}, false},
		{errors.Join(missing, fmt.Errorf("wrapped: %w", missing)), true},
		{errors.Join(missing, other), false},
		{errNotReady, false},
	} {
		if got := onlyMissing(tc.err); got != tc.want {
			t.Errorf("onlyMissing(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestForkIdKey(t *testing.T) {
	top := compile(t, gather, gatherStore())
	sq := stage(t, top, "TOP.SQ").ForkRoots()[0]

	a := ForkId{Part(sq, arrayIndexFork(1))}
	b := ForkId{Part(sq, arrayIndexFork(2))}
	u := ForkId{{Split: sq.Split(), Id: undeterminedFork{}}}

	if a.key() == b.key() || a.key() == u.key() {
		t.Errorf("keys collide: %q %q %q", a.key(), b.key(), u.key())
	}

	if !a.determined() || u.determined() {
		t.Errorf("determined: %v %v, want true false", a.determined(), u.determined())
	}
}

// TestPipelineOutputs: a pipeline's outs resolve and pass mrp's check of
// each struct; one of the wrong type fails it, as do a stage's outs of the
// wrong type, and a read the task did not stage.
func TestPipelineOutputs(t *testing.T) {
	s := gatherStore()
	s.outs["TOP.SQ|fork0"] = s.outs["TOP.SQ|0"]
	s.outs["TOP.SUM|"], s.outs["TOP.SUMM|"] = `{"s":14}`, `{"s":30}`
	top := compile(t, gather, s)

	v, typ, err := top.PipelineOutputs("TOP", nil)
	if err != nil || compact(t, v) != `{"s":14,"sm":30}` || typ == nil {
		t.Errorf("PipelineOutputs = %v, %v, %v", v, typ, err)
	}

	f := newFork(top.allNodes["TOP"], nil)
	if ok, _ := f.verifyPipelineOutput(MarshalerMap{"s": json.RawMessage("1.5"), "sm": json.RawMessage("1")}, typ); ok {
		t.Error("pipeline outs s = 1.5 pass mrp's check")
	}

	if err := top.VerifyOutput("TOP.SUM", nil, LazyArgumentMap{"s": json.RawMessage("1.5")}); !errors.Is(err, ErrInvalidOuts) {
		t.Errorf("VerifyOutput of s = 1.5: %v, want ErrInvalidOuts", err)
	}

	if err := top.VerifyOutput("TOP.SUM", nil, LazyArgumentMap{"s": json.RawMessage("2")}); err != nil {
		t.Errorf("VerifyOutput of s = 2: %v", err)
	}

	delete(s.outs, "TOP.SUMM|")

	if _, _, err := top.PipelineOutputs("TOP", nil); !errors.Is(err, ErrNotStaged) {
		t.Errorf("PipelineOutputs without SUMM staged: %v, want ErrNotStaged", err)
	}
}
