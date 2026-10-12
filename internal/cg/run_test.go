package cg

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/types"
)

// standIn runs a fixture's literal invocation as the generated main.nf runs
// it, in dependency order, every enabled instance's outs a type-correct
// stand-in for the stage's. Each instance gets the records of its
// entry-fed roots' forks as values, and stages, linked below one dir as a
// task stages them, the element dirs of its stage-fed roots' forks and the
// posted output bundles it reads of every instance of each node it reads.
type standIn struct {
	t      *testing.T
	g      *Graph
	man    types.Manifest
	dir    string
	seq    int
	fields map[string][]string
	elems  map[string][]elemDir
}

// elemDir is a fork element dir, the fork it records, and whether the
// entry fixed it, so a task gets it as a record.
type elemDir struct {
	dir   string
	el    Element
	entry bool
}

func newStandIn(t *testing.T, mro string) *standIn {
	t.Helper()

	g, err := Load(mro, mro, []string{filepath.Dir(mro)})
	if err != nil {
		t.Fatal(err)
	}

	return &standIn{t: t, g: g, man: g.Manifest(), dir: t.TempDir(), fields: map[string][]string{}, elems: map[string][]elemDir{}}
}

func (s *standIn) next(name string) string {
	s.seq++

	return filepath.Join(s.dir, strconv.Itoa(s.seq), name)
}

// collect indexes the fork elements under forks, <root>/<ordinal>/ dirs.
func (s *standIn) collect(forks string, entry bool) {
	s.t.Helper()

	dirs, err := filepath.Glob(filepath.Join(forks, "*", "*"))
	if err != nil {
		s.t.Fatal(err)
	}

	for _, d := range dirs {
		el, err := s.g.ReadElement(d)
		if err != nil {
			s.t.Fatal(err)
		}

		root := el.Fork[len(el.Fork)-1].Root.Fqid
		s.elems[root] = append(s.elems[root], elemDir{dir: d, el: el, entry: entry})
	}
}

// read is the posted output bundles of every instance of each node reads
// names: the named outputs, or all of them.
func (s *standIn) read(reads []Read) []string {
	var out []string

	for _, r := range reads {
		for _, d := range s.fields[r.Node] {
			if r.Fields == nil || slices.Contains(r.Fields, filepath.Base(d)) {
				out = append(out, d)
			}
		}
	}

	return out
}

// staged links items below one dir, as a task stages them, and returns it.
func (s *standIn) staged(items []string) string {
	s.t.Helper()

	in := s.next("in")
	if err := os.MkdirAll(in, dirPerm); err != nil {
		s.t.Fatal(err)
	}

	for i, item := range items {
		if err := os.Symlink(item, filepath.Join(in, "u"+strconv.Itoa(i))); err != nil {
			s.t.Fatal(err)
		}
	}

	return in
}

// instances are the fork elements of each instance of n: one per root, each
// agreeing with the parts the instance has on the roots before it.
func (s *standIn) instances(n TopoNode) [][]elemDir {
	type partial struct {
		fork  Fork
		elems []elemDir
	}

	cur := []partial{{}}

	for _, root := range n.Roots {
		var next []partial

		for _, p := range cur {
			for _, e := range s.elems[root] {
				if agrees(e.el.Fork, p.fork) {
					own := e.el.Fork[len(e.el.Fork)-1]
					next = append(next, partial{fork: append(slices.Clone(p.fork), own), elems: append(slices.Clone(p.elems), e)})
				}
			}
		}

		cur = next
	}

	out := make([][]elemDir, len(cur))
	for i, p := range cur {
		out[i] = p.elems
	}

	return out
}

// args writes the args of n's instance at elems, as mre args does.
func (s *standIn) args(n TopoNode, elems []elemDir) (Instance, string) {
	s.t.Helper()

	items := s.read(n.Reads)

	var recs []Element

	for _, e := range elems {
		if !e.entry {
			items = append(items, e.dir)

			continue
		}

		raw, err := os.ReadFile(filepath.Join(e.dir, elementFile))
		if err != nil {
			s.t.Fatal(err)
		}

		el, err := s.g.DecodeElement(base64.StdEncoding.EncodeToString(raw))
		if err != nil {
			s.t.Fatal(err)
		}

		recs = append(recs, el)
	}

	args := s.next("args")

	in, err := s.g.WriteArgs(ArgsSpec{Node: n.Fqid, In: []string{s.staged(items)}, Forks: recs, Manifest: s.man, Out: args})
	if err != nil {
		s.t.Fatalf("args of %s: %v", n.Fqid, err)
	}

	return in, args
}

