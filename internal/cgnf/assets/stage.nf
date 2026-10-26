// The stage processes every call-graph node runs through, each node
// including them under its own alias, so a withName: selector names the
// node, and the sub-workflows that wire a node's instances to them. meta
// carries the node's identity, the cache key of its spec (digest), the
// run's call as CALL carries it (call), the launch params its script reads
// (run), the resources it is scheduled with,
// and its own overrides file (overrides), which PLAN resolved; its GPU
// request is the config's, which reads meta.special per task. Each task
// runs one `mre stage` call: the instance's args, which it builds from the
// inputs it staged, its phase's job as mrp runs it, and the post after it
// that the phase has, each step handing the next what it needs in memory.
// A split's chunks and join stage the split's inputs and build its args
// again, so no task outputs args: each links the files they name where
// Nextflow staged them, which an object-store work dir would otherwise
// receive as one copy per fork.

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

// ownOuts maps each outs dir a task posted, ds, by the path the task wrote
// it at (postFlags: -fields outs), to where it lies.
def ownOuts(List ds) {
    return ds.collectEntries { d -> [('outs/' + d.name): d] }
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

// pool names the pool mrp's local job manager grants a job from, its
// --localcores and --localmem, where local, params.mro2nf_local_jobs as the
// task's meta carries it, holds: the cpus and whole GB of memory
// process.resourceLimits clamps the task to, so a job granted the whole
// pool requests no more than its task is given.
def pool(task, Boolean local) {
    if (!local) {
        return ''
    }
    def lim = task.resourceLimits ?: [:]
    if (!lim.cpus || !lim.memory) {
        error('params.mro2nf_local_jobs grants each job from process.resourceLimits, which sets no cpus or memory')
    }
    def mem = lim.memory instanceof nextflow.util.MemoryUnit ? lim.memory : nextflow.util.MemoryUnit.of(lim.memory.toString())
    return " -local-cores ${lim.cpus as int} -local-mem-gb ${Math.max(1L, mem.toGiga())}"
}

// mreStage runs the task's phase through mre stage, under the launch
// params its meta carries (run), which key the task's -resume.
def mreStage(task, Map meta, phase, rest) {
    def run = meta.run
    def cmd = "${shq(run.mre)} stage -phase ${phase} ${graph(meta, '')} -node ${shq(meta.node)} -psid ${shq(run.psid)} -work .${pool(task, run.local_jobs)}"
    if (run.adapters) {
        cmd += " -adapters ${shq(run.adapters)}"
    }
    if (run.mrjob) {
        cmd += " -mrjob ${shq(run.mrjob)}"
    }
    if (run.monitor) {
        cmd += ' -monitor'
    }
    if (run.confine) {
        cmd += ' -confine'
    }
    if (run.s3_mount) {
        cmd += " -s3-mount ${shq(run.s3_mount)}"
    }
    if (run.host_in_place) {
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
// The task emits them as one tuple, posted: [key, outs dirs, outs value
// records, outs refs, fork dirs, fork value records, fork refs, gates]. An
// output a node's readers do not consume still costs Nextflow's DAG an
// edge, so each posting process has the one.
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
    tuple val(key), path('outs/*', type: 'dir', arity: '0..*'), eval('cat outs_values'), eval('cat outs_refs'), path('forks/*/*', type: 'dir', arity: '0..*'), eval('cat fork_values'), eval('cat fork_refs'), eval('cat gates'), emit: posted

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
    tuple val(key), path('outs/*', type: 'dir', arity: '0..*'), eval('cat outs_values'), eval('cat outs_refs'), path('forks/*/*', type: 'dir', arity: '0..*'), eval('cat fork_values'), eval('cat fork_refs'), eval('cat gates'), emit: posted

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

// skeletons are the skeletons of the bundle dirs ds (mre's
// shim.SkeletonFile): their paths and records, none of their files' bytes,
// which OUTS stages in their place.
def skeletons(List ds) {
    return ds.collect { d -> d.resolve('.mre-skeleton.json') }
}

// bundleLocations are where the producing task's work dir keeps the bundle
// of each of the skeletons ss.
def bundleLocations(List ss) {
    return ss.collect { s -> s.parent.toUriString() }
}

// refBundles are the bundles holding the files items name by reference
// that no item holds (refsOf), each once.
def refBundles(List items) {
    return refsOf(items).collect { r -> r.parent.parent }.unique()
}

// published is the published tree the manifest in OUTS's publish dir p
// lists: each entry's source and its path in the tree. A source lies in
// the work dir of the task that wrote it, OUTS's own below p for a file no
// stage task wrote, as Nextflow publishes no other file. A stage chooses
// its files' names, so the driver globs none.
def published(Path p) {
    return new groovy.json.JsonSlurper().parseText(p.resolve('publish.json').text).collect { e -> [e.local ? p.resolve(e.source) : file(e.source, glob: false), e.path] }
}

// OUTS resolves the pipeline outs from the stage outs and the entry files
// they read and lays out what the run publishes with mrp's own
// post-processing, from the skeletons of the outs, none of the bytes of
// the files a stage task wrote: the outs JSON (json) and the manifest of
// the outs tree (manifest, publish/publish.json), each entry where the task
// that wrote it keeps it, which main.nf's workflow outputs publish from
// there. A published file no stage task wrote, an entry file or a host
// path, it copies into publish/external/, which the manifest names. One
// output dir holds both, as each output is an edge of the run's DAG. It
// runs no stage, so it compiles the project's copy of the .mro files it
// stages.
process OUTS {
    input:
    val meta
    path cf, stageAs: 'call.b64', arity: '0..*'
    path ups, stageAs: 'in/u*', arity: '0..*'
    val locs
    path refs, stageAs: 'refs/?', arity: '0..*'
    val refLocs
    val values
    tuple val(leaves), path(entry, stageAs: 'entry/?/*', arity: '0..*')
    path 'mro'

    output:
    path 'pipeline_outs.json', emit: json
    path 'publish', emit: manifest

    script:
    """
    ${callScript(meta)}
    ${valuesFile(values)}
    mkdir -p in refs
    ${locations(ups, locs, refLocs)}
    ${shq(meta.run.mre)} outs ${graph(meta, 'mro')} -in in -refs refs${values ? ' -values values' : ''} -locations locations${leaves ? ' -leaves ' + shq(leaves.join(',')) : ''} -manifest publish/publish.json -json pipeline_outs.json
    """
}

// CALL is the run's call, base64 of its gzip, as every task gets it:
// inline, a value its script writes (callScript), when it is at most bound
// bytes, or else a file the driver writes once per run and each task
// stages (callStaged).
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
    out = records.flatMap { m -> (m[root] ?: []).collect { f -> f + [true] } }

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
//
// From GATE on, an instance is [join key, key, fork records, element items,
// meta, state, additions...]: the ordinals of the roots its next join is on,
// then what it was before GATE with its tokens replaced by its tasks' meta,
// then its state when it was last keyed, then one addition per join since.
// Through GATED the state is whether it is disabled and each addition a gating
// stage's gate set (disabled); from ENABLED on, the state is the outs items it
// has read and each addition a node's outs (readItems). GATE, REGATE, ENABLED
// and REKEY key it again, keeping v.subList(1, 5) and folding the additions
// into the state.

// FORK extends each instance by one of its node's roots: it joins every
// fork of the root under its own ordinals of the root's outer roots, at
// positions outer. A fork is [outer key, own ordinal, record or element
// item, index token, whether a record]: an entry-fed root's record from
// PLAN (ENTRY_ROOT), a stage-fed root's element its producer posted
// (ELEMENTS), or its record from PLAN where the invocation's entry fixes
// its forks.
workflow FORK {
    take:
    inst
    forks
    outer
    zipped

    main:
    out = inst.map { k, fs, es, tk -> [k[outer], k, fs, es, tk] }.combine(forks, by: 0).map { _o, k, fs, es, tk, own, x, t, rec ->
        [k + [own], rec ? fs + [x] : fs, rec ? es : es + [x], zipped ? tk + [t] : tk]
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
            [at.collect { a -> a >= 0 ? kp[a] : named.next() }, t[-1], e, keyed ? name.tokenize('@')[1] : t[-1].toString(), false]
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
// It keys each by its ordinals, at positions next, of the roots of the
// gating stage its node's first GATED joins on.
workflow GATE {
    take:
    inst
    nodes
    meta
    next

    main:
    out = inst.combine(nodes).flatMap { k, fs, es, _tk, ns, c ->
        def r = ns[meta.node]
        representative(k, r) != null ? [] : [[k[next], k, fs, es, planned(c, meta, r), r[1] == '1' || r[2].contains(k.join('.'))]]
    }

    emit:
    out
}

// representative is, of instance key k of a node whose PLAN record is r,
// the key of the instance that runs its job, where the invocation's node is
// not forked on some of the roots k has ordinals of (r[6], positions in
// k): its job is the same at every fork of them, which runs at ordinal 0
// of each and serves the rest. It is null for an instance that runs its own.
def representative(List k, List r) {
    def collapse = r[6] == '-' ? [] : r[6].tokenize(',').collect { x -> x as int }
    if (!collapse.any { x -> k[x] != 0 }) {
        return null
    }
    return (0..<k.size()).collect { x -> x in collapse ? 0 : k[x] }
}

// ALIASES pairs each instance of a node that runs no job of its own, by its
// key, with its representative's, whose posted items serve it (ALIASED).
workflow ALIASES {
    take:
    inst
    nodes
    meta

    main:
    out = inst.combine(nodes).flatMap { k, _fs, _es, _tk, ns, _c ->
        def rep = representative(k, ns[meta.node])
        rep == null ? [] : [[rep, k]]
    }

    emit:
    out
}

// ALIASED is a collapsible node's posted items, by key, each instance's own
// and, for each instance that runs no job of its own, its representative's.
workflow ALIASED {
    take:
    items
    aliases

    main:
    out = items.mix(aliases.combine(items, by: 0).map { _r, k, x -> [k, x] })

    emit:
    out
}

// GATES is a gating stage's posted gates, each instance's as a set, built
// once however many nodes it gates; a disabled instance posts none.
workflow GATES {
    take:
    posted
    skip

    main:
    out = posted.map { v -> [v[0], v[7].tokenize(' ').toSet()] }.mix(skip.map { v -> [v[1], [].toSet()] })

    emit:
    out
}

// disabled is whether instance v, in the wiring's layout from GATE on, is
// disabled: as it was when it was last keyed, its state v[5], or by the gate
// sets of each gating stage it was joined to since, its additions, each
// gate's of gates, [ext, token], naming it by the token and its ordinals of
// the gate's own roots, at positions ext.
def disabled(List v, List gates) {
    def gss = v.drop(6)
    if (gss.size() != gates.size()) {
        error("an instance was joined to ${gss.size()} gating stages since it was keyed, which ${gates.size()} gates name")
    }
    return v[5] || [gss, gates].transpose().any { gs, g -> gs.contains(g[1] + v[1][g[0]].join('.')) }
}

// GATED adds to each instance, which leads with its ordinals of a gating
// stage's roots, the gates that stage's GATES posted there. A run of gates
// joining on the same roots is a combine each: REGATE keys an instance
// again only between gates joining on other roots, and it and ENABLED
// decide it from the gates of each (disabled). A map per gate cost
// CellRanger an edge of Nextflow's DAG per gate.
workflow GATED {
    take:
    gated
    gates

    main:
    out = gated.combine(gates, by: 0)

    emit:
    out
}

// REGATE keys each instance by its ordinals, at positions next, of the
// roots of the gating stage its node's next GATED joins on, with whether
// it is disabled so far, by the gates it was joined to since it was last
// keyed.
workflow REGATE {
    take:
    gated
    gates
    next

    main:
    out = gated.map { v -> [v[1][next]] + v.subList(1, 5) + [disabled(v, gates)] }

    emit:
    out
}

// ENABLED splits the instances the driver runs, which go on to stage what
// they read, from those it disables (disabled, by the gates it was joined
// to since it was last keyed), which run no task and lead with the key the
// gates left them. An instance it runs leads with its ordinals, at
// positions at, of the roots its node's first READ joins on.
workflow ENABLED {
    take:
    gated
    gates
    at

    main:
    b = gated.branch { v ->
        skip: disabled(v, gates)
        run: true
    }

    emit:
    run = b.run.map { v -> [v[1][at]] + v.subList(1, 5) + [[]] }
    skip = b.skip
}

// selected is the items of ds of the outputs a reader reads, fields, or
// all of them where fields is null.
def selected(List ds, List fields) {
    return fields == null ? ds : ds.findAll { e -> itemName(e) in fields }
}

// readItems is the outs items instance v, in the wiring's layout from GATE
// on, read: those it was last keyed with, its state v[5], then, of the outs
// of each node it read since, its additions, the outputs fields gives that
// read.
def readItems(List v, List fields) {
    def dss = v.drop(6)
    if (dss.size() != fields.size()) {
        error("an instance read ${dss.size()} nodes since it was keyed, which ${fields.size()} reads name")
    }
    return v[5] + [dss, fields].transpose().collectMany { ds, f -> selected(ds, f) }
}

// READ adds to each instance, which leads with its ordinals of the roots
// it shares with one node, that node's outs, keyed as ups keys them. A
// node's outs are keyed by its whole key, which a reader sharing every
// root of it holds, so the reader combines them as they come, and a run of
// reads joining on the same roots is a combine each: REKEY keys an
// instance again only between reads joining on other roots, and it and
// INPUT pick the outputs each read reads. Each step per read cost
// CellRanger an edge of Nextflow's DAG per read.
workflow READ {
    take:
    inst
    ups

    main:
    out = inst.combine(ups, by: 0)

    emit:
    out
}

// REKEY keys each instance by its ordinals, at positions next, of the
// roots its node's next READ joins on, with the outs items it read so far,
// of the outputs fields gives each read since it was last keyed.
workflow REKEY {
    take:
    inst
    fields
    next

    main:
    out = inst.map { v -> [v[1][next]] + v.subList(1, 5) + [readItems(v, fields)] }

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

// NEEDS keys, for each enabled instance, by its whole key, the outs of the
// instances of a node it reads that one of the read's patterns gives it,
// once all are in, ordered by the upstream's key: each instance stages only
// the upstream instances its bindings read. A pattern is [limits, mine,
// theirs]: it gives the instances whose ordinals, at positions theirs, are
// the instance's at positions mine, to an instance whose ordinal at each
// limit's position is one of its ordinals (needed). An instance no pattern
// gives anything gathers none.
workflow NEEDS {
    take:
    run
    outs
    patterns
    fields

    main:
    seeds = run.map { v -> [v[1], null] }
    picks = run.flatMap { v -> needed(v[1], patterns).collect { q -> [q, v[1]] } }
    ups = outs.flatMap { ku, ds ->
        def es = selected(ds, fields)
        (0..<patterns.size()).collect { x -> [[x] + ku[patterns[x][2]], [ku, es]] }
    }
    out = seeds.mix(picks.combine(ups, by: 0).map { _q, k, u -> [k, u] }).groupTuple().map { k, us ->
        [k, ordered(us.findAll { u -> u != null }.unique { u -> u[0] }.collectMany { u -> u[1].collect { e -> [u[0], e] } })]
    }

    emit:
    out
}

// needed is, of each pattern whose limits instance key k meets, the key it
// joins k to the upstream's instances by: the pattern's index, then k's
// ordinals at the pattern's positions mine.
def needed(List k, List patterns) {
    return (0..<patterns.size()).findAll { x -> patterns[x][0].every { g -> k[g[0]] in g[1] } }.collect { x -> [x] + k[patterns[x][1]] }
}

// planned is the meta of a node's tasks: the one every task shares, with
// the run's call, then the node's identity and resources and its digest.
// Its PLAN record d gives the cpus and memory its first task is scheduled
// with and the overrides file of its own its jobs resolve theirs under,
// both under the run's overrides.
def planned(Map shared, Map meta, List d) {
    return shared + [node: meta.node, cpus: d[3] as int, mem_gb: d[4] as double, overrides: d[5] == '-' ? '' : d[5], special: meta.special, digest: d[0]]
}

// inputOf is instance v as a stage process takes it, with the meta GATE
// planned for it, the value records of its elements and of the outs it
// read (readItems, of the outputs fields gives each read since it was last
// keyed) apart from the dirs it stages and the files those name by
// reference, the entry files it stages and their indices, and the call
// file it stages, if any.
def inputOf(List v, List fields, List ids, List entries) {
    def items = v[3] + readItems(v, fields)
    return [v[4], v[1], v[2], recordsOf(items), bundlesOf(items), refsOf(items), ids, entries, callStaged(v[4])]
}

// INPUT is each enabled instance as a stage process takes it, staging no
// entry file.
workflow INPUT {
    take:
    inst
    fields

    main:
    out = inst.map { v -> inputOf(v, fields, [], []) }

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
    fields

    main:
    out = inst.combine(entry).map { ve ->
        def v = ve.dropRight(1)
        def ef = ve[-1]
        def ids = ef.files[v[4].node + ' ' + (v[1][at].join('.') ?: '-')] ?: []
        inputOf(v, fields, ids, ids.collect { x -> ef.leaves[x] })
    }

    emit:
    out
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

// SPLITS joins what each instance's split returned, split, to what its
// split task staged, input, both by the instance's key at position 1:
// [key, meta, chunk defs, join def, resources table, chunk refs, then the
// input past its key], which its chunks and its join each read, so the
// join is made once for both.
workflow SPLITS {
    take:
    split
    input

    main:
    out = split.join(input, by: 1)

    emit:
    out
}

// CHUNKS is a main per chunk a split returned, each scheduled with the
// resources its job gets, from the split's resources table, and staging
// the split's inputs and the files its def names by reference.
workflow CHUNKS {
    take:
    splits

    main:
    out = splits.flatMap { k, m, cs, _j, res, t, _d, fs, vs, us, rs, ids, ef, _cf ->
        def r = resources(res)
        chunked(us, rs, cs, t).collect { c, crs -> [m + r[c.name] + [chunks: cs.size()], k, fs, vs, us, crs, ids, ef, c.name, c, callStaged(m)] }
    }

    emit:
    out
}

// chunkOuts is the chunk outs os of the split gk keys, by chunk name, not
// by completion. groupTuple emits a split's chunk outs once they number its
// chunks, and, when every chunk is done, any group that does not; such a
// group fails the run here, where the join would otherwise run short or
// never. Chunk outs that number a multiple of the chunks come as full
// groups, which JOIN's join fails as duplicates. groupBy checks the count
// itself, but in time quadratic in it.
def chunkOuts(gk, List os) {
    if (os.size() != gk.getGroupSize()) {
        error("split ${gk.getGroupTarget()} has ${gk.getGroupSize()} chunks, but its chunk outs came in a group of ${os.size()}")
    }
    return os.sort(false) { a, b -> a[0].name <=> b[0].name }
}

// JOIN is a split's join over every chunk's outs once all are in, scheduled
// with the resources the split gave it, and staging the split's inputs and
// the files its chunks' defs and outs name by reference.
workflow JOIN {
    take:
    splits
    chunk

    main:
    outs = chunk.map { m, k, o, recs -> [groupKey(k, m.chunks), [o, recs]] }.groupTuple(remainder: true).map { gk, os -> [gk.getGroupTarget(), chunkOuts(gk, os)] }
    none = splits.filter { v -> v[2].isEmpty() }.map { v -> [v[0], []] }
    out = splits.join(outs.mix(none), failOnDuplicate: true).map { k, m, cs, j, res, t, _d, fs, vs, us, rs, ids, ef, _cf, os ->
        [m + resources(res).join, k, fs, vs, us, joinRefs(us, rs, chunked(us, rs, cs, t), os), ids, ef, cs, j, os.collect { o -> o[0] }, callStaged(m)]
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

// POSTED is a node's outs, or its forks, each instance's dirs and value
// records as one list of items, a disabled instance standing in with none:
// the dirs, value records and refs at positions at to at + 2 of what its
// task posted. A dir names a file by reference where its producer wrote
// it, which POSTED finds among the sources of the instance's posting task
// (SOURCES), by its key, or, for a fork or partner element naming a file
// of the task's own outs, among those (ownOuts).
workflow POSTED {
    take:
    posted
    skip
    sources
    at

    main:
    all = posted.join(sources).map { v -> [v[0], referred(v[at], v[at + 2], v[-1] + ownOuts(v[1])) + v[at + 1].tokenize(' ')] }.mix(skip.map { v -> [v[1], []] })

    emit:
    all
}
