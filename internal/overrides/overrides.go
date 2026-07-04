// Package overrides converts an mrp `--overrides` file into an equivalent
// Nextflow process-scope config. mrp's overrides let an operator retune a
// stage's resources at launch without editing the .mro; the Nextflow analog is a
// `-c` overlay of `process` / `withName:` selectors, which this renders.
//
// The mrp overrides JSON is a map of (partially-)qualified stage name to a set
// of fields:
//
//	{
//	  "PIPE.ALIGN":   { "mem_gb": 8, "threads": 4, "chunk.mem_gb": 16 },
//	  "PIPE.COLLATE": { "join.mem_gb": 32 },
//	  "":             { "mem_gb": 2 }
//	}
//
// mrp matches a key as a path prefix from the pipestance root, and a node
// inherits from the nearest ancestor in its path that defines the field — so a
// key naming a sub-pipeline applies to every stage beneath it. The generated
// Nextflow process names are stage/call-named, not path-qualified, so this
// converter targets the key's last segment. With the pipeline program available
// (Convert's prog argument), a last segment that names a sub-pipeline is
// EXPANDED to the leaf stages beneath it, and a segment that names nothing is
// reported instead of silently emitting a selector that matches no process; when
// deeper and shallower keys touch the same stage/phase/field, the deeper key
// wins (mrp's most-specific-ancestor rule). Without the program, every key is
// treated as a leaf stage (the conservative legacy behavior).
//
// Each selector covers every naming family the emitter produces for a stage: the
// plain processes (STAGE, STAGE_SPLIT/_MAIN/_JOIN), the keyed fork variants
// (STAGE_MAP, STAGE_*_K), and the fused per-call processes from the BIND fold
// (STAGE_<n>_<pipeline>__<call>[_SP|_MN|_JN]), which embed the call name — for
// an aliased call (`call STAGE as X`) the override key's last segment matches
// the call name, exactly what mrp keys on.
package overrides

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/eunmann/mro2nf/internal/ir"
)

// Convert renders the Nextflow config equivalent of an mrp overrides JSON. prog
// is the parsed pipeline (or nil): when present, pipeline-scoped keys are
// expanded to their stages and unknown keys are reported rather than silently
// dropped. It returns the config text and a list of fields/keys it could not
// map (so the caller can surface them), or an error if the JSON is malformed.
func Convert(raw []byte, prog *ir.Program) (string, []string, error) {
	var spec map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &spec); err != nil {
		return "", nil, fmt.Errorf("parse overrides: %w", err)
	}

	res := newResolver(prog)
	resolved := map[[3]string]directive{}
	var unmapped []string

	for _, key := range sortedKeys(spec) {
		targets, kind, note := res.targets(key)
		if note != "" {
			unmapped = append(unmapped, fmt.Sprintf("%s: %s", orAll(key), note))

			continue
		}

		// A more specific match wins: a direct stage key over a pipeline
		// expansion, then a deeper path over a shallower one (mrp's
		// nearest-ancestor rule). Ties (same kind and depth) resolve to the last
		// in sorted-key order, deterministically. kind dominates depth, so it is
		// weighted above any realistic path length.
		rank := kind*kindWeight + keyDepth(key)

		for _, field := range sortedKeys(spec[key]) {
			phase, base, line, fnote := mapField(field, spec[key][field])
			if fnote != "" {
				unmapped = append(unmapped, fmt.Sprintf("%s.%s: %s", orAll(key), field, fnote))

				continue
			}

			// Keyed per (stage, phase, base) — NOT the call set — so precedence
			// between two keys touching the same stage is decided by rank, exactly
			// as before the fused-family unification. The selector covers every
			// call site of the stage at render time (res.callsFor).
			for _, stage := range targets {
				rk := [3]string{stage, phase, base}
				if cur, ok := resolved[rk]; !ok || rank >= cur.rank {
					resolved[rk] = directive{rank, line}
				}
			}
		}
	}

	globalDefault, groups := groupSelectors(resolved, res)

	return render(globalDefault, groups), unmapped, nil
}

// kindWeight scales a target's specificity kind above any realistic override
// key depth, so a direct stage key always outranks a pipeline expansion.
const kindWeight = 1000

// directive is one resolved override line for a (stage, phase, field) triple.
// The most specific source key wins (see the rank computed in Convert).
type directive struct {
	rank int
	line string
}

