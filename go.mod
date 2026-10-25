module github.com/eunmann/mro2nf

go 1.27

toolchain go1.27.3

require (
	github.com/google/go-cmp v0.7.0
	github.com/martian-lang/martian v0.0.0-20261019211818-50ad6c054d30
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3
)

require (
	github.com/google/shlex v0.0.0-20191202100458-e7afc7fbc510 // indirect
	golang.org/x/sys v0.30.0 // indirect
	golang.org/x/text v0.14.0 // indirect
	golang.org/x/tools v0.30.0 // indirect
)

// Martian's go.mod has no /v4 suffix, so a tag won't resolve as a Go module
// version -- it is pinned by commit (see the require above). To hack on the
// Martian parser locally, add a go.work or a replace pointing at a checkout.

tool github.com/martian-lang/martian/cmd/mrjob
