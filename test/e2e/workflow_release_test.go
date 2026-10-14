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

	// releaseSHA is the commit every job checks out: the one the gate
	// resolved the ref input to.
	releaseSHA = "${{ needs.gate.outputs.sha }}"

	// releaseNotesPath is where the tag step writes the notes the release
	// step reads.
	releaseNotesPath = "${{ runner.temp }}/release-notes.md"

	// releaseCuratedNotes has Markdown headings, which git's default cleanup
	// drops as comments, and a paragraph spanning lines.
	releaseCuratedNotes = "## Highlights\n\n- per-platform archives\n- release notes\n\n## Breaking changes\n\nThe loose binaries\nare gone.\n"

	releaseImageArchive = "runtime-image.tar"
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

// stepLiteralEnv returns a step's env entries that are not expressions,
// which a test passes to its script as Actions would.
func stepLiteralEnv(step *yamlNode) []string {
	env := step.get("env")
	if env == nil {
		return nil
	}

	var out []string

	for _, k := range env.keys {
		if v := scalarOf(env.get(k)); !strings.Contains(v, "${{") {
			out = append(out, k+"="+v)
		}
	}

	return out
}

// execReleaseStep runs a step's script in dir as Actions does, with env
// added, and returns its output and what it wrote to $GITHUB_OUTPUT.
func execReleaseStep(t *testing.T, script, dir string, env ...string) (string, string, error) {
	t.Helper()

	out := filepath.Join(t.TempDir(), "output")

	cmd := exec.Command("bash", "-e", "-c", script)
	cmd.Dir = dir
	cmd.Env = slices.Concat(os.Environ(), env, []string{"GITHUB_OUTPUT=" + out})

	log, runErr := cmd.CombinedOutput()

	data, err := os.ReadFile(out)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}

	return string(log), string(data), runErr
}

// runReleaseStep runs a step's script that must succeed and returns what it
// wrote to $GITHUB_OUTPUT.
func runReleaseStep(t *testing.T, script, dir string, env ...string) string {
	t.Helper()

	log, out, err := execReleaseStep(t, script, dir, env...)
	if err != nil {
		t.Fatalf("release step: %v\n%s", err, log)
	}

	return out
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

	releaseGit(t, dir, "init", "-q")
	releaseGit(t, dir, "commit", "-q", "--allow-empty", "-m", "release")

	return dir
}

// releaseGitEnv frees git of the user's and the system's config.
var releaseGitEnv = []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}

// releaseGit runs git in dir free of the user's and the system's config,
// as a fixed identity at releaseCommitTime, and returns its stdout.
func releaseGit(t *testing.T, dir string, args ...string) string {
	t.Helper()

	var stderr bytes.Buffer

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stderr = &stderr
	cmd.Env = slices.Concat(os.Environ(), releaseGitEnv, []string{
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_AUTHOR_DATE=" + releaseCommitTime,
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_COMMITTER_DATE=" + releaseCommitTime,
	})

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", args[0], err, stderr.Bytes())
	}

	return string(out)
}

