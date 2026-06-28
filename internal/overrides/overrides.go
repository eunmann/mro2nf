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
// The key's last segment is taken as the stage name and mapped to the generated
// process names (STAGE, STAGE_MAIN, STAGE_JOIN, and their keyed _K variants) via
// withName regexes; "" maps to the global process defaults.
package overrides

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Convert renders the Nextflow config equivalent of an mrp overrides JSON. It
// returns the config text and a list of fields it could not map (so the caller
// can surface them), or an error if the JSON is malformed.
func Convert(raw []byte) (string, []string, error) {
	var spec map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &spec); err != nil {
		return "", nil, fmt.Errorf("parse overrides: %w", err)
	}

	// selector -> ordered directive lines (selector "" is the global default).
	groups := map[string][]string{}
	var unmapped []string

	for _, stageKey := range sortedKeys(spec) {
		stage := lastSegment(stageKey)
		for _, field := range sortedKeys(spec[stageKey]) {
			sel, line, note := mapField(stage, field, spec[stageKey][field])
			switch {
			case note != "":
				unmapped = append(unmapped, fmt.Sprintf("%s.%s: %s", orAll(stageKey), field, note))
			case line != "":
				groups[sel] = append(groups[sel], line)
			}
		}
	}

	return render(groups), unmapped, nil
}

// mapField maps one override field to a (selector, directive line, note). The
// note is non-empty instead of the line when the field has no faithful Nextflow
// directive. stage is the bare stage name ("" = all stages).
func mapField(stage, field string, val json.RawMessage) (string, string, string) {
	phase, base, ok := strings.Cut(field, ".")
	if !ok { // a stage-level field: phase holds the whole name
		phase, base = "", field
	}

	switch base {
	case "mem_gb":
		// Config-file scope uses `directive = value` (not the .nf process-body
		// `directive value` form), so a `-c` overlay parses.
		return selector(stage, phase), "memory = " + memLiteral(val), ""
	case "threads":
		return selector(stage, phase), "cpus = " + strings.TrimSpace(string(val)), ""
	case "vmem_gb":
		return "", "", "no Nextflow directive for virtual memory; mro2nf -monitor enforces vmem_gb from the .mro"
	case "profile":
		return "", "", "use `nextflow run -with-trace`/`-with-report` for stage profiling"
	default:
		if field == "force_volatile" {
			return "", "", "VDR is not modeled (Nextflow retains work/); see FEATURE_COVERAGE.md"
		}

		return "", "", "unrecognized override field"
	}
}

// selector renders the withName regex for a stage + phase. An empty stage means
// all stages: "" (the global process block) for a stage-level field, or a
// phase-wide regex for chunk/join. The _K keyed fork variants are matched too.
func selector(stage, phase string) string {
	suffix := map[string]string{"": "", "chunk": "_MAIN", "join": "_JOIN"}[phase]

	if stage == "" {
		if suffix == "" {
			return "" // global process default
		}

		return ".*" + suffix + ".*"
	}

	return stage + suffix + ".*"
}

// render emits the process{} block: the global defaults first, then a withName
// selector per group, in a stable order.
func render(groups map[string][]string) string {
	var b strings.Builder

	b.WriteString("// Generated from an mrp --overrides file by `mro2nf overrides`.\n")
	b.WriteString("// Apply at launch: nextflow run main.nf -c overrides.config\n")
	b.WriteString("process {\n")

	for _, line := range groups[""] {
		fmt.Fprintf(&b, "    %s\n", line)
	}

	for _, sel := range sortedKeys(groups) {
		if sel == "" {
			continue
		}

		fmt.Fprintf(&b, "    withName: '%s' { %s }\n", sel, strings.Join(groups[sel], "; "))
	}

	b.WriteString("}\n")

	return b.String()
}

// memLiteral renders a JSON number of GB as a Nextflow memory string.
func memLiteral(val json.RawMessage) string {
	return "'" + strings.TrimSpace(string(val)) + " GB'"
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
