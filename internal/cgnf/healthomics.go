package cgnf

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
)

// templateParam is one run parameter of a HealthOmics parameter template;
// HealthOmics rejects a run param the template does not declare.
type templateParam struct {
	Description string `json:"description"`
	Optional    bool   `json:"optional"`
}

// parameterTemplate is parameter-template.json: every entry input and the
// run's mrp --overrides file, which only resizes jobs, each optional, and
// none of the project's other params, so a HealthOmics run can neither swap
// the image the package pins nor widen the file values it stages.
func (p *project) parameterTemplate() (string, error) {
	tmpl := map[string]templateParam{
		"mro2nf_overrides": {
			Description: "an s3:// URI of mrp's --overrides file, over each stage's resources",
			Optional:    true,
		},
	}

	for _, id := range slices.Sorted(maps.Keys(p.src.Invocation.Args)) {
		tmpl[id] = templateParam{
			Description: fmt.Sprintf("pipeline input %q; the transpiled value when unset; a file value is an s3:// URI, or an s3:// prefix for a directory", id),
			Optional:    true,
		}
	}

	raw, err := json.MarshalIndent(tmpl, "", "  ")
	if err != nil {
		return "", fmt.Errorf("emit parameter template: %w", err)
	}

	return string(raw) + "\n", nil
}

// packageScript is package.sh, which zips the workflow HealthOmics runs:
// the files main.nf reads, its healthomics profile made the workflow's own
// config, and the image pinned by digest.
const packageScript = `#!/usr/bin/env bash
# Packages this project as an AWS HealthOmics workflow, its healthomics
# profile the workflow's own config, as HealthOmics takes no -profile:
#   ./package.sh <image>
# Push the image every task runs in (README: Container image) to private
# ECR in the run's region and name it by digest (<repo>@sha256:...): the
# workflow pins it, so every run executes the code it was validated with,
# and a run cannot name another. Then register it:
#   aws omics create-workflow --engine NEXTFLOW --main main.nf \
#     --definition-zip fileb://workflow.zip \
#     --parameter-template file://parameter-template.json
set -euo pipefail
cd "$(dirname "$0")"
if [ $# -ne 1 ]; then
    echo "usage: package.sh <image>@sha256:<digest>" >&2
    exit 2
fi
if ! [[ $1 =~ ^[^@[:space:]\'\\]+@sha256:[0-9a-f]{64}$ ]]; then
    echo "package.sh: $1 is not pinned by a digest (<repo>@sha256:<64 hex digits>)" >&2
    exit 1
fi
pkg=$(mktemp -d)
trap 'rm -rf "$pkg"' EXIT
cp -R main.nf stage.nf nextflow.config conf _assets parameter-template.json "$pkg"/
if [ -d subworkflows ]; then
    cp -R subworkflows "$pkg"/
fi
cat >> "$pkg/nextflow.config" <<EOF

// package.sh: the image the workflow pins, and its healthomics profile,
// which reads it.
params.mro2nf_image = '$1'
includeConfig 'conf/healthomics.config'
EOF
rm -f workflow.zip
(cd "$pkg" && zip -qr - .) > workflow.zip
echo "wrote workflow.zip"
`
