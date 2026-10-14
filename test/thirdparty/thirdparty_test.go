package thirdparty

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

const repoRoot = "../.."

// TestNoticesAreCurrent fails when THIRD_PARTY_NOTICES is not what the module
// graph renders: a module a release binary starts or stops linking, or a
// license text that changed with a version bump.
func TestNoticesAreCurrent(t *testing.T) {
	notices, err := Collect(t.Context(), repoRoot)
	if err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(repoRoot, NoticesFile))
	if err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff(Render(notices), string(got)); diff != "" {
		t.Errorf("%s is stale; run make notices (-want +got):\n%s", NoticesFile, diff)
	}
}

// TestNoticesCoverShippedCode checks what Collect derives: the Go standard
// library, the parser mro2nf links, the modules the runtime image's mrjob
// links, each vendored module, and no module only the tests import
// (go-cmp).
func TestNoticesCoverShippedCode(t *testing.T) {
	notices, err := Collect(t.Context(), repoRoot)
	if err != nil {
		t.Fatal(err)
	}

	shipped := map[string][]string{}
	for _, n := range notices {
		shipped[n.Name] = n.ShippedIn
	}

	if !slices.Contains(shipped["github.com/martian-lang/martian"], "mro2nf") {
		t.Errorf("Martian ships in %v, want mro2nf among them", shipped["github.com/martian-lang/martian"])
	}

	for _, mod := range []string{"github.com/martian-lang/martian", "github.com/google/shlex"} {
		if !slices.Contains(shipped[mod], "mrjob") {
			t.Errorf("%s ships in %v, want the runtime image's mrjob among them", mod, shipped[mod])
		}
	}

	if _, ok := shipped[stdlibName]; !ok {
		t.Errorf("no notice for the %s", stdlibName)
	}

	for dir, mod := range Vendored() {
		if !slices.Contains(shipped[mod], dir+"/") {
			t.Errorf("%s ships in %v, want %s/ among them", mod, shipped[mod], dir)
		}
	}

	if in, ok := shipped["github.com/google/go-cmp"]; ok {
		t.Errorf("test-only go-cmp has a notice, shipped in %v", in)
	}
}

// TestCollectIgnoresWorkspace runs Collect under a go.work that uses the
// Martian module, as CLAUDE.md suggests for parser work: the notices must
// still come from the go.mod build list release builds use.
func TestCollectIgnoresWorkspace(t *testing.T) {
	const martian = "github.com/martian-lang/martian"

	martianDir, err := ModuleDir(t.Context(), repoRoot, martian)
	if err != nil {
		t.Fatal(err)
	}

	root, err := filepath.Abs(repoRoot)
	if err != nil {
		t.Fatal(err)
	}

	work := filepath.Join(t.TempDir(), "go.work")
	if err := os.WriteFile(work, []byte("go 1.26\n\nuse (\n\t"+root+"\n\t"+martianDir+"\n)\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GOWORK", work)

	notices, err := Collect(t.Context(), repoRoot)
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range notices {
		if n.Name == martian && !slices.Contains(n.ShippedIn, "mro2nf") {
			t.Errorf("under %s, Martian ships in %v, want mro2nf among them", work, n.ShippedIn)
		}
	}
}

// TestVendoredDirsCarryLicense fails on a vendor-* directory at the repo root
// that Vendored does not map to its module, or that lacks its LICENSE.
// TestVendoredMartianMatchesModule compares the LICENSE bytes.
func TestVendoredDirsCarryLicense(t *testing.T) {
	entries, err := os.ReadDir(repoRoot)
	if err != nil {
		t.Fatal(err)
	}

	vendored := Vendored()
	seen := 0

	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "vendor-") {
			continue
		}

		seen++

		if _, ok := vendored[e.Name()]; !ok {
			t.Errorf("%s/ is not in Vendored, so its notice is not in %s", e.Name(), NoticesFile)

			continue
		}

		if _, err := os.Stat(filepath.Join(repoRoot, e.Name(), licenseName)); err != nil {
			t.Errorf("%s/ lacks its license: %v", e.Name(), err)
		}
	}

	if seen != len(vendored) {
		t.Errorf("Vendored names %d directories, the repo root has %d vendor-* directories", len(vendored), seen)
	}
}

