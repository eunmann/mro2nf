//go:build e2e

package e2e

// Nested disable-gate conformance (#209): the native disable gate reads a
// projected disable flag (e.g. config.disable_count) on the DRIVER via
// Mro2nf.disabledField's dotted-path walk, instead of spending a DISABLE task.
// This drives the ACTUAL Groovy walk under real Nextflow over a nested sidecar
// and asserts: a single top-level flag, a nested struct path (true and false),
// and loud failure (throw) when an intermediate is missing/null or a scalar
// instead of a bool — the null case matching mrp's "disabled bound to a null
// value" refusal rather than silently running a branch.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// disabledProbe evaluates Mro2nf.disabledField for a set of paths against a
// nested sidecar and prints each result, catching the loud-failure cases so a
// throw is observable rather than aborting the run.
const disabledProbe = `workflow {
    def data = file("${projectDir}/gate.json")
    ['on', 'off', 'cfg.flag_on', 'cfg.flag_off', 'cfg.deep.flag'].each { p ->
        try { println 'OK ' + p + ' ' + Mro2nf.disabledField(data, p) }
        catch (IllegalArgumentException e) { println 'THROW ' + p }
    }
    // Loud-failure paths: a null flag, a null intermediate, a missing field, and
    // a scalar where an object is expected — each must THROW, never resolve false.
    ['nullflag', 'nullcfg.flag', 'cfg.missing', 'scalar.flag'].each { p ->
        try { println 'OK ' + p + ' ' + Mro2nf.disabledField(data, p) }
        catch (IllegalArgumentException e) { println 'THROW ' + p }
    }
    // disabledDir walks the same dotted path over a bundle DIRECTORY's data.json —
    // the keyed disable gate's read (a nested self path now routes here natively).
    def dir = file("${projectDir}/gatedir")
    ['on', 'cfg.flag_on', 'cfg.flag_off'].each { p ->
        try { println 'DIR ' + p + ' ' + Mro2nf.disabledDir(dir, p) }
        catch (IllegalArgumentException e) { println 'DIRTHROW ' + p }
    }
    try { println 'DIR scalar.flag ' + Mro2nf.disabledDir(dir, 'scalar.flag') }
    catch (IllegalArgumentException e) { println 'DIRTHROW scalar.flag' }
}
`

func TestDisabledFieldPathConformance(t *testing.T) {
	requireTools(t, "nextflow", "java")

	proj := transpile(t, "file_min")

	// A sidecar with a top-level flag, a nested struct (one level and two), a null
	// flag, a null struct, and a scalar-where-object-expected.
	gate := `{
        "on": true,
        "off": false,
        "cfg": {"flag_on": true, "flag_off": false, "deep": {"flag": true}},
        "nullflag": null,
        "nullcfg": null,
        "scalar": 7
    }`
	writeFileT(t, filepath.Join(proj, "gate.json"), gate)
	// disabledDir reads a bundle DIRECTORY's data.json (the keyed gate's shape).
	if err := os.MkdirAll(filepath.Join(proj, "gatedir"), 0o755); err != nil {
		t.Fatal(err)
	}

	writeFileT(t, filepath.Join(proj, "gatedir", "data.json"), gate)
	writeFileT(t, filepath.Join(proj, "probe.nf"), disabledProbe)

	out := runNextflowCapture(t, proj, "probe.nf")

	want := map[string]string{
		"OK on true":            "single top-level flag",
		"OK off false":          "single top-level flag (false)",
		"OK cfg.flag_on true":   "nested struct path (true)",
		"OK cfg.flag_off false": "nested struct path (false)",
		"OK cfg.deep.flag true": "two-level nested path",
		"THROW nullflag":        "a null flag must fail loudly, not run",
		"THROW nullcfg.flag":    "a null intermediate must fail loudly",
		"THROW cfg.missing":     "a missing field resolves to null -> fail loudly",
		"THROW scalar.flag":     "a scalar where an object is expected must fail loudly",
		// disabledDir (bundle-directory read) walks the same dotted path — the keyed
		// disable gate's native read for a nested self path.
		"DIR on true":            "disabledDir single field",
		"DIR cfg.flag_on true":   "disabledDir nested path (true)",
		"DIR cfg.flag_off false": "disabledDir nested path (false)",
		"DIRTHROW scalar.flag":   "disabledDir loud-fails on a scalar intermediate",
	}

	for line, why := range want {
		if !strings.Contains(out, line) {
			t.Errorf("disabledField conformance: missing %q (%s)\n--- output ---\n%s", line, why, out)
		}
	}
}
