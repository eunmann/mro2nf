package overrides_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/overrides"
)

// stageSel renders the expected stage-level selector for a stage: the bounded
// plain/keyed family on the stage name, plus the fused and keyed-scatter
// per-call families on the call alternation (the call name, or "(A|B)").
// Bounded suffix alternations, never a trailing `.*` — Nextflow full-matches
// withName, so `SORT.*` would retune an unrelated stage SORT_READS (#112).
func stageSel(stage, callAlt string) string {
	return "(" + stage + "(_MAP|_SPLIT|_SPLIT_K|_MAIN|_MAIN_K|_JOIN|_JOIN_K)?" +
		"|STAGE_[0-9]+_.+__" + callAlt + "(_SP|_MN|_JN|_K)?" +
		"|FORK_[0-9]+_.+__" + callAlt + "_KS)"
}

// phaseSel renders the expected phase-level selector: the plain phase process
// and its keyed variant, plus the fused per-call phase process.
func phaseSel(stage, plain, callAlt, fused string) string {
	return "(" + stage + "(" + plain + "|" + plain + "_K)|STAGE_[0-9]+_.+__" + callAlt + fused + ")"
}

// sampleProgram builds a two-level pipeline for the pipeline-scope tests:
// TOP calls sub-pipeline SUB (stages ALIGN, SORT) and stage COLLATE.
func sampleProgram() *ir.Program {
	return &ir.Program{
		Stages: map[string]*ir.Stage{
			"ALIGN": {Name: "ALIGN"}, "SORT": {Name: "SORT"}, "COLLATE": {Name: "COLLATE"},
		},
		Pipelines: map[string]*ir.Pipeline{
			"SUB": {Name: "SUB", Calls: []ir.Call{
				{Name: "ALIGN", Callable: "ALIGN"}, {Name: "SORT", Callable: "SORT"},
			}},
			"TOP": {Name: "TOP", Calls: []ir.Call{
				{Name: "SUB", Callable: "SUB"}, {Name: "COLLATE", Callable: "COLLATE"},
			}},
		},
		Entry: &ir.EntryCall{Callable: "TOP"},
	}
}

// TestConvertPipelineScopeExpands guards #45: a key naming a sub-pipeline expands
// to a selector for every stage beneath it, rather than emitting one dead
// selector for the pipeline name that matches no process.
func TestConvertPipelineScopeExpands(t *testing.T) {
	cfg, unmapped, err := overrides.Convert([]byte(`{"TOP.SUB": {"mem_gb": 8}}`), sampleProgram())
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	if len(unmapped) != 0 {
		t.Errorf("unexpected unmapped: %v", unmapped)
	}

	for _, stage := range []string{"ALIGN", "SORT"} {
		want := "withName: '" + stageSel(stage, stage) + "' { memory = '8 GB' }"
		if !strings.Contains(cfg, want) {
			t.Errorf("config missing expanded selector for %s:\n%s\n--- got ---\n%s", stage, want, cfg)
		}
	}

	// The sub-pipeline name itself must NOT appear as a selector (it matches no
	// process); only its stages do.
	if strings.Contains(cfg, "?SUB.*'") {
		t.Errorf("config emitted a dead selector for the sub-pipeline name:\n%s", cfg)
	}
}

// TestConvertPipelineScopeTransitive guards that expansion recurses through a
// nested sub-pipeline: the entry pipeline TOP resolves to ALIGN and SORT (via
// SUB) plus its own direct stage COLLATE.
func TestConvertPipelineScopeTransitive(t *testing.T) {
	cfg, unmapped, err := overrides.Convert([]byte(`{"TOP": {"mem_gb": 8}}`), sampleProgram())
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	if len(unmapped) != 0 {
		t.Errorf("unexpected unmapped: %v", unmapped)
	}

	for _, stage := range []string{"ALIGN", "SORT", "COLLATE"} {
		want := "withName: '" + stageSel(stage, stage) + "' { memory = '8 GB' }"
		if !strings.Contains(cfg, want) {
			t.Errorf("TOP must expand transitively to %s:\n%s", stage, cfg)
		}
	}
}

