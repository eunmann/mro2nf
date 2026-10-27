//go:build e2e

package e2e

// Real-CellRanger differential (opt-in local baseline).
//
// This drives the actual 10x Genomics CellRanger `count` testrun pipeline
// under the real Martian runner (mrp) twice and under the generated Nextflow
// in two legs: the local executor, and the cloud simulation (cloudConfig:
// each task in its own scratch dir, its inputs and outputs copied, so no
// path is shared). Each leg must publish the tree mrp publishes, path for
// path and byte for byte, and run one stage task per mrp job.
//
// The two mrp runs measure what mrp itself writes differently on each run:
// each such file is compared by the part of it both mrp runs write the same
// (stableDigests), and a file mrp varies that no stable digest covers fails
// the test.
//
// It is OPT-IN: it skips unless CELLRANGER_HOME points at an extracted
// CellRanger bundle (cellranger is licensed 10x software and is NOT committed, so
// this never runs in CI or a normal local `make test`). Run it when you have a
// bundle to re-validate the transpiler against a real production pipeline:
//
//	CELLRANGER_HOME=~/Downloads/cellranger-10.1.0 make test-cellranger
//
// Knobs: CELLRANGER_LOCALCORES / CELLRANGER_LOCALMEM are the pool of every
// run: mrp's --localcores and --localmem, and the Nextflow runs'
// process.resourceLimits. The pipeline runs four times, one after another,
// so budget about eight minutes.

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// cellrangerHome resolves an extracted CellRanger bundle from CELLRANGER_HOME and
// skips when it is absent — the differential is a LOCAL baseline, never a CI or
// normal-local gate (same opt-in shape as martianBin). It checks for the pieces
// the run needs: the launcher, the env script, the mro tree, the bundled
// Martian runtime (mrp + mrjob + the python adapter), and the samtools the
// BAM comparison reads with.
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
		crSamtools,
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

// crRun is what every leg of the differential shares: the bundle, the
// invocation, the pool, the golden pipestance, and the stable digest of each
// file mrp writes differently on each run, by its path under outs/.
type crRun struct {
	home, tmp, mro string
	cores, mem     string
	golden         string
	varying        map[string]string
}

// TestCellRangerDifferential runs CellRanger `count` under mrp twice and
// under the generated Nextflow, locally and under the cloud simulation, and
// holds each Nextflow leg to mrp's published tree and job count.
func TestCellRangerDifferential(t *testing.T) {
	home := cellrangerHome(t)
	requireTools(t, "bash", "nextflow", "java", "python3")
	buildBinaries(t)

	tmp := t.TempDir()

	// The dry run writes the count pipeline's invocation __<id>.mro into the
	// cwd without running it; it names its inputs by absolute path in the
	// bundle, so nothing else needs staging.
	crBash(t, home, tmp, "cellranger testrun --dry",
		shellQuote(filepath.Join(home, "bin", "cellranger"))+" testrun --id=crdiff --dry")

	r := crRun{
		home: home, tmp: tmp, mro: filepath.Join(tmp, "__crdiff.mro"),
		cores: crResource("CELLRANGER_LOCALCORES", "4"), mem: crResource("CELLRANGER_LOCALMEM", "8"),
	}
	if !fileExists(r.mro) {
		t.Fatalf("cellranger testrun --dry did not produce %s", r.mro)
	}

	r.golden = runCrMrp(t, r, "golden")
	r.varying = measureRunVarying(t, r, runCrMrp(t, r, "rerun"))

	t.Run("local", func(t *testing.T) { runCrLeg(t, r, "local", false) })
	t.Run("cloud", func(t *testing.T) { runCrLeg(t, r, "cloud", true) })
}

// runCrMrp runs mrp on the invocation into the pipestance <tmp>/<name> and
// returns its dir. Preflights run, as `cellranger` runs them, because mro2nf
// runs them too and the job count must pair up.
func runCrMrp(t *testing.T, r crRun, name string) string {
	t.Helper()

	start := time.Now()

	crBash(t, r.home, r.tmp, "mrp "+name,
		"mrp "+shellQuote(r.mro)+" "+name+" --jobmode=local --localcores="+r.cores+
			" --localmem="+r.mem+" --disable-ui")
	t.Logf("mrp %s: %s", name, time.Since(start).Round(time.Second))

	return filepath.Join(r.tmp, name)
}

// measureRunVarying returns the stable digest of each file the golden and
// rerun pipestances published with different bytes, by its path under outs/.
// It fails for a path only one run published, a file no stable digest
// covers, and a file whose stable part differs between the runs.
func measureRunVarying(t *testing.T, r crRun, rerun string) map[string]string {
	t.Helper()

	trees := make([]map[string]string, 2)

	for i, ps := range []string{r.golden, rerun} {
		tree, err := publishedTree(filepath.Join(ps, "outs"), true, nil)
		if err != nil {
			t.Fatal(err)
		}

		trees[i] = tree
	}

	varying := map[string]string{}

	for _, p := range slices.Sorted(maps.Keys(union(trees[0], trees[1]))) {
		a, b := trees[0][p], trees[1][p]

		switch {
		case a == b:
			continue
		case a == "" || b == "":
			t.Errorf("one mrp run published %s and the other did not", p)

			continue
		}

		want, err := stableDigest(r.home, filepath.Join(r.golden, "outs", p))
		if err != nil {
			t.Errorf("mrp writes %s differently on each run: %v", p, err)

			continue
		}

		if got, err := stableDigest(r.home, filepath.Join(rerun, "outs", p)); err != nil || got != want {
			t.Errorf("mrp writes even the stable part of %s differently on each run (err %v)", p, err)

			continue
		}

		varying[p] = want
	}

	t.Logf("run-varying set (mrp vs mrp): %v", slices.Sorted(maps.Keys(varying)))

	return varying
}

