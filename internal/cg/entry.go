package cg

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/syntax"
)

var (
	// ErrEntryFile reports an entry file value the run may not stage.
	ErrEntryFile = errors.New("entry file value refused")

	errStagedEntry = errors.New("staged entry file")

	// uriScheme is the prefix Nextflow's file() dispatches on: a value
	// starting `<scheme>:/` names a filesystem provider; anything else,
	// `a:b.txt` included, is a path.
	uriScheme = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*):/`)
)

// stageUnsafe are the characters Nextflow's task stage-in script writes
// unescaped into its `ln -s <source> <name>` lines that bash still expands
// there (Nextflow 26.04): a glob can match a different file and
// stage it silently, a brace or `$` rewrites the path.
const stageUnsafe = "*?[]{}$"

// fileScheme is the scheme of a head-node path, as Nextflow reads one.
const fileScheme = "file"

// EntryLeaf is one file value of the run's entry inputs that names a file:
// an absolute path or a URI, and the entry input it is first found in.
// InPlace is a value every task reads where it is (ReadInPlace), as mrp
// passes a stage the path it was given, so none stages it.
type EntryLeaf struct {
	Input   string
	Value   string
	InPlace bool
}

// ReadInPlace is which entry file values every task of a run reads where
// they are: a path under the S3 mount, and, where Host, any absolute path,
// as on the profiles whose tasks run on the host's own filesystem. A
// container profile's task sees only what it stages and mounts, so there
// Host is false and a head-node path is staged.
type ReadInPlace struct {
	Mount shim.Mount
	Host  bool
}

// Holds reports whether every task reads value in place.
func (r ReadInPlace) Holds(value string) bool {
	return r.Mount.Holds(value) || r.Host && filepath.IsAbs(value)
}

// EntryLeaves are the file values of the run's entry inputs that name a
// file, each once, in the order the type walk first reaches them: every
// file leaf of the call's arguments naming an absolute path or a URI. A
// relative path names nothing from a stage's job dir under mrp either, and
// reaches the stage as given.
func (g *Graph) EntryLeaves() ([]EntryLeaf, error) {
	all, err := g.entryFileValues()
	if err != nil {
		return nil, err
	}

	var out []EntryLeaf

	for _, l := range all {
		if filepath.IsAbs(l.Value) || uriScheme.MatchString(l.Value) {
			out = append(out, l)
		}
	}

	return out, nil
}

// Roots are the read-only roots of the run's tasks: the S3 mount, and each
// entry file value in reads in place.
func (g *Graph) Roots(in ReadInPlace) (shim.Roots, error) {
	leaves, err := g.EntryLeaves()
	if err != nil {
		return shim.Roots{}, err
	}

	paths := []string{string(in.Mount)}

	for _, l := range leaves {
		if in.Holds(l.Value) {
			paths = append(paths, l.Value)
		}
	}

	roots, err := shim.NewRoots(paths...)
	if err != nil {
		return shim.Roots{}, fmt.Errorf("entry roots: %w", err)
	}

	return roots, nil
}

// entryFileValues are every file value of the call's arguments, each once,
// in type-walk order.
func (g *Graph) entryFileValues() ([]EntryLeaf, error) {
	inv, err := g.Invocation()
	if err != nil {
		return nil, err
	}

	params, err := g.Params(inv.Call, types.RoleIn)
	if err != nil {
		return nil, fmt.Errorf("entry params: %w", err)
	}

	tbl := g.Table()
	seen := map[string]bool{}

	var out []EntryLeaf

	for _, p := range params {
		raw, ok := inv.Args[p.Id]
		if !ok {
			continue
		}

		v, err := types.DecodeJSON(raw)
		if err != nil {
			return nil, fmt.Errorf("decode entry input %s: %w", p.Id, err)
		}

		_, err = tbl.Apply([]*syntax.StructMember{p}, map[string]any{p.Id: v}, func(s string) (string, error) {
			if !seen[s] {
				seen[s] = true
				out = append(out, EntryLeaf{Input: p.Id, Value: s})
			}

			return s, nil
		})
		if err != nil {
			return nil, fmt.Errorf("entry input %s: %w", p.Id, err)
		}
	}

	return out, nil
}

// EntryFileReaders are the stage nodes a binding of which reads a file
// value of the call's arguments, and the pipeline, by its Fqid, when its
// outs do.
func (g *Graph) EntryFileReaders() (map[string]bool, error) {
	all, err := g.entryFileValues()
	if err != nil {
		return nil, err
	}

	values := map[string]int{}
	for i, l := range all {
		values[l.Value] = i
	}

	out := map[string]bool{}

	for fqid, st := range g.stages {
		found := map[int]bool{}

		for _, rb := range st.ResolvedInputs() {
			if err := collectValues(rb.Exp, values, found); err != nil {
				return nil, err
			}
		}

		if len(found) > 0 {
			out[fqid] = true
		}
	}

	outs, err := g.outsEntryFiles(values)
	if err != nil {
		return nil, err
	}

	if len(outs) > 0 {
		out[g.Fqid()] = true
	}

	return out, nil
}

// Fqid is the pipeline the invocation calls: the prefix of every node's
// fqid, and the one PLAN's entry files of the pipeline outs are keyed by.
func (g *Graph) Fqid() string {
	return g.root.Fqid
}

// outsEntryFiles are the indices in values of the entry file values the
// pipeline outs read, which OUTS stages: Martian's post-processing reads
// such a file where the value names it.
func (g *Graph) outsEntryFiles(values map[string]int) ([]int, error) {
	ro := g.root.ResolvedOutputs()
	if ro == nil {
		return nil, nil
	}

	found := map[int]bool{}
	if err := collectValues(ro.Exp, values, found); err != nil {
		return nil, fmt.Errorf("pipeline outs: %w", err)
	}

	return slices.Sorted(maps.Keys(found)), nil
}

// VetHostLeaf refuses a head-node path read in place where the tasks do not
// see the host's paths, as hostPaths says they do, and one that is or holds
// the run's work dir, workDir, whose stage outputs every task would then
// read as an input, the input and the work dir each by its given or its real
// path. A workDir that is a URI holds no host path. The errors name the
// input, not the value.
func VetHostLeaf(l EntryLeaf, hostPaths bool, workDir string) error {
	if !hostPaths {
		return fmt.Errorf("%w: entry input '%s': a head-node path is not read on this profile; pass an s3:// URI", ErrEntryFile, l.Input)
	}

	if !filepath.IsAbs(workDir) {
		return nil
	}

	roots, err := shim.NewRoots(l.Value)
	if err != nil {
		return fmt.Errorf("%w: entry input '%s' does not resolve: %w", ErrEntryFile, l.Input, unnamed(err))
	}

	realWork, err := filepath.EvalSymlinks(workDir)
	if errors.Is(err, fs.ErrNotExist) {
		realWork = workDir
	} else if err != nil {
		return fmt.Errorf("vet entry input '%s': resolve the run's work dir: %w", l.Input, err)
	}

	if roots.Holds(workDir) || roots.Holds(realWork) {
		return fmt.Errorf("%w: entry input '%s' is or holds the run's work dir %s, and every task reads a head-node entry input in place; pass a directory that does not hold it, or run with -work-dir outside it",
			ErrEntryFile, l.Input, workDir)
	}

	return nil
}

// unnamed is the cause of the failure err to resolve a value, without the
// value it names.
func unnamed(err error) error {
	if pe := (*fs.PathError)(nil); errors.As(err, &pe) {
		return pe.Err
	}

	if cause := errors.Unwrap(err); cause != nil {
		return cause
	}

	return err
}

// VetEntryLeaf refuses an entry file value a run may not stage. file()
// on the head node fetches any URL Nextflow has a provider for and reads
// any path the head node sees, and an entry value comes from whoever
// launches the run: an s3:// URI is always accepted, a head-node path
// (absolute, or a file: URI) only where hostPaths, and any other scheme only
// where remote. A value holding a character Nextflow's stage-in script
// leaves for bash to expand is refused everywhere. The errors name the
// input, not the value, which may carry a credential.
func VetEntryLeaf(l EntryLeaf, hostPaths, remote bool) error {
	for _, c := range l.Value {
		if c < 0x20 || c == 0x7f || strings.ContainsRune(stageUnsafe, c) {
			shown := "'" + string(c) + "'"
			if c < 0x20 || c == 0x7f {
				shown = fmt.Sprintf("U+%04X", c)
			}

			return fmt.Errorf("%w: entry input '%s': the path contains %s, which Nextflow's task stage-in script leaves unquoted, so bash would expand it and could stage another file; rename the file (a Nextflow staging limitation, see docs/TRANSPILER.md section 8)",
				ErrEntryFile, l.Input, shown)
		}
	}

	scheme := fileScheme
	if m := uriScheme.FindStringSubmatch(l.Value); m != nil {
		scheme = strings.ToLower(m[1])
	}

	switch {
	case scheme == "s3", scheme == fileScheme && hostPaths, scheme != fileScheme && remote:
		return nil
	case scheme == fileScheme:
		return fmt.Errorf("%w: entry input '%s': a head-node path is not staged on this profile; pass an s3:// URI", ErrEntryFile, l.Input)
	default:
		return fmt.Errorf("%w: entry input '%s': scheme '%s' is not allowed; pass a path or an s3:// URI, or set params.mro2nf_allow_remote_inputs to true to fetch it",
			ErrEntryFile, l.Input, scheme)
	}
}

// PlanFiles are the entry file values an instance of a node stages: at
// the fork of its entry-fed roots Key names (DottedKey), the indices into
// the run's EntryLeaves its bindings read.
type PlanFiles struct {
	Key    string
	Leaves []int
}

// entryFiles are, per fork of node n's entry-fed roots, the entry file
// values st's bindings read there, in Martian's specialization of each
// binding to the fork, so a map call over entry files stages only its own.
func (g *Graph) entryFiles(st *syntax.CallGraphStage, n TopoNode, wt Topology, planned *plannedForks, leaves map[string]int) ([]PlanFiles, error) {
	if len(leaves) == 0 {
		return nil, nil
	}

	var roots []string

	for _, r := range n.Roots {
		if slices.ContainsFunc(wt.Roots, func(x TopoRoot) bool { return x.Fqid == r && x.Producer == "" }) {
			roots = append(roots, r)
		}
	}

	r := &resolver{g: g, st: emptyStore{}}

	var out []PlanFiles

	for _, in := range assignments(roots, planned) {
		found := map[int]bool{}

		for _, k := range slices.Sorted(maps.Keys(st.ResolvedInputs())) {
			rb := st.ResolvedInputs()[k]

			exp, _, err := r.specialize(rb.Exp, rb.Type, in.fork)
			if err != nil {
				return nil, fmt.Errorf("input %s of %s at fork %s: %w", k, st.Fqid, in.fork, err)
			}

			if err := collectValues(exp, leaves, found); err != nil {
				return nil, err
			}
		}

		if len(found) > 0 {
			out = append(out, PlanFiles{Key: DottedKey(entryFork{ords: in.ords}.key(roots)), Leaves: slices.Sorted(maps.Keys(found))})
		}
	}

	return out, nil
}

// collectValues adds to found the index in values of every string exp's
// JSON form holds.
func collectValues(exp syntax.Exp, values map[string]int, found map[int]bool) error {
	if exp == nil {
		return nil
	}

	raw, err := json.Marshal(exp)
	if err != nil {
		return fmt.Errorf("encode binding: %w", err)
	}

	v, err := types.DecodeJSON(raw)
	if err != nil {
		return fmt.Errorf("decode binding: %w", err)
	}

	walkStrings(v, func(s string) {
		if i, ok := values[s]; ok {
			found[i] = true
		}
	})

	return nil
}

func walkStrings(v any, visit func(string)) {
	switch x := v.(type) {
	case string:
		visit(x)
	case []any:
		for _, e := range x {
			walkStrings(e, visit)
		}
	case types.Object:
		for _, m := range x {
			walkStrings(m.Value, visit)
		}
	}
}

// StagedEntryFiles maps each entry file value the task staged to the path
// it reads it at: the i-th of ids, an index into the run's EntryLeaves, is
// the one item Nextflow staged in dir/<i+1>/. Nextflow links a file it can
// reach in place, so a link's target is the value's own path, which mrp
// would pass the stage; a file it fetched is read where it was staged.
func (g *Graph) StagedEntryFiles(dir string, ids []int) (map[string]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	leaves, err := g.EntryLeaves()
	if err != nil {
		return nil, err
	}

	out := make(map[string]string, len(ids))

	for i, id := range ids {
		if id < 0 || id >= len(leaves) {
			return nil, fmt.Errorf("%w %d: the run has %d entry file values", errStagedEntry, id, len(leaves))
		}

		p, err := stagedItem(filepath.Join(dir, strconv.Itoa(i+1)))
		if err != nil {
			return nil, err
		}

		out[leaves[id].Value] = p
	}

	return out, nil
}

// stagedItem is the path the one item staged in dir reads at.
func stagedItem(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errStagedEntry, err)
	}

	if len(entries) != 1 {
		return "", fmt.Errorf("%w: %s holds %d items, want 1", errStagedEntry, dir, len(entries))
	}

	p := filepath.Join(dir, entries[0].Name())

	if target, err := os.Readlink(p); err == nil && filepath.IsAbs(target) {
		return target, nil
	}

	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errStagedEntry, err)
	}

	return abs, nil
}
