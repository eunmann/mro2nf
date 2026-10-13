// The stage processes every call-graph node runs through, each node
// including them under its own alias, so a withName: selector names the
// node, and the sub-workflows that wire a node's instances to them. meta
// carries the node's identity, the cache key of its spec (digest), the
// run's call as CALL carries it (call), the resources it is scheduled with,
// and its own overrides file (overrides), which PLAN resolved; its GPU
// request is the config's, which reads meta.special per task where a
// Nextflow 25.10 process accelerator directive cannot. Each task runs one
// `mre stage` call: the instance's args, which it builds from the inputs it
// staged, its phase's job as mrp runs it, and the post after it that the
// phase has, each step handing the next what it needs in memory. A split's
// chunks and join stage the split's inputs and build its args again, so no
// task outputs args: each links the files they name where Nextflow staged
// them, which an object-store work dir would otherwise receive as one copy
// per fork.

// shq quotes s for the shell: every value a script names goes through it.
def shq(s) {
    return "'" + s.toString().replace("'", "'\\''") + "'"
}

// ordered is the items of [fork key, item] pairs by key, ordinal by
// ordinal, then by the output each holds. A task stages such a gather as
// one list, whose order is part of its -resume key and would otherwise be
// the order the tasks producing it completed in.
def ordered(List pairs) {
    return pairs.sort(false) { a, b -> keyOrder(a[0], b[0]) ?: itemName(a[1]) <=> itemName(b[1]) }.collect { p -> p[1] }
}

// An item is one thing an instance posted for its readers: an output, a
// fork element or a partner element. It is a dir a reader stages, or a
// value record a reader gets as a value, which leads with the path of the
// dir it stands in for below the post's outs or forks dir and a colon (mre
// stage -field-values, -fork-values): <output>, or <root>/<element>. A dir
// that names a file by reference, one its task staged from where its
// producer wrote it, comes as a list of the dir and the ordinal dir of
// each such file where it lies (referred), which a reader stages with it.
// itemName is the name of an item's dir, the output an outs item holds,
// and itemParent the name of the dir above it, the root or partner a fork
// item is of; bundlesOf are the dirs of a list of items, refsOf the
// ordinal dirs they name that lie in none of those dirs (unstaged), and
// recordsOf its value records.
def itemPath(e) {
    if (e instanceof List) {
        return itemPath(e[0])
    }
    return e instanceof Path ? [e.parent.name, e.name] : e.substring(0, e.indexOf(':')).tokenize('/')
}

def itemName(e) {
    return itemPath(e)[-1]
}

def itemParent(e) {
    return itemPath(e)[-2]
}

def bundlesOf(List items) {
    return items.findAll { e -> e instanceof Path || e instanceof List }.collect { e -> e instanceof List ? e[0] : e }
}

def refsOf(List items) {
    return unstaged(items.findAll { e -> e instanceof List }.collectMany { e -> e.drop(1) }, bundlesOf(items))
}

// unstaged is the ordinal dirs rs a task stages by reference, each once,
// but those that lie in a dir it stages whole, ds: mre resolves a
// reference among the files of every item the task stages, so such a file
// would otherwise be staged twice.
def unstaged(List rs, List ds) {
    def whole = ds.toSet()
    def seen = [] as Set
    return rs.findAll { r -> !whole.contains(r.parent.parent) && seen.add(r) }
}

def recordsOf(List items) {
    return items.findAll { e -> !(e instanceof Path || e instanceof List) }
}

// sources maps each item a task staged, by the path the task staged it at,
// to where it lies: an outs or element bundle (in/u<i>), the bundle of a
// file staged by reference (refs/<i>), and a split's chunk and its outs
// (chunks/<name>, chunkouts/<name>). Nextflow replaces a stageAs pattern's
// ? with the position, from 1, of the path in the list it stages.
def sources(List us, List rs, List cs, List os) {
    def m = [:]
    us.eachWithIndex { p, i -> m['in/u' + (i + 1)] = p }
    rs.eachWithIndex { p, i -> m['refs/' + (i + 1)] = p.parent.parent }
    cs.each { p -> m['chunks/' + p.name] = p }
    os.each { p -> m['chunkouts/' + p.name] = p }
    return m
}

