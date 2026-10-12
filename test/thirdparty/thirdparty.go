// Package thirdparty renders THIRD_PARTY_NOTICES from the module graph: the
// license of the Go standard library, of every third-party module linked
// into a release binary, and of each vendored directory. `make notices`
// writes it; TestNoticesAreCurrent fails while the checked-in file is stale.
package thirdparty

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// NoticesFile is the notices file at the repo root.
const NoticesFile = "THIRD_PARTY_NOTICES"

const (
	stdlibName        = "Go standard library and runtime"
	releaseYAML       = ".github/workflows/release.yml"
	runtimeDockerfile = "deploy/runtime/Dockerfile"
	cmdDir            = "cmd"
	licenseName       = "LICENSE"
)

// Vendored maps each vendored directory at the repo root to the module whose
// files it copies.
func Vendored() map[string]string {
	return map[string]string{"vendor-martian": "github.com/martian-lang/martian"}
}

// Notice is one shipped component's license.
type Notice struct {
	Name      string
	License   string
	ShippedIn []string
	Source    string
	Text      string
}

// errUnknownLicense is returned for a license text spdx cannot classify, so a
// new dependency's license is read by a person before it ships.
var errUnknownLicense = errors.New("unrecognized license")

// spdx classifies a license text by its SPDX identifier.
func spdx(text string) (string, error) {
	switch {
	case strings.Contains(text, "Permission is hereby granted, free of charge"):
		return "MIT", nil
	case strings.Contains(text, "Redistribution and use in source and binary forms") &&
		strings.Contains(text, "Neither the name"):
		return "BSD-3-Clause", nil
	case strings.Contains(text, "Redistribution and use in source and binary forms"):
		return "BSD-2-Clause", nil
	case strings.Contains(text, "Apache License") && strings.Contains(text, "Version 2.0, January 2004"):
		return "Apache-2.0", nil
	default:
		return "", errUnknownLicense
	}
}

var errNoLicense = errors.New("no license file")

// LicenseFile returns the name and text of the license file at the root of
// a module source dir.
func LicenseFile(dir string) (string, string, error) {
	for _, name := range []string{licenseName, "LICENSE.txt", "LICENSE.md", "LICENCE", "COPYING"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			return "", "", fmt.Errorf("read license: %w", err)
		}

		return name, string(raw), nil
	}

	return "", "", fmt.Errorf("%w in %s", errNoLicense, dir)
}

// goCmd runs the go command in dir with extra environment and returns its
// stdout. Workspace mode is off: a go.work would make each used module a
// main module and swap its pinned source for a local checkout, so the graph
// would not be the one release builds link.
func goCmd(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GOWORK=off"), env...)

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}

	return string(out), nil
}

var errModuleNotFound = errors.New("module source not found")

// ModuleDir returns the source dir of module path in the build list of the
// repo at root, downloading the module when it is not in the module cache.
func ModuleDir(ctx context.Context, root, path string) (string, error) {
	const format = "{{.Dir}}"

	dir, err := goCmd(ctx, root, nil, "list", "-m", "-f", format, path)
	if err != nil {
		return "", err
	}

	if strings.TrimSpace(dir) == "" {
		if _, err := goCmd(ctx, root, nil, "mod", "download", path); err != nil {
			return "", err
		}

		if dir, err = goCmd(ctx, root, nil, "list", "-m", "-f", format, path); err != nil {
			return "", err
		}
	}

	if dir = strings.TrimSpace(dir); dir == "" {
		return "", fmt.Errorf("%w: %s", errModuleNotFound, path)
	}

	return dir, nil
}

var (
	matrixEntry  = regexp.MustCompile(`goos:\s*(\w+),\s*goarch:\s*(\w+)`)
	errNoTargets = errors.New("no goos/goarch build matrix")
)

