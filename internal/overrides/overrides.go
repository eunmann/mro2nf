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
// (STAGE_MAP, STAGE_*_K), and the call-named per-call processes — the fused
// family (STAGE_<n>_<pipeline>__<call>[_SP|_MN|_JN|_K]) and the keyed element
// scatter (FORK_<n>_<pipeline>__<call>_KS) — which embed the call name, so for
// an aliased call (`call STAGE as X`) the override key's last segment matches
// the call name, exactly what mrp keys on. Nextflow full-matches withName
// regexes, so every selector is bounded to the emitter's exported suffix
// inventory (internal/emit names.go) rather than trailing `.*` (#112).
//
// The <pipeline> axis of the per-call families is exact only with the program:
// the selector then carries the literal pipeline name(s) that call the stage.
// Without it the axis falls back to `.+`, which is structurally ambiguous when
// identifiers contain "__" (Martian allows this): a selector for call TRIM
// also full-matches an unrelated call X__TRIM's processes, because `.+` can
// absorb the "<pipeline>__X" prefix. That residual over-match is one more
// reason to pass -mro to `mro2nf overrides`.
package overrides

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/eunmann/mro2nf/internal/emit"
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

			// A GLOBAL (all-stages) chunk override reaches only the main markers
			// distinctive behind `.*` (_MAP/_MAIN*/_MN/_KS); a plain non-split
			// stage's bare `<stage>` main (and its fused _K) cannot be matched
			// behind `.*` without over-matching split/join, so surface the shortfall
			// loudly instead of silently under-applying. Target such a stage by name.
			if key == "" && phase == phaseChunk {
				unmapped = append(unmapped, fmt.Sprintf(
					"%s.%s: global chunk override does not reach a plain non-split stage's bare main; target it by stage name",
					orAll(key), field))
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
		if sel := selector(stage, res.qualifiers(stage), phase); sel != "" {
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

	if _, _, known := familySuffixes(phase); !known {
		return phase, base, "", "unrecognized phase prefix"
	}

	switch base {
	case "mem_gb", "threads":
		// mrp writes a float64 here; anything else would render a broken directive
		// (e.g. `memory = 'true GB'`), so report it instead.
		var n float64
		if err := json.Unmarshal(val, &n); err != nil {
			return phase, base, "", "value is not a number"
		}

		// 0 is mrp's "use the site default" sentinel (GetSystemReqs fills in
		// ThreadsPerJob/MemGBPerJob), which varies by jobmode/profile and has no
		// static Nextflow equivalent — emitting `memory = '0 GB'` / `cpus = 0` would
		// pin the stage to nothing, so report it instead of diverging silently.
		if n == 0 {
			return phase, base, "", "0 requests mrp's site default resource; no static Nextflow equivalent"
		}

		// mrp treats a negative request as a sentinel ("as much as possible" —
		// magnitude on remote schedulers), so normalize to the magnitude; Nextflow
		// has no negative directive and would reject `memory = '-8 GB'` / `cpus = -4`
		// at config parse. FormatFloat also avoids scientific notation (1e3), which
		// MemoryUnit cannot parse.
		n = math.Abs(n)

		// Config-file scope uses `directive = value` (not the .nf process-body
		// `directive value` form), so a `-c` overlay parses.
		if base == "mem_gb" {
			return phase, base, "memory = " + memLiteral(n), ""
		}

		// cpus must be a positive integer; round a fractional (centicore) thread
		// request up so the stage is not under-provisioned. Guard the int64
		// conversion: a finite-but-absurd value would otherwise wrap to a negative
		// cpu count — the very directive this normalization prevents.
		ceil := math.Ceil(n)
		if ceil > math.MaxInt64 {
			return phase, base, "", "threads value is too large to render as a cpu count"
		}

		return phase, base, "cpus = " + strconv.FormatInt(int64(ceil), 10), ""
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

// phaseChunk is the mrp override phase for a stage's main job; every stage (split
// or not) runs its main as this phase.
const phaseChunk = "chunk"

// familySuffixes returns the (plain-family, fused-family) process-name
// suffixes an mrp override phase prefix reaches, drawn from the emitter's
// exported inventory so the selector alternation cannot drift from the names
// the emitter generates (#112). The "" phase reaches every process of the
// stage; split/chunk/join reach that phase's processes and their keyed (_K)
// variants. The bool is false for an unrecognized phase.
func familySuffixes(phase string) ([]string, []string, bool) {
	switch phase {
	case "":
		return emit.PlainStageSuffixes(), emit.FusedCallSuffixes(), true
	case "split":
		return withRoot(emit.PlainStageSuffixes(), "_SPLIT"), withRoot(emit.FusedCallSuffixes(), "_SP"), true
	case phaseChunk:
		// mrp runs EVERY stage's main job as phase 'chunk' — including a non-split
		// stage, whose main processes are the bare/_MAP/_K family, NOT _MAIN. So the
		// chunk phase reaches all MAIN-phase processes: the split main (_MAIN/
		// _MAIN_K/_MN) and the non-split main (bare/_MAP/_K), but never the split or
		// join phases. A suffix that exists only for the other stage kind matches no
		// process, so this is safe for split and non-split stages alike (the
		// standalone _KS scatter main is added for chunk in selector, as for "").
		return mainPhaseSuffixes(emit.PlainStageSuffixes()), mainPhaseSuffixes(emit.FusedCallSuffixes()), true
	case "join":
		return withRoot(emit.PlainStageSuffixes(), "_JOIN"), withRoot(emit.FusedCallSuffixes(), "_JN"), true
	default:
		return nil, nil, false
	}
}

// mainPhaseSuffixes returns the main-phase (chunk) entries of a suffix inventory:
// everything except the split and join phase suffixes. What remains is the bare
// non-split main (""), the keyed non-split main (_MAP/_K), and the split stage's
// main (_MAIN/_MAIN_K/_MN) — the processes mrp runs as phase 'chunk'.
func mainPhaseSuffixes(suffixes []string) []string {
	out := make([]string, 0, len(suffixes))

	for _, s := range suffixes {
		// Exclude every split/join marker by PREFIX (matching withRoot), so a
		// future keyed fused split/join suffix (e.g. _SP_K) is also excluded rather
		// than silently misclassified as a main.
		if strings.HasPrefix(s, "_SPLIT") || strings.HasPrefix(s, "_JOIN") ||
			strings.HasPrefix(s, "_SP") || strings.HasPrefix(s, "_JN") {
			continue
		}

		out = append(out, s)
	}

	return out
}

// splitJoinSuffixes are the split/join phase markers — the complement of the
// main (chunk) phase over the full plain+fused inventory.
func splitJoinSuffixes() []string {
	full := append(append([]string{}, emit.PlainStageSuffixes()...), emit.FusedCallSuffixes()...)
	main := map[string]bool{}

	for _, s := range mainPhaseSuffixes(full) {
		main[s] = true
	}

	out := make([]string, 0, len(full))

	for _, s := range full {
		if s != "" && !main[s] {
			out = append(out, s)
		}
	}

	return out
}

// globalChunkSuffixes returns the main-phase suffixes safe behind the global `.*`
// prefix: non-empty, and not a suffix of any split/join marker (so `.*<s>` cannot
// full-match a split or join process). This drops the bare "" (matches every
// process) and _K (a suffix of _SPLIT_K/_JOIN_K); a bare or fused-_K non-split
// main is therefore not reachable by a GLOBAL chunk override — target it by stage
// name (a stage-scoped chunk override reaches every main process). The distinctive
// _KS scatter main is included.
func globalChunkSuffixes(main []string) []string {
	sj := splitJoinSuffixes()

	safe := make([]string, 0, len(main))

	for _, s := range main {
		if s == "" {
			continue
		}

		ambiguous := false

		for _, x := range sj {
			if strings.HasSuffix(x, s) {
				ambiguous = true

				break
			}
		}

		if !ambiguous {
			safe = append(safe, s)
		}
	}

	return append(safe, emit.ScatterCallSuffixes()...)
}

// withRoot filters a suffix inventory to the entries under one phase root
// (e.g. "_SPLIT" selects _SPLIT and _SPLIT_K).
func withRoot(suffixes []string, root string) []string {
	out := make([]string, 0, len(suffixes))

	for _, s := range suffixes {
		if strings.HasPrefix(s, root) {
			out = append(out, s)
		}
	}

	return out
}

// altGroup renders names as a regex alternation: a single name verbatim,
// several as a group.
func altGroup(names []string) string {
	if len(names) == 1 {
		return names[0]
	}

	return "(" + strings.Join(names, "|") + ")"
}

// suffixAlt renders a suffix inventory as a bounded regex fragment: a single
// suffix verbatim, several as an alternation group, with the empty suffix
// (the bare name) making the group optional. No trailing `.*` — Nextflow
// full-matches withName, so the alternation IS the bound (#112).
func suffixAlt(suffixes []string) string {
	opt := ""
	rest := make([]string, 0, len(suffixes))

	for _, s := range suffixes {
		if s == "" {
			opt = "?"

			continue
		}

		rest = append(rest, s)
	}

	if len(rest) == 1 && opt == "" {
		return rest[0]
	}

	return "(" + strings.Join(rest, "|") + ")" + opt
}

// selector renders the withName regex for a stage + its call-site qualifiers
// (resolver.qualifiers) + phase; Nextflow full-matches it against the process
// (or process-alias) name. An empty stage means all stages: "" (the global
// process block) for a stage-level field, or a phase-wide regex for
// split/chunk/join. The regex covers every naming family the emitter produces:
// the callable-named plain and keyed-fork processes (`<stage>`, `<stage>_MAP`,
// `<stage>_*_K`) via the stage token, and the call-named fused per-call
// (`STAGE_<n>_<pipe>__<call>*`, from #16 fusion and #76 native scatter) and
// keyed-scatter (`FORK_<n>_<pipe>__<call>_KS`) processes via the qualifiers —
// so an override reaches an aliased scattered call (call-named) and its
// stage's shared keyed process (callable-named) alike. Every branch is BOUNDED
// to the emitter's exported suffix inventory — no trailing `.*` — so a
// stage/call name that is a prefix of another's cannot over-match the longer
// name's processes (#112); with a program the qualifiers pin the pipeline axis
// too (literal names, not `.+`), closing the `__`-in-identifier ambiguity.
func selector(stage string, quals []string, phase string) string {
	plain, fused, _ := familySuffixes(phase)

	if stage == "" {
		if phase == "" {
			return "" // global process default
		}

		suffixes := append(append([]string{}, plain...), fused...)
		if phase == phaseChunk {
			// A global `.*`-prefixed chunk selector can only use suffixes that are
			// distinctive to the main phase; the bare and _K mains would over-match
			// (see globalChunkSuffixes).
			suffixes = globalChunkSuffixes(suffixes)
		}

		return ".*" + suffixAlt(suffixes)
	}

	// Escape the stage name: with a program it is a valid Martian identifier
	// (QuoteMeta is a no-op), but without one it is the raw override-key segment,
	// where a regex metacharacter would otherwise corrupt the selector. The quals
	// are already escaped where they embed a raw name (see qualifiers).
	stage = regexp.QuoteMeta(stage)

	alts := []string{stage + suffixAlt(plain)}
	for _, q := range quals {
		alts = append(alts, "STAGE_"+q+suffixAlt(fused))
	}

	// The _KS scatter is a stage's (non-split) main, so both a stage-level ("")
	// and a chunk override reach it; the per-stage split/join selectors cover the
	// split-triad processes.
	if phase == "" || phase == phaseChunk {
		for _, q := range quals {
			alts = append(alts, "FORK_"+q+suffixAlt(emit.ScatterCallSuffixes()))
		}
	}

	return "(" + strings.Join(alts, "|") + ")"
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

// memLiteral renders a GB quantity as a Nextflow memory string, without
// scientific notation (which MemoryUnit cannot parse).
func memLiteral(gb float64) string {
	return "'" + strconv.FormatFloat(gb, 'f', -1, 64) + " GB'"
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
