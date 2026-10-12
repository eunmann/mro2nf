package cgnf

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// planProcess declares PLAN, the run's one start task. It never caches: it
// rewrites the invocation at the session's plan path, which tasks read by
// that constant path instead of staging it, so a task's cache key holds
// its own node's digest and not the whole invocation. Its records reach
// the driver as values: per entry-fed root, its forks and their counts.
func (p *project) planProcess(g *gen) {
	var outs strings.Builder

	for j, root := range p.entry {
		fmt.Fprintf(&outs, "    eval(%s), emit: r%d\n", gstr("cat "+shellQuote("roots/"+root)), j)
		fmt.Fprintf(&outs, "    eval(%s), emit: n%d\n", gstr("cat "+shellQuote("counts/"+root)), j)
	}

	fmt.Fprintf(&g.procs, `process PLAN {
    cache false

    input:
    val values
    val meta
    path 'source.json'

    output:
    eval('cat nodes'), emit: nodes
%s
    script:
    """
    cat > values.json <<'MRO2NF_VALUES'
    ${values}
    MRO2NF_VALUES
    ${shq(params.mro2nf_mre)} plan -source source.json -values values.json -mropath ${shq(meta.mropath)} -o ${shq(meta.plan)} -nodes nodes -roots roots -counts counts
    """
}

`, outs.String())
}

// values is the workflow expression of the run's entry values as JSON:
// each entry input's param, null when unset, which keeps the transpiled
// value.
func (p *project) values() string {
	ids := slices.Sorted(maps.Keys(p.src.Invocation.Args))

	entries := make([]string, len(ids))
	for i, id := range ids {
		entries[i] = gstr(id) + ": params." + id
	}

	if len(entries) == 0 {
		return "'{}'"
	}

	return "groovy.json.JsonOutput.toJson([" + strings.Join(entries, ", ") + "])"
}

// outs runs OUTS over every outs bundle the pipeline outs read, ordered by
// node and instance key, not by completion.
func (p *project) outs(g *gen) {
	srcs := make([]string, 0, len(p.topo.Outs))

	for _, r := range p.topo.Outs {
		sel := "ds"
		if r.Fields != nil {
			sel = "ds.findAll { e -> e.name in " + glist(r.Fields) + " }"
		}

		srcs = append(srcs, fmt.Sprintf("o%[1]d_outs.flatMap { k, ds -> %[2]s.collect { e -> [[%[1]d] + k, e] } }", p.index[r.Node], sel))
	}

	ups := "channel.value([])"
	if len(srcs) > 0 {
		ups = "channel.empty().mix(" + strings.Join(srcs, ", ") + ").toList().map { xs -> ordered(xs) }"
	}

	// The meta waits on PLAN, which writes the invocation OUTS compiles.
	g.linef("OUTS(nodes.map { _ns -> %s }, %s, file(\"${projectDir}/_assets/types.json\"))", p.graphMeta("plan", "call: "+gstr(p.src.Invocation.Call)), ups)
}

// shellQuote is s single-quoted for the shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
