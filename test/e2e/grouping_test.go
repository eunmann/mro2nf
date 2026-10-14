//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// groupingProbes run JOIN or GATHER alone on one group of params.n values,
// a split's chunk outs or a gather's upstream outs, and print the time the
// driver took to group them, from the workflow's start to the end of its
// output, so neither the JVM's start nor the building of the values counts.
var groupingProbes = map[string]string{
	"join": `include { JOIN } from './stage.nf'

workflow {
    def n = params.n as int
    def chunks = (0..<n).collect { i -> [[chunks: n], ['k'], file("chnk${i}"), ''] }
    def t0 = System.nanoTime()
    JOIN(channel.empty(), channel.fromList(chunks)).count().view { _c -> "GROUPED_MS ${(System.nanoTime() - t0).intdiv(1000000)}" }
}
`,
	"gather": `include { GATHER } from './stage.nf'

workflow {
    def n = params.n as int
    def outs = (0..<n).collect { i -> [[i], [file("w/${i}/outs/y")]] }
    def t0 = System.nanoTime()
    GATHER(channel.of([0, []]), channel.fromList(outs), [], [], ['y']).count().view { _c -> "GROUPED_MS ${(System.nanoTime() - t0).intdiv(1000000)}" }
}
`,
}

var groupedMS = regexp.MustCompile(`GROUPED_MS (\d+)`)

// TestGroupingScalesLinearly (#588): JOIN and GATHER group a split's chunk
// outs and a gather's upstream in driver time linear in the group's size,
// as the overhead rule requires. Each groups 5,000 values, then 100,000,
// each in a JVM of its own, and the larger group may take at most 20 times
// as long, linear time with no credit for the fixed cost both pay. Below
// some 10,000 values that fixed cost hides a quadratic term, so the sizes
// are large: grouping measured about 3 times as long at 100,000 values, and
// Nextflow's groupBy, which is quadratic, 75 to 100 times, which leaves a
// slower or busier runner several-fold room on either side of the bound.
func TestGroupingScalesLinearly(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java")
	shardSkip(t)

	const small, large = 5000, 100000

	for name, probe := range groupingProbes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			proj := transpile(t, "split_test")
			if err := os.WriteFile(filepath.Join(proj, "probe.nf"), []byte(probe), 0o644); err != nil {
				t.Fatal(err)
			}

			smallMS := groupingMillis(t, proj, small)
			largeMS := groupingMillis(t, proj, large)
			verdict := fmt.Sprintf("%s grouped %d values in %d ms and %d in %d ms: %d times as many values took %.1f times as long",
				name, small, smallMS, large, largeMS, large/small, float64(largeMS)/float64(smallMS))

			if largeMS > smallMS*large/small {
				t.Error(verdict + ", more than linear")

				return
			}

			t.Log(verdict)
		})
	}
}

// groupingMillis runs proj's probe.nf on n values and returns the time it
// printed.
func groupingMillis(t *testing.T, proj string, n int) int {
	t.Helper()

	cmd := exec.Command("nextflow", "run", "probe.nf", "--n", strconv.Itoa(n))
	cmd.Dir = proj
	cmd.Env = append(os.Environ(), "NXF_ANSI_LOG=false", "NXF_OFFLINE=true")

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("nextflow run probe.nf --n %d: %v\n%s", n, err, out)
	}

	m := groupedMS.FindSubmatch(out)
	if m == nil {
		t.Fatalf("nextflow run probe.nf --n %d printed no GROUPED_MS:\n%s", n, out)
	}

	ms, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatal(err)
	}

	return ms
}
