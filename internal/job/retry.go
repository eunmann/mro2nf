package job

import (
	"errors"
	"regexp"
	"strings"
)

// TransientExitCode is the exit code mre gives a job failure mrp retries
// (sysexits.h EX_TEMPFAIL), the one the generated config retries.
const TransientExitCode = 75

// DefaultRetries is retry.json's default_retries, the attempts mrp makes
// after the first on a transient failure.
const DefaultRetries = 2

// ErrTransient marks a job failure mrp retries: one whose _errors has a
// line matching a retry_on pattern of retry.json (core/node.go:848
// isErrorTransient). mrp retries no other failure, and never an
// assertion; a stage that exceeds its memory under --monitor is retried
// only under --auto-adjust-memory, which mre does not support.
var ErrTransient = errors.New("transient job failure")

// RetryOn are the retry_on patterns of jobmanagers/retry.json at the
// Martian commit go.mod pins, which mrp reads beside its binary
// (core/runtime.go:86 getRetryRegexps).
func RetryOn() []string {
	return []string{
		`^signal: `,
		`^(?:[0-9-]+ [0-9:]+ )?Caught signal `,
		`resource temporarily unavailable`,
		`No heartbeat detected for`,
		`^error: .Errno 12. Cannot allocate memory`,
		`^error: JSV stderr: error: commlib error: got select error (Connection refused)`,
		`^Unable to run job: failed receiving gdi request response`,
		`^According to the job manager, the job for .+ has not been (?:queued or )?running`,
		`^IOError: \[Errno 116\] Stale file handle`,
		`^OSError: \[Errno 11\] Resource temporarily unavailable`,
		`^jobcmd error \(exit status \d+\)`,
		`^sbatch: error: Batch job submission failed: Socket timed out on send/recv operation`,
	}
}

// transient reports whether errlog, a job's _errors, records a failure mrp
// retries: any of its lines matching a retry_on pattern.
func transient(errlog string) bool {
	retryOn := make([]*regexp.Regexp, 0, len(RetryOn()))
	for _, p := range RetryOn() {
		retryOn = append(retryOn, regexp.MustCompile(p))
	}

	for line := range strings.SplitSeq(errlog, "\n") {
		for _, re := range retryOn {
			if re.MatchString(line) {
				return true
			}
		}
	}

	return false
}
