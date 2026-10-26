//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// uuidPipeline is a split stage and a stage after it, every job of which
// prints the pipestance uuid as the adapter reports it and MRO_UUID as its
// environment holds it.
const uuidPipeline = `stage SPLIT_UUID(
    in  int[] xs,
    out int   n,
    src py    "stages/split_uuid",
) split (
    in  int   x,
)

stage MAIN_UUID(
    in  int n,
    out int m,
    src py  "stages/main_uuid",
)

pipeline UUIDS(
    in  int[] xs,
    out int   m,
)
{
    call SPLIT_UUID(
        xs = self.xs,
    )

    call MAIN_UUID(
        n = SPLIT_UUID.n,
    )

    return (
        m = MAIN_UUID.m,
    )
}

call UUIDS(
    xs = [
        1,
        2,
    ],
)
`

const uuidSay = `import os

import martian


def say():
    print("uuid=%s env=%s" % (martian.get_pipestance_uuid(), os.environ.get("MRO_UUID", "")))
`

const uuidSplitStage = uuidSay + `

def split(args):
    say()
    return {"chunks": [{"x": x} for x in args.xs]}


def main(args, outs):
    say()


def join(args, outs, chunk_defs, chunk_outs):
    say()
    outs.n = len(chunk_outs)
`

const uuidMainStage = uuidSay + `

def main(args, outs):
    say()
    outs.m = args.n
`

var uuidLine = regexp.MustCompile(`(?m)^uuid=(\S*) env=(\S*)$`)

// TestPipestanceUUID (#625): every job of a run sees one pipestance uuid,
// as martian.get_pipestance_uuid() and as MRO_UUID, as mrp sets it for each
// job (core/pipestance.go:879 SetUuid): Nextflow's session id, a UUID, or
// MRO_FORCE_UUID where the launch environment sets it, as mrp honours it
// (core/runtime.go:446). Not parallel: it sets the driver's environment.
func TestPipestanceUUID(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "pipeline.mro"), uuidPipeline)

	for name, code := range map[string]string{"split_uuid": uuidSplitStage, "main_uuid": uuidMainStage} {
		if err := os.MkdirAll(filepath.Join(dir, "stages", name), 0o755); err != nil {
			t.Fatal(err)
		}

		writeTestFile(t, filepath.Join(dir, "stages", name, "__init__.py"), code)
	}

	proj := transpileDir(t, dir)

	session := runUUIDs(t, proj, "session")
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(session) {
		t.Errorf("the run's uuid is %q, want a v4 UUID", session)
	}

	t.Setenv("MRO_FORCE_UUID", "forced-uuid")

	if got := runUUIDs(t, proj, "forced"); got != "forced-uuid" {
		t.Errorf("under MRO_FORCE_UUID the jobs saw %q, want forced-uuid", got)
	}
}

// runUUIDs runs proj and returns the one uuid every job of the run saw,
// failing unless each of its five jobs printed the same non-empty one.
func runUUIDs(t *testing.T, proj, name string) string {
	t.Helper()

	trace := filepath.Join(t.TempDir(), name+".txt")
	if err := runNextflow(t, proj, "-with-trace", trace); err != nil {
		t.Fatalf("%s run: %v", name, err)
	}

	var seen []string

	for _, row := range readTraceColumns(t, trace, "name", "workdir") {
		out, err := os.ReadFile(filepath.Join(row[1], ".command.out"))
		if err != nil {
			t.Fatalf("read %s's stdout: %v", row[0], err)
		}

		for _, m := range uuidLine.FindAllStringSubmatch(string(out), -1) {
			if m[1] != m[2] {
				t.Errorf("%s run, %s: the adapter reports %q, MRO_UUID is %q", name, row[0], m[1], m[2])
			}

			seen = append(seen, m[2])
		}
	}

	slices.Sort(seen)

	if len(seen) != 5 || seen[0] == "" || seen[0] != seen[len(seen)-1] {
		t.Fatalf("%s run: the jobs saw the uuids %s, want one non-empty uuid in each of the 5 jobs", name, strings.Join(seen, ", "))
	}

	return seen[0]
}
