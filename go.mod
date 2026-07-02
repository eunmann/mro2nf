module github.com/eunmann/mro2nf

go 1.26

require (
	github.com/google/go-cmp v0.7.0
	github.com/martian-lang/martian v0.0.0-20260506211707-4a558e7dd93b
	github.com/rs/zerolog v1.35.1
)

require (
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	golang.org/x/sys v0.30.0 // indirect
	golang.org/x/tools v0.30.0 // indirect
)

// Martian's go.mod has no /v4 suffix, so a tag won't resolve as a Go module
// version -- it is pinned by commit (see the require above). To hack on the
// Martian parser locally, add a go.work or a replace pointing at a checkout.
