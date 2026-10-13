package cgnf

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// planProcess declares PLAN, the run's one start task, which compiles the
// project's copy of the entry's .mro files. Its records reach the driver as
// values, so a task's cache key holds what PLAN fixed for it, not the
// task's path: the run's call, which every task compiles over the entry's
// declarations where the run finds its code, each node's digest and its
// resources under the run's mrp --overrides file, the entry files each
// instance stages, and the forks of every entry-fed root, one record, so
// PLAN's outputs do not grow with the roots. Under an S3
// mount, which s3:// inputs PLAN reads in place depends on the buckets its
// instance mounts, which no input of PLAN holds, so a -resume re-runs it;
// a task downstream stays cached while PLAN's records are unchanged.
const planProcess = `process PLAN {
    cache !params.mro2nf_s3_mount

    input:
    val values
    val meta
    path 'mro'
    path overrides, stageAs: 'overrides/*', arity: '0..*'

    output:
    eval('cat call'), emit: call
    eval('cat nodes'), emit: nodes
    eval('cat leaves'), emit: leaves
    eval('cat files'), emit: files
    eval('cat roots'), emit: roots

    script:
    """
    cat > values.json <<'MRO2NF_VALUES'
    ${values}
    MRO2NF_VALUES
    ${shq(params.mro2nf_mre)} plan ${code(meta, 'mro')} -values values.json${overrides ? ' -overrides ' + shq(overrides[0]) : ''} -psid ${shq(params.mro2nf_psid)} -host-inputs=${shq(params.mro2nf_host_inputs)} -remote-inputs=${shq(params.mro2nf_allow_remote_inputs)}${params.mro2nf_s3_mount ? ' -s3-mount ' + shq(params.mro2nf_s3_mount) : ''}${pool(task)} -call call -nodes nodes -roots roots -leaves leaves -files files
    """
}

`

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

// publications maps each node the pipeline outs read to the source of the
// outs bundles it publishes for OUTS, which the workflow wiring it mixes.
func (p *project) publications() map[int]string {
	pubs := make(map[int]string, len(p.topo.Outs))

	for _, r := range p.topo.Outs {
		fields := "null"
		if r.Fields != nil {
			fields = glist(r.Fields)
		}

		u := p.index[r.Node]
		pubs[u] = fmt.Sprintf("PUBLISHED(%s, %d, %s)", outsOf(u), u, fields)
	}

	return pubs
}

// outs runs OUTS over every outs bundle the pipeline outs read, which the
// entry workflow's published channel mixes, ordered by node and instance
// key, not by completion, and the entry files they read, which PLAN lists
// by the pipeline's fqid.
func (p *project) outs() *wiring {
	w := &wiring{}

	ups := "channel.value([])"
	if len(p.topo.Outs) > 0 {
		ups = publishedChannel + ".toList().map { xs -> ordered(xs) }"
	}

	entry := "channel.value([[], []])"
	if p.readers[p.g.Fqid()] {
		entry = fmt.Sprintf("%s.map { ef -> def ids = ef.files[%s] ?: []; [ids, ids.collect { x -> ef.leaves[x] }] }", w.use("entry"), gstr(p.g.Fqid()+" -"))
	}

	w.linef("outs_in = %s", ups)
	w.linef("OUTS(%s, outs_in, outs_in.map { ds -> ds.collect { d -> d.toUriString() } }, %s, file(\"${projectDir}/_assets/types.json\"), file(\"${projectDir}/%s\"))", w.use("shared"), entry, mroAssets)

	return w
}
