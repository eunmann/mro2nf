// Module boundary marker, not a real Go module: aws-cdk's node_modules ships
// init templates named %name%.template.go that the Go toolchain rejects, so
// without this boundary any ./... walk from the repo root (make test/vet/lint,
// and CI's go test/build/govulncheck) fails on a checkout where the CDK
// README's npm install has been run. A nested go.mod makes the toolchain treat
// this subtree as a separate module and skip it entirely.
module github.com/eunmann/mro2nf/deploy/awsbatch-cdk

go 1.26
