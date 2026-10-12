// Package wide writes a synthetic Martian pipeline of many stage calls, of
// every shape the emitter wires: plain, forked over an entry value and over
// a stage's output, gathered, split, gated statically and at run time, all
// below one preflight, most in sub-pipelines, many in one pipeline and many
// reading a sub-pipeline's inputs from as many stages outside it. The
// project emitted for it must compile and run whatever its size (#503): the
// JVM caps each method Nextflow compiles a workflow into, and each call
// into 255 arguments.
package wide

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Pipeline is a synthetic pipeline: its .mro and stage code by path, its
// stage calls, its entry-fed fork roots, and WIDE's values of its outputs,
// all and, with a FANIN, rs.
type Pipeline struct {
	Files  map[string]string
	Stages int
	Roots  int
	All    int
	Rs     []int
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
	// faninGroup is the calls of FANIN each of its sums reads.
	faninGroup = 10

	dirPerm  = 0o750
	filePerm = 0o600
)

// entryXs is WIDE's xs, the forks of each BLOCK's M2.
func entryXs() []int { return []int{1, 2} }

// New is the pipeline WIDE: a preflight, TREE, a pipeline of tree stage
// calls, blocks calls of BLOCK, with fanin over 0 fanin calls R<i> and
// FANIN, which reads each, and ALL, which sums their outputs. WIDE returns
// ALL's output and, with a FANIN, each R<i>'s.
func New(blocks, tree, fanin int) Pipeline {
	var b strings.Builder

	b.WriteString(declarations)
	all := writeTree(&b, tree)
	fmt.Fprintf(&b, block, fanWidth)

	if fanin > 0 {
		writeFanin(&b, fanin)
	}

	b.WriteString("pipeline WIDE(\n    in  int   x,\n    in  int[] xs,\n    in  bool  off,\n    out int   all,\n")

	if fanin > 0 {
		b.WriteString("    out int[] rs,\n")
	}

	b.WriteString(")\n{\n")
	b.WriteString("    call CHECK(\n        x = self.x,\n    ) using (\n        preflight = true,\n    )\n\n")
	b.WriteString("    call TREE(\n        x = self.x,\n    )\n\n")

	refs := []string{"TREE.y"}

	for i := range blocks {
		fmt.Fprintf(&b, "    call BLOCK as B%d(\n        x   = %d,\n        xs  = self.xs,\n        off = self.off,\n    )\n\n", i, i)
		refs = append(refs, fmt.Sprintf("B%d.y", i))
		all += blockTotal(i)
	}

	p := Pipeline{Stages: 2 + tree + blocks*blockStages, Roots: blocks}
	outs := "        all = ALL.y,\n"

	if fanin > 0 {
		rs := callFanin(&b, fanin)
		refs = append(refs, "FANIN.y")
		outs += "        rs  = [" + strings.Join(rs, ", ") + "],\n"
		p.Stages += 2*fanin + faninGroups(fanin) + 1

		for i := range fanin {
			r := i + 1
			p.Rs = append(p.Rs, r)
			all += r + 1
		}
	}

	fmt.Fprintf(&b, "    call SUM as ALL(\n        xs = [%s],\n    )\n\n    return (\n%s    )\n}\n\n", strings.Join(refs, ", "), outs)
	fmt.Fprintf(&b, "call WIDE(\n    x   = %d,\n    xs  = %s,\n    off = true,\n)\n", wideX, strings.ReplaceAll(fmt.Sprint(entryXs()), " ", ", "))

	p.Files = map[string]string{"pipeline.mro": b.String()}
	for name, code := range stageCode() {
		p.Files[filepath.Join("stages", name, "__init__.py")] = code
	}

	p.All = all

	return p
}

// faninGroups is the sums FANIN's calls are summed in, so no one call of
// it reads more than a few others.
func faninGroups(n int) int { return (n + faninGroup - 1) / faninGroup }

// writeFanin writes FANIN, n calls of INC, F<i> reading FANIN's x<i>, a
// SUM G<g> per faninGroup of them and FSUM, which sums those: a pipeline
// whose call reads n channels.
func writeFanin(b *strings.Builder, n int) {
	b.WriteString("pipeline FANIN(\n")

	for i := range n {
		fmt.Fprintf(b, "    in  int x%d,\n", i)
	}

	b.WriteString("    out int y,\n)\n{\n")

	for i := range n {
		fmt.Fprintf(b, "    call INC as F%d(\n        x = self.x%d,\n    )\n\n", i, i)
	}

	gs := make([]string, faninGroups(n))
	for g := range gs {
		fs := make([]string, 0, faninGroup)
		for i := g * faninGroup; i < min(n, (g+1)*faninGroup); i++ {
			fs = append(fs, fmt.Sprintf("F%d.y", i))
		}

		fmt.Fprintf(b, "    call SUM as G%d(\n        xs = [%s],\n    )\n\n", g, strings.Join(fs, ", "))
		gs[g] = fmt.Sprintf("G%d.y", g)
	}

	fmt.Fprintf(b, "    call SUM as FSUM(\n        xs = [%s],\n    )\n\n    return (\n        y = FSUM.y,\n    )\n}\n\n", strings.Join(gs, ", "))
}

// callFanin writes n calls of INC, R<i> of x = i, and FANIN's call reading
// each, and returns the R<i>'s outputs.
func callFanin(b *strings.Builder, n int) []string {
	rs := make([]string, n)

	for i := range n {
		fmt.Fprintf(b, "    call INC as R%d(\n        x = %d,\n    )\n\n", i, i)
		rs[i] = fmt.Sprintf("R%d.y", i)
	}

	b.WriteString("    call FANIN(\n")

	for i := range n {
		fmt.Fprintf(b, "        x%d = R%d.y,\n", i, i)
	}

	b.WriteString("    )\n\n")

	return rs
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
