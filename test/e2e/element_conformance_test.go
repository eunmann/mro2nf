//go:build e2e

package e2e

// Element-slice byte conformance (#124): the O(1) native scatter slices the
// split collection on the DRIVER (lib/Mro2nf.groovy elementTriples) while fork
// 0 and the classic path slice it in Go (forkbind -index via json.RawMessage).
// Contract 1 (the stage _args ABI) requires the two slices to be
// byte-identical — a JsonSlurper->JsonOutput round-trip is not (1e5 -> 1E+5,
// -0.0 -> 0.0, and a >int64 integer silently overflows Long), so the driver
// must carry each element's RAW substring. This test drives the ACTUAL Groovy
// slice under real Nextflow over a numeric-edge corpus and asserts byte
// identity at both seams: the sliced element bytes against Go's raw slice,
// and the final _args bundle bytes of `mre forkbind -elementfile` against the
// `-index` write, for array and map forks.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// edgeLexemes are JSON value lexemes a JsonSlurper->JsonOutput round-trip
// perturbs (or has perturbed): exponent forms, negative zero, int64
// boundaries, a >int64 integer, high-precision decimals, plus controls
// (plain scalars, strings with escapes/HTML chars, composites with interior
// whitespace, null, bool).
var edgeLexemes = []string{
	`1e5`,
	`-0.0`,
	`9223372036854775807`,
	`-9223372036854775808`,
	`12345678901234567890`,
	`0.1000000000000000055511151231257827`,
	`2.5e-3`,
	`42.0`,
	`1E2`,
	`-1.5e-300`,
	`7`,
	`"café"`,
	`"a\"[{,}]"`,
	`"<&>"`,
	`{ "n" : 1e5 }`,
	`[1e5,-0.0]`,
	`null`,
	`true`,
}

// edgeMapEntries is the map-fork corpus: edge lexeme values under keys whose
// UTF-8 sort order (a < z < é < 🎉, an astral surrogate-pair key) exercises
// the driver's compareUtf8 against Go's sort.Strings, including keys Go's
// json.Marshal stores ESCAPED in data.json ("a&b" -> "a\u0026b",
// "x<y" -> "x\u003cy", U+2028 -> "\u2028"). Each escaped key has a partner
// key ("aZb", "x=y", "l~s") that sorts between its raw and decoded positions,
// so a scanner that sorted RAW key substrings instead of decoded keys would
// emit a different fork order — and every value is distinct, so any
// mispairing changes bytes at both seams. json.RawMessage values pass through
// json.Marshal verbatim (modulo compaction), keeping the numeric edge lexemes
// intact.
var edgeMapEntries = map[string]json.RawMessage{
	"a":        json.RawMessage(`1e5`),
	"a&b":      json.RawMessage(`1E2`),
	"aZb":      json.RawMessage(`-1.5e-300`),
	"l~s":      json.RawMessage(`-0.0`),
	"l\u2028s": json.RawMessage(`[1e5,-0.0]`),
	"x<y":      json.RawMessage(`9223372036854775807`),
	"x=y":      json.RawMessage(`12345678901234567890`),
	"z":        json.RawMessage(`2.5e-3`),
	"é":        json.RawMessage(`0.1000000000000000055511151231257827`),
	"🎉":        json.RawMessage(`{"n":1e5}`),
}

// edgeMapJSON renders the map corpus VIA Go's json.Marshal so the key escapes
// in data.json are authentic, then guards the corpus itself: the escape
// sequences must be present and raw-key order must diverge from decoded-key
// order, or the corpus would silently stop covering the decode-before-sort
// contract.
func edgeMapJSON(t *testing.T) string {
	t.Helper()

	b, err := json.Marshal(edgeMapEntries)
	if err != nil {
		t.Fatalf("marshal map corpus: %v", err)
	}

	for _, esc := range []string{`\u0026`, `\u003c`, `\u2028`} {
		if !strings.Contains(string(b), esc) {
			t.Fatalf("map corpus lost authentic escape %s in %s", esc, b)
		}
	}

	decoded := make([]string, 0, len(edgeMapEntries))
	rawOf := make(map[string]string, len(edgeMapEntries))

	for k := range edgeMapEntries {
		kb, err := json.Marshal(k)
		if err != nil {
			t.Fatalf("marshal corpus key %q: %v", k, err)
		}

		decoded = append(decoded, k)
		rawOf[k] = string(kb)
	}

	sort.Strings(decoded)

	rawSorted := append([]string(nil), decoded...)
	sort.Slice(rawSorted, func(i, j int) bool { return rawOf[rawSorted[i]] < rawOf[rawSorted[j]] })

	if slices.Equal(decoded, rawSorted) {
		t.Fatalf("map corpus no longer distinguishes raw-key from decoded-key sorting: %v", decoded)
	}

	return string(b)
}

