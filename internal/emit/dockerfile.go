package emit

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/eunmann/mro2nf/internal/apperror"
)

// isBakedToolkitCmd reports whether a stage src is a bare command resolved on the
// image's PATH rather than a vendorable filesystem path. The container contract
// mirrors the local one: a src that IS a path (has a separator) is vendored into
// the image automatically and needs nothing from the user; a bare command (no
// separator — an installed toolkit like samtools, STAR, or a multi-command
// binary such as CellRanger's cr_lib) is the image's responsibility to provide on
// PATH. A bare binary's own runtime deps (shared libs, interpreters, data) cannot
// be vendored reliably from its name alone, so the transpiler passes it through
// verbatim instead of copying it — the same classification the frontend already
// makes (ir.Stage.SrcIsPathCommand), re-derived here structurally from the path.
func isBakedToolkitCmd(src string) bool {
	return !strings.ContainsRune(src, filepath.Separator)
}

// bakedToolkitCmds returns the sorted, unique bare commands the image must
// provide on PATH — surfaced in the Dockerfile so the contract is explicit.
func bakedToolkitCmds(stageCode map[string]string) []string {
	set := map[string]bool{}
	for _, src := range stageCode {
		if isBakedToolkitCmd(src) {
			set[src] = true
		}
	}

	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}

	sort.Strings(out)

	return out
}

// In-container layout the generated scripts reference and the Dockerfile fills.
const (
	ctrRoot     = "/opt/mro2nf"
	ctrMre      = ctrRoot + "/mre"
	ctrAdapters = ctrRoot + "/adapters"
	ctrMrjob    = ctrRoot + "/mrjob"
	ctrStages   = ctrRoot + "/stages"
	ctrRunner   = ctrRoot + "/runner" // -native-runner Python runner, baked for container backends
	buildCtxDir = "runtime"           // build-context subdir under the output project
)

// checkContainerSources fails loudly if a baked runtime source is missing,
// rather than letting it surface as a cryptic `docker build` COPY error or a
// silently broken image. mre is always required; -shell, -mrjob, and each
// stage's code are verified when present (an exec/comp-only pipeline may have
// no -shell). Note the -shell check stats the FILE, while containerBuild
// copies its parent adapters directory: the dir's existence is implied by the
// file check, and per-entry read failures inside any tree still surface as
// copyTree errors.
func checkContainerSources(opts Options) error {
	if opts.Mre == "" {
		return &apperror.UnsupportedError{Construct: "container target", Detail: "-mre is required (it is baked into the image)"}
	}

	toCheck := map[string]string{"-mre": opts.Mre}
	if opts.Shell != "" {
		toCheck["-shell"] = opts.Shell
	}

	if opts.Mrjob != "" {
		toCheck["-mrjob"] = opts.Mrjob
	}

	for name, src := range opts.StageCode {
		// A bare-command src is resolved on the image PATH, not vendored, so there
		// is no host file to stat (see isBakedToolkitCmd / vendorStageCode).
		if isBakedToolkitCmd(src) {
			continue
		}

		toCheck["stage "+name] = src
	}

	// Sorted so that when several sources are missing the reported one is stable
	// (map iteration order is randomized), keeping the error reproducible.
	for _, what := range sortedKeys(toCheck) {
		if _, err := os.Stat(toCheck[what]); err != nil {
			return fmt.Errorf("container target: %s source %q: %w", what, toCheck[what], err)
		}
	}

	return nil
}