// referred is the dirs a task wrote, each with the ordinal dir of every
// file it names by reference (mre stage -field-refs, -fork-refs,
// -out-refs: one line of space-separated records, each of the dir's last
// two path components, the path the task staged the item the file lies in
// at, and its ordinal dir there), where the task's sources, src, hold it.
def referred(List ds, String refs, Map src) {
    def by = refs.tokenize(' ').collect { r -> r.split('\t') }.groupBy { f -> f[0] }
    return ds.collect { d ->
        def rs = by[itemPath(d).join('/')]
        rs ? [d] + rs.collect { f ->
            if (src[f[1]] == null) {
                error("${d} names a file in ${f[1]}, which its task did not stage")
            }
            src[f[1]].resolve(f[2])
        } : d
    }
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
// run's call in call.b64, which the task wrote (callScript) or staged
// (callStaged), over the entry's declarations.
def graph(Map meta, String root) {
    return "${code(meta, root)} -call call.b64"
}

// callScript writes the run's call into call.b64 when CALL carries it
// inline, and nothing when the task stages it. The here-document's lines
// start their own lines, where the script's indent does not reach.
def callScript(Map meta) {
    return meta.call instanceof Path ? '' : "cat > call.b64 <<'MRO2NF_CALL'\n${meta.call}\nMRO2NF_CALL"
}

// callStaged is the call file a task stages as call.b64: the one CALL
// staged, or none.
def callStaged(Map meta) {
    return meta.call instanceof Path ? [meta.call] : []
}

// args is what a task builds the instance's args from: what it
// staged, the files it staged by reference, the value records valuesFile
// wrote, its entry fork records, which are base64 and need no quoting, and
// the entry files it staged below entry/, by their indices among the run's.
// The driver ran the instance because its gates hold it enabled; mre must
// agree, and the node must compile to the spec PLAN planned.
def args(Map meta, List forks, List values, List leaves) {
    def cmd = "-digest ${meta.digest} -in in -refs refs"
    if (values) {
        cmd += ' -values values'
    }
    forks.each { f -> cmd += " -fork ${f}" }
    if (leaves) {
        cmd += " -leaves ${shq(leaves.join(','))}"
    }
    return cmd
}

// valuesFile writes the value records a task got, a line each, which are
// base64 after an item's path and need no quoting: as arguments, a
// gather's could pass the kernel's limit on a command line. The
// here-document's lines start their own lines, where the script's indent
// does not reach.
def valuesFile(List values) {
    return values ? "cat > values <<'MRO2NF_VALUES'\n${values.join('\n')}\nMRO2NF_VALUES" : ''
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
    if (on(params.mro2nf_host_in_place)) {
        cmd += ' -host-in-place'
    }
    if (meta.overrides) {
        cmd = "printf '%s\\n' ${shq(meta.overrides)} > overrides.json\n${cmd} -overrides overrides.json"
    }
    return cmd + ' ' + rest
}

// postFlags name where a task that posts its instance's outs writes them,
// one bundle per output, and the fork and partner elements they set, one
// dir per element, or for each with no file leaf and small JSON a value
// record; the files those dirs name by reference; and the gates they set.
def postFlags() {
    return '-fields outs -field-values outs_values -field-refs outs_refs -forks forks -fork-values fork_values -fork-refs fork_refs -gates gates'
}

// resources reads the table a split writes (mre stage -phase split): per
// chunk, by its name, and for the join, the cpus, memory and special its
// job is scheduled with, those its _jobinfo records. The table is a file
// the driver reads, not an eval output, which Nextflow joins line by line
// in time quadratic in the chunks.
def resources(Path table) {
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
    tuple val(meta), val(key), val(forks), val(values), path(ups, stageAs: 'in/u?', arity: '0..*'), path(refs, stageAs: 'refs/?/f/*', arity: '0..*'), val(leaves), path(entry, stageAs: 'entry/?/*', arity: '0..*'), path(cf, stageAs: 'call.b64', arity: '0..*')

    output:
    tuple val(key), path('outs/*', type: 'dir', arity: '0..*'), eval('cat outs_values'), eval('cat outs_refs'), emit: outs
    tuple val(key), path('forks/*/*', type: 'dir', arity: '0..*'), eval('cat fork_values'), eval('cat fork_refs'), emit: forks
    tuple val(key), eval('cat gates'), emit: gates

    script:
    """
    ${callScript(meta)}
    ${valuesFile(values)}
    mkdir -p in refs
    ${mreStage(task, meta, 'main', "${args(meta, forks, values, leaves)} -args args ${postFlags()}")}
    """
}

process STAGE_SPLIT {
    tag "${meta.node} ${key.join('.')}"
    cpus { meta.cpus }
    memory { "${meta.mem_gb} GB" }
    clusterOptions { params.job_resources[meta.special] ?: '' }

    input:
    tuple val(meta), val(key), val(forks), val(values), path(ups, stageAs: 'in/u?', arity: '0..*'), path(refs, stageAs: 'refs/?/f/*', arity: '0..*'), val(leaves), path(entry, stageAs: 'entry/?/*', arity: '0..*'), path(cf, stageAs: 'call.b64', arity: '0..*')

    output:
    tuple val(meta), val(key), path('chunks/chnk*', type: 'dir', arity: '0..*'), path('chunks/join.json'), path('chunks/resources.tsv'), path('chunk_refs'), emit: split

    script:
    """
    ${callScript(meta)}
    ${valuesFile(values)}
    mkdir -p in refs
    ${mreStage(task, meta, 'split', "${args(meta, forks, values, leaves)} -args args -o chunks -out-refs chunk_refs")}
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
    tuple val(meta), val(key), val(forks), val(values), path(ups, stageAs: 'in/u?', arity: '0..*'), path(refs, stageAs: 'refs/?/f/*', arity: '0..*'), val(leaves), path(entry, stageAs: 'entry/?/*', arity: '0..*'), val(name), path(chunk, stageAs: 'chunk/*'), path(cf, stageAs: 'call.b64', arity: '0..*')

    output:
    tuple val(meta), val(key), path('chunkout/*', type: 'dir'), eval('cat chunkout_refs'), emit: chunk

    script:
    """
    ${callScript(meta)}
    ${valuesFile(values)}
    mkdir -p in refs
    ${mreStage(task, meta, 'main', "${args(meta, forks, values, leaves)} -args args -chunk ${shq(chunk)} -o ${shq('chunkout/' + name)} -out-refs chunkout_refs")}
    """
}

process STAGE_JOIN {
    tag "${meta.node} ${key.join('.')}"
    cpus { meta.cpus }
    memory { "${meta.mem_gb} GB" }
    clusterOptions { params.job_resources[meta.special] ?: '' }

    input:
    tuple val(meta), val(key), val(forks), val(values), path(ups, stageAs: 'in/u?', arity: '0..*'), path(refs, stageAs: 'refs/?/f/*', arity: '0..*'), val(leaves), path(entry, stageAs: 'entry/?/*', arity: '0..*'), path(chunks, stageAs: 'chunks/*', arity: '0..*'), path('chunks/join.json'), path(couts, stageAs: 'chunkouts/*', arity: '0..*'), path(cf, stageAs: 'call.b64', arity: '0..*')

    output:
    tuple val(key), path('outs/*', type: 'dir', arity: '0..*'), eval('cat outs_values'), eval('cat outs_refs'), emit: outs
    tuple val(key), path('forks/*/*', type: 'dir', arity: '0..*'), eval('cat fork_values'), eval('cat fork_refs'), emit: forks
    tuple val(key), eval('cat gates'), emit: gates

    script:
    """
    ${callScript(meta)}
    ${valuesFile(values)}
    mkdir -p in refs
    ${mreStage(task, meta, 'join', "${args(meta, forks, values, leaves)} -args args -chunks chunks -chunkouts chunkouts ${postFlags()}")}
    """
}

// locations writes the file mre outs names an unpublished file by: each
// item OUTS staged, and the bundle of each file it staged by reference,
// refs/<i>, and where its producing task's work dir keeps it, which no
// task sees of itself. The here-document's lines start their own lines,
// where the script's indent does not reach.
def locations(ups, List locs, List refLocs) {
    def lines = [ups, locs].transpose().collect { u, l -> "${u}\t${l}\n" } + refLocs.withIndex().collect { l, i -> "refs/${i + 1}\t${l}\n" }
    return "cat > locations <<'MRO2NF_LOCATIONS'\n${lines.join('')}MRO2NF_LOCATIONS"
}

// OUTS resolves the pipeline outs from the stage outs and the entry files
// they read and publishes them with mrp's own post-processing. It runs no
// stage, so it compiles the project's copy of the .mro files it stages.
process OUTS {
    publishDir params.outdir, mode: 'copy', saveAs: { f -> f.startsWith('results/') ? f.substring(8) : f }

    input:
    val meta
    path cf, stageAs: 'call.b64', arity: '0..*'
    path ups, stageAs: 'in/u*', arity: '0..*'
    val locs
    path refs, stageAs: 'refs/?/f/*', arity: '0..*'
    val refLocs
    val values
    tuple val(leaves), path(entry, stageAs: 'entry/?/*', arity: '0..*')
    path 'mro'

    output:
    path 'pipeline_outs.json'
    path 'results/*', hidden: true, arity: '0..*'

    script:
    """
    ${callScript(meta)}
    ${valuesFile(values)}
    mkdir -p in refs
    ${locations(ups, locs, refLocs)}
    ${shq(params.mro2nf_mre)} outs ${graph(meta, 'mro')} -in in -refs refs${values ? ' -values values' : ''} -locations locations${leaves ? ' -leaves ' + shq(leaves.join(',')) : ''} -o results -json pipeline_outs.json
    """
}

// CALL is the run's call, base64, as every task gets it: inline, a value
// its script writes (callScript), when it is at most bound bytes, or else
// a file the driver writes once per run and each task stages (callStaged).
// The call grows with the entry values; inline, a large one would grow
// every task's script, and the -resume key Nextflow hashes from it, while
// a staged file costs each task on an object store a download. collectFile
// names the file's dir by the call and the run's session, which a -resume
// keeps, so a -resume that re-runs PLAN to the same call keeps the -resume
// key of every task staging it.
workflow CALL {
    take:
    call
    bound

    main:
    c = call.branch { v ->
        inline: v.size() <= bound
        staged: true
    }
    out = c.inline.mix(c.staged.collectFile(name: 'call.b64')).first()

    emit:
    out
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

// PUBLISHED is the outs items of a node the pipeline outs read, each keyed
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
// fork records, element items, index tokens]: its ordinal of each of the
// node's roots, the fork records of its entry-fed roots and the fork
// elements of its stage-fed ones, and the index token of its fork of each
// root a zip of the node splits over. A list of positions picks those
// entries of a key (k[at]).

// FORK extends each instance by one of its node's roots: it joins every
// fork of the root under its own ordinals of the root's outer roots, at
// positions outer. A fork is [outer key, own ordinal, record or element
// item, index token]: an entry-fed root's record from PLAN, a stage-fed
// root's element its producer posted (ELEMENTS).
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

// ELEMENTS is the forks of a stage-fed root its producer posted, the items
// of forks/<root>/: per outer root, at its position in at, the producer's
// ordinal of it, or, at -1, where the producer is not forked on it, the
// ordinal the item's name carries before its own. A map key's token
// follows the ordinals after an @; an array fork's is its ordinal.
workflow ELEMENTS {
    take:
    forks
    root
    at
    keyed

    main:
    out = forks.flatMap { kp, ds ->
        ds.findAll { e -> itemParent(e) == root }.collect { e ->
            def name = itemName(e)
            def t = name.tokenize('@')[0].tokenize('.').collect { x -> x as int }
            def named = t.iterator()
            [at.collect { a -> a >= 0 ? kp[a] : named.next() }, t[-1], e, keyed ? name.tokenize('@')[1] : t[-1].toString()]
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
    out = inst.map { k, fs, es, tk -> [k[at], k, fs, es, tk] }.combine(forks.map { kq, ds -> [kq, ds.findAll { e -> itemParent(e) == partner }.collectEntries { e -> [(itemName(e)): e] }] }, by: 0).map { _q, k, fs, es, tk, m ->
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
// they read, from those it disables, which run no task. An instance it
// runs leads with its ordinals, at positions at, of the roots its node's
// first READ joins on, and each READ keys it so for the next, so no read
// adds a step to key it.
workflow ENABLED {
    take:
    gated
    at

    main:
    b = gated.branch { v ->
        skip: v[4]
        run: true
    }

    emit:
    run = b.run.map { k, fs, es, d, _off -> [k[at], k, fs, es, d, []] }
    skip = b.skip
}

// selected is the items of ds of the outputs a reader reads, fields, or
// all of them where fields is null.
def selected(List ds, List fields) {
    return fields == null ? ds : ds.findAll { e -> itemName(e) in fields }
}

// READ adds to each instance the outs items it reads, fields, of one node,
// keyed as ups keys them, as the instance leads with its ordinals of the
// roots they share, and keys it by its ordinals, at positions next, of
// those the node's next READ joins on. A node's outs are keyed by its
// whole key, which a reader sharing every root of it holds, so the reader
// combines them as they come; keying or selecting them per reader would
// cost Nextflow's DAG edges per read.
workflow READ {
    take:
    inst
    ups
    fields
    next

    main:
    out = inst.combine(ups, by: 0).map { _c, k, fs, es, d, us, ds -> [k[next], k, fs, es, d, us + selected(ds, fields)] }

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
    seeds = run.map { v -> [v[1][mine], null] }.unique()
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
// meta GATE planned for it, the value records of its elements and of the
// outs it reads apart from the dirs it stages and the files those name by
// reference, and the call file it stages, if any; it stages no entry file.
workflow INPUT {
    take:
    inst

    main:
    out = inst.map { _c, k, fs, es, d, us -> [d, k, fs, recordsOf(es + us), bundlesOf(es + us), refsOf(es + us), [], [], callStaged(d)] }

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
    out = inst.combine(entry).map { _c, k, fs, es, d, us, ef ->
        def ids = ef.files[d.node + ' ' + (k[at].join('.') ?: '-')] ?: []
        [d, k, fs, recordsOf(es + us), bundlesOf(es + us), refsOf(es + us), ids, ids.collect { x -> ef.leaves[x] }, callStaged(d)]
    }

    emit:
    out
}

// staged is an instance the split process takes, by its key, with what the
// task stages to build its args: [key, fork records, value records, staged
// items, files staged by reference, entry indices, entry files].
def staged(List v) {
    return v.subList(1, 8)
}

// SOURCES is each instance a non-split stage's process takes, input, by
// its key, with its sources, which POSTED resolves the files its post
// names by reference against.
workflow SOURCES {
    take:
    input

    main:
    out = input.map { v -> [v[1], sources(v[4], v[5], [], [])] }

    emit:
    out
}

// JOIN_SOURCES is SOURCES of a split's join, input, whose sources hold the
// split's chunks and their outs too.
workflow JOIN_SOURCES {
    take:
    input

    main:
    out = input.map { v -> [v[1], sources(v[4], v[5], v[8], v[10])] }

    emit:
    out
}

// chunked is each chunk a split wrote, cs, with the ordinal dirs its task
// stages by reference: the instance's, rs, and those of the files its def
// names by reference (mre stage -out-refs, the split's records in table),
// where the split task's sources, us and rs, hold them, but those in the
// instance's items, us, which the chunk's task stages too.
def chunked(List us, List rs, List cs, Path table) {
    return referred(cs, table.text, sources(us, rs, [], [])).collect { e -> [bundlesOf([e])[0], unstaged(rs + refsOf([e]), us)] }
}

// joinRefs are the ordinal dirs a split's join stages by reference: the
// instance's, rs, those each chunk of chunked stages, and those of the
// files each chunk's outs of os, [outs dir, its records], name by reference
// (mre stage -out-refs), where that chunk's task's sources hold them, but
// those in the items, chunks and chunk outs the join stages.
def joinRefs(List us, List rs, List chunked, List os) {
    def by = chunked.collectEntries { c, crs -> [(c.name): [c, crs]] }
    def outs = os.collectMany { o, recs ->
        def c = by[o.name]
        refsOf(referred([o], recs, sources(us, c[1], [], []) + [('chunk/' + o.name): c[0]]))
    }
    return unstaged(rs + chunked.collectMany { e -> e[1] } + outs, us + chunked.collect { e -> e[0] } + os.collect { o -> o[0] })
}

// CHUNKS is a main per chunk a split returned, each scheduled with the
// resources its job gets, from the split's resources table, and staging
// the split's inputs, input, and the files its def names by reference.
workflow CHUNKS {
    take:
    split
    input

    main:
    out = split.map { m, k, cs, _j, res, t -> [k, m, cs, res, t] }.join(input.map { v -> staged(v) }).flatMap { k, m, cs, res, t, fs, vs, us, rs, ids, ef ->
        def r = resources(res)
        chunked(us, rs, cs, t).collect { c, crs -> [m + r[c.name] + [chunks: cs.size()], k, fs, vs, us, crs, ids, ef, c.name, c, callStaged(m)] }
    }

    emit:
    out
}

// JOIN is a split's join over every chunk's outs once all are in, by chunk
// name, not by completion, scheduled with the resources the split gave it,
// and staging the split's inputs, input, and the files its chunks' defs
// and outs name by reference.
workflow JOIN {
    take:
    split
    chunk
    input

    main:
    outs = chunk.map { m, k, o, recs -> [groupKey(k, m.chunks), [o, recs]] }.groupTuple().map { gk, os -> [gk.getGroupTarget(), os.sort(false) { a, b -> a[0].name <=> b[0].name }] }
    none = split.filter { v -> v[2].isEmpty() }.map { v -> [v[1], []] }
    out = split.map { m, k, cs, j, res, t -> [k, m + resources(res).join, cs, j, t] }.join(input.map { v -> staged(v) }).join(outs.mix(none)).map { k, m, cs, j, t, fs, vs, us, rs, ids, ef, os ->
        [m, k, fs, vs, us, joinRefs(us, rs, chunked(us, rs, cs, t), os), ids, ef, cs, j, os.collect { o -> o[0] }, callStaged(m)]
    }

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

// POSTED is a node's outs or forks, each instance's dirs and value records
// as one list of items, a disabled instance standing in with none. A dir
// names a file by reference where its producer wrote it, which POSTED finds
// among the sources of the instance's posting task (SOURCES), by its key.
workflow POSTED {
    take:
    posted
    skip
    sources

    main:
    all = posted.join(sources).map { k, ds, vs, rs, src -> [k, referred(ds, rs, src) + vs.tokenize(' ')] }.mix(skip.map { v -> [v[0], []] })

    emit:
    all
}
