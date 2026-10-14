//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// joinProbe runs JOIN alone on one split of two chunks, chnk0 and chnk1,
// with no chunk refs, and the chunk outs %s, and prints the key of each
// join it schedules.
const joinProbe = `include { JOIN } from './stage.nf'

workflow {
    def split = [['k'], [node: 'S'], [file('chnk0'), file('chnk1')], file('join.json'), file("${projectDir}/probe_resources.tsv"), file("${projectDir}/probe_refs"), null, [], [], [], [], [], [], []]
    JOIN(channel.of(split), channel.of(%s)).view { v -> "JOINED ${v[1]}" }
}
`

// TestJoinFailsOnAChunkCountMismatch (#579, #588): JOIN groups a split's
// chunk outs by the split's chunk count, and a count of chunk outs other
// than it fails the run, where grouping alone would drop a short group, or
// the outs past a full one, silently: a group of another size names the
// split's key and both counts, and full groups past the first fail the
// split's join as duplicates. A count equal to the chunks joins the split.
func TestJoinFailsOnAChunkCountMismatch(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java")
	shardSkip(t)

	for _, c := range []struct {
		name   string
		chunks []string
		want   string
	}{
		{"complete", []string{"chnk0", "chnk1"}, "JOINED [k]"},
		{"short", []string{"chnk0"}, "split [k] has 2 chunks, but its chunk outs came in a group of 1"},
		{"long", []string{"chnk0", "chnk1", "chnk0"}, "split [k] has 2 chunks, but its chunk outs came in a group of 1"},
		{"twice", []string{"chnk0", "chnk1", "chnk0", "chnk1"}, "Detected join operation duplicate emission on right channel -- offending element: key=[k];"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			proj := transpile(t, "split_test")

			outs := make([]string, len(c.chunks))
			for i, name := range c.chunks {
				outs[i] = "[[chunks: 2], ['k'], file('" + name + "'), '']"
			}

			for name, content := range map[string]string{
				"probe.nf":            strings.Replace(joinProbe, "%s", strings.Join(outs, ", "), 1),
				"probe_resources.tsv": "join\t1\t1\t\n",
				"probe_refs":          "",
			} {
				if err := os.WriteFile(filepath.Join(proj, name), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			cmd := exec.Command("nextflow", "run", "probe.nf")
			cmd.Dir = proj
			cmd.Env = append(os.Environ(), "NXF_ANSI_LOG=false", "NXF_OFFLINE=true")

			out, err := cmd.CombinedOutput()

			switch {
			case c.name == "complete" && err != nil:
				t.Fatalf("nextflow run probe.nf: %v\n%s", err, out)
			case c.name != "complete" && err == nil:
				t.Fatalf("run succeeded on %d chunk outs of a split of 2; it must fail:\n%s", len(c.chunks), out)
			case !strings.Contains(string(out), c.want):
				t.Errorf("run output lacks %q:\n%s", c.want, out)
			}
		})
	}
}