// archiveDest is the last argument of release.yml's copy into the archive's
// top-level directory.
const archiveDest = `"$name/"`

// archiveSources returns the files release.yml copies into each release
// archive; test/e2e's TestReleaseArchiveLayout runs that step.
func archiveSources(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(repoRoot, releaseYAML))
	if err != nil {
		t.Fatal(err)
	}

	var copies [][]string

	for line := range strings.Lines(string(raw)) {
		f := strings.Fields(line)
		if len(f) > 2 && f[0] == "cp" && f[len(f)-1] == archiveDest {
			copies = append(copies, f[1:len(f)-1])
		}
	}

	if len(copies) != 1 {
		t.Fatalf("%s has %d `cp ... %s` lines, want the one that fills the archive", releaseYAML, len(copies), archiveDest)
	}

	return copies[0]
}

// TestShippedArtifactsCarryNotices keeps LICENSE and THIRD_PARTY_NOTICES in
// each release archive and in the runtime image beside mre.
func TestShippedArtifactsCarryNotices(t *testing.T) {
	sources := archiveSources(t)
	for _, f := range []string{licenseName, NoticesFile} {
		if !slices.Contains(sources, f) {
			t.Errorf("%s does not put %s in the release archive: copies %q", releaseYAML, f, sources)
		}
	}

	raw, err := os.ReadFile(filepath.Join(repoRoot, runtimeDockerfile))
	if err != nil {
		t.Fatal(err)
	}

	const copyLine = "\nCOPY " + licenseName + " " + NoticesFile + " /opt/mro2nf/\n"
	if !strings.Contains(string(raw), copyLine) {
		t.Errorf("%s lacks %q", runtimeDockerfile, strings.TrimSpace(copyLine))
	}
}

// TestReleaseShipsEveryBinary keeps each shipped binary, mrjob included, in
// each release archive, so a downloaded mre finds its mrjob beside it.
func TestReleaseShipsEveryBinary(t *testing.T) {
	bins, err := binaries(repoRoot)
	if err != nil {
		t.Fatal(err)
	}

	sources := archiveSources(t)
	for _, name := range names(bins) {
		if !slices.Contains(sources, "dist/"+name) {
			t.Errorf("%s does not put dist/%s in the release archive: copies %q", releaseYAML, name, sources)
		}
	}
}

func TestSPDX(t *testing.T) {
	cases := []struct {
		name, text, want string
		err              error
	}{
		{name: "mit", text: "Permission is hereby granted, free of charge, to any person", want: "MIT"},
		{
			name: "bsd3",
			text: "Redistribution and use in source and binary forms ... Neither the name of Google LLC",
			want: "BSD-3-Clause",
		},
		{name: "bsd2", text: "Redistribution and use in source and binary forms, with or without", want: "BSD-2-Clause"},
		{name: "apache", text: "Apache License\n                           Version 2.0, January 2004", want: "Apache-2.0"},
		{name: "gpl", text: "GNU GENERAL PUBLIC LICENSE\nVersion 3, 29 June 2007", err: errUnknownLicense},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := spdx(c.text)
			if !errors.Is(err, c.err) {
				t.Fatalf("spdx error = %v, want %v", err, c.err)
			}

			if got != c.want {
				t.Errorf("spdx = %q, want %q", got, c.want)
			}
		})
	}
}

func TestLicenseFileMissing(t *testing.T) {
	if _, _, err := LicenseFile(t.TempDir()); !errors.Is(err, errNoLicense) {
		t.Errorf("LicenseFile(empty dir) error = %v, want %v", err, errNoLicense)
	}
}

func TestReleaseTargetsNeedsMatrix(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, releaseYAML)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("jobs: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := releaseTargets(root); !errors.Is(err, errNoTargets) {
		t.Errorf("releaseTargets(no matrix) error = %v, want %v", err, errNoTargets)
	}
}