// releaseTargets returns the GOOS/GOARCH environments release.yml builds
// the binaries for.
func releaseTargets(root string) ([][]string, error) {
	raw, err := os.ReadFile(filepath.Join(root, releaseYAML))
	if err != nil {
		return nil, fmt.Errorf("read release workflow: %w", err)
	}

	var targets [][]string

	for _, m := range matrixEntry.FindAllStringSubmatch(string(raw), -1) {
		targets = append(targets, []string{"GOOS=" + m[1], "GOARCH=" + m[2], "CGO_ENABLED=0"})
	}

	if len(targets) == 0 {
		return nil, fmt.Errorf("%s: %w", releaseYAML, errNoTargets)
	}

	return targets, nil
}

// binary is a shipped binary and the package it is built from.
type binary struct {
	name, pkg string
}

// imageBuild is a `go build -o /out/<name> <package>` of the runtime image.
var imageBuild = regexp.MustCompile(`-o /out/(\S+) (\S+)`)

// binaries returns the shipped binaries, each a release binary: each
// command under cmd/, and each third-party command the runtime image builds
// (mrjob).
func binaries(root string) ([]binary, error) {
	entries, err := os.ReadDir(filepath.Join(root, cmdDir))
	if err != nil {
		return nil, fmt.Errorf("list binaries: %w", err)
	}

	var bins []binary

	for _, e := range entries {
		if e.IsDir() {
			bins = append(bins, binary{name: e.Name(), pkg: "./" + cmdDir + "/" + e.Name()})
		}
	}

	raw, err := os.ReadFile(filepath.Join(root, runtimeDockerfile))
	if err != nil {
		return nil, fmt.Errorf("read runtime image recipe: %w", err)
	}

	for _, m := range imageBuild.FindAllStringSubmatch(string(raw), -1) {
		if !strings.HasPrefix(m[2], "./") {
			bins = append(bins, binary{name: m[1], pkg: m[2]})
		}
	}

	return bins, nil
}

// names are the binaries' names.
func names(bins []binary) []string {
	out := make([]string, len(bins))
	for i, b := range bins {
		out[i] = b.name
	}

	return out
}

// linkedModule is a third-party module linked into some shipped binaries.
type linkedModule struct {
	dir  string
	bins []string
}

// linkedModules returns the third-party modules that provide a package of
// some shipped binary for some release target, keyed by module path.
func linkedModules(ctx context.Context, root string, bins []binary) (map[string]*linkedModule, error) {
	targets, err := releaseTargets(root)
	if err != nil {
		return nil, err
	}

	const format = `{{with .Module}}{{if not .Main}}{{.Path}}{{"\t"}}{{.Dir}}{{end}}{{end}}`

	mods := map[string]*linkedModule{}

	for _, bin := range bins {
		for _, env := range targets {
			out, err := goCmd(ctx, root, env, "list", "-deps", "-f", format, bin.pkg)
			if err != nil {
				return nil, err
			}

			addLinked(mods, bin.name, out)
		}
	}

	return mods, nil
}

// addLinked records each "<module path>\t<dir>" line of out as linked into
// bin.
func addLinked(mods map[string]*linkedModule, bin, out string) {
	for line := range strings.Lines(out) {
		path, dir, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}

		m := mods[path]
		if m == nil {
			m = &linkedModule{dir: dir}
			mods[path] = m
		}

		if !slices.Contains(m.bins, bin) {
			m.bins = append(m.bins, bin)
		}
	}
}

// moduleNotice builds the notice of the module path at dir, shipped in the
// binaries bins and copied by the vendored dirs vendors.
func moduleNotice(path, dir string, bins, vendors []string) (Notice, error) {
	file, text, err := LicenseFile(dir)
	if err != nil {
		return Notice{}, fmt.Errorf("module %s: %w", path, err)
	}

	id, err := spdx(text)
	if err != nil {
		return Notice{}, fmt.Errorf("module %s %s: %w", path, file, err)
	}

	sources := []string{file + " in the " + path + " module source"}
	shipped := slices.Clone(bins)

	for _, v := range vendors {
		sources = append(sources, v+"/"+licenseName)
		shipped = append(shipped, v+"/")
	}

	return Notice{Name: path, License: id, ShippedIn: shipped, Source: strings.Join(sources, "; "), Text: text}, nil
}

