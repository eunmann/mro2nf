package emit_test

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/emit"
)

// processDecl matches an emitted process definition's opening line.
var processDecl = regexp.MustCompile(`(?m)^process (\w+) \{\n`)

// processScript is one triple-quoted script string of an emitted process.
type processScript struct {
	where string // fixture, mode, file and process, for failure messages
	text  string // the .nf source between the string's quotes
}

// emittedScripts emits every fixture with the given version, in each mode
// that emits process templates the others do not, and returns each process's
// script strings.
func emittedScripts(t *testing.T, version string) []processScript {
	t.Helper()

	fixtures, err := filepath.Glob("../../testdata/*/pipeline.mro")
	if err != nil || len(fixtures) == 0 {
		t.Fatalf("no fixtures found (err %v)", err)
	}

	modes := map[string]emit.Options{
		"default": {},
		"native":  {Native: true},
		"inline":  {InlinePipelines: true},
	}

	var out []processScript

	for _, mro := range fixtures {
		for mode, opts := range modes {
			opts.Version = version

			for path, src := range emitNFOpts(t, mro, opts) {
				where := fmt.Sprintf("%s (%s) %s", filepath.Base(filepath.Dir(mro)), mode, path)
				out = append(out, processScripts(t, where, src)...)
			}
		}
	}

	return out
}

// processScripts returns the script strings of every process in one .nf
// file. A process ends at the first column-0 `}`, as every template closes
// it, and its strings are the `"""` pairs after its `script:` label.
func processScripts(t *testing.T, where, src string) []processScript {
	t.Helper()

	var out []processScript

	for _, m := range processDecl.FindAllStringSubmatchIndex(src, -1) {
		name := src[m[2]:m[3]]
		body := src[m[1]:]

		if end := strings.Index(body, "\n}\n"); end >= 0 {
			body = body[:end]
		}

		_, script, ok := strings.Cut(body, "  script:\n")
		if !ok {
			t.Errorf("%s: process %s has no script block", where, name)

			continue
		}

		parts := strings.Split(script, `"""`)
		if len(parts) < 3 {
			t.Errorf("%s: process %s has no script string", where, name)

			continue
		}

		for i := 1; i < len(parts); i += 2 {
			out = append(out, processScript{where: where + " process " + name, text: parts[i]})
		}
	}

	return out
}

// TestEveryProcessScriptNamesTheBuild (#449): Nextflow keys a task's -resume
// cache on its script text and inputs, not on the mre binary that runs it, so
// every script string of every emitted process opens with the transpiler
// build. Without it, a task cached by another build is reused although that
// build's mre shapes its outputs differently.
func TestEveryProcessScriptNamesTheBuild(t *testing.T) {
	const version = "v9.9.9-3-gabcdef0-dirty"

	want := `# mro2nf \"` + version + `\"`
	scripts := emittedScripts(t, version)

	for _, s := range scripts {
		first, _, _ := strings.Cut(strings.TrimLeft(s.text, "\n"), "\n")
		if strings.TrimSpace(first) != want {
			t.Errorf("%s: script opens with %q, want the build key %q", s.where, first, want)
		}
	}

	if len(scripts) == 0 {
		t.Error("property is vacuous: no process scripts across the fixtures")
	}
}