// Selector breadth levels, from broadest to narrowest. render emits selectors in
// this order so the narrowest is applied last (Nextflow applies the LAST matching
// withName, so last = wins).
const (
	breadthPhaseWide  = iota // .*(_MAIN|…) — every stage's phase
	breadthStage             // one stage, all phases
	breadthStagePhase        // one stage + phase
)

// selGroup is one withName selector and its directive lines, tagged with a
// breadth so render can order broad selectors before narrow ones.
type selGroup struct {
	breadth int
	sel     string
	lines   []string
}

// selBreadth classifies a selector's (stage, phase) by how many processes it
// matches. The bare process default (stage=="" && phase=="") is not a withName
// selector and is handled separately.
func selBreadth(stage, phase string) int {
	switch {
	case stage == "":
		return breadthPhaseWide
	case phase == "":
		return breadthStage
	default:
		return breadthStagePhase
	}
}

// groupSelectors collapses the resolved directives into the global process
// default plus an ordered list of withName groups. Each group's lines are sorted
// by field (base) so `memory` precedes `cpus`; the groups are ordered
// broad-to-narrow so a specific override wins over a phase-wide one at runtime.
func groupSelectors(resolved map[[3]string]directive, res *resolver) ([]string, []selGroup) {
	type baseLine struct{ base, line string }

	type acc struct {
		breadth int
		items   []baseLine
	}

	var globalDefault []baseLine

	bySel := map[string]*acc{}

	for rk, d := range resolved {
		stage, phase := rk[0], rk[1]
		if sel := selector(stage, res.callsFor(stage), phase); sel != "" {
			a := bySel[sel]
			if a == nil {
				a = &acc{breadth: selBreadth(stage, phase)}
				bySel[sel] = a
			}

			a.items = append(a.items, baseLine{rk[2], d.line})
		} else {
			globalDefault = append(globalDefault, baseLine{rk[2], d.line})
		}
	}

	linesOf := func(items []baseLine) []string {
		sort.Slice(items, func(i, j int) bool { return items[i].base < items[j].base })

		out := make([]string, len(items))
		for i, it := range items {
			out[i] = it.line
		}

		return out
	}

	groups := make([]selGroup, 0, len(bySel))
	for sel, a := range bySel {
		groups = append(groups, selGroup{a.breadth, sel, linesOf(a.items)})
	}

	// Broad first, ties by selector text — a stable, deterministic order that puts
	// the most specific selector last.
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].breadth != groups[j].breadth {
			return groups[i].breadth < groups[j].breadth
		}

		return groups[i].sel < groups[j].sel
	})

	return linesOf(globalDefault), groups
}

// mapField maps one override field to (phase, base, directive line, note). The
// note is non-empty instead of the line when the field has no faithful Nextflow
// directive; the returned line omits the selector (the caller applies it).
func mapField(field string, val json.RawMessage) (string, string, string, string) {
	phase, base, ok := strings.Cut(field, ".")
	if !ok { // a stage-level field: phase holds the whole name
		phase, base = "", field
	}

	if _, _, known := phaseSuffixes(phase); !known {
		return phase, base, "", "unrecognized phase prefix"
	}

	switch base {
	case "mem_gb", "threads":
		// mrp only writes numbers here; anything else would render a broken
		// directive (e.g. `memory = 'true GB'`), so report it instead.
		if !isNumber(val) {
			return phase, base, "", "value is not a number"
		}

		// Config-file scope uses `directive = value` (not the .nf process-body
		// `directive value` form), so a `-c` overlay parses.
		if base == "mem_gb" {
			return phase, base, "memory = " + memLiteral(val), ""
		}

		return phase, base, "cpus = " + strings.TrimSpace(string(val)), ""
	case "vmem_gb":
		return phase, base, "", "no Nextflow directive for virtual memory; mro2nf -monitor enforces vmem_gb from the .mro"
	case "profile":
		return phase, base, "", "use `nextflow run -with-trace`/`-with-report` for stage profiling"
	default:
		if field == "force_volatile" {
			return phase, base, "", "VDR is not modeled (Nextflow retains work/); see FEATURE_COVERAGE.md"
		}

		return phase, base, "", "unrecognized override field"
	}
}