// containerBuild assembles a self-contained Docker build context under the
// output project and returns the in-container paths the generated scripts bake
// (so mre, the adapters, the stage code, and — under -native-runner — the
// Python runner resolve inside an isolated task). The host artifacts are copied
// into <out>/runtime/ and a Dockerfile is written so `docker build -t <image>
// <out>` produces the runtime image.
func containerBuild(opts Options, target Target) (genCtx, error) {
	g := genCtx{mre: ctrMre, shell: opts.Shell, mrjob: opts.Mrjob, code: map[string]string{}}

	if err := checkContainerSources(opts); err != nil {
		return g, err
	}

	rt := filepath.Join(opts.OutDir, buildCtxDir)
	if err := os.MkdirAll(rt, dirPerm); err != nil {
		return g, fmt.Errorf("create build context: %w", err)
	}

	if err := copyTree(opts.Mre, filepath.Join(rt, "mre")); err != nil {
		return g, err
	}

	if opts.Shell != "" {
		if err := copyTree(filepath.Dir(opts.Shell), filepath.Join(rt, "adapters")); err != nil {
			return g, err
		}

		g.shell = ctrAdapters + "/" + filepath.Base(opts.Shell)
	}

	if opts.Mrjob != "" {
		if err := copyTree(opts.Mrjob, filepath.Join(rt, "mrjob")); err != nil {
			return g, err
		}

		g.mrjob = ctrMrjob
	}

	if err := vendorStageCode(rt, opts.StageCode, g.code); err != nil {
		return g, err
	}

	// -native-runner reads run_stage.py from the project dir at task runtime,
	// which an isolated container worker (AWS Batch / HealthOmics) cannot see —
	// so bake the embedded runner into the image and point the generated scripts
	// at the fixed in-image path (#99).
	if opts.NativeRunner {
		if err := copyEmbeddedDir(runnerAssets, runnerDir, filepath.Join(rt, "runner"), "runner"); err != nil {
			return g, err
		}

		g.runnerBase = ctrRunner
	}

	hasVendored := false
	for _, src := range opts.StageCode {
		if !isBakedToolkitCmd(src) {
			hasVendored = true

			break
		}
	}

	return g, writeFile(filepath.Join(opts.OutDir, "Dockerfile"),
		[]byte(dockerfile(target, opts.Shell != "", hasVendored, opts.Mrjob != "",
			opts.NativeRunner, bakedToolkitCmds(opts.StageCode))))
}

// vendorStageCode copies each stage's code under runtime/stages/<name>, deduped
// by resolved source: a source that backs many stage names is vendored once and
// the rest become relative symlinks to that copy. A handful of multi-command
// binaries (CellRanger's cr_lib, cr_vdj, ...) each implement dozens of comp
// stage names, so copying per name inflates the build context and image by the
// fan-out factor (#215). Docker COPY preserves in-tree symlinks verbatim, so the
// image keeps the same file at the same path for every name at a fraction of the
// size. Names are walked in sorted order so the canonical copy (the symlink
// target) is stable and the build reproducible.
func vendorStageCode(rt string, stageCode, code map[string]string) error {
	stagesDir := filepath.Join(rt, "stages")
	canonical := map[string]string{} // resolved src -> first stage name copied there

	for _, name := range sortedKeys(stageCode) {
		src := stageCode[name]
		// A bare command is provided on the image PATH (bake the toolkit); pass it
		// through verbatim so the stage runs it via PATH, exactly as the local
		// target does. Only path-based srcs are vendored into the image.
		if isBakedToolkitCmd(src) {
			code[name] = src

			continue
		}

		key := stageCodeKey(src)
		if first, ok := canonical[key]; ok {
			if err := symlinkSibling(stagesDir, first, name); err != nil {
				return err
			}
		} else {
			if err := copyTree(src, filepath.Join(stagesDir, name)); err != nil {
				return err
			}

			canonical[key] = name
		}

		code[name] = ctrStages + "/" + name
	}

	return nil
}

// stageCodeKey canonicalizes a stage source path for dedup: two stage names
// resolving to the same on-disk file (after resolving symlinks — e.g. two mropath
// compatibility symlinks pointing at one binary) share a single copy. Best
// effort: an un-resolvable path keys on its cleaned absolute form, which at worst
// misses a dedup, never over-dedups distinct sources.
func stageCodeKey(src string) string {
	abs, err := filepath.Abs(src)
	if err != nil {
		return filepath.Clean(src)
	}

	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}

	return abs
}

// symlinkSibling links stagesDir/name to sibling stagesDir/target with a relative
// symlink (just the target's name), so the stages tree stays self-contained and
// relocatable and Docker COPY preserves it into the image unchanged.
func symlinkSibling(stagesDir, target, name string) error {
	if err := os.MkdirAll(stagesDir, dirPerm); err != nil {
		return fmt.Errorf("mkdir %s: %w", stagesDir, err)
	}

	dst := filepath.Join(stagesDir, name)
	if err := os.Symlink(target, dst); err != nil {
		return fmt.Errorf("symlink %s -> %s: %w", dst, target, err)
	}

	return nil
}

