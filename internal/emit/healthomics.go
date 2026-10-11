package emit

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/eunmann/mro2nf/internal/ir"
)

// healthOmicsParam is one entry in a HealthOmics parameter-template.json: a
// description plus whether the parameter is optional (absent => required).
type healthOmicsParam struct {
	Description string `json:"description"`
	Optional    bool   `json:"optional,omitempty"`
}

// writeHealthOmicsPackaging emits the artifacts AWS HealthOmics needs alongside
// the workflow definition: parameter-template.json (the run-time inputs the
// console/API prompts for) and package.sh (builds the upload zip from the
// healthOmicsZipEntries allowlist).
func writeHealthOmicsPackaging(prog *ir.Program, outDir string, native bool) error {
	tmpl := map[string]healthOmicsParam{
		"container": {
			Description: "Private Amazon ECR image URI for the runtime (same region as the run); see Dockerfile",
		},
	}

	// Declare each entry input so HealthOmics accepts it as a run parameter —
	// undeclared parameters are rejected, which would make the param-override path
	// silently unavailable. File-bearing inputs are included: HealthOmics resolves
	// their S3 URIs (or s3:// prefixes for directories) into staged read-only files,
	// which genEntry routes through Nextflow staging. Under -native the values are
	// baked and the workflow rejects a supplied entry parameter (genNativeEntry),
	// but the parameters stay declared — dropping them would make HealthOmics
	// reject the whole request instead of the run failing with the workflow's own
	// re-transpile message — with a description stating the bake.
	// allowRemoteParam is deliberately left undeclared: HealthOmics then rejects
	// it, so whoever starts a run cannot widen the file schemes it fetches.
	for _, p := range entryInParams(prog) {
		tmpl[p.Name] = healthOmicsParam{
			Description: entryParamDesc(prog, p, native),
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

	script := fmt.Sprintf(healthOmicsPackageScript, strings.Join(healthOmicsZipEntries(prog, native), " "))

	return writeFile(filepath.Join(outDir, "package.sh"), []byte(script))
}

// healthOmicsZipEntries returns the project paths the workflow zip holds:
// exactly what main.nf reads at run time. The runtime/ build context and the
// Dockerfile ship as the image, and nothing a run or the user leaves in the
// project dir (logs, params files, work/, .env) is ever packaged.
func healthOmicsZipEntries(prog *ir.Program, native bool) []string {
	entries := []string{"main.nf", "nextflow.config", "parameter-template.json", "modules", libDir, assetsDir}

	if native {
		entries = append(entries, "entry_resolved")
	} else {
		entries = append(entries, "entry_args")
	}

	if hasDisabledCall(prog) {
		entries = append(entries, "nulls")
	}

	return entries
}

// hasDisabledCall reports whether any call has a disable gate, so the project
// has the nulls/ bundles writeDisableArtifacts writes for them.
func hasDisabledCall(prog *ir.Program) bool {
	for _, p := range prog.Pipelines {
		for _, c := range p.Calls {
			if c.Disabled != nil {
				return true
			}
		}
	}

	return false
}

// entryParamDesc describes one entry input in the parameter template. Under
// -native the value was baked at transpile time and any supplied override fails
// the run at launch, so the description must say so instead of inviting one.
func entryParamDesc(prog *ir.Program, p ir.Param, native bool) string {
	if native {
		return fmt.Sprintf("pipeline input %q: baked at transpile time by -native and cannot be overridden; re-transpile to change it", p.Name)
	}

	if hasFileLeaf(p, prog.Structs) {
		return fmt.Sprintf("pipeline file input %q: an S3 URI (or s3:// prefix for a directory) staged into the run; optional, defaults to the baked value", p.Name)
	}

	return fmt.Sprintf("pipeline input %q (optional; defaults to the value baked from the .mro)", p.Name)
}

// healthOmicsPackageScript zips the workflow for `aws omics create-workflow`;
// %s is the healthOmicsZipEntries allowlist.
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
#
# The zip holds exactly the files the workflow reads, listed below; anything
# else in this directory (logs, params files, work/, .env) is never packaged.
set -euo pipefail
cd "$(dirname "$0")"
files=(%s)
for f in "${files[@]}"; do
    if [ ! -e "$f" ]; then
        echo "package.sh: $f is missing; re-run mro2nf" >&2
        exit 1
    fi
done
rm -f workflow.zip
zip -r workflow.zip "${files[@]}"
echo "wrote workflow.zip"
`
