package cgnf

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// config renders nextflow.config: the engine floor, the tools and knobs
// the stage module reads, one param per entry input, and the retry policy.
func (p *project) config() string {
	var b strings.Builder

	b.WriteString(`// The path 'arity' option, eval outputs and the local profile's
// process.resourceLimits clamp need Nextflow 24.04.
manifest.nextflowVersion = '!>=24.04.0'

params.outdir = 'results'

// Martian 'special' scheduler-resource map (the MRO_JOBRESOURCES analog):
// a stage's special key to the clusterOptions of a grid executor.
params.job_resources = [:]

`)

	fmt.Fprintf(&b, "params.mro2nf_mre = %s\n", gstr(p.opts.Mre))
	fmt.Fprintf(&b, "params.mro2nf_mrjob = %s\n", gstr(p.opts.Mrjob))
	fmt.Fprintf(&b, "params.mro2nf_adapters = %s\n", gstr(p.opts.Adapters))
	fmt.Fprintf(&b, "params.mro2nf_psid = %s\n", gstr(p.src.Invocation.Call))
	fmt.Fprintf(&b, "params.mro2nf_monitor = %t\n\n", p.opts.Monitor)

	b.WriteString("// The entry inputs, null keeping the transpiled invocation's value.\n")

	for _, id := range slices.Sorted(maps.Keys(p.src.Invocation.Args)) {
		fmt.Fprintf(&b, "params.%s = null\n", id)
	}

	b.WriteString(`
// mre exits 42 for an ASSERT-class stage failure, which mrp never retries,
// and 1 for any other.
process {
    errorStrategy = { task.exitStatus == 42 ? 'terminate' : 'retry' }
    maxRetries = 2
`)

	p.gpuSelectors(&b)

	b.WriteString(`}

trace {
    enabled = true
    fields  = 'task_id,hash,native_id,name,status,exit,attempt,submit,duration,realtime,%cpu,peak_rss,peak_vmem,rchar,wchar,workdir'
}

// Override the clamp with a -c config setting process.resourceLimits.
profiles {
    standard {
        process.executor = 'local'
        process.resourceLimits = [ cpus: nextflow.util.SysHelper.getAvailCpus(), memory: nextflow.util.SysHelper.getAvailMemory() ]
    }
    slurm    { process.executor = 'slurm' }
    sge      { process.executor = 'sge' }
    lsf      { process.executor = 'lsf' }
    pbs      { process.executor = 'pbs' }
}
`)

	return b.String()
}

// gpuSelectors requests the GPUs `special = "gpu[:N]"` asks for on the
// process alias that runs the stage's code: a split stage's chunks do the
// GPU work, its split and join none.
func (p *project) gpuSelectors(b *strings.Builder) {
	for i, n := range p.topo.Nodes {
		s, err := p.stage(n.Fqid)
		if err != nil || s.Resources == nil || gpus(s.Resources.Special) == 0 {
			continue
		}

		proc := alias(i, n.Fqid)
		if n.Split {
			proc += "_MN"
		}

		fmt.Fprintf(b, "    withName: %s {\n        accelerator = %d\n    }\n", gstr(proc), gpus(s.Resources.Special))
	}
}
