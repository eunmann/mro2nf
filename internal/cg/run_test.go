package cg

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/martian-lang/martian/martian/core"
)

// standIn runs a fixture's literal invocation as the generated main.nf runs
// it, in dependency order, every enabled instance's outs a type-correct
// stand-in for the stage's. Each instance gets the records of its
// entry-fed roots' forks as values, and the items its stage-fed roots'
// producers posted: the elements of their forks, the partner element at
// its index of each output it zips, and the outputs it reads of every
// instance of each node it reads. It stages the items that are dirs,
// linked below one dir as a task stages them, and gets the value records
// as values.
type standIn struct {
	t        *testing.T
	g        *Graph
	dir      string
	seq      int
	fields   map[string][]string
	values   map[string][]string
	elems    map[string][]elemItem
	partners map[string][]postedPartner
}

// postedPartner is one instance's partner element items of one output, by
// name, and its fork.
type postedPartner struct {
	items map[string]string
	fork  Fork
}

// elemItem is a fork element item, the fork it records, and whether the
// entry fixed it, so a task gets it as a fork record.
type elemItem struct {
	item  string
	el    Element
	entry bool
}

func newStandIn(t *testing.T, mro string) *standIn {
	t.Helper()

	g, err := Load(mro, "", []string{filepath.Dir(mro)})
	if err != nil {
		t.Fatal(err)
	}

	return &standIn{
		t: t, g: g, dir: t.TempDir(),
		fields: map[string][]string{}, values: map[string][]string{}, elems: map[string][]elemItem{}, partners: map[string][]postedPartner{},
	}
}

func (s *standIn) next(name string) string {
	s.seq++

	return filepath.Join(s.dir, strconv.Itoa(s.seq), name)
}

// collect indexes the fork element items the instance at fork posted into
// forks, <root>/<ordinal>/ dirs and their value records, and the partner
// element items beside them.
func (s *standIn) collect(forks string, entry bool, fork Fork) {
	s.t.Helper()

	items, err := filepath.Glob(filepath.Join(forks, "*", "*"))
	if err != nil {
		s.t.Fatal(err)
	}

	if raw, err := os.ReadFile(forkValues(forks)); err == nil {
		items = append(items, strings.Fields(string(raw))...)
	}

	partners := map[string]map[string]string{}

	for _, it := range items {
		path, _, _ := strings.Cut(it, ":")
		if dir := filepath.Base(filepath.Dir(path)); strings.Contains(dir, "@") {
			if partners[dir] == nil {
				partners[dir] = map[string]string{}
			}

			partners[dir][filepath.Base(path)] = it

			continue
		}

		el := itemElement(s.t, s.g, it)
		root := el.Fork[len(el.Fork)-1].Root.Fqid
		s.elems[root] = append(s.elems[root], elemItem{item: it, el: el, entry: entry})
	}

	for dir, byName := range partners {
		s.partners[dir] = append(s.partners[dir], postedPartner{items: byName, fork: fork})
	}
}

