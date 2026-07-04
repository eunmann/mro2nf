// Mro2nf — hand-written helper library shipped verbatim into every generated
// Nextflow project's lib/, which Nextflow auto-adds to the driver classpath.
//
// Every method here runs on the DRIVER (head node) while wiring channels, never
// inside a task, so it changes nothing for AWS Batch / S3 / HealthOmics
// execution: no container or staging impact — the file rides along in the
// HealthOmics definition zip like _assets/ and nulls/ already do.
//
// Constraints:
//   - java.nio.file.Path APIs only (never java.io.File), so an s3:// work dir
//     keeps working — the same rule the emitter documents for its inline reads.
//   - No generated identifiers: anything call-specific stays a method argument.
//
// No package declaration: Nextflow adds lib/ to the classpath in the default
// package, so generated code references `Mro2nf` directly.

import groovy.json.JsonSlurper
import groovy.transform.CompileStatic
import java.nio.file.Path

@CompileStatic
class Mro2nf {
    // parseSidecar reads and parses a bundle directory's data.json sidecar,
    // preserving the filesystem/scheme by operating on the Path (a bare
    // interpolation would drop an s3:// scheme).
    static Map parseSidecar(Path bundleDir) {
        (Map) new JsonSlurper().parseText(bundleDir.resolve('data.json').text)
    }

    // disabled reports a DISABLE gate's resolved flag: the `disabled` field of
    // its output bundle's data.json. Used by the run/skip branch of a
    // conditionally-disabled call.
    static boolean disabled(Path bundleDir) {
        (boolean) parseSidecar(bundleDir).get('disabled')
    }

    // disabledField reads a disable-gate boolean directly from a source bundle's
    // data.json (jsonFile) by top-level field name — the native alternative to a
    // DISABLE task when the gate's ref resolves to a single top-level field: the
    // pipeline input (self.<field>) or an upstream output (CALL.out.<field>) (#59).
    static boolean disabledField(Path jsonFile, String field) {
        (boolean) ((Map) parseJson(jsonFile)).get(field)
    }

    // disabledDir is disabledField for a bundle DIRECTORY (its data.json) rather
    // than the data.json file — used by the keyed disable gate, whose per-fork
    // pipeline args arrive as a staged bundle dir (#59).
    static boolean disabledDir(Path bundleDir, String field) {
        (boolean) parseSidecar(bundleDir).get(field)
    }

    // parseJson parses a JSON file by Path (e.g. a split's joinres.json), keeping
    // the filesystem/scheme intact for an s3:// work dir.
    static Object parseJson(Path jsonFile) {
        new JsonSlurper().parseText(jsonFile.text)
    }

    // chunkRes returns a chunk bundle's requested resources — its data.json
    // `resources` object — read on the driver to size the MAIN process.
    static Object chunkRes(Path chunkDir) {
        parseSidecar(chunkDir).get('resources')
    }

    // asList wraps a lone value in a singleton list and passes a List through: a
    // SPLIT's chunks output is a single bundle for a one-chunk split, a list of
    // bundles otherwise.
    static List asList(Object x) {
        (x instanceof List) ? (List) x : [x]
    }

    // keyedChunks expands one keyed SPLIT output into per-chunk
    // [key, resources, chunkDir] tuples for the keyed MAIN fan-out.
    static List keyedChunks(Object key, Object chunks) {
        asList(chunks).collect { c -> [key, chunkRes((Path) c), c] }
    }

    // forkTuples enumerates a keyed FORKBIND's per-fork bundles from its
    // forknames.json (object-store-safe — never listFiles), tagging each with the
    // composite "<outer>~<fork>" key.
    static List forkTuples(Object outerKey, Path forksDir) {
        ((List) parseJson(forksDir.resolve('forknames.json'))).collect { fn ->
            ["${outerKey}~${fn}".toString(), forksDir.resolve((String) fn)]
        }
    }

    // outerKey strips the innermost "~<fork>" segment of a composite fork key, so
    // nested map results regroup by their outer key (arbitrary nesting depth).
    static String outerKey(String compositeKey) {
        compositeKey.substring(0, compositeKey.lastIndexOf('~' as String))
    }

    // forkCount is the native-map scatter width (#76): the size of the map
    // call's split collection, read from the enclosing pipeline args' data.json
    // on the driver so no FORK task runs. mapMode is the call's STATIC fork
    // kind ('map' or 'array'); a value of the wrong kind counts as ONE fork, so
    // a single instance runs forkbind and fails with the same errNotArray /
    // errNotMap the FORK task gave — one loud task, not size-of-collection
    // failures and never a silent skip. A null (or absent) source and the
    // empty right-kind collection fork zero times; the scatter then runs the
    // keys-only sentinel instance, which validates every binding like the FORK
    // task did and feeds the gather its keys.
    static int forkCount(Path jsonFile, String field, String mapMode) {
        def v = ((Map) parseJson(jsonFile)).get(field)
        if (v == null) return 0
        if (v instanceof List) return mapMode == 'array' ? ((List) v).size() : 1
        if (v instanceof Map) return mapMode == 'map' ? ((Map) v).size() : 1
        return 1
    }

    // forkScatter expands a pipeline-args tuple into one [key, index, data,
    // leaves] tuple per fork of the split collection in `field` — the
    // driver-side replacement for the FORK task's fork_NNNNN enumeration. The
    // key matches the full-fork write's bundle name (cmd/mre fork_%05d,
    // rendered locale-independently), so out bundle names (outs__<key>) sort
    // identically for the gather. An empty/null collection yields ONE sentinel
    // tuple with index -1: its instance runs `forkbind -keysonly`, preserving
    // the FORK task's always-validate behavior and keys output while staying
    // dormant when the enclosing pipeline itself is skipped (no pipeargs item
    // -> no tuples at all).
    static List forkScatter(Path jsonFile, Object leaves, String field, String mapMode) {
        int n = forkCount(jsonFile, field, mapMode)
        if (n == 0) return [['fork_none', -1, jsonFile, leaves]]
        (0..<n).collect { int i ->
            ['fork_' + Integer.toString(i).padLeft(5, '0'), i, jsonFile, leaves]
        }
    }

    // forkScatterRef is forkScatter for an UPSTREAM-ref split source (#99): the
    // fork WIDTH is read from the producer's bundle (refJson), while the scatter
    // tuples still carry the enclosing pipeargs bundle (paJson + paLeaves) — the
    // fused instance stages the producer's whole output separately as an in_<id>
    // broadcast input, which forkbind resolves the per-fork split from. The
    // empty/null-source sentinel behaves exactly as the self-source path.
    static List forkScatterRef(Path refJson, Path paJson, Object paLeaves, String field, String mapMode) {
        int n = forkCount(refJson, field, mapMode)
        if (n == 0) return [['fork_none', -1, paJson, paLeaves]]
        (0..<n).collect { int i ->
            ['fork_' + Integer.toString(i).padLeft(5, '0'), i, paJson, paLeaves]
        }
    }
}
