module github.com/eunmann/martian-nextflow

go 1.26

require (
	github.com/google/go-cmp v0.7.0
	github.com/martian-lang/martian v0.0.0-20260506211707-4a558e7dd93b
	github.com/rs/zerolog v1.34.0
)

require (
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.19 // indirect
	golang.org/x/sys v0.30.0 // indirect
	golang.org/x/tools v0.30.0 // indirect
)

// Develop against the local Martian checkout (the aws-batch jobmode fork),
// whose go.mod has no /v4 suffix so a tag will not resolve as a module version.
replace github.com/martian-lang/martian => ../martian
