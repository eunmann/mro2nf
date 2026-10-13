// The stage processes every call-graph node runs through, each node
// including them under its own alias, so a withName: selector names the
// node, and the sub-workflows that wire a node's instances to them. meta
// carries the node's identity, the cache key of its spec (digest), the
// run's call (call), the resources it is scheduled with, and its own
// overrides file (overrides), which PLAN resolved; its GPU request is the
// config's, which reads meta.special per task where a Nextflow 25.10
// process accelerator directive cannot. Each task runs one `mre stage`
// call: the instance's args, which it builds from the inputs it staged,
// its phase's job as mrp runs it, and the post after it that the phase
// has. A split's chunks and join stage the split's inputs and build its
// args again, so no task outputs args: each links the files they name
// where Nextflow staged them, which an object-store work dir would
// otherwise receive as one copy per fork.

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

// code names the entry .mro and the include path of the run's invocation
// below root: '' for a stage task, which finds the code at the paths the
// project was transpiled from, on the host or in the image, or the
// project's copy of its .mro files a task staged. Martian resolves the
// entry's includes and stage code beside it, and the _jobinfo invocation
// records its include.
def code(Map meta, String root) {
    return "-entry ${shq(root + meta.entry)} -mropath ${shq(meta.mropath.tokenize(':').collect { p -> root + p }.join(':'))}"
}

// graph names the invocation an mre call-graph subcommand compiles: the
// run's call callFile wrote, over the entry's declarations.
def graph(Map meta, String root) {
    return "${code(meta, root)} -call call.b64"
}

// callFile writes the run's call, base64, from the task's own value, which
// stays its -resume key. As an argument, a wide run's call would pass the
// kernel's limit on one argument (128 KiB). The here-document's lines start
// their own lines, where the script's indent does not reach.
def callFile(Map meta) {
    return "cat > call.b64 <<'MRO2NF_CALL'\n${meta.call}\nMRO2NF_CALL"
}

// args is what a task builds the instance's args bundle from: what it
// staged, its entry fork records, which are base64 and need no quoting,
// and the entry files it staged below entry/, by their indices among the
// run's. The driver ran the instance because its gates hold it enabled; mre
// must agree, and the node must compile to the spec PLAN planned.
def args(Map meta, List forks, List leaves) {
    def cmd = "-digest ${meta.digest} -in in"
    forks.each { f -> cmd += " -fork ${f}" }
    if (leaves) {
        cmd += " -leaves ${shq(leaves.join(','))}"
    }
    return cmd
}

// on reads a boolean param, which a launch's --<name> false sets to the
// string 'false' under the v2 syntax parser or
// NXF_DISABLE_PARAMS_TYPE_DETECTION, a value Groovy takes as true.
def on(v) {
    return v.toString() == 'true'
}

// pool names the pool mrp's local job manager grants a job from, its
// --localcores and --localmem, where params.mro2nf_local_jobs holds: the
// cpus and whole GB of memory process.resourceLimits clamps the task to, so
// a job granted the whole pool requests no more than its task is given.
def pool(task) {
    if (!on(params.mro2nf_local_jobs)) {
        return ''
    }
    def lim = task.resourceLimits ?: [:]
    if (!lim.cpus || !lim.memory) {
        error('params.mro2nf_local_jobs grants each job from process.resourceLimits, which sets no cpus or memory')
    }
    def mem = lim.memory instanceof nextflow.util.MemoryUnit ? lim.memory : nextflow.util.MemoryUnit.of(lim.memory.toString())
    return " -local-cores ${lim.cpus as int} -local-mem-gb ${Math.max(1L, mem.toGiga())}"
}

// binds is the docker run options that bind each path of PLAN's binds
// record, base64 by line, the entry files every task reads in place,
// read-only into a task's container at the path itself, which the docker
// profile adds to every task's: a stage opens the path it was given, or
// one an earlier stage recorded, as under mrp. A path the head node does
// not see is not bound, so it fails only a stage that opens it, as under
// mrp, where docker would refuse to start every task.
def binds(String record) {
    return record.readLines().collect { l -> new String(l.decodeBase64(), 'UTF-8') }.findAll { p -> new File(p).exists() }.collect { p ->
        ' --mount ' + shq('type=bind,' + mountField('source=' + p) + ',' + mountField('target=' + p) + ',readonly')
    }.join('')
}

