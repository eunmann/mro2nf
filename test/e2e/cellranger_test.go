//go:build e2e

package e2e

// Real-CellRanger differential (opt-in local baseline).
//
// This drives the actual 10x Genomics CellRanger `count` testrun pipeline BOTH
// ways — the real Martian runner (mrp) for the golden, and the generated Nextflow
// — and asserts the published metrics are byte-identical, then reports the
// Nextflow task-shape so it can be tracked as a baseline over time.
//
// It is OPT-IN: it skips unless CELLRANGER_HOME points at an extracted
// CellRanger bundle (cellranger is licensed 10x software and is NOT committed, so
// this never runs in CI or a normal local `make test`). Run it when you have a
// bundle to re-validate the transpiler against a real production pipeline:
//
//	CELLRANGER_HOME=~/Downloads/cellranger-10.1.0 make test-cellranger
//
// Knobs: CELLRANGER_LOCALCORES / CELLRANGER_LOCALMEM size the mrp golden (the
// Nextflow run clamps to the host via process.resourceLimits). The whole thing
// runs the pipeline twice, so budget a few minutes.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cellrangerHome resolves an extracted CellRanger bundle from CELLRANGER_HOME and
// skips when it is absent — the differential is a LOCAL baseline, never a CI or
// normal-local gate (same opt-in shape as martianBin). It checks for the pieces
// the run needs: the launcher, the env script, the mro tree, and the bundled
// Martian runtime (mrp + mrjob + the python adapter).
func cellrangerHome(t *testing.T) string {
	t.Helper()

	home := os.Getenv("CELLRANGER_HOME")
	if home == "" {
		t.Skip("CELLRANGER_HOME not set; skipping the CellRanger differential " +
			"(opt-in local baseline — point it at an extracted cellranger-x.y.z bundle)")
	}

	for _, rel := range []string{
		"bin/cellranger",
		"sourceme.bash",
		"mro",
		"external/martian/bin/mrp",
		"external/martian/bin/mrjob",
		"external/martian/adapters/python/martian_shell.py",
	} {
		if !fileExists(filepath.Join(home, rel)) {
			t.Skipf("CELLRANGER_HOME=%s is missing %s; not a complete CellRanger bundle", home, rel)
		}
	}

	return home
}

// crBash runs a command line with the CellRanger environment sourced from dir.
// sourceme.bash prepends the bundle's bin dirs (cr_*/mrjob/mrp and the bundled
// python3) to PATH and sets PYTHONPATH/MROPATH, so mrp and the Nextflow tasks
// resolve the toolkit exactly as `cellranger` itself does. sourceme references
// unset shell vars, so it must run WITHOUT `set -u`.
func crBash(t *testing.T, home, dir, label, cmdline string) string {
	t.Helper()

	cmd := exec.Command("bash", "-c", "source '"+home+"/sourceme.bash'; "+cmdline)
	cmd.Dir = dir

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", label, err, tail(out, 30))
	}

	return string(out)
}

// crResource reads a positive integer resource knob, defaulting when unset.
func crResource(env, def string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}

	return def
}

