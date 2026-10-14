//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

const (
	releaseWorkflow = "release.yml"

	// releaseCommitTime is the commit time of the checkout the archive step
	// runs in, which every archive entry must carry.
	releaseCommitTime = "2001-02-03T04:05:06Z"

	releaseTag = "v1.2.3"
)

// releaseBinaries ship under the names mre looks mrjob up by.
var releaseBinaries = []string{"mro2nf", "mre", "mrjob"}

// releaseArchiveFiles are each archive's files below its top-level
// directory, in the byte order `tar --sort=name` writes them.
var releaseArchiveFiles = []string{"LICENSE", "THIRD_PARTY_NOTICES", "mre", "mrjob", "mro2nf"}

var releaseMatrixEntry = regexp.MustCompile(`^\{ goos: (\w+), goarch: (\w+) \}$`)

// releasePlatforms returns the build matrix's GOOS-GOARCH pairs.
func releasePlatforms(t *testing.T) []string {
	t.Helper()

	items := readWorkflow(t, releaseWorkflow).get("jobs").get("build").get("strategy").get("matrix").get("include").items

	out := make([]string, 0, len(items))

	for _, it := range items {
		m := releaseMatrixEntry.FindStringSubmatch(it.scalar)
		if m == nil {
			t.Fatalf("%s build matrix entry %q is not { goos: <os>, goarch: <arch> }", releaseWorkflow, it.scalar)
		}

		out = append(out, m[1]+"-"+m[2])
	}

	return out
}

// releaseStep returns the first step of a release.yml job that match accepts.
func releaseStep(t *testing.T, job string, match func(*yamlNode) bool) *yamlNode {
	t.Helper()

	steps := readWorkflow(t, releaseWorkflow).get("jobs").get(job).get("steps").items

	i := slices.IndexFunc(steps, match)
	if i < 0 {
		t.Fatalf("%s %s job lacks the step", releaseWorkflow, job)
	}

	return steps[i]
}

func stepID(id string) func(*yamlNode) bool {
	return func(s *yamlNode) bool { return scalarOf(s.get("id")) == id }
}

func stepUses(action string) func(*yamlNode) bool {
	return func(s *yamlNode) bool { return strings.HasPrefix(scalarOf(s.get("uses")), action+"@") }
}

func stepRuns(cmd string) func(*yamlNode) bool {
	return func(s *yamlNode) bool { return strings.Contains(scalarOf(s.get("run")), cmd) }
}

// runReleaseStep runs a step's script in dir as Actions does, with env
// added, and returns what it wrote to $GITHUB_OUTPUT.
func runReleaseStep(t *testing.T, script, dir string, env ...string) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), "output")

	cmd := exec.Command("bash", "-e", "-c", script)
	cmd.Dir = dir
	cmd.Env = slices.Concat(os.Environ(), env, []string{"GITHUB_OUTPUT=" + out})

	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("release step: %v\n%s", err, b)
	}

	data, err := os.ReadFile(out)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}

	return string(data)
}

// releaseCheckout returns a git checkout committed at releaseCommitTime
// holding what the archive step packs, each file modified at fileTime and
// group-writable, as a runner with umask 002 would leave it.
func releaseCheckout(t *testing.T, fileTime time.Time) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}

	files := map[string]os.FileMode{"LICENSE": 0o664, "THIRD_PARTY_NOTICES": 0o664}
	for _, b := range releaseBinaries {
		files[filepath.Join("dist", b)] = 0o775
	}

	for rel, mode := range files {
		p := filepath.Join(dir, rel)
		if err := os.WriteFile(p, []byte(rel+"\n"), mode); err != nil {
			t.Fatal(err)
		}

		if err := errors.Join(os.Chmod(p, mode), os.Chtimes(p, fileTime, fileTime)); err != nil {
			t.Fatal(err)
		}
	}

	gitEnv := []string{
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_AUTHOR_DATE=" + releaseCommitTime,
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_COMMITTER_DATE=" + releaseCommitTime,
	}

	for _, args := range [][]string{{"init", "-q"}, {"commit", "-q", "--allow-empty", "-m", "release"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), gitEnv...)

		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", args[0], err, b)
		}
	}

	return dir
}