// mountField quotes a --mount field as docker reads it, so a comma or quote
// in a path stays the path's.
def mountField(String s) {
    return '"' + s.replace('"', '""') + '"'
}

// mreStage runs the task's phase through mre stage.
def mreStage(task, Map meta, phase, rest) {
    def cmd = "${shq(params.mro2nf_mre)} stage -phase ${phase} ${graph(meta, '')} -node ${shq(meta.node)} -psid ${shq(params.mro2nf_psid)} -work .${pool(task)}"
    if (params.mro2nf_adapters) {
        cmd += " -adapters ${shq(params.mro2nf_adapters)}"
    }
    if (params.mro2nf_mrjob) {
        cmd += " -mrjob ${shq(params.mro2nf_mrjob)}"
    }
    if (on(params.mro2nf_monitor)) {
        cmd += ' -monitor'
    }
    if (on(params.mro2nf_confine)) {
        cmd += ' -confine'
    }
    if (params.mro2nf_s3_mount) {
        cmd += " -s3-mount ${shq(params.mro2nf_s3_mount)}"
    }
    if (meta.overrides) {
        cmd = "printf '%s\\n' ${shq(meta.overrides)} > overrides.json\n${cmd} -overrides overrides.json"
    }
    return cmd + ' ' + rest
}

// postFlags name where a task that posts its instance's outs writes them,
// one bundle per output, and the fork elements and gates they set.
def postFlags() {
    return '-fields outs -forks forks -gates gates'
}

// resources reads the table a split writes (mre stage -phase split): per
// chunk, by its name, and for the join, the cpus, memory and special its
// job is scheduled with, those its _jobinfo records.
def resources(String table) {
    return table.readLines().collectEntries { l ->
        def f = l.split('\t', 4)
        [(f[0]): [cpus: f[1] as int, mem_gb: f[2] as double, special: f[3]]]
    }
}

process STAGE {
    tag "${meta.node} ${key.join('.')}"
    cpus { meta.cpus }
    memory { "${meta.mem_gb} GB" }
    clusterOptions { params.job_resources[meta.special] ?: '' }

    input:
    tuple val(meta), val(key), val(forks), path(ups, stageAs: 'in/u*', arity: '0..*'), val(leaves), path(entry, stageAs: 'entry/?/*', arity: '0..*')

    output:
    tuple val(key), path('outs/*', type: 'dir', arity: '0..*'), emit: outs
    tuple val(key), path('forks/*/*', type: 'dir', arity: '0..*'), emit: forks
    tuple val(key), eval('cat gates'), emit: gates

    script:
    """
    ${callFile(meta)}
    mkdir -p in
    ${mreStage(task, meta, 'main', "${args(meta, forks, leaves)} -args args -o stage_outs ${postFlags()}")}
    """
}

process STAGE_SPLIT {
    tag "${meta.node} ${key.join('.')}"
    cpus { meta.cpus }
    memory { "${meta.mem_gb} GB" }
    clusterOptions { params.job_resources[meta.special] ?: '' }

    input:
    tuple val(meta), val(key), val(forks), path(ups, stageAs: 'in/u*', arity: '0..*'), val(leaves), path(entry, stageAs: 'entry/?/*', arity: '0..*')

    output:
    tuple val(meta), val(key), path('chunks/chnk*', type: 'dir', arity: '0..*'), path('chunks/join.json'), eval('cat chunks/resources.tsv'), emit: split

    script:
    """
    ${callFile(meta)}
    mkdir -p in
    ${mreStage(task, meta, 'split', "${args(meta, forks, leaves)} -args args -o chunks")}
    """
}

