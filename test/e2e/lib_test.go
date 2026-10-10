//go:build e2e

package e2e

// Unit coverage for the shipped Groovy helper library (lib/Mro2nf.groovy, #49):
// a probe workflow exercises each helper against fixture JSON, run with the same
// Nextflow the rest of the suite uses. This pins the helpers' behavior directly,
// independent of a full pipeline run, so a regression in a helper is a precise
// failure rather than a mysterious downstream one.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLibHelpers exercises each Mro2nf helper against fixture JSON via a probe
// workflow, pinning their behavior directly (independent of a full pipeline).
func TestLibHelpers(t *testing.T) {
	requireTools(t, "nextflow", "java")

	// Transpile any fixture to obtain a project whose lib/ carries Mro2nf.groovy.
	proj := transpile(t, "file_min")

	// Probe bundles: disable gate (true/false), a chunk with resources, and a
	// keyed FORKBIND-style dir enumerated from forknames.json.
	writeBundleSidecar(t, filepath.Join(proj, "probe_on"), `{"disabled": true}`)
	writeBundleSidecar(t, filepath.Join(proj, "probe_off"), `{"disabled": false}`)
	writeBundleSidecar(t, filepath.Join(proj, "probe_null"), `{"disabled": null}`)
	writeBundleSidecar(t, filepath.Join(proj, "probe_chunk"), `{"resources": {"threads": 3}}`)
	writeFileT(t, filepath.Join(proj, "probe_join.json"), `{"mem_gb": 7}`)
	writeBundleSidecar(t, filepath.Join(proj, "probe_disabled"), `null`)

	forks := filepath.Join(proj, "probe_forks")
	if err := os.MkdirAll(forks, 0o755); err != nil {
		t.Fatalf("mkdir forks: %v", err)
	}

	writeFileT(t, filepath.Join(forks, "forknames.json"), `["fa","fb"]`)
	writeFileT(t, filepath.Join(forks, "fa"), "")
	writeFileT(t, filepath.Join(forks, "fb"), "")

	probe := `workflow {
    def on = file("${projectDir}/probe_on")
    def off = file("${projectDir}/probe_off")
    def nullp = file("${projectDir}/probe_null")
    def chunk = file("${projectDir}/probe_chunk")
    def joinf = file("${projectDir}/probe_join.json")
    def forks = file("${projectDir}/probe_forks")
    // A null disable flag must fail loudly (mrp errors), not coerce to false.
    def nullResult = "NO_THROW"
    try { Mro2nf.disabled(nullp) } catch (IllegalArgumentException e) { nullResult = "THREW" }
    println "R nullDisabled=" + nullResult
    // compareUtf8 must order by UTF-8 byte value (matching Go sort.Strings), so
    // the native entry-file flatten pairs with mre's Go type-walk for keys whose
    // Groovy natural (UTF-16) order would diverge — a supplementary-plane key
    // (F0 9F ...) sorts AFTER a BMP key (EF ...), unlike UTF-16 (surrogate < FFxx).
    def ukeys = ["😀x", "！y", "az"]
    println "R utf8sort=" + ukeys.sort(false) { a, b -> Mro2nf.compareUtf8(a, b) }.join(",")
    println "R disabled=" + Mro2nf.disabled(on) + "/" + Mro2nf.disabled(off)
    println "R chunkRes=" + Mro2nf.chunkRes(chunk).threads
    println "R parseJson=" + Mro2nf.parseJson(joinf).mem_gb
    println "R asList=" + Mro2nf.asList(5) + "/" + Mro2nf.asList([1, 2])
    println "R outerKey=" + Mro2nf.outerKey("a~b~c")
    println "R forkKeys=" + Mro2nf.forkTuples("K", forks).collect { it[0] }.join(",")
    // A disabled upstream's null bundle scatters as an empty collection.
    def disabledSrc = file("${projectDir}/probe_disabled/data.json")
    println "R nullDocFork=" + Mro2nf.forkElements(disabledSrc, 'xs', 'array').collect { it[0] }.join(",")
}
`
	writeFileT(t, filepath.Join(proj, "probe.nf"), probe)

	out := runNextflowCapture(t, proj, "probe.nf")

	for _, want := range []string{
		"R nullDisabled=THREW",
		"R utf8sort=az,！y,😀x", // UTF-8 byte order (== Go sort.Strings), NOT UTF-16
		"R disabled=true/false",
		"R chunkRes=3",
		"R parseJson=7",
		"R asList=[5]/[1, 2]",
		"R outerKey=a~b",
		"R forkKeys=K~fa,K~fb",
		"R nullDocFork=fork_none",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("helper probe missing %q in:\n%s", want, out)
		}
	}
}

