// The stage processes every call-graph node runs through, each node
// including them under its own alias, so a withName: selector names the
// node. meta carries the node's identity, the cache key of its spec
// (digest), where its run's invocation lives (plan), and the resources it
// is scheduled with; a GPU request is the config's, per alias, as Nextflow
// 24.04 takes no dynamic accelerator. Each phase runs `mre args`, then the
// phase's jobs through `mre run` as mrp runs them, then `mre post`.

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
// run's plan, linked into the task under the entry file's name so the
// _jobinfo invocation records mrp's include.
def graph(Map meta) {
    return "-mro ${shq(meta.mro)} -mropath ${shq(meta.mropath)}"
}

def linkPlan(Map meta) {
    return "ln -s ${shq(meta.plan + '/' + meta.mro)} ${shq(meta.mro)}"
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
    return cmd + ' ' + rest
}

def post(Map meta) {
    return "${shq(params.mro2nf_mre)} post ${graph(meta)} -args args -outs stage_outs -fields outs -forks forks -gates gates"
}

def jsonOf(Path p) {
    return new groovy.json.JsonSlurper().parseText(p.text)
}

// sized is meta scheduled with the resources a split returned for a chunk
// or for its join (chunk.json, join.json), read on the driver: each of
// __threads, __mem_gb and __special it sets replaces the stage's, a
// negative count at its magnitude, as the stage's own.
def sized(Map meta, Object res) {
    if (!(res instanceof Map)) {
        return meta
    }
    def t = Math.abs((res['__threads'] ?: 0) as double)
    def m = Math.abs((res['__mem_gb'] ?: 0) as double)
    return meta + [
        cpus: t > 0 ? Math.max(1, Math.ceil(t) as int) : meta.cpus,
        mem_gb: m > 0 ? m : meta.mem_gb,
        special: res['__special'] ?: meta.special,
    ]
}

// mem is the memory a task is scheduled with, raised on each retry.
def mem(Map meta, int attempt) {
    return "${meta.mem_gb * attempt} GB"
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
    ${linkPlan(meta)}
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
    tuple val(meta), val(key), path('args', type: 'dir'), path('chunks/chnk*', type: 'dir', arity: '0..*'), path('chunks/join.json'), emit: split

    script:
    """
    ${linkPlan(meta)}
    ${args(meta, forks)}
    ${job(meta, 'split', '-args args -o chunks')}
    """
}

process STAGE_MAIN {
    tag "${meta.node} ${key.join('.')} ${chunk.name}"
    cpus { meta.cpus }
    memory { mem(meta, task.attempt) }
    clusterOptions { params.job_resources[meta.special] ?: '' }

    input:
    tuple val(meta), val(key), path('args'), path(chunk)

    output:
    tuple val(meta), val(key), path('chunkout/*', type: 'dir'), emit: chunk

    script:
    """
    ${linkPlan(meta)}
    ${job(meta, 'main', "-args args -chunk ${shq(chunk.name)} -o ${shq('chunkout/' + chunk.name)}")}
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
    ${linkPlan(meta)}
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
    ${linkPlan(meta)}
    mkdir -p in
    ${shq(params.mro2nf_mre)} outs ${graph(meta)} -types types.json -in in -o final
    ${shq(params.mro2nf_mre)} publish-layout -sidecar final/data.json -dir . -types types.json -callable ${shq(meta.call)} -role out -leaves final/f -outs outs
    """
}