process STAGE_MAIN {
    tag "${meta.node} ${key.join('.')} ${name}"
    cpus { meta.cpus }
    memory { "${meta.mem_gb} GB" }
    clusterOptions { params.job_resources[meta.special] ?: '' }

    // The chunk is staged below chunk/: the job's metadata dir is named as
    // the chunk, and must not write through the link into the split's.
    input:
    tuple val(meta), val(key), val(forks), path(ups, stageAs: 'in/u*', arity: '0..*'), val(leaves), path(entry, stageAs: 'entry/?/*', arity: '0..*'), val(name), path(chunk, stageAs: 'chunk/*')

    output:
    tuple val(meta), val(key), path('chunkout/*', type: 'dir'), emit: chunk

    script:
    """
    ${callFile(meta)}
    mkdir -p in
    ${mreStage(task, meta, 'main', "${args(meta, forks, leaves)} -args args -chunk ${shq(chunk)} -o ${shq('chunkout/' + name)}")}
    """
}

process STAGE_JOIN {
    tag "${meta.node} ${key.join('.')}"
    cpus { meta.cpus }
    memory { "${meta.mem_gb} GB" }
    clusterOptions { params.job_resources[meta.special] ?: '' }

    input:
    tuple val(meta), val(key), val(forks), path(ups, stageAs: 'in/u*', arity: '0..*'), val(leaves), path(entry, stageAs: 'entry/?/*', arity: '0..*'), path(chunks, stageAs: 'chunks/*', arity: '0..*'), path('chunks/join.json'), path(couts, stageAs: 'chunkouts/*', arity: '0..*')

    output:
    tuple val(key), path('outs/*', type: 'dir', arity: '0..*'), emit: outs
    tuple val(key), path('forks/*/*', type: 'dir', arity: '0..*'), emit: forks
    tuple val(key), eval('cat gates'), emit: gates

    script:
    """
    ${callFile(meta)}
    mkdir -p in
    ${mreStage(task, meta, 'join', "${args(meta, forks, leaves)} -args args -chunks chunks -chunkouts chunkouts -o stage_outs ${postFlags()}")}
    """
}

// locations writes the file mre outs names an unpublished file by: each
// item OUTS staged and where its producing task's work dir keeps it, which
// no task sees of itself. The here-document's lines start their own lines,
// where the script's indent does not reach.
def locations(ups, List locs) {
    def lines = [ups, locs].transpose().collect { u, l -> "${u}\t${l}\n" }.join('')
    return "cat > locations <<'MRO2NF_LOCATIONS'\n${lines}MRO2NF_LOCATIONS"
}

// OUTS resolves the pipeline outs from the stage outs and the entry files
// they read and publishes them with mrp's own post-processing. It runs no
// stage, so it compiles the project's copy of the .mro files it stages.
process OUTS {
    publishDir params.outdir, mode: 'copy', saveAs: { f -> f.startsWith('results/') ? f.substring(8) : f }

    input:
    val meta
    path ups, stageAs: 'in/u*', arity: '0..*'
    val locs
    tuple val(leaves), path(entry, stageAs: 'entry/?/*', arity: '0..*')
    path 'mro'

    output:
    path 'pipeline_outs.json'
    path 'results/*', hidden: true, arity: '0..*'

    script:
    """
    mkdir -p in
    ${callFile(meta)}
    ${locations(ups, locs)}
    ${shq(params.mro2nf_mre)} outs ${graph(meta, 'mro')} -in in -locations locations${leaves ? ' -leaves ' + shq(leaves.join(',')) : ''} -o results -json pipeline_outs.json
    """
}

// PLAN's records, as the driver reads them. NODES maps each node to its
// digest, its off flag, the set of its statically disabled fork keys, the
// cpus and memory its first task is scheduled with and its own overrides,
// paired with the meta every task shares, so a node reads both from one
// channel.
workflow NODES {
    take:
    nodes
    shared

    main:
    out = nodes.combine(shared).map { s, c -> [s.readLines().collectEntries { l -> def f = l.tokenize(' '); [(f[0]): [f[1], f[2], f[3].tokenize(',').toSet()] + f.drop(4)] }, c] }

    emit:
    out
}

