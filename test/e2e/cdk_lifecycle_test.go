//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// expiringPrefix matches a CDK S3 lifecycle rule that expires a prefix.
var expiringPrefix = regexp.MustCompile(`\{\s*prefix:\s*'([^']*)',\s*expiration:`)

// TestCDKExpiresOnlyWorkDir guards #258: the example stack's bucket also holds
// the final outputs (results/ for awsbatch --aws_outdir, omics-out/ for the
// HealthOmics --output-uri the runbooks pass), so only the intermediate
// Nextflow work dir may expire.
func TestCDKExpiresOnlyWorkDir(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(root, "deploy", "awsbatch-cdk", "lib", "stack.ts"))
	if err != nil {
		t.Fatalf("read stack: %v", err)
	}

	matches := expiringPrefix.FindAllSubmatch(src, -1)

	got := make([]string, 0, len(matches))
	for _, m := range matches {
		got = append(got, string(m[1]))
	}

	if diff := cmp.Diff([]string{"work/"}, got); diff != "" {
		t.Errorf("expiring lifecycle prefixes (-want +got):\n%s", diff)
	}
}