// stdlibNotice builds the notice of the Go standard library and runtime,
// which every binary links.
func stdlibNotice(ctx context.Context, root string, bins []string) (Notice, error) {
	goroot, err := goCmd(ctx, root, nil, "env", "GOROOT")
	if err != nil {
		return Notice{}, err
	}

	raw, err := os.ReadFile(filepath.Join(strings.TrimSpace(goroot), licenseName))
	if err != nil {
		return Notice{}, fmt.Errorf("read Go license: %w", err)
	}

	id, err := spdx(string(raw))
	if err != nil {
		return Notice{}, fmt.Errorf("go %s: %w", licenseName, err)
	}

	return Notice{
		Name: stdlibName, License: id, ShippedIn: bins,
		Source: licenseName + " in the Go distribution", Text: string(raw),
	}, nil
}

// Collect returns the notices for the repo at root: the Go standard library
// first, then each linked or vendored module by path.
func Collect(ctx context.Context, root string) ([]Notice, error) {
	bins, err := binaries(root)
	if err != nil {
		return nil, err
	}

	mods, err := linkedModules(ctx, root, bins)
	if err != nil {
		return nil, err
	}

	vendors := map[string][]string{}

	for dir, path := range Vendored() {
		vendors[path] = append(vendors[path], dir)

		if mods[path] == nil {
			mdir, err := ModuleDir(ctx, root, path)
			if err != nil {
				return nil, err
			}

			mods[path] = &linkedModule{dir: mdir}
		}
	}

	std, err := stdlibNotice(ctx, root, names(bins))
	if err != nil {
		return nil, err
	}

	notices := []Notice{std}

	for _, path := range slices.Sorted(maps.Keys(mods)) {
		slices.Sort(vendors[path])

		n, err := moduleNotice(path, mods[path].dir, mods[path].bins, vendors[path])
		if err != nil {
			return nil, err
		}

		notices = append(notices, n)
	}

	return notices, nil
}

const header = `Third-party notices

mro2nf is under the MIT license in LICENSE. Its release binaries (mro2nf,
mre, mrjob) and its runtime image (mre, mrjob and Martian's adapters at
/opt/mro2nf), which a pipeline's own image builds FROM, also contain
the third-party code below, each under the license reproduced with its
entry. "Shipped in" names the binaries that statically link the code and the
vendored directories that copy it.

Generated from the module graph; regenerate with: make notices
`

// Render formats notices as THIRD_PARTY_NOTICES.
func Render(notices []Notice) string {
	const width = 80

	var b strings.Builder

	b.WriteString(header)

	for _, n := range notices {
		b.WriteString("\n" + strings.Repeat("=", width) + "\n")
		b.WriteString(n.Name + "\n")
		b.WriteString("License: " + n.License + "\n")
		b.WriteString("Shipped in: " + strings.Join(n.ShippedIn, ", ") + "\n")
		b.WriteString("Notice: " + n.Source + "\n")
		b.WriteString(strings.Repeat("-", width) + "\n\n")
		b.WriteString(n.Text)

		if !strings.HasSuffix(n.Text, "\n") {
			b.WriteString("\n")
		}
	}

	return b.String()
}

// Write regenerates THIRD_PARTY_NOTICES in the repo at root, keeping its mode.
func Write(ctx context.Context, root string) error {
	path := filepath.Join(root, NoticesFile)

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", NoticesFile, err)
	}

	notices, err := Collect(ctx, root)
	if err != nil {
		return err
	}

	if err := os.WriteFile(path, []byte(Render(notices)), info.Mode().Perm()); err != nil {
		return fmt.Errorf("write %s: %w", NoticesFile, err)
	}

	return nil
}