// TestLibEntryFile pins Mro2nf.entryFile, the head-node vetting of each entry
// file-override leaf (#330): an s3:// URI on a cloud target, a local path on
// the local target, anything else only with the remote-input opt-in, which
// never admits a local path on a cloud target. An accepted value holding a
// character Nextflow's stage-in leaves for bash to expand is still rejected
// (#276). A rejection names the input and the scheme or character, never the
// value.
func TestLibEntryFile(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java")

	const (
		ok         = "ok"
		localPath  = "a local path is not allowed on target"
		mustBeBool = "params.mro2nf_allow_remote_inputs must be true or false"
	)

	cases := []struct {
		target, value, allow string // value and allow are Groovy literals
		want                 string
	}{
		{"local", "'/abs/x.txt'", "null", ok},
		{"local", "'rel/x.txt'", "null", ok},
		{"local", "'a:b.txt'", "null", ok},
		{"local", "'file:///etc/hostname'", "null", ok},
		{"local", "'http://169.254.169.254/latest/meta-data/'", "null", "entry input 'in': scheme 'http' is not allowed on target 'local'"},
		{"local", "'HTTPS://example.com/x'", "null", "scheme 'https'"},
		{"local", "'ftp://example.com/x'", "null", "scheme 'ftp'"},
		{"local", "'s3://b/k'", "null", "scheme 's3'"},
		{"local", "'s3:/b/k'", "null", "scheme 's3'"},
		{"local", "'http://example.com/x'", "true", ok},
		{"local", "'http://example.com/x'", "'true'", ok},
		{"local", "'http://example.com/x'", "'false'", "scheme 'http'"},
		{"local", "'http://example.com/x'", "'yes'", mustBeBool},
		{"local", "5", "null", "a file value must be a string, got Integer"},
		{"awsbatch", "'s3://b/k'", "null", ok},
		{"awsbatch", "'/abs/x'", "null", localPath + " 'awsbatch'"},
		{"awsbatch", "'rel/x'", "true", localPath + " 'awsbatch'"},
		{"awsbatch", "'file:///etc/passwd'", "null", localPath + " 'awsbatch'"},
		{"awsbatch", "'https://example.com/x'", "null", "scheme 'https' is not allowed on target 'awsbatch'"},
		{"awsbatch", "'https://example.com/x'", "true", ok},
		{"healthomics", "'s3://b/dir/'", "null", ok},
		{"healthomics", "'gs://b/k'", "null", "scheme 'gs' is not allowed on target 'healthomics'"},
		{"local", "'/in/a,b;c=d n~#%^@+.txt'", "null", ok},
		{"local", "'/in/s[1].txt'", "null", "entry input 'in': the path contains '['"},
		{"local", "'/in/s]1.txt'", "null", "contains ']'"},
		{"local", "'/in/s*.txt'", "null", "contains '*'"},
		{"local", "'/in/s?.txt'", "null", "contains '?'"},
		{"local", "'/in/t{1}.txt'", "null", "contains '{'"},
		{"local", "'/in/t}.txt'", "null", "contains '}'"},
		{"local", "'/in/$HOME.txt'", "null", "contains '$'"},
		{"local", "'/in/a\\tb.txt'", "null", "contains U+0009"},
		{"local", "'/run[1]/a.txt'", "null", "Nextflow staging limitation, see docs/TRANSPILER.md section 8"},
		{"awsbatch", "'s3://b/k*'", "null", "contains '*'"},
		{"local", "'http://example.com/x?sig=1'", "true", "contains '?'"},
	}

	var probe strings.Builder

	probe.WriteString(`def vet(String target, Object value, Object allow) {
    try { Mro2nf.entryFile('in', value, target, allow); return 'ok' } catch (IllegalArgumentException e) { return 'err ' + e.message }
}

workflow {
`)

	for i, c := range cases {
		fmt.Fprintf(&probe, "    println 'R%d ' + vet('%s', %s, %s)\n", i, c.target, c.value, c.allow)
	}

	probe.WriteString("}\n")

	proj := transpile(t, "file_min")
	writeFileT(t, filepath.Join(proj, "probe.nf"), probe.String())

	out := runNextflowCapture(t, proj, "probe.nf")

	for i, c := range cases {
		line := probeLine(out, fmt.Sprintf("R%d ", i))

		switch {
		case c.want == ok && line != ok:
			t.Errorf("entryFile(%s, %s, allow %s) = %q, want accepted", c.target, c.value, c.allow, line)
		case c.want != ok && (!strings.HasPrefix(line, "err ") || !strings.Contains(line, c.want)):
			t.Errorf("entryFile(%s, %s, allow %s) = %q, want an error containing %q", c.target, c.value, c.allow, line, c.want)
		case strings.Contains(line, "169.254") || strings.Contains(line, "example.com"):
			t.Errorf("entryFile(%s, %s) error echoes the value: %q", c.target, c.value, line)
		}
	}
}

// probeLine returns the rest of the first output line starting with prefix,
// or "" when none does.
func probeLine(out, prefix string) string {
	for line := range strings.SplitSeq(out, "\n") {
		if rest, found := strings.CutPrefix(line, prefix); found {
			return rest
		}
	}

	return ""
}

// runNextflowCapture runs `nextflow run <script>` in proj and returns the
// combined output (the driver's stdout carries a probe workflow's println).
func runNextflowCapture(t *testing.T, proj, script string) string {
	t.Helper()

	cmd := exec.Command("nextflow", "run", script)
	cmd.Dir = proj

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("nextflow run %s: %v\n%s", script, err, out)
	}

	return string(out)
}

// writeBundleSidecar creates dir/data.json with the given JSON — the minimal
// bundle shape the driver-side helpers read.
func writeBundleSidecar(t *testing.T, dir, dataJSON string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}

	writeFileT(t, filepath.Join(dir, "data.json"), dataJSON)
}