// malformedJSON are data.json corruptions the driver scanner must reject
// loudly (throw) rather than emit a truncated element: an unterminated
// string, a truncated composite, and an unclosed root object.
var malformedJSON = map[string]string{
	"bad_unterminated": `{"xs":["abc`,
	"bad_truncated":    `{"xs":[1,2`,
	"bad_unclosed":     `{"xs":[1,2]`,
}

// elementProbe prints every [key, index, elementB64] triple the driver-side
// slice emits, plus the sentinel triples for null/missing/empty/wrong-kind
// sources ('x' prefixes the b64 so an empty payload is still visible).
const elementProbe = `workflow {
    def data = file("${projectDir}/probe_elems.json")
    Mro2nf.forkElements(data, 'xs', 'array').each { tr -> println 'CA ' + tr[0] + ' ' + tr[1] + ' ' + tr[2] }
    Mro2nf.forkElements(data, 'm', 'map').each { tr -> println 'CM ' + tr[0] + ' ' + tr[1] + ' ' + tr[2] }
    ['nullf', 'missing', 'earr', 'str', 'm'].each { f ->
        Mro2nf.forkElements(data, f, 'array').each { tr -> println 'CSA:' + f + ' ' + tr[0] + ' ' + tr[1] + ' x' + tr[2] }
    }
    ['emap', 'xs'].each { f ->
        Mro2nf.forkElements(data, f, 'map').each { tr -> println 'CSM:' + f + ' ' + tr[0] + ' ' + tr[1] + ' x' + tr[2] }
    }
    ['bad_unterminated', 'bad_truncated', 'bad_unclosed'].each { f ->
        try {
            def n = Mro2nf.forkElements(file("${projectDir}/" + f + '.json'), 'xs', 'array').size()
            println 'CE:' + f + ' returned ' + n
        } catch (IllegalArgumentException e) {
            println 'CE:' + f + ' threw'
        }
    }
}
`

// TestElementSliceByteConformance runs the real Groovy element slice over the
// corpus and asserts byte identity with the Go slice and with the -index
// forkbind path, plus the sentinel contract for degenerate sources.
func TestElementSliceByteConformance(t *testing.T) {
	requireTools(t, "nextflow", "java")

	proj := transpile(t, "file_min")

	xsJSON := "[" + strings.Join(edgeLexemes, ",") + "]"
	mJSON := edgeMapJSON(t)
	dataJSON := `{"factor":10,"xs":` + xsJSON + `,"m":` + mJSON +
		`,"nullf":null,"earr":[],"emap":{},"str":"not a collection"}`

	writeFileT(t, filepath.Join(proj, "probe_elems.json"), dataJSON)
	writeFileT(t, filepath.Join(proj, "probe.nf"), elementProbe)

	for name, content := range malformedJSON {
		writeFileT(t, filepath.Join(proj, name+".json"), content)
	}

	out := runNextflowCapture(t, proj, "probe.nf")

	arrElems := decodeProbeElements(t, out, "CA ")
	mapElems := decodeProbeElements(t, out, "CM ")

	// Seam 1: each driver-sliced element must equal Go's raw slice of the same
	// collection — exactly the bytes ResolveForks embeds on the -index path.
	var xs []json.RawMessage
	if err := json.Unmarshal([]byte(xsJSON), &xs); err != nil {
		t.Fatalf("slice xs corpus: %v", err)
	}

	if len(arrElems) != len(xs) {
		t.Fatalf("driver sliced %d array elements, want %d", len(arrElems), len(xs))
	}

	for i := range xs {
		if !bytes.Equal(arrElems[i], xs[i]) {
			t.Errorf("array element %d: driver slice %q, want Go slice %q", i, arrElems[i], xs[i])
		}
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(mJSON), &m); err != nil {
		t.Fatalf("slice map corpus: %v", err)
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	if len(mapElems) != len(keys) {
		t.Fatalf("driver sliced %d map elements, want %d", len(mapElems), len(keys))
	}

	for i, k := range keys {
		if !bytes.Equal(mapElems[i], m[k]) {
			t.Errorf("map element %d (key %q): driver slice %q, want Go slice %q", i, k, mapElems[i], m[k])
		}
	}

	// Sentinel contract: null, missing, empty, and wrong-kind sources yield the
	// single index -1 sentinel with an empty element.
	for _, want := range []string{
		"CSA:nullf fork_none -1 x",
		"CSA:missing fork_none -1 x",
		"CSA:earr fork_none -1 x",
		"CSA:str fork_none -1 x",
		"CSA:m fork_none -1 x",
		"CSM:emap fork_none -1 x",
		"CSM:xs fork_none -1 x",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("probe output missing sentinel %q in:\n%s", want, out)
		}
	}

	// Loudness contract: a malformed data.json must make the scanner THROW —
	// never emit a truncated element (a "returned N" line means it sliced).
	for name := range malformedJSON {
		if !strings.Contains(out, "CE:"+name+" threw") {
			t.Errorf("scanner did not throw on %s in:\n%s", name, out)
		}
	}

	// Seam 2: the final _args bundle bytes through the real forkbind binary —
	// the -elementfile write fed the driver's element must equal the -index
	// write for every fork (what production mixes: fi==0 runs -index, fi>0
	// runs -elementfile).
	assertForkbindParity(t, dataJSON, "xs", "", arrElems)
	assertForkbindParity(t, dataJSON, "m", "map", mapElems)
}