// dockerfile renders the runtime image: the mre shim plus the Martian adapters,
// stage code, and (comp pipelines) the mrjob wrapper at fixed paths, on a base
// with bash + ps and no ENTRYPOINT (both AWS Batch and HealthOmics inject a bash
// launcher). x86_64 only. Each COPY is emitted only when its source was staged
// (checkContainerSources verifies the on-disk sources and copyTree errors on a
// missing one; the -native-runner runner is copied from the embedded FS by
// copyEmbeddedDir, so it cannot be absent), so an absent optional piece never
// breaks `docker build`.
func dockerfile(target Target, hasAdapters, hasStages, hasMrjob, hasRunner bool, pathCmds []string) string {
	awsCLI := ""
	if target == TargetAWSBatch {
		// Classic AWS Batch staging copies inputs/outputs with the aws CLI.
		awsCLI = " awscli"
	}

	copies := fmt.Sprintf("COPY %s/mre %s\nRUN chmod +x %s\n", buildCtxDir, ctrMre, ctrMre)
	if hasAdapters {
		copies += fmt.Sprintf("COPY %s/adapters %s\n", buildCtxDir, ctrAdapters)
	}
	if hasStages {
		copies += fmt.Sprintf("COPY %s/stages %s\n", buildCtxDir, ctrStages)
	}
	if hasMrjob {
		copies += fmt.Sprintf("COPY %s/mrjob %s\nRUN chmod +x %s\n", buildCtxDir, ctrMrjob, ctrMrjob)
	}
	if hasRunner {
		copies += fmt.Sprintf("COPY %s/runner %s\n", buildCtxDir, ctrRunner)
	}

	return fmt.Sprintf(`# Runtime image for the transpiled pipeline. Build (x86_64 only):
#   docker build --platform linux/amd64 -t <image> .
# Then transpile with --container <image> and push to your registry
# (a private ECR repo in the same region for AWS HealthOmics).
FROM --platform=linux/amd64 python:3.12-slim

# bash (launcher), ps (Nextflow metrics), coreutils%s. No ENTRYPOINT: the backend
# invokes the generated command with a bash launcher directly.
RUN apt-get update \
 && apt-get install -y --no-install-recommends procps coreutils%s \
 && rm -rf /var/lib/apt/lists/*

%s%s
# Stage code and tools must be self-contained: HealthOmics tasks have no internet.
# Add any third-party stage dependencies here, e.g.: RUN pip install --no-cache-dir numpy
`,
		awsCLIComment(target), awsCLI, copies, pathCmdContract(pathCmds))
}

// pathCmdContract renders the container contract for any bare-command stage srcs:
// they are resolved on the image PATH (not vendored), so the image MUST provide
// them. Empty when the pipeline has none, so a fully self-contained image reads
// exactly as before.
func pathCmdContract(cmds []string) string {
	if len(cmds) == 0 {
		return ""
	}

	return fmt.Sprintf(`# This pipeline runs these stage commands from the image PATH (bare `+"`src comp`"+`/
# `+"`src exec`"+` names, not vendored — a bare binary's own runtime deps cannot be
# copied from its name alone): %s
# You MUST install them, with their runtime dependencies, into this image so they
# resolve on PATH (e.g. COPY the toolkit in and prepend its bin dir to PATH).
# Stage code given as a path is vendored into %s automatically above.
`, strings.Join(cmds, ", "), ctrStages)
}

func awsCLIComment(target Target) string {
	if target == TargetAWSBatch {
		return " + aws CLI (S3 staging)"
	}

	return ""
}

// copyTree copies a file or directory tree from src to dst, preserving the
// executable bit. It is not a best-effort copier: callers pre-verify sources
// via checkContainerSources, so a missing src is a hard error (for the
// adapters dir the pre-check stats the -shell file, which implies the parent
// dir exists), and any per-entry read failure aborts the copy.
func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}

	if !info.IsDir() {
		return copyFile(src, dst, info.Mode())
	}

	err = filepath.WalkDir(src, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return fmt.Errorf("rel %s: %w", path, err)
		}

		target := filepath.Join(dst, rel)
		if d.IsDir() {
			if err := os.MkdirAll(target, dirPerm); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}

			return nil
		}

		fi, err := d.Info()
		if err != nil {
			return fmt.Errorf("info %s: %w", path, err)
		}

		return copyFile(path, target, fi.Mode())
	})
	if err != nil {
		return fmt.Errorf("copy tree %s: %w", src, err)
	}

	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), dirPerm); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
	}

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()

		return fmt.Errorf("copy to %s: %w", dst, err)
	}

	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}

	return nil
}
