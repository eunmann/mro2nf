package wide

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// Roots is the pipeline ROOTS, of many fork roots the entry values feed,
// which PLAN's records fork (#509): enabled map calls E<i> of INC over
// entry inputs a<i> of different lengths, disabled ones D<j> over ds,
// which fork but run no task, and nested map calls N<k> of FROM_ENTRY over
// xss<k>, each a root whose forks each gather a map call over an entry
// root within it, of a different size. ROOTS returns the E<i>'s outputs as
// ys and the N<k>'s as ns.
func Roots(enabled, disabled, nested int) Pipeline {
	var b strings.Builder

	b.WriteString(declarations)
	b.WriteString(fromEntry)

	ins := append(make([]string, 0, 2+enabled+nested), "    in  int[]   ds,\n", "    in  bool    off,\n")
	args := append(make([]string, 0, 2+enabled+nested), "    ds  = [0],\n", "    off = true,\n")

	var calls strings.Builder

	ys := make([]string, enabled)
	for i := range ys {
		ins = append(ins, fmt.Sprintf("    in  int[]   a%d,\n", i))
		args = append(args, fmt.Sprintf("    a%d = %s,\n", i, intList(rootValues(i))))
		fmt.Fprintf(&calls, "    map call INC as E%d(\n        x = split self.a%d,\n    )\n\n", i, i)
		ys[i] = fmt.Sprintf("E%d.y", i)
	}

	for j := range disabled {
		fmt.Fprintf(&calls, "    map call INC as D%d(\n        x = split self.ds,\n    ) using (\n        disabled = self.off,\n    )\n\n", j)
	}

	ns := make([]string, nested)
	for k := range ns {
		ins = append(ins, fmt.Sprintf("    in  int[][] xss%d,\n", k))
		args = append(args, fmt.Sprintf("    xss%d = %s,\n", k, nestedValues(k)))
		fmt.Fprintf(&calls, "    map call FROM_ENTRY as N%d(\n        xs = split self.xss%d,\n    )\n\n", k, k)
		ns[k] = fmt.Sprintf("N%d.s", k)
	}

	b.WriteString("pipeline ROOTS(\n" + strings.Join(ins, ""))
	b.WriteString("    out int[][] ys,\n    out int[][] ns,\n)\n{\n")
	b.WriteString(calls.String())
	fmt.Fprintf(&b, "    return (\n        ys = [%s],\n        ns = [%s],\n    )\n}\n\n", strings.Join(ys, ", "), strings.Join(ns, ", "))
	b.WriteString("call ROOTS(\n" + strings.Join(args, "") + ")\n")

	p := Pipeline{
		Files:  map[string]string{"pipeline.mro": b.String()},
		Stages: enabled + disabled + 2*nested,
		Roots:  enabled + disabled + 2*nested,
	}

	for name, code := range stageCode() {
		p.Files[filepath.Join("stages", name, "__init__.py")] = code
	}

	return p
}

const (
	// rootLens is the most values of an E<i>'s a<i>.
	rootLens = 4
	// nestedLens bounds the values of an N<k> fork, and its forks less
	// minForks.
	nestedLens = 3
	minForks   = 2
	// stride keeps each entry input's values, and each fork's, apart.
	stride = 10
)

// rootValues is E<i>'s a<i>: 1 to rootLens values, by i.
func rootValues(i int) []int {
	xs := make([]int, i%rootLens+1)
	for m := range xs {
		xs[m] = stride*i + m
	}

	return xs
}

// nestedValues is N<k>'s xss<k>: 2 to 4 forks, by k, of 0 to 2 values
// each, so the gathers within them differ in size, one gathering none.
// Some fork has a value: mrp panics building a nested map call's forks
// when none has.
func nestedValues(k int) string {
	forks := make([]string, k%nestedLens+minForks)
	for o := range forks {
		xs := make([]int, (o+k)%nestedLens)
		for m := range xs {
			xs[m] = stride*stride*k + stride*o + m
		}

		forks[o] = intList(xs)
	}

	return "[" + strings.Join(forks, ", ") + "]"
}

func intList(xs []int) string {
	s := make([]string, len(xs))
	for i, x := range xs {
		s[i] = strconv.Itoa(x)
	}

	return "[" + strings.Join(s, ", ") + "]"
}

// fromEntry sums INC over xs, a map call within each fork of the map call
// over it.
const fromEntry = `pipeline FROM_ENTRY(
    in  int[] xs,
    out int   s,
)
{
    map call INC as SQ(
        x = split self.xs,
    )

    call SUM as TOTAL(
        xs = SQ.y,
    )

    return (
        s = TOTAL.y,
    )
}

`