// phaseSuffixes returns the two process-name suffixes an mrp override phase
// prefix runs under: the plain family (STAGE_MAIN, and its keyed _K variant via
// the trailing .*) and the fused per-call alias family
// (STAGE_<n>_<pipe>__<call>_MN). A stage-level field ("" phase) has no suffix —
// it applies to every phase of the stage. The bool is false for an unknown
// phase; the returns are (plain suffix, fused suffix, known).
func phaseSuffixes(phase string) (string, string, bool) {
	switch phase {
	case "":
		return "", "", true
	case "split":
		return "_SPLIT", "_SP", true
	case "chunk":
		return "_MAIN", "_MN", true
	case "join":
		return "_JOIN", "_JN", true
	default:
		return "", "", false
	}
}

// fusedPrefix matches the fused per-call process-name prefix the BIND fold
// emits: STAGE_<len>_<pipeline>__ (see emit's fusedName/qualify). [0-9] rather
// than \d: the selector is rendered inside a single-quoted Groovy string,
// where a backslash escape fails config parsing.
const fusedPrefix = `STAGE_[0-9]+_.+__`

// selector renders the withName regex for a stage + its call instances + phase;
// Nextflow full-matches it against the process (or process-alias) name. An empty
// stage means all stages: "" (the global process block) for a stage-level field,
// or a phase-wide regex for split/chunk/join. The regex covers BOTH naming
// families the emitter produces: the callable-named plain and keyed-fork
// processes (`<stage>`, `<stage>_MAP`, `<stage>_*_K`) via the stage token, and
// the call-named fused per-call processes (`STAGE_<n>_<pipe>__<call>`, from #16
// fusion and #76 native scatter) via the call tokens — so an override reaches an
// aliased scattered call (call-named) and its stage's shared keyed process
// (callable-named) alike. calls falls back to the stage name (a bare-stage entry
// or the legacy no-program path, where call == callable).
func selector(stage string, calls []string, phase string) string {
	plain, fused, _ := phaseSuffixes(phase)

	if stage == "" {
		if phase == "" {
			return "" // global process default
		}

		return fmt.Sprintf(".*(%s|%s).*", plain, fused)
	}

	if len(calls) == 0 {
		calls = []string{stage}
	}

	callAlt := calls[0]
	if len(calls) > 1 {
		callAlt = "(" + strings.Join(calls, "|") + ")"
	}

	// Two explicit alternatives: the callable-named plain/keyed family via the
	// stage token (with `.*` to reach `_MAP`/`_SPLIT`/`_*_K`), and the call-named
	// fused per-call family via the call token(s). The fused branch is BOUNDED —
	// `…__<call>` with only an optional phase suffix, no trailing `.*` — so a call
	// name that is a prefix of another call's name cannot over-match the longer
	// call's process.
	if phase == "" {
		return fmt.Sprintf("(%s.*|%s%s(_SP|_MN|_JN)?)", stage, fusedPrefix, callAlt)
	}

	return fmt.Sprintf("(%s%s.*|%s%s%s)", stage, plain, fusedPrefix, callAlt, fused)
}

// render emits the process{} block: the global process default first, then each
// withName selector broad-to-narrow so the most specific one is applied last
// (Nextflow's last-matching-withName-wins rule) and thus takes effect.
func render(globalDefault []string, groups []selGroup) string {
	var b strings.Builder

	b.WriteString("// Generated from an mrp --overrides file by `mro2nf overrides`.\n")
	b.WriteString("// Apply at launch: nextflow run main.nf -c overrides.config\n")
	b.WriteString("process {\n")

	for _, line := range globalDefault {
		fmt.Fprintf(&b, "    %s\n", line)
	}

	for _, g := range groups {
		fmt.Fprintf(&b, "    withName: '%s' { %s }\n", g.sel, strings.Join(g.lines, "; "))
	}

	b.WriteString("}\n")

	return b.String()
}

// memLiteral renders a JSON number of GB as a Nextflow memory string.
func memLiteral(val json.RawMessage) string {
	return "'" + strings.TrimSpace(string(val)) + " GB'"
}

// isNumber reports whether raw is a JSON number.
func isNumber(raw json.RawMessage) bool {
	var f float64

	return json.Unmarshal(raw, &f) == nil
}

// keyDepth is the number of path segments in an override key; "" (the global
// default) is 0 so any specific key outranks it.
func keyDepth(key string) int {
	if key == "" {
		return 0
	}

	return strings.Count(key, ".") + 1
}

func lastSegment(qualified string) string {
	if i := strings.LastIndex(qualified, "."); i >= 0 {
		return qualified[i+1:]
	}

	return qualified
}

func orAll(key string) string {
	if key == "" {
		return "(all stages)"
	}

	return key
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}
