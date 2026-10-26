//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// groupingProbes run one driver step alone on params.n values and print the
// time the driver took, from the workflow's start to the end of its output,
// so neither the JVM's start nor the building of the values counts: JOIN on
// a split's chunk outs, GATHER on a gather's upstream outs, NEEDS on the
// upstream outs an instance reads both aligned and whole, which two patterns
// give it, CHUNKS on a split's chunks, ELEMENTS on a producer's fork
// elements, ZIP on the instances a producer's partner elements zip with,
// and OUTS's refBundles on the files the pipeline outs name by reference.
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
	"needs": `include { NEEDS } from './stage.nf'

workflow {
    def n = params.n as int
    def outs = (0..<n).collect { i -> [[i], [file("w/${i}/outs/y")]] }
    def t0 = System.nanoTime()
    NEEDS(channel.of([null, [0]]), channel.fromList(outs), [[[], [], []], [[], [0], [0]]], ['y']).count().view { _c -> "GROUPED_MS ${(System.nanoTime() - t0).intdiv(1000000)}" }
}
`,
	"chunks": `include { CHUNKS } from './stage.nf'

workflow {
    def n = params.n as int
    def res = file('resources.tsv')
    res.text = (0..<n).collect { i -> "chnk${i}\t1\t1\t\n" }.join('')
    def refs = file('chunk_refs')
    refs.text = ''
    def cs = (0..<n).collect { i -> file("w/chunks/chnk${i}") }
    def t0 = System.nanoTime()
    CHUNKS(channel.of([[0], [:], cs, null, res, refs, null, [], [], [], [], [], [], []])).count().view { _c -> "GROUPED_MS ${(System.nanoTime() - t0).intdiv(1000000)}" }
}
`,
	"elements": `include { ELEMENTS } from './stage.nf'

workflow {
    def n = params.n as int
    def ds = (0..<n).collect { i -> file("w/forks/r/${i}") }
    def t0 = System.nanoTime()
    ELEMENTS(channel.of([[], ds]), 'r', [-1], false).count().view { _c -> "GROUPED_MS ${(System.nanoTime() - t0).intdiv(1000000)}" }
}
`,
	"zip": `include { ZIP } from './stage.nf'

workflow {
    def n = params.n as int
    def inst = (0..<n).collect { i -> [[i], [], [], [i.toString()]] }
    def ds = (0..<n).collect { i -> file("w/forks/p/${i}") }
    def t0 = System.nanoTime()
    ZIP(channel.fromList(inst), channel.of([[], ds]), [], 'p', 0, 'absent').count().view { _c -> "GROUPED_MS ${(System.nanoTime() - t0).intdiv(1000000)}" }
}
`,
	"outs": `include { refBundles } from './stage.nf'

workflow {
    def n = params.n as int
    def items = (0..<n).collect { i -> [file("w/${i}/outs/y"), file("w/r${i}/f/0")] }
    def t0 = System.nanoTime()
    channel.of(items).map { xs -> refBundles(xs) }.count().view { _c -> "GROUPED_MS ${(System.nanoTime() - t0).intdiv(1000000)}" }
}
`,
}

var groupedMS = regexp.MustCompile(`GROUPED_MS (\d+)`)

// TestGroupingScalesLinearly (#588, #622): each driver step of
// groupingProbes runs in driver time linear in the values it takes, as the
// overhead rule requires. Each takes 5,000 values, then 100,000, each in a
// JVM of its own, and the larger run may take at most 20 times as long,
// linear time with no credit for the fixed cost both pay. Below some 10,000
// values that fixed cost hides a quadratic term, so the sizes are large:
// grouping measured about 3 times as long at 100,000 values, and Nextflow's
// groupBy, which is quadratic, 75 to 100 times, which leaves a slower or
// busier runner several-fold room on either side of the bound. A larger
// run still going past the bound and groupingStartup is stopped and fails,
// as a quadratic step would take tens of minutes to finish.
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

			smallMS, done := groupingMillis(t, proj, small, groupingSmallLimit)
			if !done {
				t.Fatalf("%s did not finish on %d values within %v", name, small, groupingSmallLimit)
			}

			bound := smallMS * large / small

			largeMS, done := groupingMillis(t, proj, large, time.Duration(bound)*time.Millisecond+groupingStartup)
			if !done {
				t.Errorf("%s took %d ms on %d values and did not finish on %d within %d ms and %v, more than linear", name, smallMS, small, large, bound, groupingStartup)

				return
			}

			verdict := fmt.Sprintf("%s took %d ms on %d values and %d ms on %d: %d times as many values took %.1f times as long",
				name, smallMS, small, largeMS, large, large/small, float64(largeMS)/float64(smallMS))

			if largeMS > bound {
				t.Error(verdict + ", more than linear")

				return
			}

			t.Log(verdict)
		})
	}
}

const (
	// groupingStartup is what a probe's run may take beyond the time it
	// prints: the JVM's start and the building of its values.
	groupingStartup = 2 * time.Minute
	// groupingSmallLimit stops a probe's smaller run that hangs.
	groupingSmallLimit = 10 * time.Minute
)

// groupingMillis runs proj's probe.nf on n values and returns the time it
// printed, or false where the run outlasted limit.
func groupingMillis(t *testing.T, proj string, n int, limit time.Duration) (int, bool) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), limit)
	defer cancel()

	cmd := exec.CommandContext(ctx, "nextflow", "run", "probe.nf", "--n", strconv.Itoa(n))
	cmd.Dir = proj
	cmd.Env = append(os.Environ(), "NXF_ANSI_LOG=false", "NXF_OFFLINE=true")
	cmd.WaitDelay = 10 * time.Second

	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return 0, false
	}

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

	return ms, true
}
