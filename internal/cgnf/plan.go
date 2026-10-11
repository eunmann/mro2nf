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
// the driver as values: one eval per entry-fed root.
func (p *project) planProcess(g *gen) {
	var outs strings.Builder

	for j, root := range p.entry {
		fmt.Fprintf(&outs, "    eval(%s), emit: r%d\n", gstr("cat "+shellQuote("roots/"+root)), j)
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
    ${shq(params.mro2nf_mre)} plan -source source.json -values values.json -mropath ${shq(meta.mropath)} -o ${shq(meta.plan)} -nodes nodes -roots roots
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

// outs runs OUTS over every outs bundle the pipeline outs read.
func (p *project) outs(g *gen) {
	srcs := make([]string, 0, len(p.topo.Outs))

	for _, r := range p.topo.Outs {
		sel := "ds"
		if r.Fields != nil {
			sel = "ds.findAll { e -> e.name in " + glist(r.Fields) + " }"
		}

		srcs = append(srcs, fmt.Sprintf("o%d_outs.flatMap { _k, ds -> %s }", p.index[r.Node], sel))
	}

	ups := "channel.value([])"
	if len(srcs) > 0 {
		ups = "channel.empty().mix(" + strings.Join(srcs, ", ") + ").collect().ifEmpty([])"
	}

	// The meta waits on PLAN, which writes the invocation OUTS compiles.
	g.linef("OUTS(nodes.map { _ns -> %s }, %s, file(\"${projectDir}/_assets/types.json\"))", p.graphMeta("plan", "call: "+gstr(p.src.Invocation.Call)), ups)
}

// shellQuote is s single-quoted for the shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
