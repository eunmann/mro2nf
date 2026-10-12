// Package wide writes a synthetic Martian pipeline of many stage calls, of
// every shape the emitter wires: plain, forked over an entry value and over
// a stage's output, gathered, split, gated statically and at run time, all
// below one preflight, most in sub-pipelines and many in one pipeline. The
// project emitted for it must compile and run whatever its size (#503): the
// JVM caps each method Nextflow compiles a workflow into.
package wide

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Pipeline is a synthetic pipeline: its .mro and stage code by path, its
// stage calls, and the value of its one output, all.
type Pipeline struct {
	Files  map[string]string
	Stages int
	All    int
}

const (
	// blockStages is the stage calls of one BLOCK.
	blockStages = 11
	// wideX is WIDE's x.
	wideX = 1
	// fanWidth is FAN's n, the forks of each BLOCK's M1.
	fanWidth = 2
	// splitChunks is the chunks SPLIT_ADD splits into.
	splitChunks = 2
	treeFanout  = 2

	dirPerm  = 0o750
	filePerm = 0o600
)

// entryXs is WIDE's xs, the forks of each BLOCK's M2.
func entryXs() []int { return []int{1, 2} }

// New is the pipeline WIDE: a preflight, TREE, a pipeline of tree stage
// calls, blocks calls of BLOCK, and ALL, which sums their outputs.
func New(blocks, tree int) Pipeline {
	var b strings.Builder

	b.WriteString(declarations)
	all := writeTree(&b, tree)
	fmt.Fprintf(&b, block, fanWidth)

	b.WriteString("pipeline WIDE(\n    in  int   x,\n    in  int[] xs,\n    in  bool  off,\n    out int   all,\n)\n{\n")
	b.WriteString("    call CHECK(\n        x = self.x,\n    ) using (\n        preflight = true,\n    )\n\n")
	b.WriteString("    call TREE(\n        x = self.x,\n    )\n\n")

	refs := make([]string, 0, 1+blocks)
	refs = append(refs, "TREE.y")

	for i := range blocks {
		fmt.Fprintf(&b, "    call BLOCK as B%d(\n        x   = %d,\n        xs  = self.xs,\n        off = self.off,\n    )\n\n", i, i)
		refs = append(refs, fmt.Sprintf("B%d.y", i))
		all += blockTotal(i)
	}

	fmt.Fprintf(&b, "    call SUM as ALL(\n        xs = [%s],\n    )\n\n    return (\n        all = ALL.y,\n    )\n}\n\n", strings.Join(refs, ", "))
	fmt.Fprintf(&b, "call WIDE(\n    x   = %d,\n    xs  = %s,\n    off = true,\n)\n", wideX, strings.ReplaceAll(fmt.Sprint(entryXs()), " ", ", "))

	files := map[string]string{"pipeline.mro": b.String()}
	for name, code := range stageCode() {
		files[filepath.Join("stages", name, "__init__.py")] = code
	}

	return Pipeline{Files: files, Stages: 2 + tree + blocks*blockStages, All: all}
}

// blockTotal is the output of BLOCK called with x = i: the sum of A's y,
// G1's sum of M1's forks, G2's of M2's, S's sum of its chunks, and GATED's
// S.y+1 where FLAG holds it enabled, at an odd A.y; OFF is disabled.
func blockTotal(i int) int {
	a := i + 1
	t := a + splitChunks*a

	for k := range fanWidth {
		t += k + 1
	}

	for _, x := range entryXs() {
		t += x + 1
	}

	if a%2 != 0 {
		t += splitChunks*a + 1
	}

	return t
}

// writeTree writes TREE, n calls of INC, each but the first reading its
// parent in a tree of treeFanout children per call, so they outweigh one
// workflow, yet a run of them waits for only log n of them in turn, and
// returns its output: WIDE's x incremented once per call on the path to
// the last.
func writeTree(b *strings.Builder, n int) int {
	b.WriteString("pipeline TREE(\n    in  int x,\n    out int y,\n)\n{\n")

	ys := make([]int, n)

	for i := range n {
		if i == 0 {
			b.WriteString("    call INC as C0(\n        x = self.x,\n    )\n\n")
			ys[i] = wideX + 1

			continue
		}

		parent := (i - 1) / treeFanout
		fmt.Fprintf(b, "    call INC as C%d(\n        x = C%d.y,\n    )\n\n", i, parent)
		ys[i] = ys[parent] + 1
	}

	fmt.Fprintf(b, "    return (\n        y = C%d.y,\n    )\n}\n\n", n-1)

	return ys[n-1]
}

// Write writes p's files below dir.
func (p Pipeline) Write(dir string) error {
	for name, body := range p.Files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
			return fmt.Errorf("write wide pipeline: %w", err)
		}

		if err := os.WriteFile(path, []byte(body), filePerm); err != nil {
			return fmt.Errorf("write wide pipeline: %w", err)
		}
	}

	return nil
}

const declarations = `stage CHECK(
    in  int x,
    src py  "stages/check",
)

stage INC(
    in  int x,
    out int y,
    src py  "stages/inc",
)

stage SUM(
    in  int[] xs,
    out int   y,
    src py    "stages/sum",
)

stage FAN(
    in  int   n,
    out int[] xs,
    src py    "stages/fan",
)

stage FLAG(
    in  int  x,
    out bool off,
    src py   "stages/flag",
)

stage SPLIT_ADD(
    in  int x,
    out int y,
    src py  "stages/split_add",
) split (
    in  int v,
    out int w,
)

`

const block = `pipeline BLOCK(
    in  int   x,
    in  int[] xs,
    in  bool  off,
    out int   y,
)
{
    call INC as A(
        x = self.x,
    )

    call FAN(
        n = %d,
    )

    map call INC as M1(
        x = split FAN.xs,
    )

    call SUM as G1(
        xs = M1.y,
    )

    map call INC as M2(
        x = split self.xs,
    )

    call SUM as G2(
        xs = M2.y,
    )

    call SPLIT_ADD as S(
        x = A.y,
    )

    call FLAG(
        x = A.y,
    )

    call INC as GATED(
        x = S.y,
    ) using (
        disabled = FLAG.off,
    )

    call INC as OFF(
        x = 1,
    ) using (
        disabled = self.off,
    )

    call SUM as TOTAL(
        xs = [A.y, G1.y, G2.y, S.y, GATED.y, OFF.y],
    )

    return (
        y = TOTAL.y,
    )
}

`

func stageCode() map[string]string {
	return map[string]string{
		"check": fmt.Sprintf("def main(args, outs):\n    assert args.x == %d\n", wideX),
		"inc":   "def main(args, outs):\n    outs.y = args.x + 1\n",
		"sum":   "def main(args, outs):\n    outs.y = sum(x for x in args.xs if x is not None)\n",
		"fan":   "def main(args, outs):\n    outs.xs = list(range(args.n))\n",
		"flag":  "def main(args, outs):\n    outs.off = args.x % 2 == 0\n",
		"split_add": fmt.Sprintf("def split(args):\n    return {\"chunks\": [{\"v\": args.x}] * %d}\n\n\n", splitChunks) +
			"def main(args, outs):\n    outs.w = args.v\n\n\n" +
			"def join(args, outs, chunk_defs, chunk_outs):\n    outs.y = sum(o.w for o in chunk_outs)\n",
	}
}
