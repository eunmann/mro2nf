package emit

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/eunmann/martian-nextflow/internal/ir"
)

// healthOmicsParam is one entry in a HealthOmics parameter-template.json: a
// description plus whether the parameter is optional (absent => required).
type healthOmicsParam struct {
	Description string `json:"description"`
	Optional    bool   `json:"optional,omitempty"`
}

// writeHealthOmicsPackaging emits the artifacts AWS HealthOmics needs alongside
// the workflow definition: parameter-template.json (the run-time inputs the
// console/API prompts for) and package.sh (builds the upload zip, excluding the
// Docker build context which ships as the image instead).
func writeHealthOmicsPackaging(prog *ir.Program, outDir string) error {
	tmpl := map[string]healthOmicsParam{
		"container": {
			Description: "Private Amazon ECR image URI for the runtime (same region as the run); see Dockerfile",
		},
	}

	// Declare each entry input so HealthOmics accepts it as a run parameter —
	// undeclared parameters are rejected, which would make the param-override path
	// silently unavailable. File-bearing inputs are included: HealthOmics resolves
	// their S3 URIs (or s3:// prefixes for directories) into staged read-only files,
	// which genEntry routes through Nextflow staging.
	for _, p := range entryInParams(prog) {
		desc := fmt.Sprintf("pipeline input %q (optional; defaults to the value baked from the .mro)", p.Name)
		if hasFileLeaf(p, prog.Structs) {
			desc = fmt.Sprintf("pipeline file input %q: an S3 URI (or s3:// prefix for a directory) staged into the run; optional, defaults to the baked value", p.Name)
		}

		tmpl[p.Name] = healthOmicsParam{
			Description: desc,
			Optional:    true,
		}
	}

	data, err := json.MarshalIndent(tmpl, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal parameter template: %w", err)
	}

	if err := writeFile(filepath.Join(outDir, "parameter-template.json"), data); err != nil {
		return err
	}

	return writeFile(filepath.Join(outDir, "package.sh"), []byte(healthOmicsPackageScript))
}

// healthOmicsPackageScript zips the workflow for `aws omics create-workflow`. The
// runtime/ build context and the Dockerfile are excluded: they become the ECR
// image, not part of the workflow definition. _assets, modules, nulls, and
// entry_args ARE included — HealthOmics stages them with the run.
const healthOmicsPackageScript = `#!/usr/bin/env bash
# Package this project for AWS HealthOmics and register it.
#
#   1. Build + push the runtime image (see Dockerfile) to a private ECR repo in
#      the run's region, then pass its URI as the 'container' parameter.
#   2. Build the workflow zip (this script).
#   3. aws omics create-workflow --engine NEXTFLOW --main main.nf \
#        --definition-zip fileb://workflow.zip \
#        --parameter-template file://parameter-template.json
#
# Tasks run with no internet and a private-ECR-only container, so all tools must
# be baked into the image and all data passed as S3-URI run parameters.
set -euo pipefail
cd "$(dirname "$0")"
rm -f workflow.zip
zip -r workflow.zip . \
    -x 'runtime/*' -x 'Dockerfile' -x 'package.sh' -x 'work/*' -x '.nextflow/*' -x 'results/*'
echo "wrote workflow.zip"
`