// releaseArchives runs the build job's archive step for every platform in a
// fresh checkout and returns the checkout and the archives' names.
func releaseArchives(t *testing.T, version string, fileTime time.Time) (string, []string) {
	t.Helper()

	step := releaseStep(t, "build", stepID("archive"))

	env := step.get("env")
	if scalarOf(env.get("GOOS")) != "${{ matrix.goos }}" || scalarOf(env.get("GOARCH")) != "${{ matrix.goarch }}" {
		t.Fatalf("%s archive step env GOOS/GOARCH are not the matrix's", releaseWorkflow)
	}

	dir := releaseCheckout(t, fileTime)
	platforms := releasePlatforms(t)
	files := make([]string, 0, len(platforms))

	for _, p := range platforms {
		goos, goarch, _ := strings.Cut(p, "-")
		out := runReleaseStep(t, scalarOf(step.get("run")), dir, "VERSION="+version, "GOOS="+goos, "GOARCH="+goarch)

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
// of its matrix, for a release and a pre-release: each archive is
// mro2nf-<version>-<os>-<arch>.tar.gz holding one directory of that name
// with the executable binaries and the notices, owned by root, writable by
// it alone, at the commit time.
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

	for _, version := range []string{releaseTag, releaseTag + "-rc1"} {
		dir, files := releaseArchives(t, version, commit.Add(time.Hour))

		for i, p := range platforms {
			top := "mro2nf-" + version + "-" + p
			if files[i] != top+".tar.gz" {
				t.Errorf("version %q %s: archive %q, want %q", version, p, files[i], top+".tar.gz")

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

	attest := releaseStep(t, "publish", func(s *yamlNode) bool { return s.get("with").get("subject-checksums") != nil })
	if !stepUses("actions/attest-build-provenance")(attest) || scalarOf(attest.get("with").get("subject-checksums")) != "SHA256SUMS" {
		t.Errorf("%s publish does not attest SHA256SUMS with attest-build-provenance", releaseWorkflow)
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

	pattern := scalarOf(releaseStep(t, "publish", func(s *yamlNode) bool { return s.get("with").get("pattern") != nil }).get("with").get("pattern"))

	gh := releaseStep(t, "publish", stepUses("softprops/action-gh-release")).get("with")

	assets := strings.Split(scalarOf(gh.get("files")), "\n")
	if diff := cmp.Diff([]string{"mro2nf-*.tar.gz", "SHA256SUMS"}, assets); diff != "" {
		t.Errorf("%s release files (-want +got):\n%s", releaseWorkflow, diff)
	}

	if got := scalarOf(gh.get("fail_on_unmatched_files")); got != "true" {
		t.Errorf("%s fail_on_unmatched_files = %q, want true", releaseWorkflow, got)
	}

	image := scalarOf(releaseStep(t, "image", stepUses("actions/upload-artifact")).get("with").get("name"))
	if ok, err := path.Match(pattern, image); err != nil || ok {
		t.Errorf("%s archive download pattern %q also takes the image artifact %q", releaseWorkflow, pattern, image)
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

// TestReleaseNotesConfig pins .github/release.yml, which groups the generated
// notes: Dependabot's PRs, labelled dependencies, below every other PR.
func TestReleaseNotesConfig(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(workflowsDir, "..", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}

	doc, err := parseYAML(string(raw))
	if err != nil {
		t.Fatalf("parse .github/release.yml: %v", err)
	}

	type category struct{ Title, Labels, Exclude string }

	items := doc.get("changelog").get("categories").items
	got := make([]category, 0, len(items))

	for _, c := range items {
		got = append(got, category{
			Title:   scalarOf(c.get("title")),
			Labels:  strings.Join(c.get("labels").list(), ","),
			Exclude: strings.Join(c.get("exclude").get("labels").list(), ","),
		})
	}

	want := []category{{"Changes", "*", "dependencies"}, {"Dependency updates", "dependencies", ""}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf(".github/release.yml categories (-want +got):\n%s", diff)
	}
}

// TestReleaseDispatchOnly pins release.yml's one trigger, a dispatch with
// the inputs a release is cut from, so a pushed tag releases nothing, and
// one run at a time per version.
func TestReleaseDispatchOnly(t *testing.T) {
	wf := readWorkflow(t, releaseWorkflow)

	on := wf.get("on")
	if on == nil {
		t.Fatalf("%s has no triggers", releaseWorkflow)
	}

	if diff := cmp.Diff([]string{"workflow_dispatch"}, on.keys); diff != "" {
		t.Errorf("%s triggers (-want +got):\n%s", releaseWorkflow, diff)
	}

	inputs := on.get("workflow_dispatch").get("inputs")
	if inputs == nil {
		t.Fatalf("%s workflow_dispatch has no inputs", releaseWorkflow)
	}

	type input struct{ Name, Type, Required, Default string }

	got := make([]input, 0, len(inputs.keys))

	for _, k := range inputs.keys {
		in := inputs.get(k)
		got = append(got, input{k, scalarOf(in.get("type")), scalarOf(in.get("required")), scalarOf(in.get("default"))})
	}

	want := []input{
		{"version", "string", "true", ""},
		{"ref", "string", "false", "main"},
		{"notes", "string", "false", ""},
		{"dry_run", "boolean", "false", "false"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("%s dispatch inputs (-want +got):\n%s", releaseWorkflow, diff)
	}

	concurrency := wf.get("concurrency")
	if got := scalarOf(concurrency.get("group")); got != "release-${{ inputs.version }}" {
		t.Errorf("%s concurrency group %q, want one per version", releaseWorkflow, got)
	}

	if got := scalarOf(concurrency.get("cancel-in-progress")); got != "" && got != "false" {
		t.Errorf("%s cancel-in-progress = %q; a cancelled run can stop between the tag and the release", releaseWorkflow, got)
	}
}

// TestReleaseScriptsReadInputsFromEnv keeps every expression out of
// release.yml's scripts: an input reaches a script only through env, where
// the shell reads it as data.
func TestReleaseScriptsReadInputsFromEnv(t *testing.T) {
	wf := readWorkflow(t, releaseWorkflow)

	env := wf.get("env")
	if got := scalarOf(env.get("VERSION")); got != "${{ inputs.version }}" {
		t.Errorf("%s env VERSION = %q, want the version input", releaseWorkflow, got)
	}

	if got := scalarOf(env.get("PRERELEASE")); got != "${{ contains(inputs.version, '-') }}" {
		t.Errorf("%s env PRERELEASE = %q, want a version with a -suffix", releaseWorkflow, got)
	}

	jobs := wf.get("jobs")
	scripts := 0

	for _, id := range jobs.keys {
		for i, s := range jobs.get(id).get("steps").items {
			run := scalarOf(s.get("run"))
			if run == "" {
				continue
			}

			scripts++

			if strings.Contains(run, "${{") {
				t.Errorf("%s %s step %d interpolates an expression into its script; pass it through env", releaseWorkflow, id, i)
			}
		}
	}

	if scripts < 5 {
		t.Fatalf("%s: found %d scripts; the walk is wrong", releaseWorkflow, scripts)
	}
}

// TestReleaseValidatesInputs runs the gate's validation step: only a
// v<major>.<minor>.<patch>[-<prerelease>] version that git takes as a tag
// name and a ref spelled as a branch, tag or SHA pass, and neither ever runs
// as code.
func TestReleaseValidatesInputs(t *testing.T) {
	requireTools(t, "bash", "git")

	step := releaseStep(t, "gate", stepRuns("version_re"))
	if got := scalarOf(step.get("env").get("REF")); got != "${{ inputs.ref }}" {
		t.Errorf("%s validation env REF = %q, want the ref input", releaseWorkflow, got)
	}

	const ref = "main"

	cases := []struct {
		version, ref string
		ok           bool
	}{
		{"v0.6.0", ref, true},
		{"v1.2.3-rc1", ref, true},
		{"v1.2.3-rc.1", "feat/release-dispatch", true},
		{"v0.6.0", "0123456789abcdef0123456789abcdef01234567", true},
		{"v0.6.0", "v0.5.0", true},
		{"0.6.0", ref, false},
		{"v1.2", ref, false},
		{"v1.2.3;rm -rf /", ref, false},
		{"v1.2.3-$(touch pwned)", ref, false},
		{"v1.2.3\nv1.2.4", ref, false},
		{"v1.2.3-", ref, false},
		{"v1.2.3-rc.", ref, false},
		{"v1.2.3-.rc", ref, false},
		{"v1.2.3-a..b", ref, false},
		{"v1.2.3-x.lock", ref, false},
		{"", ref, false},
		{"v0.6.0", "", false},
		{"v0.6.0", "../../pulls", false},
		{"v0.6.0", "-main", false},
		{"v0.6.0", "main?per_page=1", false},
		{"v0.6.0", "main;$(touch pwned)", false},
	}

	dir := t.TempDir()

	for _, c := range cases {
		log, _, err := execReleaseStep(t, scalarOf(step.get("run")), dir, "VERSION="+c.version, "REF="+c.ref)
		if (err == nil) != c.ok {
			t.Errorf("version %q ref %q: accepted %t, want %t\n%s", c.version, c.ref, err == nil, c.ok, log)
		}
	}

	if _, err := os.Stat(filepath.Join(dir, "pwned")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("an input ran as code: %v", err)
	}
}

// TestReleaseResolveStep runs the gate's resolve step with a stand-in gh
// answering from canned API responses: it refuses a version whose tag
// exists or cannot be looked up, takes one that only prefixes another tag,
// and outputs the commit the ref resolves to, which every job checks out.
func TestReleaseResolveStep(t *testing.T) {
	requireTools(t, "bash", "jq")

	step := releaseStep(t, "gate", stepID("resolve"))
	if got := scalarOf(step.get("env").get("REF")); got != "${{ inputs.ref }}" {
		t.Errorf("%s resolve env REF = %q, want the ref input", releaseWorkflow, got)
	}

	const (
		sha    = "0123456789abcdef0123456789abcdef01234567"
		tags   = "git/matching-refs/tags/" + releaseTag
		commit = "commits/main"
	)

	resolved := `{"sha":"` + sha + `"}`

	cases := []struct {
		name      string
		responses map[string]string
		want      string
	}{
		{"new tag", map[string]string{tags: `[]`, commit: resolved}, "sha=" + sha},
		{"prefix of another tag", map[string]string{tags: `[{"ref":"refs/tags/v1.2.3-rc1"}]`, commit: resolved}, "sha=" + sha},
		{"existing tag", map[string]string{tags: `[{"ref":"refs/tags/v1.2.3-rc1"},{"ref":"refs/tags/v1.2.3"}]`, commit: resolved}, ""},
		{"tag lookup fails", map[string]string{commit: resolved}, ""},
		{"unknown ref", map[string]string{tags: `[]`}, ""},
	}

	for _, c := range cases {
		log, out, err := runGhStep(t, scalarOf(step.get("run")), c.responses, "VERSION="+releaseTag, "REF=main")
		if (err == nil) != (c.want != "") || strings.TrimSpace(out) != c.want {
			t.Errorf("%s: err %v, output %q; want output %q\n%s", c.name, err, out, c.want, log)
		}
	}
}

// TestReleaseRequiresValidatedTree runs the gate's PR Validation check with a
// stand-in gh: it accepts a PR's merge commit whose tree is its head's and
// whose head has a green run, and refuses any other commit or any lookup
// that fails.
func TestReleaseRequiresValidatedTree(t *testing.T) {
	requireTools(t, "bash", "jq")

	step := releaseStep(t, "gate", stepRuns("pr-validation.yml"))
	if got := scalarOf(step.get("if")); got != "${{ !inputs.dry_run }}" {
		t.Errorf("%s PR Validation check if = %q; a real release must pass it", releaseWorkflow, got)
	}

	const (
		sha   = "0123456789abcdef0123456789abcdef01234567"
		head  = "89abcdef0123456789abcdef0123456789abcdef"
		pulls = "commits/" + sha + "/pulls"
		tree  = "git/commits/" + sha
		hTree = "git/commits/" + head
		runs  = "actions/workflows/pr-validation.yml/runs?head_sha=" + head + "&event=pull_request&status=success"
	)

	merged := `[{"merge_commit_sha":"` + sha + `","head":{"sha":"` + head + `"}}]`
	same := `{"tree":{"sha":"aaaa"}}`
	green := `{"total_count":1}`

	cases := []struct {
		name      string
		responses map[string]string
		ok        bool
	}{
		{"validated", map[string]string{pulls: merged, tree: same, hTree: same, runs: green}, true},
		{"not a merge commit", map[string]string{pulls: `[]`, tree: same, hTree: same, runs: green}, false},
		{"pulls lookup fails", map[string]string{tree: same, hTree: same, runs: green}, false},
		{"trees differ", map[string]string{pulls: merged, tree: same, hTree: `{"tree":{"sha":"bbbb"}}`, runs: green}, false},
		{"tree lookups fail", map[string]string{pulls: merged, runs: green}, false},
		{"head tree lookup fails", map[string]string{pulls: merged, tree: same, runs: green}, false},
		{"no green run", map[string]string{pulls: merged, tree: same, hTree: same, runs: `{"total_count":0}`}, false},
		{"runs lookup fails", map[string]string{pulls: merged, tree: same, hTree: same}, false},
	}

	for _, c := range cases {
		log, _, err := runGhStep(t, scalarOf(step.get("run")), c.responses, "SHA="+sha)
		if (err == nil) != c.ok {
			t.Errorf("%s: accepted %t, want %t\n%s", c.name, err == nil, c.ok, log)
		}
	}
}

// runGhStep runs a gate step with env added and a stand-in gh that answers
// `gh api` from responses, keyed by the path below the repository, through
// jq as gh does, and fails as a 404 on any other.
func runGhStep(t *testing.T, script string, responses map[string]string, env ...string) (string, string, error) {
	t.Helper()

	const (
		repo = "o/mro2nf"
		fake = `#!/bin/bash
[ "$1" = api ] && [ "$3" = --jq ] || { echo "unexpected gh $*" >&2; exit 2; }
path=${2#"repos/$GITHUB_REPOSITORY/"}
f="$GH_RESPONSES/${path//\//_}"
[ "$path" != "$2" ] && [ -f "$f" ] || { echo "gh: Not Found (HTTP 404) for $2" >&2; exit 1; }
jq -r "$4" "$f"
`
	)

	bin, dir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}

	for p, body := range responses {
		if err := os.WriteFile(filepath.Join(dir, strings.ReplaceAll(p, "/", "_")), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return execReleaseStep(t, script, t.TempDir(), slices.Concat([]string{
		"PATH=" + bin + ":" + os.Getenv("PATH"),
		"GH_RESPONSES=" + dir, "GITHUB_REPOSITORY=" + repo,
	}, env)...)
}

// TestReleaseRunsMainWorkflow runs the gate's workflow check: a release runs
// only main's release.yml, which a PR run validated, and a dry run, which
// checks a branch's, skips the check.
func TestReleaseRunsMainWorkflow(t *testing.T) {
	requireTools(t, "bash")

	step := releaseStep(t, "gate", stepRuns("GITHUB_REF"))
	if got := scalarOf(step.get("if")); got != "${{ !inputs.dry_run }}" {
		t.Errorf("%s workflow check if = %q; a real release must pass it", releaseWorkflow, got)
	}

	for ref, ok := range map[string]bool{
		"refs/heads/main":          true,
		"refs/heads/ci/bump-setup": false,
		"refs/heads/main-old":      false,
		"refs/tags/v0.5.0":         false,
		"":                         false,
	} {
		log, _, err := execReleaseStep(t, scalarOf(step.get("run")), t.TempDir(), "GITHUB_REF="+ref)
		if (err == nil) != ok {
			t.Errorf("workflow from %q: accepted %t, want %t\n%s", ref, err == nil, ok, log)
		}
	}
}

type releaseJob struct {
	Needs, If string
	Writes    []string
}

// TestReleasePublishesAfterBuilds pins the job graph: the gate, then the
// archive and image builds of the commit it resolved, then one publish job
// that alone writes, needs every build and is skipped in a dry run. A failed
// build or a dry run therefore tags, pushes and releases nothing.
func TestReleasePublishesAfterBuilds(t *testing.T) {
	wf := readWorkflow(t, releaseWorkflow)
	if got := scalarOf(wf.get("permissions")); got != "{}" {
		t.Errorf("%s default permissions %q, want {}", releaseWorkflow, got)
	}

	jobs := wf.get("jobs")
	got := map[string]releaseJob{}

	for _, id := range jobs.keys {
		j := jobs.get(id)

		var writes []string

		if perms := j.get("permissions"); perms != nil {
			for _, k := range perms.keys {
				if scalarOf(perms.get(k)) == "write" {
					writes = append(writes, k)
				}
			}
		}

		got[id] = releaseJob{strings.Join(j.get("needs").list(), ","), scalarOf(j.get("if")), writes}
	}

	want := map[string]releaseJob{
		"gate":    {},
		"build":   {Needs: "gate"},
		"image":   {Needs: "gate"},
		"publish": {Needs: "gate,build,image", If: "${{ !inputs.dry_run }}", Writes: []string{"contents", "packages", "id-token", "attestations"}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("%s jobs (-want +got):\n%s", releaseWorkflow, diff)
	}

	if got := scalarOf(releaseStep(t, "gate", stepRuns("pr-validation.yml")).get("if")); got != "${{ !inputs.dry_run }}" {
		t.Errorf("%s CI-green check if = %q; a real release must pass it", releaseWorkflow, got)
	}

	if got := scalarOf(jobs.get("gate").get("outputs").get("sha")); got != "${{ steps.resolve.outputs.sha }}" {
		t.Errorf("%s gate outputs sha %q, want the resolve step's", releaseWorkflow, got)
	}

	for _, id := range []string{"build", "image", "publish"} {
		if got := scalarOf(releaseStep(t, id, stepUses("actions/checkout")).get("with").get("ref")); got != releaseSHA {
			t.Errorf("%s %s checks out %q, want the gate's commit", releaseWorkflow, id, got)
		}
	}
}

// releasePublishing matches the steps that tag, push or release.
var releasePublishing = map[string]func(*yamlNode) bool{
	"tag":      stepRuns("git tag"),
	"git push": stepRuns("git push"),
	"skopeo":   stepRuns("skopeo copy"),
	"login":    stepUses("docker/login-action"),
	"attest":   stepUses("actions/attest-build-provenance"),
	"release":  stepUses("softprops/action-gh-release"),
}

// TestReleasePublishesInOrder keeps every step that tags, pushes or releases
// in the publish job, in the order tag, image, release, and the image job
// building to an archive rather than a registry.
func TestReleasePublishesInOrder(t *testing.T) {
	jobs := readWorkflow(t, releaseWorkflow).get("jobs")

	for _, id := range jobs.keys {
		if id == "publish" {
			continue
		}

		for i, s := range jobs.get(id).get("steps").items {
			for what, match := range releasePublishing {
				if match(s) {
					t.Errorf("%s %s step %d does %s; only publish may", releaseWorkflow, id, i, what)
				}
			}
		}
	}

	steps := jobs.get("publish").get("steps").items
	tag := slices.IndexFunc(steps, stepRuns("git push"))
	image := slices.IndexFunc(steps, stepRuns("skopeo copy"))
	release := slices.IndexFunc(steps, stepUses("softprops/action-gh-release"))

	if tag < 0 || image < tag || release < image {
		t.Errorf("%s publish steps: tag %d, image %d, release %d; want them in that order", releaseWorkflow, tag, image, release)
	}

	build := releaseStep(t, "image", stepUses("docker/build-push-action")).get("with")
	if got := scalarOf(build.get("outputs")); got != "type=oci,dest=${{ runner.temp }}/"+releaseImageArchive {
		t.Errorf("%s image build outputs %q, want an OCI archive", releaseWorkflow, got)
	}

	if push := build.get("push"); push != nil {
		t.Errorf("%s image build sets push %q; only publish pushes", releaseWorkflow, push.scalar)
	}
}

// TestReleaseRequiresSkopeo pins the skopeo preflight in the image job,
// which a dry run runs too, and in publish before the tag exists, and runs
// it: it fails on a runner without skopeo and passes on one with it.
func TestReleaseRequiresSkopeo(t *testing.T) {
	requireTools(t, "bash")

	preflight := stepRuns("command -v skopeo")

	image := readWorkflow(t, releaseWorkflow).get("jobs").get("image").get("steps").items
	if slices.IndexFunc(image, preflight) < 0 {
		t.Errorf("%s image job does not check for skopeo", releaseWorkflow)
	}

	publish := readWorkflow(t, releaseWorkflow).get("jobs").get("publish").get("steps").items
	if check, tag := slices.IndexFunc(publish, preflight), slices.IndexFunc(publish, stepRuns("git tag")); check < 0 || tag < check {
		t.Errorf("%s publish steps: skopeo check %d, tag %d; want the check first", releaseWorkflow, check, tag)
	}

	script := scalarOf(releaseStep(t, "publish", preflight).get("run"))
	if got := scalarOf(releaseStep(t, "image", preflight).get("run")); got != script {
		t.Errorf("%s image and publish check for skopeo differently:\n%s\n%s", releaseWorkflow, got, script)
	}

	without, with := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(with, "skopeo"), []byte("#!/bin/sh\necho skopeo version 1.13.3\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if log, _, err := execReleaseStep(t, script, t.TempDir(), "PATH="+without); err == nil {
		t.Errorf("skopeo check passed without skopeo:\n%s", log)
	}

	if log, _, err := execReleaseStep(t, script, t.TempDir(), "PATH="+with); err != nil {
		t.Errorf("skopeo check failed with skopeo: %v\n%s", err, log)
	}
}

// TestReleaseNotesFromInput pins the release on the tag the workflow
// created: its title is the tag and its body the notes the tag step wrote,
// above GitHub's generated list of merged PRs.
func TestReleaseNotesFromInput(t *testing.T) {
	gh := releaseStep(t, "publish", stepUses("softprops/action-gh-release")).get("with")

	got := map[string]string{}
	for _, k := range []string{"tag_name", "body_path", "generate_release_notes", "prerelease"} {
		got[k] = scalarOf(gh.get(k))
	}

	want := map[string]string{
		"tag_name":               "${{ inputs.version }}",
		"body_path":              releaseNotesPath,
		"generate_release_notes": "true",
		"prerelease":             "${{ env.PRERELEASE }}",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("%s release (-want +got):\n%s", releaseWorkflow, diff)
	}

	if gh.get("name") != nil || gh.get("body") != nil {
		t.Errorf("%s release sets name or body; the title is the tag and the body the notes file", releaseWorkflow)
	}
}

// releaseRemote returns a bare remote whose main holds two commits, and the
// first, which is not main's head.
func releaseRemote(t *testing.T) (string, string) {
	t.Helper()

	remote := filepath.Join(t.TempDir(), "origin.git")
	releaseGit(t, "", "init", "-q", "--bare", remote)

	src := t.TempDir()
	releaseGit(t, src, "init", "-q")
	releaseGit(t, src, "commit", "-q", "--allow-empty", "-m", "released")
	commit := strings.TrimSpace(releaseGit(t, src, "rev-parse", "HEAD"))
	releaseGit(t, src, "commit", "-q", "--allow-empty", "-m", "later")
	releaseGit(t, src, "push", "-q", remote, "HEAD:refs/heads/main")

	return remote, commit
}

// releaseClone fetches commit alone from remote into a new repository and
// checks it out, as actions/checkout does for a SHA.
func releaseClone(t *testing.T, remote, commit string) string {
	t.Helper()

	dir := t.TempDir()
	releaseGit(t, dir, "init", "-q")
	releaseGit(t, dir, "remote", "add", "origin", "file://"+remote)
	releaseGit(t, dir, "fetch", "-q", "--no-tags", "--depth=1", "origin", commit)
	releaseGit(t, dir, "checkout", "-q", "--detach", "FETCH_HEAD")

	return dir
}

// runTagStep runs the publish job's tag step in dir to tag commit as
// version with notes. On success the notes file the release reads must hold
// the notes.
func runTagStep(t *testing.T, step *yamlNode, dir, commit, version, notes string) (string, error) {
	t.Helper()

	tmp := t.TempDir()
	env := slices.Concat(releaseGitEnv, stepLiteralEnv(step), []string{"VERSION=" + version, "SHA=" + commit, "NOTES=" + notes, "RUNNER_TEMP=" + tmp})

	log, _, err := execReleaseStep(t, scalarOf(step.get("run")), dir, env...)
	if err != nil {
		return log, err
	}

	written, err := os.ReadFile(strings.Replace(releaseNotesPath, "${{ runner.temp }}", tmp, 1))
	if err != nil {
		t.Fatal(err)
	}

	if string(written) != notes {
		t.Errorf("%s notes file %q, want %q", version, written, notes)
	}

	return log, nil
}

// TestReleaseTagStep runs the publish job's tag step in a checkout of a
// commit behind main, against a bare remote: it pushes an annotated tag on
// that commit, by github-actions[bot], whose message is the notes as
// written, possibly none, and writes the notes the release reads. A tag that
// exists, here or on the remote, is refused and left as it was.
func TestReleaseTagStep(t *testing.T) {
	requireTools(t, "bash", "git")

	step := releaseStep(t, "publish", stepRuns("git tag"))
	if env := step.get("env"); scalarOf(env.get("NOTES")) != "${{ inputs.notes }}" || scalarOf(env.get("SHA")) != releaseSHA {
		t.Errorf("%s tag step env NOTES/SHA are not the notes input and the gate's commit", releaseWorkflow)
	}

	remote, commit := releaseRemote(t)
	first, second := releaseClone(t, remote, commit), releaseClone(t, remote, commit)

	for version, notes := range map[string]string{releaseTag: releaseCuratedNotes, "v1.2.4": ""} {
		if log, err := runTagStep(t, step, releaseClone(t, remote, commit), commit, version, notes); err != nil {
			t.Fatalf("tag %s: %v\n%s", version, err, log)
		}

		ref := "refs/tags/" + version

		got := strings.TrimSpace(releaseGit(t, remote, "for-each-ref", "--format=%(objecttype) %(*objectname) %(taggername) %(taggeremail)", ref))
		if want := "tag " + commit + " github-actions[bot] <41898282+github-actions[bot]@users.noreply.github.com>"; got != want {
			t.Errorf("%s on the remote is %q, want %q", version, got, want)
		}

		_, msg, _ := strings.Cut(releaseGit(t, remote, "cat-file", "tag", ref), "\n\n")
		if diff := cmp.Diff(notes, msg); diff != "" {
			t.Errorf("%s message (-want +got):\n%s", version, diff)
		}
	}

	if log, err := runTagStep(t, step, first, commit, "v1.2.5", releaseCuratedNotes); err != nil {
		t.Fatalf("tag v1.2.5: %v\n%s", err, log)
	}

	before := releaseGit(t, remote, "for-each-ref", "refs/tags/")

	for where, dir := range map[string]string{"on the remote": second, "here and on the remote": first} {
		if _, err := runTagStep(t, step, dir, commit, "v1.2.5", "again"); err == nil {
			t.Errorf("a tag that exists %s was retagged", where)
		}
	}

	if after := releaseGit(t, remote, "for-each-ref", "refs/tags/"); after != before {
		t.Errorf("refused tags changed the remote's:\n%s\nwant\n%s", after, before)
	}
}

// TestReleaseImagePush runs the publish job's image push with a stand-in
// skopeo: it copies the OCI archive the image job uploaded, with every
// manifest and digest as built, to the version's tag and, unless a
// pre-release, to latest, and outputs the digest the provenance attests.
func TestReleaseImagePush(t *testing.T) {
	requireTools(t, "bash")

	upload := releaseStep(t, "image", stepUses("actions/upload-artifact")).get("with")
	download := releaseStep(t, "publish", func(s *yamlNode) bool {
		return stepUses("actions/download-artifact")(s) && s.get("with").get("name") != nil
	}).get("with")

	chain := []string{scalarOf(upload.get("name")), scalarOf(upload.get("path")), scalarOf(download.get("name")), scalarOf(download.get("path"))}
	if diff := cmp.Diff([]string{"runtime-image", "${{ runner.temp }}/" + releaseImageArchive, "runtime-image", "${{ runner.temp }}"}, chain); diff != "" {
		t.Errorf("%s image artifact upload name, path, download name, path (-want +got):\n%s", releaseWorkflow, diff)
	}

	step := releaseStep(t, "publish", stepRuns("skopeo copy"))

	attest := releaseStep(t, "publish", func(s *yamlNode) bool { return s.get("with").get("subject-digest") != nil }).get("with")
	if scalarOf(step.get("id")) != "push" || scalarOf(attest.get("subject-digest")) != "${{ steps.push.outputs.digest }}" {
		t.Errorf("%s image provenance does not attest the digest the push step outputs", releaseWorkflow)
	}

	const image = "ghcr.io/o/mro2nf-runtime"

	for prerelease, tags := range map[string][]string{"false": {releaseTag, "latest"}, "true": {releaseTag}} {
		want := make([]string, 0, len(tags))
		for _, tag := range tags {
			want = append(want, "docker://"+image+":"+tag)
		}

		if diff := cmp.Diff(want, runImagePush(t, scalarOf(step.get("run")), image, prerelease)); diff != "" {
			t.Errorf("prerelease %s: pushed tags (-want +got):\n%s", prerelease, diff)
		}
	}
}

// TestReleaseImagePushNeedsDigest runs the publish job's image push with a
// stand-in skopeo that writes no digest file: the step fails and outputs no
// digest, rather than release an image whose digest nothing recorded.
func TestReleaseImagePushNeedsDigest(t *testing.T) {
	requireTools(t, "bash")

	bin, tmp := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "skopeo"), []byte("#!/bin/bash\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	script := scalarOf(releaseStep(t, "publish", stepRuns("skopeo copy")).get("run"))

	log, out, err := execReleaseStep(t, script, tmp, "PATH="+bin+":"+os.Getenv("PATH"),
		"VERSION="+releaseTag, "PRERELEASE=false", "IMAGE=ghcr.io/o/mro2nf-runtime", "RUNNER_TEMP="+tmp)
	if err == nil || strings.Contains(out, "digest=") {
		t.Errorf("push without a digest file: err %v, output %q; want a failure and no digest\n%s", err, out, log)
	}
}

// runImagePush runs the publish job's image push script with a stand-in
// skopeo that records its arguments and returns the destinations it copied
// the downloaded archive to, each whole and as built.
func runImagePush(t *testing.T, script, image, prerelease string) []string {
	t.Helper()

	const fake = "#!/bin/bash\nprintf '%s\\n' \"$*\" >> \"$SKOPEO_LOG\"\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = --digestfile ]; then printf sha256:built > \"$2\"; fi\n  shift\ndone\n"

	bin, tmp := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "skopeo"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}

	log := filepath.Join(tmp, "skopeo.log")

	out := runReleaseStep(t, script, tmp, "PATH="+bin+":"+os.Getenv("PATH"), "SKOPEO_LOG="+log,
		"VERSION="+releaseTag, "PRERELEASE="+prerelease, "IMAGE="+image, "RUNNER_TEMP="+tmp)

	if got := strings.TrimSpace(out); got != "digest=sha256:built" {
		t.Errorf("prerelease %s: push step output %q, want the pushed digest", prerelease, got)
	}

	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}

	var dests []string

	for line := range strings.Lines(string(raw)) {
		args := strings.Fields(line)
		if !slices.Contains(args, "--all") || !slices.Contains(args, "--preserve-digests") || !slices.Contains(args, "oci-archive:"+filepath.Join(tmp, releaseImageArchive)) {
			t.Errorf("prerelease %s: skopeo %s does not copy the whole archive as built", prerelease, line)
		}

		dests = append(dests, args[len(args)-1])
	}

	return dests
}