// runCrLeg transpiles the invocation into its own project, runs it under
// Nextflow with its own work dir, and holds it to the golden pipestance.
func runCrLeg(t *testing.T, r crRun, name string, cloud bool) {
	t.Helper()

	proj := filepath.Join(r.tmp, "nf-"+name)
	transpileCellRanger(t, r.home, r.mro, proj)

	// The local profile grants each job from mrp's pool and clamps its task
	// to it (#207, #545), so an oversized reservation (the 30 GB cloupe join)
	// runs instead of failing.
	pool := fmt.Sprintf("process.resourceLimits = [cpus: %s, memory: %s.GB]\n", r.cores, r.mem)
	if err := os.WriteFile(filepath.Join(proj, "pool.config"), []byte(pool), 0o644); err != nil {
		t.Fatal(err)
	}

	configs := " -c pool.config"

	if cloud {
		writeCloudConfig(t, proj)

		configs += " -c cloud.config"
	}

	martian := filepath.Join(r.home, "external", "martian")
	start := time.Now()

	crBash(t, r.home, proj, "nextflow run "+name,
		"NXF_ANSI_LOG=false NXF_OFFLINE=true NXF_OPTS='-Xms256m -Xmx2g' "+
			"nextflow -q run main.nf -profile standard"+configs+" -with-trace trace.txt "+
			"-work-dir "+shellQuote(filepath.Join(r.tmp, "work-"+name))+
			" --mro2nf_mre "+shellQuote(filepath.Join(root, "mre"))+
			" --mro2nf_mrjob "+shellQuote(filepath.Join(martian, "bin", "mrjob"))+
			" --mro2nf_adapters "+shellQuote(filepath.Join(martian, "adapters")))
	t.Logf("nextflow %s: %s", name, time.Since(start).Round(time.Second))

	trace := filepath.Join(proj, "trace.txt")
	if err := checkAttempts(readTrace(t, trace)); err != nil {
		t.Error(err)
	}

	compareCrTree(t, r, filepath.Join(proj, "results"))
	checkCrStageTasks(t, name, r.golden, proj)
	reportTaskShape(t, trace)
}

// stableMark prefixes a published tree's value for a file compared by its
// stable digest, so it cannot equal a whole-file digest.
const stableMark = "<stable part> "

// compareCrTree holds the results dir to the golden pipestance's outs as the
// mrp differential does, each run-varying file by its stable digest.
func compareCrTree(t *testing.T, r crRun, results string) {
	t.Helper()

	pub, err := loadPublished(mustAllowlist(t), "cellranger", r.golden, results)
	if err != nil {
		t.Fatal(err)
	}

	for p, want := range r.varying {
		pub.mrpTree[p] = stableMark + want

		got, ok := pub.nfTree[p]
		if !ok {
			continue
		}

		digest, err := stableDigest(r.home, filepath.Join(results, p))
		if err != nil {
			t.Errorf("stable digest of published %s: %v", p, err)

			continue
		}

		mark := ""
		if strings.HasPrefix(got, linkMark) {
			mark = linkMark
		}

		pub.nfTree[p] = mark + stableMark + digest
	}

	reports, err := pub.diff()
	if err != nil {
		t.Fatal(err)
	}

	for _, rep := range reports {
		t.Error(rep)
	}
}

// checkCrStageTasks holds the leg's stage tasks to the jobs mrp ran, one
// task per job, as gen-diff does.
func checkCrStageTasks(t *testing.T, leg, golden, proj string) {
	t.Helper()

	v, err := stageTasksVerdict(leg, golden, proj)
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range v.notes {
		t.Log(n)
	}

	for _, f := range v.findings {
		t.Errorf("%s: stage tasks differ from mrp's jobs: %s", f.leg, f.detail)
	}
}

// transpileCellRanger runs the built mro2nf on a bundle .mro into proj.
func transpileCellRanger(t *testing.T, home, mro, proj string) {
	t.Helper()

	cmd := exec.Command(filepath.Join(root, "mro2nf"), "-mropath", filepath.Join(home, "mro"), "-o", proj, mro)
	cmd.Dir = root

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("transpile CellRanger: %v\n%s", err, tail(out, 30))
	}
}

// plumbingProc matches a non-stage process base name in a trace: the run's
// PLAN and its OUTS layout.
var plumbingProc = regexp.MustCompile(`^(PLAN|OUTS)$`)

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
