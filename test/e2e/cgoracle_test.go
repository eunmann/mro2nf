//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/eunmann/mro2nf/internal/cg"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
)

// coreFork is one stage fork as mrp's own runtime resolves it: its stage's
// call-graph fqid, its args, and whether they are all resolved. Its
// disabled verdict is not among them: core decides it only when it steps the
// fork (Fork.disabled, core/stage.go:883, unexported), which a read-only
// reattach never does.
type coreFork struct {
	node  string
	args  json.RawMessage
	ready bool
}

// warmCoreRuntime builds one Martian runtime before any parallel reattach:
// the first fixes the base path jobmanagers/config.json is read from
// (martian/util RelPath caches it unsynchronized).
var warmCoreRuntime = sync.OnceValue(func() error {
	opts := core.DefaultRuntimeOptions()
	_, err := opts.NewRuntime()

	return err
})

// coreForks reattaches read-only to the pipestance at ps with mrp's own
// runtime (core Runtime.ReattachToPipestance, as mrstat does) and returns
// every stage fork it holds, keyed by its fork dir relative to ps, with the
// input bindings mrp's resolver yields from the _outs under ps
// (Fork.serializeState -> Node.inputBindingInfo -> TopNode.resolve). The
// runtime reads jobmanagers/config.json beside MARTIAN_BASE, which TestMain
// points at the Martian bin dir.
func coreForks(ctx context.Context, t *testing.T, ps string, mroPaths []string) map[string]coreFork {
	t.Helper()

	if err := warmCoreRuntime(); err != nil {
		t.Fatalf("martian runtime (MARTIAN_BASE=%q): %v", os.Getenv("MARTIAN_BASE"), err)
	}

	opts := core.DefaultRuntimeOptions()
	opts.VdrMode = core.VdrDisable

	rt, err := opts.NewRuntime()
	if err != nil {
		t.Fatalf("martian runtime (MARTIAN_BASE=%q): %v", os.Getenv("MARTIAN_BASE"), err)
	}

	pipe, err := rt.ReattachToPipestance(filepath.Base(ps), ps, "", "", mroPaths, "", nil, false, true, ctx)
	if err != nil {
		t.Fatalf("reattach %s: %v", ps, err)
	}

	out := map[string]coreFork{}
	psPrefix := "ID." + filepath.Base(ps) + "."

	for _, n := range pipe.SerializeState(ctx) {
		if n.Type != syntax.KindStage {
			continue
		}

		for _, f := range n.Forks {
			rel, err := filepath.Rel(ps, f.Metadata.Path)
			if err != nil {
				t.Fatal(err)
			}

			out[rel] = coreFork{
				node:  strings.TrimPrefix(n.Fqname, psPrefix),
				args:  bindingArgs(t, f.Bindings.Argument),
				ready: bindingsReady(f.Bindings.Argument),
			}
		}
	}

	return out
}

func bindingsReady(bs []core.BindingInfo) bool {
	for _, b := range bs {
		if b.Waiting {
			return false
		}
	}

	return true
}

func bindingArgs(t *testing.T, bs []core.BindingInfo) json.RawMessage {
	t.Helper()

	m := make(core.MarshalerMap, len(bs))

	for _, b := range bs {
		if v, ok := b.Value.(json.Marshaler); ok {
			m[b.Id] = v
		} else {
			m[b.Id] = nil
		}
	}

	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal core bindings: %v", err)
	}

	return raw
}

// forkDir names f's fork dir as mrp does (core/fork.go:1011-1224
// ForkId.ForkIdString): lens[i] is how many forks part i ranges over under
// the parts before it.
func forkDir(f cg.Fork, lens []int) string {
	const def = "fork0"

	switch len(f) {
	case 0:
		return def
	case 1:
		if f[0].Index.Mode() == syntax.ModeMapCall {
			return "fork_" + url.PathEscape(f[0].Index.MapKey())
		}

		return "fork" + strconv.Itoa(f[0].Index.ArrayIndex())
	}

	var b strings.Builder
	if forkDirFrom(&b, f, lens, 0) {
		return def
	}

	return b.String()
}

