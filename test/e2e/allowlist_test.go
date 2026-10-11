package e2e

import (
	"sync"
	"testing"

	"github.com/eunmann/mro2nf/test/divergence"
)

// allowlistFile is the checked-in divergence allowlist, relative to this
// package's dir, where go test runs.
const allowlistFile = "divergences.json"

var checkedInAllowlist = sync.OnceValues(func() (divergence.Allowlist, error) {
	return divergence.Load(allowlistFile)
})

// mustAllowlist returns the checked-in allowlist, failing the test when it does
// not load.
func mustAllowlist(tb testing.TB) divergence.Allowlist {
	tb.Helper()

	allow, err := checkedInAllowlist()
	if err != nil {
		tb.Fatal(err)
	}

	return allow
}
