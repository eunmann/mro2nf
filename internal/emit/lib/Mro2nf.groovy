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
}