// decodeProbeElements collects the base64 elements from the probe lines with
// the given prefix, asserting each line carries the expected sequential fork
// key and index.
func decodeProbeElements(t *testing.T, out, prefix string) [][]byte {
	t.Helper()

	var elems [][]byte

	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}

		f := strings.Fields(strings.TrimPrefix(line, prefix))
		if len(f) != 3 {
			t.Fatalf("malformed probe line %q", line)
		}

		wantKey := fmt.Sprintf("fork_%05d", len(elems))
		if f[0] != wantKey || f[1] != strconv.Itoa(len(elems)) {
			t.Fatalf("probe line %q: want key %s index %d", line, wantKey, len(elems))
		}

		b, err := base64.StdEncoding.DecodeString(f[2])
		if err != nil {
			t.Fatalf("decode element %q: %v", line, err)
		}

		elems = append(elems, b)
	}

	if len(elems) == 0 {
		t.Fatalf("no %q probe lines in:\n%s", prefix, out)
	}

	return elems
}

// assertForkbindParity runs the real `mre forkbind` twice per fork — once fed
// the driver's pre-sliced element (-elementfile), once resolving the whole
// collection (-index i) — and asserts the two _args bundles are byte-identical.
func assertForkbindParity(t *testing.T, dataJSON, field, mapMode string, elems [][]byte) {
	t.Helper()

	work := t.TempDir()
	specPath := filepath.Join(work, "spec.json")
	writeFileT(t, specPath,
		`{"v":{"ref":{"kind":"self","id":"`+field+`","output":""},"split":true},"f":{"ref":{"kind":"self","id":"factor","output":""}}}`)

	pipeargs := filepath.Join(work, "pipeargs")
	writeBundleSidecar(t, pipeargs, dataJSON)

	for i, elem := range elems {
		elemPath := filepath.Join(work, fmt.Sprintf("elem_%d.json", i))
		writeFileT(t, elemPath, string(elem))

		byElem := filepath.Join(work, fmt.Sprintf("by_elem_%d", i))
		byIndex := filepath.Join(work, fmt.Sprintf("by_index_%d", i))

		runMre(t, work, "forkbind", "-spec", specPath, "-pipeargs", pipeargs,
			"-elementfile", elemPath, "-o", byElem)

		idxArgs := []string{"forkbind", "-spec", specPath, "-pipeargs", pipeargs,
			"-index", strconv.Itoa(i), "-o", byIndex}
		if mapMode != "" {
			idxArgs = append(idxArgs, "-mapmode", mapMode)
		}

		runMre(t, work, idxArgs...)

		got := readFileT(t, filepath.Join(byElem, "data.json"))
		want := readFileT(t, filepath.Join(byIndex, "data.json"))

		if !bytes.Equal(got, want) {
			t.Errorf("%s fork %d: -elementfile args %q != -index args %q", field, i, got, want)
		}
	}
}

// runMre runs the built mre binary in dir, failing the test on any error.
func runMre(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command(filepath.Join(root, "mre"), args...)
	cmd.Dir = dir

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mre %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// readFileT reads a file, failing the test on error.
func readFileT(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return data
}