// TestConvertPipelineScopePhase guards a phase-qualified pipeline-scoped key: the
// phase selector must be applied to every expanded stage.
func TestConvertPipelineScopePhase(t *testing.T) {
	cfg, _, err := overrides.Convert([]byte(`{"TOP.SUB": {"chunk.mem_gb": 8}}`), sampleProgram())
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	for _, stage := range []string{"ALIGN", "SORT"} {
		want := "withName: '" + phaseSel(stage, "_MAIN", stage, "_MN") + "' { memory = '8 GB' }"
		if !strings.Contains(cfg, want) {
			t.Errorf("chunk.mem_gb must reach %s's main-phase selector:\n%s", stage, cfg)
		}
	}
}

// TestConvertResolvesByAlias guards that an override key matches a call's alias
// (`call ALIGN as TRIM`), which is what mrp keys on — not just the callable name.
func TestConvertResolvesByAlias(t *testing.T) {
	prog := sampleProgram()
	// Rename SUB's ALIGN call to the alias TRIM (callable stays ALIGN).
	prog.Pipelines["SUB"].Calls[0].Name = "TRIM"

	cfg, unmapped, err := overrides.Convert([]byte(`{"SUB.TRIM": {"mem_gb": 8}}`), prog)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	if len(unmapped) != 0 {
		t.Errorf("aliased call key should resolve, got unmapped: %v", unmapped)
	}

	// The alias key must reach BOTH naming families (#99): the call-named fused
	// per-call process (STAGE_..__TRIM, the native-scatter/#16 form) AND the
	// callable-named plain/keyed process (ALIGN, ALIGN_MAP) that the same stage
	// runs through in default mode.
	sel := stageSel("ALIGN", "TRIM")
	if !strings.Contains(cfg, "withName: '"+sel+"' { memory = '8 GB' }") {
		t.Errorf("alias key must target both TRIM (fused) and ALIGN (plain/keyed):\n%s", cfg)
	}

	re := regexp.MustCompile(`^(?:` + sel + `)$`)
	for _, name := range []string{"ALIGN", "ALIGN_MAP", "STAGE_4_SUB__TRIM", "STAGE_4_SUB__TRIM_MN"} {
		if !re.MatchString(name) {
			t.Errorf("alias-key selector must full-match %q", name)
		}
	}
}

// TestConvertStageKeyBeatsPipelineScope guards precedence: an explicit stage key
// wins over a pipeline-scoped key that also covers the stage, even though the
// stage key has fewer path segments (a narrower target is more specific).
func TestConvertStageKeyBeatsPipelineScope(t *testing.T) {
	cfg, _, err := overrides.Convert(
		[]byte(`{"ALIGN": {"mem_gb": 16}, "TOP.SUB": {"mem_gb": 8}}`), sampleProgram())
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	if !strings.Contains(cfg, "withName: '"+stageSel("ALIGN", "ALIGN")+"' { memory = '16 GB' }") {
		t.Errorf("explicit ALIGN key (16) must win over the pipeline scope (8):\n%s", cfg)
	}

	if strings.Contains(cfg, "withName: '"+stageSel("ALIGN", "ALIGN")+"' { memory = '8 GB' }") {
		t.Errorf("ALIGN must not take the pipeline-scoped 8 GB:\n%s", cfg)
	}

	// SORT, covered only by the pipeline scope, keeps 8 GB.
	if !strings.Contains(cfg, "withName: '"+stageSel("SORT", "SORT")+"' { memory = '8 GB' }") {
		t.Errorf("SORT should keep the pipeline-scoped 8 GB:\n%s", cfg)
	}
}

