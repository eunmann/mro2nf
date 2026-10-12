// The stage processes every call-graph node runs through, each node
// including them under its own alias, so a withName: selector names the
// node. meta carries the node's identity, the cache key of its spec
// (digest), where its run's invocation lives (plan), and the resources it
// is scheduled with; its GPU request is the config's, which reads
// meta.special per task where a Nextflow 24.04 process directive cannot.
// Each phase runs `mre args`, then the phase's jobs through `mre run` as mrp
// runs them, then `mre post`.

// shq quotes s for the shell: every value a script names goes through it.
def shq(s) {
    return "'" + s.toString().replace("'", "'\\''") + "'"
}

// ordered is the dirs of [fork key, dir] pairs by key, ordinal by ordinal,
// then by dir name. A task stages such a gather as one list, whose order is
// part of its -resume key and would otherwise be the order the tasks
// producing it completed in.
def ordered(List pairs) {
    return pairs.sort(false) { a, b -> keyOrder(a[0], b[0]) ?: a[1].name <=> b[1].name }.collect { p -> p[1] }
}

def keyOrder(List a, List b) {
    def c = [a, b].transpose().collect { x, y -> x <=> y }.find { v -> v != 0 }
    return c ?: a.size() <=> b.size()
}

// graph names the invocation an mre call-graph subcommand compiles: the
// run's plan, compiled as the entry file, so Martian resolves the entry's
// includes and stage code beside it and the _jobinfo invocation records
// mrp's include.
def graph(Map meta) {
    return "-mro ${shq(meta.plan + '/' + meta.mro)} -as ${shq(meta.entry)} -mropath ${shq(meta.mropath)}"
}

// args builds the instance's args bundle from what it staged and its entry
// fork records, which are base64 and need no quoting. The driver ran the
// instance because its gates hold it enabled; mre args must agree.
def args(Map meta, List forks) {
    def cmd = "mkdir -p in\nstate=\$(${shq(params.mro2nf_mre)} args ${graph(meta)} -node ${shq(meta.node)} -in in -o args"
    forks.each { f -> cmd += " -fork ${f}" }
    return cmd + ")\n[ \"\$state\" = enabled ] || { echo ${shq('mre args: ' + meta.node + ' is not enabled where its gates said enabled')} >&2; exit 1; }"
}

// rest may be a GString, which Nextflow 24.04 does not pass for a String.
def job(Map meta, phase, rest) {
    def cmd = "${shq(params.mro2nf_mre)} run -phase ${phase} ${graph(meta)} -adapters ${shq(params.mro2nf_adapters)} -mrjob ${shq(params.mro2nf_mrjob)} -psid ${shq(params.mro2nf_psid)} -work ."
    if (params.mro2nf_monitor) {
        cmd += ' -monitor'
    }
    if (params.mro2nf_confine) {
        cmd += ' -confine'
    }
    return cmd + ' ' + rest
}

def post(Map meta) {
    return "${shq(params.mro2nf_mre)} post ${graph(meta)} -args args -outs stage_outs -fields outs -forks forks -gates gates"
}

// resources reads the table a split writes (mre run -phase split): per
// chunk, by its name, and for the join, the cpus, memory and special its
// job is scheduled with, those its _jobinfo records.
def resources(String table) {
    return table.readLines().collectEntries { l ->
        def f = l.split('\t', 4)
        [(f[0]): [cpus: f[1] as int, mem_gb: f[2] as double, special: f[3]]]
    }
}

// mem is the memory a task is scheduled with. Under -monitor mrjob kills a
// stage at its own request, as mrp does, so a retry gets the same memory;
// otherwise a retry, after a signal such as the executor's out-of-memory
// kill, gets more. The stage sees its request either way (_jobinfo).
def mem(Map meta, int attempt) {
    return "${params.mro2nf_monitor ? meta.mem_gb : meta.mem_gb * attempt} GB"
}