// read is the posted output bundles and value records of every instance of
// each node reads names: the named outputs, or all of them.
func (s *standIn) read(reads []Read) ([]string, []string) {
	var dirs, values []string

	for _, r := range reads {
		for _, d := range s.fields[r.Node] {
			if r.Fields == nil || slices.Contains(r.Fields, filepath.Base(d)) {
				dirs = append(dirs, d)
			}
		}

		for _, v := range s.values[r.Node] {
			if field, _, _ := strings.Cut(v, ":"); r.Fields == nil || slices.Contains(r.Fields, field) {
				values = append(values, v)
			}
		}
	}

	return dirs, values
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
func (s *standIn) instances(n TopoNode) [][]elemItem {
	type partial struct {
		fork  Fork
		elems []elemItem
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

	out := make([][]elemItem, len(cur))
	for i, p := range cur {
		out[i] = p.elems
	}

	return out
}

// args resolves n's instance at elems and its args, as mre stage's args
// step does.
func (s *standIn) args(n TopoNode, elems []elemItem) Instance {
	s.t.Helper()

	items, values := s.read(n.Reads)

	var recs []Element

	fork := make(Fork, 0, len(elems))
	for _, e := range elems {
		fork = append(fork, e.el.Fork[len(e.el.Fork)-1])
	}

	for _, z := range n.Zips {
		items = append(items, s.partner(z, fork)...)
	}

	for _, e := range elems {
		if !e.entry {
			items = append(items, e.item)

			continue
		}

		raw, err := os.ReadFile(filepath.Join(e.item, elementFile))
		if err != nil {
			s.t.Fatal(err)
		}

		el, err := s.g.DecodeElement(base64.StdEncoding.EncodeToString(raw))
		if err != nil {
			s.t.Fatal(err)
		}

		recs = append(recs, el)
	}

	dirs, elemValues := staging(items)

	in, _, err := s.g.WriteArgs(ArgsSpec{Node: n.Fqid, In: []string{s.staged(dirs)}, Values: append(values, elemValues...), Forks: recs, Out: s.next("args")})
	if err != nil {
		s.t.Fatalf("args of %s: %v", n.Fqid, err)
	}

	return in
}

// partner is what the driver gives of z to the instance at fork: the
// partner element at its fork's index token, posted by the instance of
// z.Node agreeing with it, or that instance's absent record; nothing when
// no enabled instance posted one.
func (s *standIn) partner(z Zip, fork Fork) []string {
	own := slices.IndexFunc(fork, func(p Part) bool { return p.Root.Fqid == z.Root })
	if own < 0 {
		s.t.Fatalf("an instance zipping %s has no fork of it", z.Root)
	}

	for _, p := range s.partners[PartnerDir(z.Root, z.Node, z.Output)] {
		if !agrees(p.fork, fork) {
			continue
		}

		at, ok := p.items[IndexToken(fork[own].Index)]
		if !ok {
			at = p.items[AbsentName]
		}

		return []string{at}
	}

	return nil
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
		in := s.args(n, elems)
		if in.Disabled {
			continue
		}

		var outs core.LazyArgumentMap
		if err := json.Unmarshal(s.standInOuts(n.Fqid), &outs); err != nil {
			s.t.Fatal(err)
		}

		spec := PostSpec{Instance: in, Outs: outs, Forks: s.next("forks"), Fields: s.next("fields"), Values: s.next("values"), Gates: s.next("gates")}
		spec.ForkValues = forkValues(spec.Forks)
		if err := s.g.Post(spec); err != nil {
			s.t.Fatalf("post %s: %v", n.Fqid, err)
		}

		fields, err := filepath.Glob(filepath.Join(spec.Fields, "*"))
		if err != nil {
			s.t.Fatal(err)
		}

		values, err := os.ReadFile(spec.Values)
		if err != nil {
			s.t.Fatal(err)
		}

		s.fields[n.Fqid] = append(s.fields[n.Fqid], fields...)
		s.values[n.Fqid] = append(s.values[n.Fqid], strings.Fields(string(values))...)
		s.collect(spec.Forks, false, in.Fork)
		ran++
	}

	return ran
}

// stages runs every instance of every node, returning the spec of the
// pipeline outs task and how many instances ran enabled.
func (s *standIn) stages() (OutsSpec, int) {
	s.t.Helper()

	topo, err := s.g.Topology()
	if err != nil {
		s.t.Fatal(err)
	}

	entry := s.next("entry")
	if err := s.g.PostEntry(entry); err != nil {
		s.t.Fatal(err)
	}

	s.collect(entry, true, nil)

	ran := 0
	for _, n := range topo.Nodes {
		ran += s.node(n)
	}

	dirs, values := s.read(topo.Outs)

	return OutsSpec{
		In: []string{s.staged(dirs)}, Values: values,
		Work: s.next("work"), Results: s.next("results"), OutsJSON: s.next("pipeline_outs.json"),
	}, ran
}

// requirePanic fails t unless f panics with a value whose text contains want.
func requirePanic(t *testing.T, want string, f func()) {
	t.Helper()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("no panic, want one containing %q", want)
		}

		if got := fmt.Sprint(r); !strings.Contains(got, want) {
			t.Fatalf("panic %q, want one containing %q", got, want)
		}
	}()

	f()
}

// TestStandInRunOfEveryFixture runs every fixture's call graph end to end
// on stand-in outs: every instance's _args resolve from what its task
// stages, every enabled one posts its outs and fork elements, and the
// pipeline outs resolve from the outs they read and publish, or panic as
// Martian's pipestance does under mrp.
func TestStandInRunOfEveryFixture(t *testing.T) {
	skipped := map[string]string{
		"rt_gate_split_reject":      "mrp refuses to instantiate it (#387)",
		"rt_gate_split_reject_pass": "mrp refuses to instantiate it (#387)",
		"rt_gate_split_reject_sub":  "mrp refuses to instantiate it (#387)",
		"map_pipe_nested":           "mrp fails it: OUTER.INNER.DBL's merge matches no fork",
		"split_gate_entry":          "it zips entry collections with SRC's outs, whose keys and lengths no stand-in knows",
		"forkdisabled":              "GATED zips the entry's xs with FLAGS's skips, whose length no stand-in knows",
	}
	panics := map[string]string{
		"nested_empty_forks": "index out of range",
	}
	ran := 0

	for _, mro := range entryFiles(t) {
		fixture := filepath.Base(filepath.Dir(mro))
		if _, skip := skipped[fixture]; skip {
			continue
		}

		t.Run(fixture+"/"+filepath.Base(mro), func(t *testing.T) {
			s := newStandIn(t, mro)

			out, n := s.stages()
			ran += n

			if want, ok := panics[fixture]; ok {
				requirePanic(t, want, func() { _ = s.g.WriteOuts(t.Context(), out) })

				return
			}

			if err := s.g.WriteOuts(t.Context(), out); err != nil {
				t.Fatalf("pipeline outs: %v", err)
			}
		})
	}

	if ran < 500 {
		t.Errorf("%d instances ran, want every fixture's", ran)
	}
}