// standInOuts are type-correct outs for every output of node's stage.
func (s *standIn) standInOuts(node string) json.RawMessage {
	s.t.Helper()

	st, err := s.g.Stage(node)
	if err != nil {
		s.t.Fatal(err)
	}

	var fields []string

	if ps := st.Callable().GetOutParams(); ps != nil {
		for _, p := range ps.List {
			fields = append(fields, strconv.Quote(p.GetId())+":"+string(witnessValue(s.g.types.Get(p.GetTname()), s.g.types, witnessWidth)))
		}
	}

	return json.RawMessage("{" + strings.Join(fields, ",") + "}")
}

// node runs every instance of n, returning how many ran enabled.
func (s *standIn) node(n TopoNode) int {
	s.t.Helper()

	ran := 0

	for _, elems := range s.instances(n) {
		in, args := s.args(n, elems)
		if in.Disabled {
			continue
		}

		st, err := s.g.Stage(n.Fqid)
		if err != nil {
			s.t.Fatal(err)
		}

		outs := s.next("outs")
		if err := writeValue(outs, s.standInOuts(n.Fqid), s.man, st.Call().DecId, types.RoleOut); err != nil {
			s.t.Fatal(err)
		}

		spec := PostSpec{Args: args, Outs: outs, Manifest: s.man, Forks: s.next("forks"), Fields: s.next("fields"), Gates: s.next("gates")}
		if err := os.MkdirAll(filepath.Dir(spec.Gates), dirPerm); err != nil {
			s.t.Fatal(err)
		}

		if err := s.g.Post(spec); err != nil {
			s.t.Fatalf("post %s: %v", n.Fqid, err)
		}

		fields, err := filepath.Glob(filepath.Join(spec.Fields, "*"))
		if err != nil {
			s.t.Fatal(err)
		}

		s.fields[n.Fqid] = append(s.fields[n.Fqid], fields...)
		s.collect(spec.Forks, false)
		ran++
	}

	return ran
}

// TestStandInRunOfEveryFixture runs every fixture's call graph end to end
// on stand-in outs: every instance's _args resolve from what its task
// stages, every enabled one posts its outs and fork elements, and the
// pipeline outs resolve from the outs they read.
func TestStandInRunOfEveryFixture(t *testing.T) {
	skipped := map[string]string{
		"rt_gate_split_reject":      "mrp refuses to instantiate it (#387)",
		"rt_gate_split_reject_pass": "mrp refuses to instantiate it (#387)",
		"rt_gate_split_reject_sub":  "mrp refuses to instantiate it (#387)",
		"map_pipe_nested":           "mrp fails it: OUTER.INNER.DBL's merge matches no fork",
		"split_gate_entry":          "it zips entry collections with SRC's outs, whose keys and lengths no stand-in knows",
	}
	ran := 0

	for _, mro := range entryFiles(t) {
		if _, skip := skipped[filepath.Base(filepath.Dir(mro))]; skip {
			continue
		}

		t.Run(filepath.Base(filepath.Dir(mro))+"/"+filepath.Base(mro), func(t *testing.T) {
			s := newStandIn(t, mro)

			topo, err := s.g.Topology()
			if err != nil {
				t.Fatal(err)
			}

			entry := s.next("entry")
			if err := s.g.PostEntry(entry); err != nil {
				t.Fatal(err)
			}

			s.collect(entry, true)

			for _, n := range topo.Nodes {
				ran += s.node(n)
			}

			out := OutsSpec{In: []string{s.staged(s.read(topo.Outs))}, Manifest: s.man, Out: s.next("pipeline")}
			if err := s.g.WriteOuts(out); err != nil {
				t.Fatalf("pipeline outs: %v", err)
			}
		})
	}

	if ran < 500 {
		t.Errorf("%d instances ran, want every fixture's", ran)
	}
}