// TestConvertSpecificPhaseWinsOverGlobal guards the selector ordering: when a
// phase-wide default and a specific stage-phase override both match a process,
// the specific one must be emitted LATER, because Nextflow applies the last
// matching withName. A global chunk default and an expanded sub-pipeline chunk
// override both hit ALIGN_MAIN; the broad `.*` selector must precede the specific
// one so the specific 8 GB wins over the global 4 GB at runtime.
func TestConvertSpecificPhaseWinsOverGlobal(t *testing.T) {
	cfg, _, err := overrides.Convert(
		[]byte(`{"": {"chunk.mem_gb": 4}, "TOP.SUB": {"chunk.mem_gb": 8}}`), sampleProgram())
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	broad := strings.Index(cfg, `withName: '.*(_MAIN|_MAIN_K|_MN)'`)
	specific := strings.Index(cfg, `withName: '(ALIGN(_MAIN|_MAIN_K)|`)

	if broad < 0 || specific < 0 {
		t.Fatalf("expected both a phase-wide and a specific selector:\n%s", cfg)
	}

	if broad > specific {
		t.Errorf("phase-wide selector must precede the specific one (so the specific wins under "+
			"Nextflow's last-match rule); broad@%d specific@%d:\n%s", broad, specific, cfg)
	}
}

// TestConvertUnknownKeyReported guards #45: a key naming neither a stage nor a
// sub-pipeline is reported, not silently dropped, when the program is known.
func TestConvertUnknownKeyReported(t *testing.T) {
	cfg, unmapped, err := overrides.Convert([]byte(`{"NOPE": {"mem_gb": 8}}`), sampleProgram())
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	if len(unmapped) != 1 || !strings.Contains(unmapped[0], "NOPE") {
		t.Errorf("expected NOPE reported as unmapped, got %v", unmapped)
	}

	if strings.Contains(cfg, "NOPE") {
		t.Errorf("unknown key leaked a selector into the config:\n%s", cfg)
	}
}

// TestConvertDeeperKeyWins guards mrp's nearest-ancestor precedence: a
// stage-specific key overrides the pipeline-scoped value for that stage, while
// the pipeline value still applies to the stage's siblings.
func TestConvertDeeperKeyWins(t *testing.T) {
	cfg, _, err := overrides.Convert(
		[]byte(`{"TOP.SUB": {"mem_gb": 8}, "TOP.SUB.ALIGN": {"mem_gb": 16}}`), sampleProgram())
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	alignSel := "withName: '" + stageSel("ALIGN", "ALIGN") + "' { memory = '16 GB' }"
	sortSel := "withName: '" + stageSel("SORT", "SORT") + "' { memory = '8 GB' }"

	if !strings.Contains(cfg, alignSel) {
		t.Errorf("ALIGN should take the deeper 16 GB override:\n%s", cfg)
	}

	if !strings.Contains(cfg, sortSel) {
		t.Errorf("SORT should keep the pipeline-scoped 8 GB override:\n%s", cfg)
	}

	if strings.Contains(cfg, "withName: '"+stageSel("ALIGN", "ALIGN")+"' { memory = '8 GB' }") {
		t.Errorf("ALIGN must not also carry the shallower 8 GB value:\n%s", cfg)
	}
}

// TestConvertLeafKeyStillWorks confirms a plain stage key is unaffected by the
// program-aware resolution.
func TestConvertLeafKeyStillWorks(t *testing.T) {
	cfg, unmapped, err := overrides.Convert([]byte(`{"TOP.COLLATE": {"threads": 3}}`), sampleProgram())
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	if len(unmapped) != 0 {
		t.Errorf("unexpected unmapped: %v", unmapped)
	}

	if !strings.Contains(cfg, "withName: '"+stageSel("COLLATE", "COLLATE")+"' { cpus = 3 }") {
		t.Errorf("leaf stage key mis-rendered:\n%s", cfg)
	}
}

func TestConvert(t *testing.T) {
	in := []byte(`{
	  "MAIN.PHASER.ALIGN":   { "mem_gb": 8, "threads": 4, "chunk.mem_gb": 16 },
	  "MAIN.PHASER.COLLATE": { "join.mem_gb": 32, "join.threads": 2 },
	  "MAIN.PHASER.SORT":    { "split.mem_gb": 6 },
	  "":                    { "mem_gb": 2 }
	}`)

	cfg, unmapped, err := overrides.Convert(in, nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	for _, want := range []string{
		"process {",
		"memory = '2 GB'", // the "" key -> global default
		"withName: '" + stageSel("ALIGN", "ALIGN") + "' { memory = '8 GB'; cpus = 4 }",
		"withName: '" + phaseSel("ALIGN", "_MAIN", "ALIGN", "_MN") + "' { memory = '16 GB' }",               // chunk.* -> main phase
		"withName: '" + phaseSel("COLLATE", "_JOIN", "COLLATE", "_JN") + "' { memory = '32 GB'; cpus = 2 }", // join.* -> join phase
		"withName: '" + phaseSel("SORT", "_SPLIT", "SORT", "_SP") + "' { memory = '6 GB' }",                 // split.* -> split phase
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config missing %q\n--- got ---\n%s", want, cfg)
		}
	}

	if len(unmapped) != 0 {
		t.Errorf("unexpected unmapped fields: %v", unmapped)
	}
}