// ENTRY maps each node and fork of its entry-fed roots to the entry files
// staged there, and each of those to its file(), which globs no name.
workflow ENTRY {
    take:
    files
    leaves

    main:
    out = files.combine(leaves).map { fl, ll ->
        [
            files: fl.readLines().collectEntries { l -> def f = l.tokenize(' '); [(f[0] + ' ' + f[1]): f[2].tokenize(',')] },
            leaves: ll.readLines().collectEntries { l -> def f = l.tokenize(' '); [(f[0]): file(new String(f[1].decodeBase64(), 'UTF-8'), glob: false)] }
        ]
    }

    emit:
    out
}

// ENTRY_FORKS maps each entry-fed root to its forks, as FORK takes them.
workflow ENTRY_FORKS {
    take:
    roots

    main:
    out = roots.map { s ->
        s.readLines().collect { l -> l.tokenize(' ') }.groupBy { f -> f[0] }.collectEntries { r, fs ->
            [(r): fs.collect { f ->
                def k = f[1].tokenize('.').collect { t -> t as int }
                [k.take(k.size() - 1), k[-1], f[2], f[3]]
            }]
        }
    }

    emit:
    out
}

// ENTRY_ROOT is one entry-fed root's forks of ENTRY_FORKS, a lookup, so
// splitting the one record among the roots is linear in it.
workflow ENTRY_ROOT {
    take:
    records
    root

    main:
    out = records.flatMap { m -> m[root] ?: [] }

    emit:
    out
}

// PUBLISHED is the outs bundles of a node the pipeline outs read, each keyed
// by the node and its instance's key, which order OUTS's inputs.
workflow PUBLISHED {
    take:
    outs
    node
    fields

    main:
    out = outs.flatMap { k, ds -> selected(ds, fields).collect { e -> [[node] + k, e] } }

    emit:
    out
}

// The node wiring. main.nf runs each node's instances through these, one
// call per step with the node's static data, so its own lines stay few and
// no generated method grows with the node's wiring. An instance is [key,
// fork records, element dirs, index tokens]: its ordinal of each of the
// node's roots, the fork records of its entry-fed roots and the fork
// elements of its stage-fed ones, and the index token of its fork of each
// root a zip of the node splits over. A list of positions picks those
// entries of a key (k[at]).

// FORK extends each instance by one of its node's roots: it joins every
// fork of the root under its own ordinals of the root's outer roots, at
// positions outer. A fork is [outer key, own ordinal, record or element
// dir, index token]: an entry-fed root's record from PLAN, a stage-fed
// root's dir its producer posted (ELEMENTS).
workflow FORK {
    take:
    inst
    forks
    outer
    entryFed
    zipped

    main:
    out = inst.map { k, fs, es, tk -> [k[outer], k, fs, es, tk] }.combine(forks, by: 0).map { _o, k, fs, es, tk, own, x, t ->
        [k + [own], entryFed ? fs + [x] : fs, entryFed ? es : es + [x], zipped ? tk + [t] : tk]
    }

    emit:
    out
}

// ELEMENTS is the forks of a stage-fed root its producer posted, from the
// dirs below forks/<root>/: per outer root, at its position in at, the
// producer's ordinal of it, or, at -1, where the producer is not forked on
// it, the ordinal the dir's name carries before its own. A map key's token
// follows the ordinals after an @; an array fork's is its ordinal.
workflow ELEMENTS {
    take:
    forks
    root
    at
    keyed

    main:
    out = forks.flatMap { kp, ds ->
        ds.findAll { e -> e.parent.name == root }.collect { e ->
            def t = e.name.tokenize('@')[0].tokenize('.').collect { x -> x as int }
            def named = t.iterator()
            [at.collect { a -> a >= 0 ? kp[a] : named.next() }, t[-1], e, keyed ? e.name.tokenize('@')[1] : t[-1].toString()]
        }
    }

    emit:
    out
}