// releaseArchives runs the build job's archive step for every platform in a
// fresh checkout and returns the checkout and the archives' names.
func releaseArchives(t *testing.T, ref string, fileTime time.Time) (string, []string) {
	t.Helper()

	step := releaseStep(t, "build", stepID("archive"))

	script := scalarOf(step.get("run"))
	if strings.Contains(script, "${{") {
		t.Fatalf("%s archive step interpolates an expression into its script; read it from env", releaseWorkflow)
	}

	env := step.get("env")
	if scalarOf(env.get("GOOS")) != "${{ matrix.goos }}" || scalarOf(env.get("GOARCH")) != "${{ matrix.goarch }}" {
		t.Fatalf("%s archive step env GOOS/GOARCH are not the matrix's", releaseWorkflow)
	}

	dir := releaseCheckout(t, fileTime)
	platforms := releasePlatforms(t)
	files := make([]string, 0, len(platforms))

	for _, p := range platforms {
		goos, goarch, _ := strings.Cut(p, "-")
		out := runReleaseStep(t, script, dir, "GITHUB_REF_NAME="+ref, "GOOS="+goos, "GOARCH="+goarch)

		file, ok := strings.CutPrefix(strings.TrimSpace(out), "file=")
		if !ok {
			t.Fatalf("archive step output %q, want file=<archive>", out)
		}

		files = append(files, file)
	}

	return dir, files
}

type archiveEntry struct {
	Name         string
	Type         byte
	Perm         os.FileMode
	UID, GID     int
	Uname, Gname string
	ModTime      time.Time
}

// readArchive lists a .tar.gz's entries; its gzip header must record no
// file name or time.
func readArchive(t *testing.T, file string) []archiveEntry {
	t.Helper()

	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}

	if zr.Name != "" || !zr.ModTime.IsZero() {
		t.Errorf("%s gzip header records name %q, time %v; want neither (gzip -n)", file, zr.Name, zr.ModTime)
	}

	var out []archiveEntry

	tr := tar.NewReader(zr)

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}

		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}

		out = append(out, archiveEntry{
			Name: h.Name, Type: h.Typeflag, Perm: h.FileInfo().Mode().Perm(),
			UID: h.Uid, GID: h.Gid, Uname: h.Uname, Gname: h.Gname, ModTime: h.ModTime,
		})
	}
}

// TestReleaseArchiveLayout runs release.yml's archive step for each platform
// of its matrix, on a tag and on a dry run's branch: each archive is
// mro2nf-<ref>-<os>-<arch>.tar.gz holding one directory of that name with the
// executable binaries and the notices, owned by root, writable by it alone,
// at the commit time.
func TestReleaseArchiveLayout(t *testing.T) {
	requireTools(t, "bash", "git", "tar", "gzip")

	platforms := releasePlatforms(t)
	if diff := cmp.Diff([]string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64"}, platforms); diff != "" {
		t.Errorf("%s build platforms (-want +got):\n%s", releaseWorkflow, diff)
	}

	commit, err := time.Parse(time.RFC3339, releaseCommitTime)
	if err != nil {
		t.Fatal(err)
	}

	for ref, label := range map[string]string{releaseTag: releaseTag, "feat/dry-run": "feat-dry-run"} {
		dir, files := releaseArchives(t, ref, commit.Add(time.Hour))

		for i, p := range platforms {
			top := "mro2nf-" + label + "-" + p
			if files[i] != top+".tar.gz" {
				t.Errorf("ref %q %s: archive %q, want %q", ref, p, files[i], top+".tar.gz")

				continue
			}

			want := make([]archiveEntry, 0, 1+len(releaseArchiveFiles))
			want = append(want, archiveEntry{Name: top + "/", Type: tar.TypeDir, Perm: 0o755, ModTime: commit})

			for _, f := range releaseArchiveFiles {
				perm := os.FileMode(0o644)
				if slices.Contains(releaseBinaries, f) {
					perm = 0o755
				}

				want = append(want, archiveEntry{Name: top + "/" + f, Type: tar.TypeReg, Perm: perm, ModTime: commit})
			}

			if diff := cmp.Diff(want, readArchive(t, filepath.Join(dir, files[i]))); diff != "" {
				t.Errorf("%s entries (-want +got):\n%s", files[i], diff)
			}
		}
	}
}

