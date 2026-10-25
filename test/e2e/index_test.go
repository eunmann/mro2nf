//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// indexScript publishes the JSON file params.f as one value, as an index
// writer of pipeline_outs.json would hold it, with Nextflow's workflow-output
// index writing it back out.
const indexScript = `workflow {
    main:
    outs = channel.value(new groovy.json.JsonSlurper().parseText(file(params.f).text))

    publish:
    outs = outs
}

output {
    outs {
        index { path 'outs.json' }
    }
}
`

// TestWorkflowIndexRespellsNumbers (#612) pins why OUTS, not Nextflow's
// workflow-output index, writes pipeline_outs.json: the index writes mrp's
// float_lexemes outs back with other numbers, 1E+21 for 1e+21 and 0.0 for
// -0.0, as Groovy's parsed values print. Once this fails, the index keeps
// them, and #612's other checks decide whether it can write the file.
func TestWorkflowIndexRespellsNumbers(t *testing.T) {
	t.Parallel()
	requireTools(t, "nextflow", "java")

	golden := filepath.Join(root, "testdata", "float_lexemes", "expected", "outs.json")

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.nf"), []byte(indexScript), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runNextflow(t, dir, "--f", golden, "-output-dir", "out"); err != nil {
		t.Fatal(err)
	}

	want, err := decodeOutsFile(golden, nil)
	if err != nil {
		t.Fatal(err)
	}

	index := filepath.Join(dir, "out", "outs.json")

	got, err := decodeOutsFile(index, nil)
	if err != nil {
		t.Fatal(err)
	}

	if cmp.Equal(want, got) {
		t.Errorf("the workflow-output index %s now writes mrp's outs as mrp does: revisit #612", index)
	}
}