func forkDirFrom(b *strings.Builder, f cg.Fork, lens []int, start int) bool {
	index, dim := 0, 1

	for i, p := range f[start:] {
		alen := lens[start+i]
		if alen == 0 {
			return index == 0
		}

		if p.Index.Mode() == syntax.ModeMapCall {
			if i != 0 {
				writeForkIndex(b, dim, index)
				b.WriteByte('/')

				return forkDirFrom(b, f, lens, start+i+1)
			}

			b.WriteString("fork_" + url.PathEscape(p.Index.MapKey()))

			if start+i < len(f)-1 {
				b.WriteByte('/')

				if forkDirFrom(b, f, lens, start+i+1) {
					b.WriteString("fork0")
				}
			}

			return false
		}

		if alen != p.Root.Split().Source.ArrayLength() && alen > 1 && i != 0 {
			writeForkIndex(b, dim, index)
			b.WriteByte('_')

			index, dim = 0, 1
		}

		index += dim * p.Index.ArrayIndex()
		dim *= alen
	}

	if index == 0 && b.Len() == 0 {
		return true
	}

	writeForkIndex(b, dim, index)

	return false
}

func writeForkIndex(b *strings.Builder, dim, index int) {
	if dim < 10 && index == 0 {
		b.WriteString("fork0")

		return
	}

	b.WriteString("fork")

	id := strconv.Itoa(index)
	for x := len(id); x < len(strconv.Itoa(dim-1)); x++ {
		b.WriteByte('0')
	}

	b.WriteString(id)
}

// leafMode says how normJSON renders a string naming an existing file:
// leafNamed keeps its basename, which a stage sees (#284); leafContent keeps
// only its content, for mrp's pipeline outs, whose files mrp renames into
// outs/.
type leafMode int

const (
	leafNamed leafMode = iota
	leafContent
)

// normJSON re-encodes raw as mrp writes metadata (json.MarshalIndent with a
// four-space indent), keeping object key order and number lexemes, with every
// absolute path to an existing file or directory replaced by its digest: the
// two runtimes stage the same file at different paths.
func normJSON(t *testing.T, raw []byte, mode leafMode) string {
	t.Helper()

	v, err := types.DecodeJSON(raw)
	if err != nil {
		t.Fatalf("decode %.80s: %v", raw, err)
	}

	out, err := json.MarshalIndent(normValue(t, v, mode), "", "    ")
	if err != nil {
		t.Fatal(err)
	}

	return string(out)
}

func normValue(t *testing.T, v any, mode leafMode) any {
	t.Helper()

	switch x := v.(type) {
	case string:
		return normLeaf(t, x, mode)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normValue(t, e, mode)
		}

		return out
	case types.Object:
		out := make(types.Object, len(x))
		for i, m := range x {
			out[i] = types.Member{Key: m.Key, Value: normValue(t, m.Value, mode)}
		}

		return out
	default:
		return v
	}
}

func normLeaf(t *testing.T, s string, mode leafMode) string {
	t.Helper()

	if !filepath.IsAbs(s) {
		return s
	}

	if _, err := os.Stat(s); err != nil {
		return s
	}

	h := sha256.New()

	err := filepath.WalkDir(s, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		rel, _ := filepath.Rel(s, p)

		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}

		if _, err := fmt.Fprintf(h, "%s\x00%d\x00", rel, len(b)); err != nil {
			return err
		}

		_, err = h.Write(b)

		return err
	})
	if err != nil {
		t.Fatalf("digest %s: %v", s, err)
	}

	tag := "<file sha256:" + hex.EncodeToString(h.Sum(nil))[:16]
	if mode == leafNamed {
		tag += " " + filepath.Base(s)
	}

	return tag + ">"
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}