process STAGE {
    tag "${meta.node} ${key.join('.')}"
    cpus { meta.cpus }
    memory { mem(meta, task.attempt) }
    clusterOptions { params.job_resources[meta.special] ?: '' }

    input:
    tuple val(meta), val(key), val(forks), path(ups, stageAs: 'in/u*', arity: '0..*')

    output:
    tuple val(key), path('outs/*', type: 'dir', arity: '0..*'), emit: outs
    tuple val(key), path('forks/*/*', type: 'dir', arity: '0..*'), emit: forks
    tuple val(key), eval('cat gates'), emit: gates

    script:
    """
    ${args(meta, forks)}
    ${job(meta, 'main', '-args args -o stage_outs')}
    ${post(meta)}
    """
}

process STAGE_SPLIT {
    tag "${meta.node} ${key.join('.')}"
    cpus { meta.cpus }
    memory { mem(meta, task.attempt) }
    clusterOptions { params.job_resources[meta.special] ?: '' }

    input:
    tuple val(meta), val(key), val(forks), path(ups, stageAs: 'in/u*', arity: '0..*')

    output:
    tuple val(meta), val(key), path('args', type: 'dir'), path('chunks/chnk*', type: 'dir', arity: '0..*'), path('chunks/join.json'), eval('cat chunks/resources.tsv'), emit: split

    script:
    """
    ${args(meta, forks)}
    ${job(meta, 'split', '-args args -o chunks')}
    """
}

process STAGE_MAIN {
    tag "${meta.node} ${key.join('.')} ${name}"
    cpus { meta.cpus }
    memory { mem(meta, task.attempt) }
    clusterOptions { params.job_resources[meta.special] ?: '' }

    // The chunk is staged below chunk/: the job's metadata dir is named as
    // the chunk, and must not write through the link into the split's.
    input:
    tuple val(meta), val(key), path('args'), val(name), path(chunk, stageAs: 'chunk/*')

    output:
    tuple val(meta), val(key), path('chunkout/*', type: 'dir'), emit: chunk

    script:
    """
    ${job(meta, 'main', "-args args -chunk ${shq(chunk)} -o ${shq('chunkout/' + name)}")}
    """
}

process STAGE_JOIN {
    tag "${meta.node} ${key.join('.')}"
    cpus { meta.cpus }
    memory { mem(meta, task.attempt) }
    clusterOptions { params.job_resources[meta.special] ?: '' }

    input:
    tuple val(meta), val(key), path('args'), path(chunks, stageAs: 'chunks/*', arity: '0..*'), path('chunks/join.json'), path(couts, stageAs: 'chunkouts/*', arity: '0..*')

    output:
    tuple val(key), path('outs/*', type: 'dir', arity: '0..*'), emit: outs
    tuple val(key), path('forks/*/*', type: 'dir', arity: '0..*'), emit: forks
    tuple val(key), eval('cat gates'), emit: gates

    script:
    """
    ${job(meta, 'join', '-args args -chunks chunks -chunkouts chunkouts -o stage_outs')}
    ${post(meta)}
    """
}

// OUTS resolves the pipeline outs from the stage outs they read and lays
// them out as mrp publishes them.
process OUTS {
    publishDir params.outdir, mode: 'copy', pattern: '{pipeline_outs.json,manifest.json.gz}'
    publishDir params.outdir, mode: 'copy', saveAs: { f -> f.startsWith('outs/') ? f.substring(5) : null }

    input:
    val meta
    path ups, stageAs: 'in/u*', arity: '0..*'
    path 'types.json'

    output:
    path 'pipeline_outs.json'
    path 'manifest.json.gz'
    path 'outs/*', hidden: true, arity: '0..*'

    script:
    """
    mkdir -p in
    ${shq(params.mro2nf_mre)} outs ${graph(meta)} -types types.json -in in -o final
    ${shq(params.mro2nf_mre)} publish-layout -sidecar final/data.json -dir . -types types.json -callable ${shq(meta.call)} -role out -leaves final/f -outs outs
    """
}
