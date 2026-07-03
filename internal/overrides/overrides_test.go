package overrides_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/overrides"
)

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
		want := "withName: '(STAGE_[0-9]+_.+__)?" + stage + ".*' { memory = '8 GB' }"
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

	alignSel := "withName: '(STAGE_[0-9]+_.+__)?ALIGN.*' { memory = '16 GB' }"
	sortSel := "withName: '(STAGE_[0-9]+_.+__)?SORT.*' { memory = '8 GB' }"

	if !strings.Contains(cfg, alignSel) {
		t.Errorf("ALIGN should take the deeper 16 GB override:\n%s", cfg)
	}

	if !strings.Contains(cfg, sortSel) {
		t.Errorf("SORT should keep the pipeline-scoped 8 GB override:\n%s", cfg)
	}

	if strings.Contains(cfg, "ALIGN.*' { memory = '8 GB' }") {
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

	if !strings.Contains(cfg, "withName: '(STAGE_[0-9]+_.+__)?COLLATE.*' { cpus = 3 }") {
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
		`withName: '(STAGE_[0-9]+_.+__)?ALIGN.*' { memory = '8 GB'; cpus = 4 }`,
		`withName: '(ALIGN_MAIN|STAGE_[0-9]+_.+__ALIGN_MN).*' { memory = '16 GB' }`,               // chunk.* -> main phase
		`withName: '(COLLATE_JOIN|STAGE_[0-9]+_.+__COLLATE_JN).*' { memory = '32 GB'; cpus = 2 }`, // join.* -> join phase
		`withName: '(SORT_SPLIT|STAGE_[0-9]+_.+__SORT_SP).*' { memory = '6 GB' }`,                 // split.* -> split phase
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config missing %q\n--- got ---\n%s", want, cfg)
		}
	}

	if len(unmapped) != 0 {
		t.Errorf("unexpected unmapped fields: %v", unmapped)
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
		"STAGE_4_PIPE__OTHER":    nil,          // another call's fused process
	}

	// Selector order in the config is stable (sorted); map them back by content.
	phaseOf := map[string]string{}
	for _, m := range sels {
		switch {
		case strings.Contains(m[1], "_SP"):
			phaseOf["split"] = m[1]
		case strings.Contains(m[1], "_MN"):
			phaseOf["chunk"] = m[1]
		case strings.Contains(m[1], "_JN"):
			phaseOf["join"] = m[1]
		default:
			phaseOf[""] = m[1]
		}
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

	if !strings.Contains(cfg, `withName: '.*(_MAIN|_MN).*' { memory = '4 GB' }`) {
		t.Errorf("global chunk override must use the phase-wide selector, got:\n%s", cfg)
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
