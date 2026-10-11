//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

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

	// mrp leaves a symlink in the job's files dir to each output it moved
	// into outs/, and WalkDir does not follow a root that is one.
	walkRoot, err := filepath.EvalSymlinks(s)
	if err != nil {
		t.Fatalf("resolve %s: %v", s, err)
	}

	h := sha256.New()

	err = filepath.WalkDir(walkRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		rel, _ := filepath.Rel(walkRoot, p)

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