// TestConvertNoPrefixOverMatch guards finding 2 and #112: a stage/call name
// that is a prefix of another's must not let one stage's selector retune the
// longer name's processes — in the fused per-call family OR the plain/keyed
// family (Nextflow full-matches withName, so an unbounded `A.*` would match
// every process of an unrelated stage AB).
func TestConvertNoPrefixOverMatch(t *testing.T) {
	prog := &ir.Program{
		Stages: map[string]*ir.Stage{"A": {Name: "A"}, "AB": {Name: "AB"}},
		Pipelines: map[string]*ir.Pipeline{
			"P": {Name: "P", Calls: []ir.Call{
				{Name: "A", Callable: "A", Mapped: true},
				{Name: "AB", Callable: "AB", Mapped: true},
			}},
		},
		Entry: &ir.EntryCall{Callable: "P"},
	}

	cfg, _, err := overrides.Convert([]byte(`{"A": {"mem_gb": 8}}`), prog)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	sel := regexp.MustCompile(`withName: '([^']+)'`).FindStringSubmatch(cfg)
	re := regexp.MustCompile("^(?:" + sel[1] + ")$")

	for _, name := range []string{
		"STAGE_1_P__AB", "STAGE_1_P__AB_MN", // fused family of the unrelated call
		"AB", "AB_MAIN", "AB_MAP", // plain/keyed family of the unrelated stage
	} {
		if re.MatchString(name) {
			t.Errorf("stage A selector %q over-matches unrelated process %q", sel[1], name)
		}
	}

	for _, name := range []string{
		"STAGE_1_P__A",                                  // fused family of its own call
		"A", "A_MAP", "A_SPLIT", "A_MAIN_K", "A_JOIN_K", // every plain/keyed variant
	} {
		if !re.MatchString(name) {
			t.Errorf("stage A selector %q must match stage A's process %q", sel[1], name)
		}
	}
}

// TestConvertStageKeyReachesAliasedScatter guards the other direction of the
// #99 unification: a STAGE-name key must reach an ALIASED map call's fused
// per-call process (STAGE_..__<alias>, the #76 native-scatter form) as well as
// every other call site of that stage — the case a bare `(fusedPrefix)?<stage>`
// selector missed, since the aliased call's process embeds the alias, not the
// stage. ALIGN is called once as ALIGN (SUB) and once aliased as TRIM.
func TestConvertStageKeyReachesAliasedScatter(t *testing.T) {
	prog := sampleProgram()
	prog.Pipelines["TOP"].Calls = append(prog.Pipelines["TOP"].Calls,
		ir.Call{Name: "TRIM", Callable: "ALIGN", Mapped: true})

	cfg, unmapped, err := overrides.Convert([]byte(`{"ALIGN": {"mem_gb": 8}}`), prog)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if len(unmapped) != 0 {
		t.Errorf("stage key should resolve, got unmapped: %v", unmapped)
	}

	sels := regexp.MustCompile(`withName: '([^']+)'`).FindAllStringSubmatch(cfg, -1)
	if len(sels) != 1 {
		t.Fatalf("want 1 stage-level selector, got %d:\n%s", len(sels), cfg)
	}

	re := regexp.MustCompile("^(?:" + sels[0][1] + ")$")
	// Both call sites' fused and keyed-scatter processes and the shared
	// plain/keyed family.
	for _, name := range []string{"ALIGN", "ALIGN_MAP", "STAGE_5_TOP__TRIM", "FORK_5_TOP__TRIM_KS", "STAGE_4_SUB__ALIGN"} {
		if !re.MatchString(name) {
			t.Errorf("stage-key selector %q must full-match %q", sels[0][1], name)
		}
	}
	// It must not bleed onto an unrelated stage.
	if re.MatchString("SORT") || re.MatchString("STAGE_4_SUB__SORT") {
		t.Errorf("stage-key selector %q over-matches an unrelated stage", sels[0][1])
	}
}

