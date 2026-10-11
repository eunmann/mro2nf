package e2e

import (
	"os"

	"github.com/eunmann/mro2nf/test/divergence"
)

// v2Emitter reports whether the suites run against the call-graph emitter
// (E2E_EMITTER=v2, #463). Its stages all run under the real mrjob, as mrp
// runs py and comp stages, so a fixture's mrjob.sh stand-in is not passed.
func v2Emitter() bool { return os.Getenv("E2E_EMITTER") == divergence.EmitterV2 }

// emitter names the emitter the suites run against, the one whose
// divergences the allowlist applies.
func emitter() string {
	if v2Emitter() {
		return divergence.EmitterV2
	}

	return divergence.EmitterV1
}
