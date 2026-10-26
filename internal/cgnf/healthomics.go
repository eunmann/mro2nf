package cgnf

import (
	"encoding/json"
	"fmt"
)

// templateParam is one run parameter of a HealthOmics parameter template;
// HealthOmics rejects a run param the template does not declare.
type templateParam struct {
	Description string `json:"description"`
	Optional    bool   `json:"optional"`
}

// parameterTemplate is parameter-template.json: the launch params a
// HealthOmics run may set, every entry input and the run's mrp --overrides
// file, which only resizes jobs, each optional, as each has a value when
// unset, and none of the project's other params, so a HealthOmics run can
// neither swap the image the package pins nor widen the file values it
// stages.
func (p *project) parameterTemplate() (string, error) {
	tmpl := map[string]templateParam{}

	for _, lp := range p.launchParams() {
		if lp.omics != "" {
			tmpl[lp.id] = templateParam{Description: lp.omics, Optional: true}
		}
	}

	raw, err := json.MarshalIndent(tmpl, "", "  ")
	if err != nil {
		return "", fmt.Errorf("emit parameter template: %w", err)
	}

	return string(raw) + "\n", nil
}

// packageScript is package.sh, which zips the workflow HealthOmics runs:
// the files main.nf reads, a nextflow.config of conf/base.config and its
// healthomics profile's config, and the image pinned by digest. That
// config holds no profiles, as HealthOmics applies standard to a run that
// names none. nextflow_schema.json stays out: HealthOmics detects a
// workflow's params from a schema in its package, and the template alone
// names those a run may set.
const packageScript = `#!/usr/bin/env bash
# Packages this project as an AWS HealthOmics workflow whose config is
# conf/base.config and the healthomics profile's, with no profiles, as
# HealthOmics applies the standard profile to a run that names none:
#   ./package.sh <image>
# Push the image every task runs in (mro2nf's
# docs/usage.md, "Container images") to private ECR in the run's region
# and name it by digest (<repo>@sha256:...): the workflow pins it, so every
# run executes the code it was validated with, and a run cannot name
# another. Then register it:
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
cp -R main.nf stage.nf conf _assets parameter-template.json "$pkg"/
if [ -d subworkflows ]; then
    cp -R subworkflows "$pkg"/
fi
cat > "$pkg/nextflow.config" <<EOF
` + engineFloor + `includeConfig 'conf/base.config'

// The image the workflow pins, which conf/healthomics.config reads.
params.mro2nf_image = '$1'
includeConfig 'conf/healthomics.config'
EOF
rm -f workflow.zip
zipfile="$PWD/workflow.zip"
# Fixed modes, times and entry order, and no extra fields (-X), so a project
# packages to the same bytes on every run and host.
(
    cd "$pkg"
    export TZ=UTC
    chmod -R u=rwX,go=rX .
    find . -exec touch -h -d '2000-01-01 00:00:00' {} +
    find . -mindepth 1 | LC_ALL=C sort | zip -qX "$zipfile" -@
)
echo "wrote workflow.zip"
`