// ZIP adds to each instance, whose fork of the zipped root has the index
// token tk[token], the partner element of a stage's output there,
// posted by the instance of that stage agreeing with it on the stage's
// roots, at positions at, or, where it has none, its absent record, which
// reads null or a missing element as mrp reads the output's element there.
workflow ZIP {
    take:
    inst
    forks
    at
    partner
    token
    absent

    main:
    out = inst.map { k, fs, es, tk -> [k[at], k, fs, es, tk] }.combine(forks.map { kq, ds -> [kq, ds.findAll { e -> e.parent.name == partner }.collectEntries { e -> [(e.name): e] }] }, by: 0).map { _q, k, fs, es, tk, m ->
        [k, fs, es + ([m[tk[token]] ?: m[absent]] - [null]), tk]
    }

    emit:
    out
}

// AFTER holds PLAN's node records until a preflight is done: each node
// waiting for it gates its instances (GATE) on the records it holds.
workflow AFTER {
    take:
    nodes
    done

    main:
    out = nodes.combine(done).map { ns, c, _d -> [ns, c] }

    emit:
    out
}

// GATE gives each instance the meta of its node's tasks, planned from its
// node's PLAN record and its own meta, which it carries on as its d, and
// whether that record disables it: the whole node, or the instance's fork.
workflow GATE {
    take:
    inst
    nodes
    meta

    main:
    out = inst.combine(nodes).map { k, fs, es, _tk, ns, c ->
        def r = ns[meta.node]
        [k, fs, es, planned(c, meta, r), r[1] == '1' || r[2].contains(k.join('.'))]
    }

    emit:
    out
}

// GATES is a gating stage's posted gates, each instance's as a set, built
// once however many nodes it gates; a disabled instance posts none.
workflow GATES {
    take:
    gates
    skip

    main:
    out = gates.map { kp, gs -> [kp, gs.tokenize(' ').toSet()] }.mix(skip.map { v -> [v[0], [].toSet()] })

    emit:
    out
}

// GATED disables each instance a gating stage's GATES name, joined on the
// gating stage's roots, at positions at, by the gate's token and the
// instance's ordinals of the gate's own roots, at positions ext.
workflow GATED {
    take:
    gated
    gates
    at
    ext
    token

    main:
    out = gated.map { k, fs, es, d, off -> [k[at], k, fs, es, d, off] }.combine(gates, by: 0).map { _c, k, fs, es, d, off, gs ->
        [k, fs, es, d, off || gs.contains(token + k[ext].join('.'))]
    }

    emit:
    out
}

// ENABLED splits the instances the driver runs, which go on to stage what
// they read, from those it disables, which run no task.
workflow ENABLED {
    take:
    gated

    main:
    b = gated.branch { v ->
        skip: v[4]
        run: true
    }

    emit:
    run = b.run.map { k, fs, es, d, _off -> [k, fs, es, d, []] }
    skip = b.skip
}

// SELECT keys each outs bundle by its instance's ordinals, at positions
// theirs, of the roots a reader shares, with the outputs it reads, fields,
// or all of them where fields is null.
workflow SELECT {
    take:
    outs
    theirs
    fields

    main:
    out = outs.map { ku, ds -> [ku[theirs], selected(ds, fields)] }

    emit:
    out
}

def selected(List ds, List fields) {
    return fields == null ? ds : ds.findAll { e -> e.name in fields }
}

// READ adds to each instance the outs it reads of one node, keyed as ups
// keys them, at its own positions mine of the roots they share.
workflow READ {
    take:
    inst
    ups
    mine

    main:
    out = inst.map { k, fs, es, d, us -> [k[mine], k, fs, es, d, us] }.combine(ups, by: 0).map { _c, k, fs, es, d, us, ds -> [k, fs, es, d, us + ds] }

    emit:
    out
}

