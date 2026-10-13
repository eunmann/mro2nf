//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
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

var (
	// cdkMountRoot matches the example stack's S3_MOUNT_ROOT.
	cdkMountRoot = regexp.MustCompile(`export const S3_MOUNT_ROOT = '([^']*)';`)
	// profileMountRoot matches the generated awsbatch profile's mount root.
	profileMountRoot = regexp.MustCompile(`\n        params\.mro2nf_s3_mount = '([^']*)'\n        aws\.batch\.volumes = '([^']*)'\n`)
)

// TestCDKMountsWhereTheProfileReads: the example stack's instances mount the
// input buckets read-only, readable by every container user, under the root
// the generated awsbatch profile hands its stages and bind-mounts into every
// container, read-only.
func TestCDKMountsWhereTheProfileReads(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(root, "deploy", "awsbatch-cdk", "lib", "stack.ts"))
	if err != nil {
		t.Fatalf("read stack: %v", err)
	}

	m := cdkMountRoot.FindSubmatch(src)
	if m == nil {
		t.Fatal("the stack declares no S3_MOUNT_ROOT")
	}

	stackRoot := string(m[1])

	for _, want := range []string{"mount-s3 --read-only --allow-other ", "${S3_MOUNT_ROOT}/${b}`"} {
		if !strings.Contains(string(src), want) {
			t.Errorf("the stack's user data lacks %q", want)
		}
	}

	cfg, err := os.ReadFile(filepath.Join(transpile(t, "entry_file"), "nextflow.config"))
	if err != nil {
		t.Fatal(err)
	}

	p := profileMountRoot.FindSubmatch(cfg)
	if p == nil {
		t.Fatalf("the awsbatch profile names no S3 mount:\n%s", cfg)
	}

	if got, want := []string{string(p[1]), string(p[2])}, []string{stackRoot, stackRoot + ":" + stackRoot + ":ro"}; !slices.Equal(got, want) {
		t.Errorf("the awsbatch profile's mount root and volume are %q, want %q", got, want)
	}
}
