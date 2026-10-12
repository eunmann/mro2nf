package cgnf

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// codeRoot is a host dir the stage image holds, at image.
type codeRoot struct {
	host, image string
}

// codeRoots are the host dirs the stage image holds, none below another:
// each at its own path below imageCode, the entry's dir, the search path's
// roots, the dir of every .mro file the entry compiles from, and every
// stage's code Martian finds as a file; and at its own path, the code of
// a stage whose src is absolute, which Martian finds there unchanged.
// commands are the stage srcs found on no root, which the image's PATH must
// provide.
func (p *project) codeRoots() ([]codeRoot, []string, error) {
	dirs := append([]string{p.src.Dir}, p.opts.MroPaths...)

	for _, f := range p.src.Files {
		dirs = append(dirs, filepath.Dir(f))
	}

	var abs, commands []string

	for _, n := range p.topo.Nodes {
		s, err := p.stage(n.Fqid)
		if err != nil {
			return nil, nil, err
		}

		code, err := s.Src.FindPath(p.opts.MroPaths)
		if err != nil {
			if !slices.Contains(commands, s.Src.Path) {
				commands = append(commands, s.Src.Path)
			}

			continue
		}

		if info, err := os.Stat(code); err == nil && !info.IsDir() {
			code = filepath.Dir(code)
		}

		if filepath.IsAbs(s.Src.Path) {
			abs = append(abs, code)
		} else {
			dirs = append(dirs, code)
		}
	}

	slices.Sort(commands)

	var roots []codeRoot

	for _, d := range outermost(dirs) {
		roots = append(roots, codeRoot{host: d, image: imageCode + d})
	}

	for _, d := range outermost(abs) {
		roots = append(roots, codeRoot{host: d, image: d})
	}

	return roots, commands, nil
}

// outermost are the dirs, cleaned, each once, less those below another.
func outermost(dirs []string) []string {
	clean := make([]string, len(dirs))
	for i, d := range dirs {
		clean[i] = filepath.Clean(d)
	}

	slices.Sort(clean)
	clean = slices.Compact(clean)

	var out []string

	for _, d := range clean {
		if len(out) > 0 && within(d, out[len(out)-1]) {
			continue
		}

		out = append(out, d)
	}

	return out
}

func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)

	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// dockerfile is the project's stage image recipe: the release runtime
// image, the one definition of /opt/mro2nf, and the pipeline's code, each
// root a named build context (build.sh), so no code is copied into the
// project. BASE layers the runtime onto a toolkit image instead.
func (p *project) dockerfile(roots []codeRoot, commands []string) string {
	var b strings.Builder

	fmt.Fprintf(&b, `# This project's stage image: mro2nf's runtime image (mre, mrjob and
# Martian's adapters below %[1]s) and the pipeline's code, each root at its
# host path below %[2]s, where the container profiles find it, and the code
# of a stage whose src is absolute at that path itself. Build it with
# ./build.sh <tag>. RUNTIME names the runtime image, by default the release's
# that transpiled the project, and must be passed for a transpiler build no
# release published; BASE layers the runtime onto a toolkit image, which
# must provide bash, ps and, for py stages, python3.
ARG RUNTIME%[3]s
ARG BASE=${RUNTIME}
FROM ${RUNTIME} AS runtime
FROM ${BASE}
COPY --from=runtime %[1]s %[1]s
`, imageRoot, imageCode, orEmpty("=", p.releaseRuntime()))

	for i, r := range roots {
		dest, _ := json.Marshal(r.image)
		fmt.Fprintf(&b, "COPY --from=code%d [\".\", %s]\n", i, dest)
	}

	if len(commands) > 0 {
		b.WriteString("# The image's PATH must provide these stage srcs, which name no file of\n# the code: " + strings.Join(commands, ", ") + "\n")
	}

	return b.String()
}

// orEmpty is prefix+s, or "" for an empty s.
func orEmpty(prefix, s string) string {
	if s == "" {
		return ""
	}

	return prefix + s
}

// unpublishedRuntime is build.sh's refusal, for a transpiler build no
// release published the runtime image of, to build without one named.
const unpublishedRuntime = `case " $* " in
    *" RUNTIME="* | *"=RUNTIME="*) ;;
    *)
        echo "build.sh: no release published this transpiler build's runtime image: build it with docker build -f deploy/runtime/Dockerfile -t <image> . in an mro2nf checkout, then pass --build-arg RUNTIME=<image>" >&2
        exit 2
        ;;
esac
`

// buildScript is build.sh, which builds the stage image with each code
// root as its named build context, FROM the runtime image runtime, or one
// the caller names.
func buildScript(roots []codeRoot, runtime string) string {
	var b strings.Builder

	b.WriteString(`#!/usr/bin/env bash
# Builds this project's stage image from its Dockerfile, each root of the
# pipeline's code a named build context:
#   ./build.sh <tag> [docker build flags], e.g. --build-arg RUNTIME=<image>
set -euo pipefail
cd "$(dirname "$0")"
if [ $# -lt 1 ]; then
    echo "usage: build.sh <tag> [docker build flags]" >&2
    exit 2
fi
tag=$1
shift
`)

	if runtime == "" {
		b.WriteString(unpublishedRuntime)
	}

	b.WriteString("exec docker build")

	for i, r := range roots {
		fmt.Fprintf(&b, " \\\n    --build-context code%d=%s", i, shellQuote(r.host))
	}

	b.WriteString(` \
    -t "$tag" "$@" .
`)

	return b.String()
}

// dockerignore keeps the project out of the build: the Dockerfile copies
// only from the runtime image and the named code contexts.
const dockerignore = "*\n"

// executable reports a generated file a user runs.
func executable(name string) bool {
	return name == "build.sh" || name == "package.sh"
}

// fileMode is the mode a generated file is written with.
func fileMode(name string) os.FileMode {
	if executable(name) {
		return execPerm
	}

	return filePerm
}
