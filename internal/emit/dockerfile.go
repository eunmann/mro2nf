package emit

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/eunmann/mro2nf/internal/apperror"
)

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
// no -shell).
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

	for name, src := range opts.StageCode {
		if err := copyTree(src, filepath.Join(rt, "stages", name)); err != nil {
			return g, err
		}

		g.code[name] = ctrStages + "/" + name
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

	return g, writeFile(filepath.Join(opts.OutDir, "Dockerfile"),
		[]byte(dockerfile(target, opts.Shell != "", len(opts.StageCode) > 0, opts.Mrjob != "", opts.NativeRunner)))
}

// dockerfile renders the runtime image: the mre shim plus the Martian adapters,
// stage code, and (comp pipelines) the mrjob wrapper at fixed paths, on a base
// with bash + ps and no ENTRYPOINT (both AWS Batch and HealthOmics inject a bash
// launcher). x86_64 only. Each COPY is emitted only when its source was staged
// (checkContainerSources verifies every source and copyTree errors on a missing
// one), so an absent optional piece never breaks `docker build`.
func dockerfile(target Target, hasAdapters, hasStages, hasMrjob, hasRunner bool) string {
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

%s
# Stage code and tools must be self-contained: HealthOmics tasks have no internet.
# Add any third-party stage dependencies here, e.g.: RUN pip install --no-cache-dir numpy
`,
		awsCLIComment(target), awsCLI, copies)
}

func awsCLIComment(target Target) string {
	if target == TargetAWSBatch {
		return " + aws CLI (S3 staging)"
	}

	return ""
}

// copyTree copies a file or directory tree from src to dst, preserving the
// executable bit. A missing src is an error: every source is pre-verified by
// checkContainerSources, so absence here means the tree changed under us.
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