// TestConvertSelectorsMatchGeneratedNames full-matches the emitted withName
// regexes (Nextflow's matching semantics) against the process names the emitter
// generates for a stage, across every naming family: the plain and keyed stage
// processes, and the fused per-call processes from the BIND fold (#16).
func TestConvertSelectorsMatchGeneratedNames(t *testing.T) {
	in := []byte(`{"P.ALIGN": {"mem_gb": 8, "split.mem_gb": 4, "chunk.mem_gb": 16, "join.mem_gb": 32}}`)

	cfg, _, err := overrides.Convert(in, nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	sels := regexp.MustCompile(`withName: '([^']+)'`).FindAllStringSubmatch(cfg, -1)
	if len(sels) != 4 {
		t.Fatalf("want 4 selectors, got %d in:\n%s", len(sels), cfg)
	}

	// Process name -> the override phases that must reach it ("" = stage-level).
	names := map[string][]string{
		"ALIGN":                  {""}, // plain non-split stage
		"ALIGN_MAP":              {""}, // keyed non-split variant
		"STAGE_4_PIPE__ALIGN":    {""}, // fused non-split call (#16)
		"STAGE_4_PIPE__ALIGN_K":  {""}, // keyed fused bind+main (#99)
		"FORK_4_PIPE__ALIGN_KS":  {""}, // keyed element scatter running main (#99)
		"ALIGN_SPLIT":            {"", "split"},
		"ALIGN_SPLIT_K":          {"", "split"},
		"STAGE_4_PIPE__ALIGN_SP": {"", "split"}, // fused bind+split
		"ALIGN_MAIN":             {"", "chunk"},
		"ALIGN_MAIN_K":           {"", "chunk"},
		"STAGE_4_PIPE__ALIGN_MN": {"", "chunk"}, // fused MAIN alias
		"ALIGN_JOIN":             {"", "join"},
		"ALIGN_JOIN_K":           {"", "join"},
		"STAGE_4_PIPE__ALIGN_JN": {"", "join"}, // fused JOIN alias
		"BIND_4_PIPE__ALIGN":     nil,          // a bind helper: no phase override applies
		"FORK_4_PIPE__ALIGN":     nil,          // a forkbind helper (only _KS runs stage code)
		"STAGE_4_PIPE__OTHER":    nil,          // another call's fused process
		"ALIGN_READS":            nil,          // an unrelated stage sharing the prefix (#112)
		"ALIGN_READS_MAIN":       nil,
	}

	// Map each selector back to its phase by the distinctive plain process it
	// full-matches (only the stage-level selector matches the bare stage name).
	phaseOf := map[string]string{}

	for _, m := range sels {
		re := regexp.MustCompile("^(?:" + m[1] + ")$")

		switch {
		case re.MatchString("ALIGN"):
			phaseOf[""] = m[1]
		case re.MatchString("ALIGN_SPLIT"):
			phaseOf["split"] = m[1]
		case re.MatchString("ALIGN_MAIN"):
			phaseOf["chunk"] = m[1]
		case re.MatchString("ALIGN_JOIN"):
			phaseOf["join"] = m[1]
		}
	}

	if len(phaseOf) != 4 {
		t.Fatalf("could not classify all 4 selectors by phase, got %v", phaseOf)
	}

	for name, phases := range names {
		want := map[string]bool{}
		for _, ph := range phases {
			want[ph] = true
		}

		for ph, sel := range phaseOf {
			re := regexp.MustCompile("^(?:" + sel + ")$") // Nextflow full-matches withName
			if got := re.MatchString(name); got != want[ph] {
				t.Errorf("selector %q (phase %q) match %q = %v, want %v", sel, ph, name, got, want[ph])
			}
		}
	}
}

// TestConvertUnknownPhase checks an unrecognized phase prefix is reported as
// unmapped rather than silently widening to the whole stage.
func TestConvertUnknownPhase(t *testing.T) {
	cfg, unmapped, err := overrides.Convert([]byte(`{"P.S": {"bogus.mem_gb": 8}}`), nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	if strings.Contains(cfg, "withName") {
		t.Errorf("unknown phase must emit no selector, got:\n%s", cfg)
	}

	if len(unmapped) != 1 {
		t.Errorf("want 1 unmapped field, got %v", unmapped)
	}
}

// TestConvertNonNumericValue checks a non-numeric mem_gb/threads value is
// reported as unmapped instead of emitted as a broken directive (previously
// {"mem_gb": true} produced `memory = 'true GB'`).
func TestConvertNonNumericValue(t *testing.T) {
	cfg, unmapped, err := overrides.Convert([]byte(`{"P.S": {"mem_gb": true, "threads": "four", "chunk.mem_gb": 8}}`), nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	if strings.Contains(cfg, "'true GB'") || strings.Contains(cfg, "cpus = \"four\"") {
		t.Errorf("non-numeric values must not be emitted, got:\n%s", cfg)
	}

	if !strings.Contains(cfg, "memory = '8 GB'") {
		t.Errorf("the valid chunk.mem_gb must still be emitted, got:\n%s", cfg)
	}

	if len(unmapped) != 2 {
		t.Errorf("want 2 unmapped fields (mem_gb, threads), got %v", unmapped)
	}
}

// TestConvertUnknownField checks an unrecognized override field is reported.
func TestConvertUnknownField(t *testing.T) {
	_, unmapped, err := overrides.Convert([]byte(`{"P.S": {"bogus_field": 1}}`), nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	if len(unmapped) != 1 || !strings.Contains(unmapped[0], "unrecognized") {
		t.Errorf("want 1 'unrecognized' unmapped field, got %v", unmapped)
	}
}

// TestConvertGlobalPhaseSelector checks a phase field under the all-stages key
// maps to the phase-wide selector covering both naming families.
func TestConvertGlobalPhaseSelector(t *testing.T) {
	cfg, _, err := overrides.Convert([]byte(`{"": {"chunk.mem_gb": 4}}`), nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	if !strings.Contains(cfg, `withName: '.*(_MAIN|_MAIN_K|_MN)' { memory = '4 GB' }`) {
		t.Errorf("global chunk override must use the phase-wide selector, got:\n%s", cfg)
	}

	// The bounded phase-wide selector must not reach a stage whose NAME merely
	// contains the phase marker (#112): full-match, no trailing `.*`.
	sel := regexp.MustCompile(`withName: '([^']+)'`).FindStringSubmatch(cfg)
	match := regexp.MustCompile("^(?:" + sel[1] + ")$")

	for _, name := range []string{"SORT_MAIN", "SORT_MAIN_K", "STAGE_1_P__SORT_MN"} {
		if !match.MatchString(name) {
			t.Errorf("phase-wide selector %q must match %q", sel[1], name)
		}
	}

	for _, name := range []string{"SORT_MAINLINE", "SORT_MAINLINE_SPLIT", "SORT_MN_X"} {
		if match.MatchString(name) {
			t.Errorf("phase-wide selector %q over-matches %q", sel[1], name)
		}
	}
}

// TestConvertUnmappable checks that fields with no faithful Nextflow directive
// (virtual memory, VDR volatility, profiling) are reported, not silently emitted.
func TestConvertUnmappable(t *testing.T) {
	in := []byte(`{"P.S": {"vmem_gb": 20, "force_volatile": true, "profile": "cpu"}}`)

	cfg, unmapped, err := overrides.Convert(in, nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	if strings.Contains(cfg, "withName") {
		t.Errorf("unmappable-only override should emit no selectors, got:\n%s", cfg)
	}
	if len(unmapped) != 3 {
		t.Errorf("want 3 unmapped fields (vmem_gb, force_volatile, profile), got %d: %v", len(unmapped), unmapped)
	}
}

func TestConvertMalformed(t *testing.T) {
	if _, _, err := overrides.Convert([]byte("not json"), nil); err == nil {
		t.Error("expected an error for malformed JSON")
	}
}