// TestReleaseArchiveReproducible packs the same commit in two checkouts
// whose files differ in mtime: the archives are byte-identical.
func TestReleaseArchiveReproducible(t *testing.T) {
	requireTools(t, "bash", "git", "tar", "gzip")

	commit, err := time.Parse(time.RFC3339, releaseCommitTime)
	if err != nil {
		t.Fatal(err)
	}

	dirA, files := releaseArchives(t, releaseTag, commit.Add(time.Hour))
	dirB, _ := releaseArchives(t, releaseTag, commit.Add(48*time.Hour))

	for _, f := range files {
		a, errA := os.ReadFile(filepath.Join(dirA, f))
		b, errB := os.ReadFile(filepath.Join(dirB, f))

		if err := errors.Join(errA, errB); err != nil {
			t.Fatal(err)
		}

		if string(a) != string(b) {
			t.Errorf("%s differs between two checkouts of one commit", f)
		}
	}
}

// TestReleaseChecksumsCoverArchives runs the publish job's SHA256SUMS step
// on the downloaded archives: it lists each archive with its digest and
// nothing else, and the provenance attests exactly that list.
func TestReleaseChecksumsCoverArchives(t *testing.T) {
	requireTools(t, "bash", "git", "tar", "gzip", "sha256sum")

	src, files := releaseArchives(t, releaseTag, time.Now())

	dl := t.TempDir()

	want := map[string]string{}

	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(dl, f), data, 0o644); err != nil {
			t.Fatal(err)
		}

		sum := sha256.Sum256(data)
		want[f] = hex.EncodeToString(sum[:])
	}

	runReleaseStep(t, scalarOf(releaseStep(t, "publish", stepRuns("sha256sum")).get("run")), dl)

	raw, err := os.ReadFile(filepath.Join(dl, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}

	for line := range strings.Lines(string(raw)) {
		digest, name, ok := strings.Cut(strings.TrimSpace(line), "  ")
		if !ok {
			t.Fatalf("SHA256SUMS line %q is not `<digest>  <file>`", line)
		}

		got[name] = digest
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("SHA256SUMS (-want +got):\n%s", diff)
	}

	attest := releaseStep(t, "publish", stepUses("actions/attest-build-provenance"))
	if got := scalarOf(attest.get("with").get("subject-checksums")); got != "SHA256SUMS" {
		t.Errorf("%s provenance subject-checksums = %q, want SHA256SUMS", releaseWorkflow, got)
	}
}

// TestReleasePublishesArchives follows each archive from the build job's
// upload to the release: the publish job downloads every platform's
// artifact and attaches the archives and SHA256SUMS, and nothing else.
func TestReleasePublishesArchives(t *testing.T) {
	upload := releaseStep(t, "build", stepUses("actions/upload-artifact")).get("with")
	if got := scalarOf(upload.get("path")); got != "${{ steps.archive.outputs.file }}" {
		t.Errorf("%s build uploads %q, want the archive step's file", releaseWorkflow, got)
	}

	pattern := scalarOf(releaseStep(t, "publish", stepUses("actions/download-artifact")).get("with").get("pattern"))

	gh := releaseStep(t, "publish", stepUses("softprops/action-gh-release")).get("with")

	assets := strings.Split(scalarOf(gh.get("files")), "\n")
	if diff := cmp.Diff([]string{"mro2nf-*.tar.gz", "SHA256SUMS"}, assets); diff != "" {
		t.Errorf("%s release files (-want +got):\n%s", releaseWorkflow, diff)
	}

	if got := scalarOf(gh.get("fail_on_unmatched_files")); got != "true" {
		t.Errorf("%s fail_on_unmatched_files = %q, want true", releaseWorkflow, got)
	}

	for _, p := range releasePlatforms(t) {
		goos, goarch, _ := strings.Cut(p, "-")
		name := strings.NewReplacer("${{ matrix.goos }}", goos, "${{ matrix.goarch }}", goarch).Replace(scalarOf(upload.get("name")))

		if ok, err := path.Match(pattern, name); err != nil || !ok {
			t.Errorf("%s download pattern %q misses artifact %q", releaseWorkflow, pattern, name)
		}

		if ok, err := path.Match(assets[0], "mro2nf-"+releaseTag+"-"+p+".tar.gz"); err != nil || !ok {
			t.Errorf("%s release files %q miss the %s archive", releaseWorkflow, assets[0], p)
		}
	}
}
