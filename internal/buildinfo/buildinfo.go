// Package buildinfo names an mro2nf build: the project mro2nf transpiles
// records it, and the mre that runs the project must be the same build.
package buildinfo

import "runtime/debug"

// Version is the name of this build, whose -ldflags stamp is stamp.
func Version(stamp string) string {
	info, _ := debug.ReadBuildInfo()

	return choose(stamp, info)
}

// choose is the -ldflags stamp, else the main module version the Go
// toolchain stamps: the tag of a `go install …@v`, or for a plain `go build`
// in a clone a pseudo-version naming the commit (+dirty when modified), so
// unstamped builds of different commits do not share one "dev" name. Go
// stamps no version ("(devel)") under -buildvcs=false, outside a repository
// or in a git worktree, and those builds stay "dev".
func choose(stamp string, info *debug.BuildInfo) string {
	if stamp != "dev" {
		return stamp
	}

	if info != nil && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}

	return stamp
}
