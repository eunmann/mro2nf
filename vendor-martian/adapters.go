// Package vendormartian is the files mro2nf copies from the Martian module
// go.mod pins (test/thirdparty fails when they drift from it).
package vendormartian

import "embed"

// Adapters is Martian's adapters dir as mrp's runtime lays it out: python/
// holds martian_shell.py, which mrjob runs a py stage with, and the martian
// module it imports. mre embeds it, so a run needs no Martian install for
// its adapters.
//
//go:embed python
var Adapters embed.FS