// TestCellRangerDifferential runs CellRanger `count` under mrp and under the
// generated Nextflow and asserts byte-identical published metrics.
func TestCellRangerDifferential(t *testing.T) {
	home := cellrangerHome(t)
	requireTools(t, "bash", "nextflow", "java", "python3")
	buildBinaries(t)

	tmp := t.TempDir()

	// 1. Emit the count-pipeline invocation .mro (cellranger testrun --dry). The
	//    dry run writes __<id>.mro into the cwd and does not execute the pipeline.
	crBash(t, home, tmp, "cellranger testrun --dry",
		shellQuote(filepath.Join(home, "bin", "cellranger"))+" testrun --id=crdiff --dry")

	mro := filepath.Join(tmp, "__crdiff.mro")
	if !fileExists(mro) {
		t.Fatalf("cellranger testrun --dry did not produce %s", mro)
	}

	// 2. Golden: the real Martian runner on that .mro. mrp creates the pipestance
	//    (named "golden") in the cwd; the .mro references its inputs by absolute
	//    path in the bundle, so nothing else needs staging.
	cores, mem := crResource("CELLRANGER_LOCALCORES", "4"), crResource("CELLRANGER_LOCALMEM", "8")
	crBash(t, home, tmp, "mrp golden run",
		"mrp "+shellQuote(mro)+" golden --jobmode=local --localcores="+cores+
			" --localmem="+mem+" --disable-ui --nopreflight")

	goldenMetrics := filepath.Join(tmp, "golden", "outs", "metrics_summary.csv")
	if !fileExists(goldenMetrics) {
		t.Fatalf("mrp golden did not publish %s", goldenMetrics)
	}

	// 3. Transpile the SAME .mro (pure Go; no CellRanger env needed). Point
	//    -mrjob/-shell at the bundle's own Martian runtime.
	proj := filepath.Join(tmp, "nf")
	transpileCellRanger(t, home, mro, proj)

	// 4. Run the generated Nextflow with the bundle env sourced. The emitted
	//    local profile clamps each task's request to the host (#207), so an
	//    oversized reservation (the 30 GB cloupe join) runs instead of parking.
	crBash(t, home, proj, "nextflow run",
		"NXF_ANSI_LOG=false NXF_OFFLINE=true NXF_OPTS='-Xms256m -Xmx2g' "+
			"nextflow -q run main.nf -profile standard -with-trace trace.txt "+
			"-work-dir "+shellQuote(filepath.Join(tmp, "nfwork")))

	nfOuts := filepath.Join(proj, "results")

	// 5. The biological result must be byte-identical to mrp. metrics_summary.csv
	//    is the canonical single-cell result (cells, reads, mapping) and carries
	//    no run-id/timestamp, so it compares exactly across the two runners.
	assertBytesEqual(t, goldenMetrics, filepath.Join(nfOuts, "metrics_summary.csv"), "metrics_summary.csv")

	// 6. The expected published outputs are all present.
	for _, o := range []string{
		"filtered_feature_bc_matrix.h5",
		"raw_feature_bc_matrix.h5",
		"cloupe.cloupe",
		"web_summary.html",
		"possorted_genome_bam.bam",
		"molecule_info.h5",
	} {
		if !fileExists(filepath.Join(nfOuts, o)) {
			t.Errorf("generated Nextflow did not publish expected output %s", o)
		}
	}

	// 7. Record the task-shape as a tracked baseline (stage vs plumbing task
	//    counts) — the number future optimizer changes are compared against.
	reportTaskShape(t, filepath.Join(proj, "trace.txt"))
}

// transpileCellRanger runs the built mro2nf on a bundle .mro into proj, wiring the
// bundle's Martian runtime for the comp/py stages.
func transpileCellRanger(t *testing.T, home, mro, proj string) {
	t.Helper()

	cmd := exec.Command(filepath.Join(root, "mro2nf"),
		"-mropath", filepath.Join(home, "mro"),
		"-mre", filepath.Join(root, "mre"),
		"-mrjob", filepath.Join(home, "external", "martian", "bin", "mrjob"),
		"-shell", filepath.Join(home, "external", "martian", "adapters", "python", "martian_shell.py"),
		"-o", proj, mro,
	)
	cmd.Dir = root

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("transpile CellRanger: %v\n%s", err, tail(out, 30))
	}
}

// assertBytesEqual fails if two files differ byte-for-byte.
func assertBytesEqual(t *testing.T, want, got, label string) {
	t.Helper()

	a, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("read golden %s: %v", label, err)
	}

	b, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("read generated %s: %v", label, err)
	}

	if !bytes.Equal(a, b) {
		t.Errorf("%s differs between mrp and generated Nextflow\n--- mrp ---\n%s\n--- nextflow ---\n%s",
			label, tail(a, 6), tail(b, 6))
	}
}

// plumbingProc matches a data-plane (non-stage) process base name in a trace.
var plumbingProc = regexp.MustCompile(`^(BIND|FORK|MERGE|DISABLE)_|^(PUBLISH|PUBLISH_LEAF|LAYOUT|BUILD_ENTRY_ARGS)$`)

// reportTaskShape logs the Nextflow run's total / stage / plumbing task counts
// from its -with-trace file — the baseline a future optimizer change is measured
// against (t.Log so it prints under `go test -v`, not an assertion).
func reportTaskShape(t *testing.T, trace string) {
	t.Helper()

	data, err := os.ReadFile(trace)
	if err != nil {
		t.Logf("task-shape: no trace at %s: %v", trace, err)

		return
	}

	seen := map[string]bool{}
	total, plumbing := 0, 0

	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue // header / blank
		}

		cols := strings.Split(line, "\t")
		if len(cols) < 4 {
			continue
		}

		name := cols[3]
		if seen[name] {
			continue // dedupe retries by fully-qualified name
		}

		seen[name] = true
		total++

		base := name[strings.LastIndex(name, ":")+1:]
		if idx := strings.Index(base, " ("); idx >= 0 {
			base = base[:idx]
		}

		if plumbingProc.MatchString(base) {
			plumbing++
		}
	}

	t.Logf("CellRanger task-shape baseline: %d tasks (%d stage, %d plumbing)",
		total, total-plumbing, plumbing)
}

// shellQuote single-quotes a path for embedding in a bash -c command line.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
