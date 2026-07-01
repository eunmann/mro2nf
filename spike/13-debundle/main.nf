// Spike for issue #13 — de-bundle the data plane.
//
// Proves the chosen "Option E" transport works as idiomatic Nextflow: each stage
// output is a per-output-param channel of tuple(param, sidecar, leaves) where the
// sidecar is a small typed data.json and the leaves are the real files, written
// ONCE and staged by Nextflow as first-class `path` items — no `mre` byte-copy for
// transport. This answers the only real unknown the issue flags: that the four
// nastiest shapes stage correctly, including multi-input non-collision, on both a
// symlink work dir (local/HealthOmics) and a copy work dir (the S3 proxy).
//
// The emit/check helpers (bin/) stand in for `mre`'s sidecar read/write; the point
// of the spike is the Nextflow staging declarations, not the adapter. DSL2 forbids
// reusing a process, so one keyed EMIT produces every output and consumers filter
// by key.

nextflow.enable.dsl = 2

// Produce one output param as (key, param, sidecar, flat leaves). `path("${param}.L*")`
// captures the flat ordinal leaf set as individual path items — written once.
process EMIT {
    tag "$key"
    input:
        tuple val(key), val(param), path(spec)
    output:
        tuple val(key), val(param), path("${param}.sidecar.json"), path("${param}.L*")
    script:
        """
        emit.py ${param} ${spec}
        """
}

// Single-input consumer: sidecar + its leaves stage into a private dir `a/`, so
// markers resolve within that input's own directory.
process CHECK1 {
    tag "$label"
    input:
        tuple val(label), val(pn), path('a/sidecar.json'), path('a/*'), path('expected.json')
    output:
        stdout
    script:
        """
        check.py ${label} expected.json a/sidecar.json a
        """
}

// Multi-input consumer: two inputs whose leaf sets share the SAME ordinal names
// (both params are 'p'). Staging each under its own dir (a/, b/) is what makes
// them non-colliding — the mechanic the issue calls the fiddly bit. This is the
// array<map>/merge shape: N runtime-sized leaf collections in one task.
process CHECK2 {
    tag "$label"
    input:
        val label
        tuple val(p0), path('a/sidecar.json'), path('a/*')
        tuple val(p1), path('b/sidecar.json'), path('b/*')
        path 'expected.json'
    output:
        stdout
    script:
        """
        check.py ${label} expected.json a/sidecar.json a b/sidecar.json b
        """
}

// Split main: each chunk stages its OWN shard leaves (c/) plus the ONE shared
// stage-level leaf set (s/). The shared file is a single object Nextflow stages
// into each chunk — never re-materialized per chunk as the bundle model does.
process CHUNK_MAIN {
    tag "$label"
    input:
        tuple val(label), val(shardContent), val(cp), path('c/sidecar.json'), path('c/*'), val(sk), val(sp), path('s/sidecar.json'), path('s/*'), path('exp_shared.json')
    output:
        stdout
    script:
        """
        check.py ${label}:shared exp_shared.json s/sidecar.json s
        got=\$(cat c/p.L0000)
        [ "\$got" = "${shardContent}" ] && echo "OK[${label}:shard]" || { echo "FAIL[${label}:shard] got=\$got"; exit 1; }
        """
}

// Zero-chunk join: the chunk-outs channel is empty, but JOIN must still run. The
// collected leaves list collapses to [] via ifEmpty and the join runs once.
process ZERO_JOIN {
    tag 'zerojoin'
    input:
        val chunkOuts
    output:
        stdout
    script:
        """
        [ ${chunkOuts.size()} -eq 0 ] && echo 'OK[zerojoin]' || { echo 'FAIL[zerojoin]'; exit 1; }
        """
}

def spec(n) { file("${projectDir}/specs/${n}.json") }
def exp(n) { file("${projectDir}/expected/${n}.json") }

workflow {
    all = EMIT(Channel.of(
        tuple('s1', 'p', spec('map_file_array')),
        tuple('s2', 'p', spec('struct_file_array')),
        tuple('ma', 'p', spec('map_file_array')),
        tuple('mb', 'p', spec('multi_b')),
        tuple('shared', 'p', spec('split_shared')),
        tuple('c0', 'p', spec('chunk0')),
        tuple('c1', 'p', spec('chunk1')),
    ))

    // Shapes 1 & 2 through one CHECK1 call.
    r1 = all.filter { it[0] == 's1' }.map { k, p, s, l -> ['map_file_array', p, s, l, exp('map_file_array')] }
    r2 = all.filter { it[0] == 's2' }.map { k, p, s, l -> ['struct_file_array', p, s, l, exp('struct_file_array')] }
    CHECK1(r1.mix(r2)) | view

    // Multi-input non-collision.
    ma = all.filter { it[0] == 'ma' }.map { k, p, s, l -> tuple(p, s, l) }
    mb = all.filter { it[0] == 'mb' }.map { k, p, s, l -> tuple(p, s, l) }
    CHECK2('multi', ma, mb, exp('multi')) | view

    // Split: one shared stage output combined with each per-chunk shard.
    shared = all.filter { it[0] == 'shared' }
    c0 = all.filter { it[0] == 'c0' }.map { k, p, s, l -> ['chunk0', 'C0', p, s, l] }
    c1 = all.filter { it[0] == 'c1' }.map { k, p, s, l -> ['chunk1', 'C1', p, s, l] }
    CHUNK_MAIN(c0.mix(c1).combine(shared).combine(Channel.value(exp('split_shared')))) | view

    // Zero-chunk join: an empty chunk-outs channel still runs JOIN once.
    ZERO_JOIN(Channel.empty().collect().ifEmpty([])) | view
}
