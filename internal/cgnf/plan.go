package cgnf

import "fmt"

// planProcess declares PLAN, the run's one start task, which compiles the
// project's copy of the entry's .mro files. The driver reads its records
// into values, so a task's cache key holds what PLAN fixed for it, not the
// path of PLAN's file: the run's call, which every task compiles over the
// entry's declarations where the run finds its code, each node's digest and
// its resources under the run's mrp --overrides file, the entry files each
// instance stages, the forks of every entry-fed root, one record, so
// PLAN's outputs do not grow with the roots, and the pipelines' version,
// found once, as mrp finds it, on the include path the stage tasks find the
// code on, where its own copy of the .mro files has no version. A
// multi-line record is a file
// the driver reads, not an eval output, which Nextflow joins line by line
// in time quadratic in its lines. Under an S3 mount, which s3:// inputs
// PLAN reads in place depends on the buckets its instance mounts, which no
// input of PLAN holds, so a -resume re-runs it; a task downstream stays
// cached while PLAN's records are unchanged. The run's work dir is an input
// string, not workflow.workDir in the script: on Nextflow's v2 parser a
// global a script reads is no part of the task's -resume key, so a run
// under another work dir would reuse a PLAN that checked the entry values
// against the old one. The unsupported v1 parser would key it on the Path's
// modification time, which each run's new task dirs change, so PLAN would
// never resume. The launch params it reads are its meta's (planRun), as no
// 26.04 release keys a param read by a function its script calls (gen.go).
// It fails the run unless the mre it runs is the mro2nf build that
// transpiled the project (meta.build), whose flags and records every task
// relies on.
const planProcess = `process PLAN {
    cache !params.mro2nf_s3_mount

    input:
    val values
    val meta
    val workdir
    path 'mro'
    path overrides, stageAs: 'overrides/*', arity: '0..*'

    output:
    eval('cat call'), emit: call
    path('nodes'), emit: nodes
    path('leaves'), emit: leaves
    path('files'), emit: files
    path('roots'), emit: roots
    eval('cat pipelines'), emit: pipelines

    script:
    """
    cat > values.json <<'MRO2NF_VALUES'
    ${values}
    MRO2NF_VALUES
    ${shq(meta.run.mre)} plan -build ${shq(meta.build)} ${code(meta, 'mro')} -values values.json${overrides ? ' -overrides ' + shq(overrides[0]) : ''} -psid ${shq(meta.run.psid)}${meta.run.profile ? ' -profile ' + shq(meta.run.profile) : ''} -host-inputs=${meta.run.host_inputs} -host-in-place=${meta.run.host_in_place} -remote-inputs=${meta.run.remote_inputs} -run-workdir ${shq(workdir)}${meta.run.s3_mount ? ' -s3-mount ' + shq(meta.run.s3_mount) : ''}${pool(task, meta.run.local_jobs)} -call call -nodes nodes -roots roots -leaves leaves -files files -code-mropath ${shq(meta.mropath)} -pipelines pipelines
    """
}

`

// values is the workflow expression of the run's entry values as JSON:
// each entry input's param that is not its default (changed), which an
// unset input is. mre plan keeps an input the values leave out as the .mro
// writes it, where a value round-tripped through Groovy and JSON would lose
// its lexeme, such as a float's -0.
func (p *project) values() string {
	values := make([]string, len(p.inputs))
	defaults := []string{}

	for i, lp := range p.inputs {
		values[i] = gstr(lp.id) + ": params." + lp.id
		if lp.defaultFunc != "" {
			defaults = append(defaults, gstr(lp.id)+": "+lp.defaultFunc+"()")
		}
	}

	if len(values) == 0 {
		return "'{}'"
	}

	return "groovy.json.JsonOutput.toJson(changed(" + gmap(values) + ", " + gmap(defaults) + "))"
}

// publications maps each node the pipeline outs read to the source of the
// outs items it publishes for OUTS, which the workflow wiring it mixes.
func (p *project) publications() map[int]string {
	pubs := make(map[int]string, len(p.topo.Outs))

	for _, r := range p.topo.Outs {
		u := p.index[r.Node]
		pubs[u] = fmt.Sprintf("PUBLISHED(%s, %d, %s)", outsOf(u), u, fieldsArg(r.Fields))
	}

	return pubs
}

// publication ends the entry workflow with its outputs, OUTS's outs JSON
// and each entry of the outs tree its manifest lists, and declares how they
// are published below Nextflow's outputDir: each entry from where the task
// that wrote it keeps it to its path in the tree, so no other task moves a
// stage's file's bytes (#610), as copies, so an object-store or HealthOmics
// output dir holds the files themselves. A -preview run publishes nothing:
// it runs no task, and Nextflow 26.04 to 26.10 then waits forever on an
// enabled output's channel.
const publication = `
    publish:
    pipeline_outs = OUTS.out.json
    results = OUTS.out.manifest.flatMap { m -> published(m) }
}

output {
    pipeline_outs {
        enabled !workflow.preview
        mode 'copy'
    }
    results {
        enabled !workflow.preview
        mode 'copy'
        path { e -> e[0] >> e[1] }
    }
}
`

// outs runs OUTS over every outs item the pipeline outs read, which the
// entry workflow's published channel mixes, ordered by node and instance
// key, not by completion: the skeletons of the bundles and of those
// holding the files they name by reference, which it stages, with where
// each producing task keeps the bundle, and the value records it gets as a
// value; and the entry files they read, which PLAN lists by the pipeline's
// fqid.
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
	shared := w.use("shared")
	w.linef("outs_files = outs_in.map { xs -> skeletons(bundlesOf(xs)) }")
	w.linef("outs_refs = outs_in.map { xs -> skeletons(refBundles(xs)) }")
	w.linef("OUTS(%s, %s.map { m -> callStaged(m) }, outs_files, outs_files.map { ss -> bundleLocations(ss) }, "+
		"outs_refs, outs_refs.map { ss -> bundleLocations(ss) }, outs_in.map { xs -> recordsOf(xs) }, %s, file(\"${projectDir}/%s\"))",
		shared, shared, entry, mroAssets)

	return w
}
