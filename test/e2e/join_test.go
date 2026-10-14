//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestJoinFailsOnAChunkCountMismatch (#579): JOIN groups a split's chunk
// outs by the split's chunk count, and a count of chunk outs other than it
// fails the run, naming the split's key, where a group short of its count
// would otherwise never reach the join and the run end without it. JOIN
// runs alone on chunk outs of one split of two chunks; with no split to
// join them to, a group of the right size ends the run as it should.
func TestJoinFailsOnAChunkCountMismatch(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java")

	for _, c := range []struct {
		name   string
		chunks []string
		want   string
	}{
		{"complete", []string{"chnk0", "chnk1"}, ""},
		{"short", []string{"chnk0"}, "Operator `groupBy` received too few values for grouping keys: [k]"},
		{"long", []string{"chnk0", "chnk1", "chnk2"}, "Operator `groupBy` received too many values for grouping key: [k] (expected 2)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			proj := transpile(t, "split_test")

			outs := make([]string, len(c.chunks))
			for i, name := range c.chunks {
				outs[i] = "[[chunks: 2], ['k'], file('" + name + "'), '']"
			}

			probe := "include { JOIN } from './stage.nf'\n\nworkflow {\n    JOIN(channel.empty(), channel.of(" + strings.Join(outs, ", ") + "))\n}\n"
			if err := os.WriteFile(filepath.Join(proj, "probe.nf"), []byte(probe), 0o644); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command("nextflow", "run", "probe.nf")
			cmd.Dir = proj
			cmd.Env = append(os.Environ(), "NXF_ANSI_LOG=false", "NXF_OFFLINE=true")

			out, err := cmd.CombinedOutput()

			switch {
			case c.want == "" && err != nil:
				t.Fatalf("nextflow run probe.nf: %v\n%s", err, out)
			case c.want != "" && err == nil:
				t.Fatalf("run succeeded on %d chunk outs of a split of 2; it must fail:\n%s", len(c.chunks), out)
			case !strings.Contains(string(out), c.want):
				t.Errorf("run output lacks %q:\n%s", c.want, out)
			}
		})
	}
}
