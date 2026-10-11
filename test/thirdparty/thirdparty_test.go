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
// library, the parser mro2nf links, each vendored module, and no module only
// the tests import (go-cmp).
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

// TestVendoredDirsCarryLicense fails on a vendor-* directory at the repo root
// that Vendored does not map to its module, or whose LICENSE is missing or
// differs from that module's.
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

		mod, ok := vendored[e.Name()]
		if !ok {
			t.Errorf("%s/ is not in Vendored, so its notice is not in %s", e.Name(), NoticesFile)

			continue
		}

		got, err := os.ReadFile(filepath.Join(repoRoot, e.Name(), licenseName))
		if err != nil {
			t.Errorf("%s/ lacks its license: %v", e.Name(), err)

			continue
		}

		dir, err := ModuleDir(t.Context(), repoRoot, mod)
		if err != nil {
			t.Fatal(err)
		}

		_, want, err := LicenseFile(dir)
		if err != nil {
			t.Fatal(err)
		}

		if want != string(got) {
			t.Errorf("%s/%s differs from %s's license", e.Name(), licenseName, mod)
		}
	}

	if seen != len(vendored) {
		t.Errorf("Vendored names %d directories, the repo root has %d vendor-* directories", len(vendored), seen)
	}
}

// TestShippedArtifactsCarryNotices keeps LICENSE and THIRD_PARTY_NOTICES in
// the release assets and in the runtime image beside mre.
func TestShippedArtifactsCarryNotices(t *testing.T) {
	read := func(rel string) string {
		raw, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatal(err)
		}

		return string(raw)
	}

	release := read(releaseYAML)
	for _, f := range []string{licenseName, NoticesFile} {
		if !strings.Contains(release, "\n            "+f+"\n") {
			t.Errorf("%s does not attach %s to the release", releaseYAML, f)
		}
	}

	const copyLine = "\nCOPY " + licenseName + " " + NoticesFile + " /opt/mro2nf/\n"
	if !strings.Contains(read("deploy/runtime/Dockerfile"), copyLine) {
		t.Errorf("deploy/runtime/Dockerfile lacks %q", strings.TrimSpace(copyLine))
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
		{name: "apache", text: "Apache License\nVersion 2.0, January 2004", err: errUnknownLicense},
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
