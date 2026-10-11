package emit

import (
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
)

// TestResourceFlagsPassStageMemory pins #243: the memory a stage sees is its
// Martian request, not Nextflow's integer-truncated, attempt-scaled task.memory
// (mrp gives a using(mem_gb = 1.5) stage 1.5). An overrides overlay's
// ext.mem_gb rides as its own flag, since it beats a chunk's request (#248),
// and only the -monitor limits read the task's allocation.
func TestResourceFlagsPassStageMemory(t *testing.T) {
	s := &ir.Stage{Name: "MEM", Lang: ir.LangPy, Resources: ir.Resources{MemGB: 1.5}}

	plain := genCtx{}.resourceFlags("main", s)
	if !strings.Contains(plain, " -memgb 1.5 ") || !strings.Contains(plain, " -overridememgb ${task.ext.mem_gb ?: 0}") {
		t.Errorf("main must pass the stage's mem_gb unrounded and the override apart: %q", plain)
	}

	if strings.Contains(plain, "task.memory") || strings.Contains(plain, "-attempt") {
		t.Errorf("without -monitor no task allocation may reach mre: %q", plain)
	}

	mon := genCtx{monitor: true}.resourceFlags("join", s)
	for _, want := range []string{" -joinres joinres.json", " -monitor -attempt ${task.attempt} -allocbytes ${task.memory.toBytes()}"} {
		if !strings.Contains(mon, want) {
			t.Errorf("join under -monitor missing %q: %q", want, mon)
		}
	}

	small := &ir.Stage{Name: "SMALL", Resources: ir.Resources{MemGB: float64(float32(0.3))}}
	if got := (genCtx{}).resourceFlags("main", small); !strings.Contains(got, " -memgb 0.30000001192092896 ") {
		t.Errorf("a float32 mem_gb must reach mre exactly as mrp widens it: %q", got)
	}
}

// TestResourceFlagsPassStageThreads pins #370: the threads a stage sees are its
// Martian request (0.5 stays 0.5, as in mrp), not Nextflow's integer task.cpus;
// an overrides overlay's ext.threads rides as its own flag.
func TestResourceFlagsPassStageThreads(t *testing.T) {
	s := &ir.Stage{Name: "REPORT", Resources: ir.Resources{Threads: 0.5}}

	got := genCtx{}.resourceFlags("main", s)
	for _, want := range []string{" -threads 0.5 ", " -overridethreads ${task.ext.threads ?: 0}"} {
		if !strings.Contains(got, want) {
			t.Errorf("main flags missing %q: %q", want, got)
		}
	}

	if strings.Contains(got, "task.cpus") {
		t.Errorf("the scheduler's cpus must not reach the stage: %q", got)
	}
}

// TestResourceFlagsPassStaticSpecial pins #369: mrp writes a static
// using(special) into main and join _args as __special, so every phase passes
// it to mre as one bash word that survives both the GString and bash.
func TestResourceFlagsPassStaticSpecial(t *testing.T) {
	s := &ir.Stage{Name: "S", Resources: ir.Resources{Special: "gpu:2"}}
	if got := (genCtx{}).resourceFlags("main", s); !strings.Contains(got, " -special 'gpu:2'") {
		t.Errorf("static special missing from the main flags: %q", got)
	}

	if got := (genCtx{}).resourceFlags("main", &ir.Stage{Name: "N"}); strings.Contains(got, "-special") {
		t.Errorf("a stage without special must pass none: %q", got)
	}

	// Groovy turns \\ into \, \$ into $ and \" into ", leaving the bash word
	// 'a'\''b"c$d\e', which bash reads as a'b"c$d\e.
	if got, want := scriptArg(`a'b"c$d\e`), `'a'\\''b\"c\$d\\e'`; got != want {
		t.Errorf("scriptArg = %s, want %s", got, want)
	}
}

// TestResourceDirectivesAbsNegativeSentinel guards the adaptive-resource model:
// a negative request is Martian's "at least |x|" sentinel, which its cluster path
// resolves to |x|. The emitted Nextflow directives must provision |x|, not floor
// a negative to 1 (static) or fall back to the stage default (dynamic).
func TestResourceDirectivesAbsNegativeSentinel(t *testing.T) {
	s := &ir.Stage{Resources: ir.Resources{Threads: -2, MemGB: -4}}

	if got := cpusOf(s); got != 2 {
		t.Errorf("cpusOf(threads=-2) = %d, want 2", got)
	}
	if got := memOf(s); got != 4 {
		t.Errorf("memOf(mem_gb=-4) = %d, want 4", got)
	}

	// The dynamic directives must take the magnitude of the RAW per-task value
	// (Math.abs wraps the nil-coalesced field read, BEFORE any positivity test,
	// so a negative sentinel provisions |x|) and reach the stage default only
	// in the not-positive arm. Assert those parts rather than the full
	// generated Groovy: a cosmetic template change stays green while an
	// abs-after-fallback or dropped-default regression still fails.
	mem := dynMem("res", 3)
	// "(m * task.attempt) + ' GB'" pins the retry-escalation operator and the
	// unit: a *→+ swap or a lost ' GB' changes real provisioning silently.
	for _, part := range []string{"memory {", "Math.abs((res?.mem_gb ?: 0)", "m > 0 ? m : 3;", "(m * task.attempt) + ' GB'"} {
		if !strings.Contains(mem, part) {
			t.Errorf("dynMem = %q, missing %q", mem, part)
		}
	}

	cpus := dynCpus("res", 3)
	// "Math.max(1, Math.ceil(t) as int)" pins whole-CPU rounding UP and the
	// floor of 1: ceil→floor or a dropped max under-provisions CPUs.
	for _, part := range []string{"cpus {", "Math.abs((res?.threads ?: 0)", "Math.max(1, Math.ceil(t) as int) : 3 }"} {
		if !strings.Contains(cpus, part) {
			t.Errorf("dynCpus = %q, missing %q", cpus, part)
		}
	}
}