// GATHER keys, for each enabled instance, the outs of every instance under
// it of a node forked on roots it is not, once all are in, ordered by the
// upstream's key, not by completion; an instance under which there are none
// gathers none.
workflow GATHER {
    take:
    run
    outs
    mine
    theirs
    fields

    main:
    seeds = run.map { v -> [v[0][mine], null] }.unique()
    out = seeds.mix(outs.map { ku, ds -> [ku[theirs], selected(ds, fields).collect { e -> [ku, e] }] }).groupTuple().map { c, dss ->
        [c, ordered(dss.findAll { x -> x != null }.collectMany { x -> x })]
    }

    emit:
    out
}

// planned is the meta of a node's tasks: the one every task shares, with
// the run's call, then the node's identity and resources and its digest.
// Its PLAN record d gives the cpus and memory its first task is scheduled
// with and the overrides file of its own its jobs resolve theirs under,
// both under the run's overrides.
def planned(Map shared, Map meta, List d) {
    return shared + [node: meta.node, cpus: d[3] as int, mem_gb: d[4] as double, overrides: d[5] == '-' ? '' : d[5], special: meta.special, digest: d[0]]
}

// INPUT is each enabled instance as a stage process takes it, with the
// meta GATE planned for it; it stages no entry file.
workflow INPUT {
    take:
    inst

    main:
    out = inst.map { k, fs, es, d, us -> [d, k, fs, es + us, [], []] }

    emit:
    out
}

// INPUT_ENTRY is INPUT staging the entry files PLAN lists for the
// instance's fork of its node's entry-fed roots, at positions at, and their
// indices, which mre stage maps back to the run's values.
workflow INPUT_ENTRY {
    take:
    inst
    entry
    at

    main:
    out = inst.combine(entry).map { k, fs, es, d, us, ef ->
        def ids = ef.files[d.node + ' ' + (k[at].join('.') ?: '-')] ?: []
        [d, k, fs, es + us, ids, ids.collect { x -> ef.leaves[x] }]
    }

    emit:
    out
}

// staged is an instance the split process takes, by its key, with what the
// task stages to build its args: [key, fork records, staged items, entry
// indices, entry files].
def staged(List v) {
    return v.drop(1)
}

// CHUNKS is a main per chunk a split returned, each scheduled with the
// resources its job gets, from the split's resources table, and staging
// the split's inputs, input.
workflow CHUNKS {
    take:
    split
    input

    main:
    out = split.map { m, k, cs, _j, res -> [k, m, cs, res] }.join(input.map { v -> staged(v) }).flatMap { k, m, cs, res, fs, us, ids, ef ->
        def r = resources(res)
        cs.collect { c -> [m + r[c.name] + [chunks: cs.size()], k, fs, us, ids, ef, c.name, c] }
    }

    emit:
    out
}

// JOIN is a split's join over every chunk's outs once all are in, by chunk
// name, not by completion, scheduled with the resources the split gave it,
// and staging the split's inputs, input.
workflow JOIN {
    take:
    split
    chunk
    input

    main:
    outs = chunk.map { m, k, o -> [groupKey(k, m.chunks), o] }.groupTuple().map { gk, os -> [gk.getGroupTarget(), os.sort(false) { a, b -> a.name <=> b.name }] }
    none = split.filter { v -> v[2].isEmpty() }.map { v -> [v[1], []] }
    out = split.map { m, k, cs, j, res -> [k, m + resources(res).join, cs, j] }.join(input.map { v -> staged(v) }).join(outs.mix(none)).map { k, m, cs, j, fs, us, ids, ef, os -> [m, k, fs, us, ids, ef, cs, j, os] }

    emit:
    out
}

// DONE signals once every instance of a node is done: its tasks have all
// completed, and a disabled instance runs none.
workflow DONE {
    take:
    outs

    main:
    out = outs.toList().map { _xs -> true }

    emit:
    out
}

// OR_SKIPPED is a node's outs or fork dirs, a disabled instance standing in
// with none.
workflow OR_SKIPPED {
    take:
    out
    skip

    main:
    all = out.mix(skip.map { v -> [v[0], []] })

    emit:
    all
}
