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
import groovy.json.JsonOutput
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

    // forkElements is the O(1)-per-instance native scatter for a VALUE-only
    // split source (#99): the split collection in `field` is parsed ONCE here on
    // the driver and each fork gets ONLY its own pre-sliced element, so no
    // instance re-parses the whole collection (the O(N^2) the per-fork forkbind
    // -index did across N instances). Emits [key, index, elementB64] per fork in
    // the SAME order/index the full-fork write uses (array order; sorted map
    // keys, matching bind.go sortedKeys), so the gather sees identical ordering.
    // The element is base64-encoded JSON so it transports through the task script
    // safely regardless of its contents (a string value with shell metacharacters
    // cannot break the command). An empty/null/wrong-kind source yields the single
    // index -1 sentinel (its instance validates via forkbind -keysonly and feeds
    // the gather the typed empty). Value-only: `field` carries no file leaves
    // (the emitter gates this), so the JSON element is the whole split value —
    // no bundle marker rewrite is needed, unlike a file fork.
    static List forkElements(Path jsonFile, String field, String mapMode) {
        elementTriples((Map) parseJson(jsonFile), field, mapMode)
    }

    // forkElementsPa is forkElements for a QUEUE-pipeargs pipeline (#99): pa is a
    // ≤1-item queue that cannot broadcast into N element instances, so each
    // tuple carries the pipeargs bundle (paJson + paLeaves) alongside its
    // element — the fused instance stages `pipeargs` from the tuple instead of a
    // broadcast input. Upstream refs are barred in queue-pipeargs pipelines
    // (nativeScatterable), so the collection is always in pipeargs itself.
    static List forkElementsPa(Path paJson, Object paLeaves, String field, String mapMode) {
        elementTriples((Map) parseJson(paJson), field, mapMode).collect { Object t ->
            List tuple = (List) t
            [tuple[0], tuple[1], tuple[2], paJson, paLeaves]
        }
    }

    // forkElementsKeyed is the O(1)-per-instance scatter for a NESTED map inside
    // a keyed (map-called) pipeline (#99): for one outer fork it slices that
    // fork's inner collection (in its pipeargs bundle) ONCE on the driver into
    // per-inner-fork elements, so no per-outer-fork FORK_K task resolves the
    // inner collection. Emits [outerKey, "outerKey~innerKey", index, elementB64,
    // bundleDir] per inner fork: the composite key threads fork identity for the
    // regroup (Mro2nf.outerKey), and bundleDir is the outer fork's pipeargs,
    // staged into the fused inner process for its broadcast bindings and the
    // index-0 keys resolve. An empty/null inner collection yields the single
    // "outerKey~fork_none" sentinel (index -1), exactly like forkElements.
    static List forkElementsKeyed(Object outerKey, Path bundleDir, String field, String mapMode) {
        elementTriples(parseSidecar(bundleDir), field, mapMode).collect { Object t ->
            List tuple = (List) t
            [outerKey, outerKey.toString() + '~' + (String) tuple[0], tuple[1], tuple[2], bundleDir]
        }
    }

    // elementTriples slices `field`'s collection into [key, index, elementB64]
    // per fork — the shared core of forkElements/forkElementsPa. The collection
    // is parsed ONCE here on the driver and each fork gets ONLY its own
    // pre-sliced element, so no instance re-parses the whole collection (the
    // O(N^2) the per-fork forkbind -index did across N instances). Emits in the
    // SAME order/index the full-fork write uses (array order; map keys sorted by
    // UTF-8 byte value, matching bind.go sortedKeys), so the gather sees
    // identical ordering. The element is base64-encoded JSON so it transports
    // through the task script safely regardless of contents (a string value with
    // shell metacharacters cannot break the command). An empty/null/wrong-kind
    // source yields the single index -1 sentinel (its instance validates via
    // forkbind -keysonly and feeds the gather the typed empty). Value-only:
    // `field` carries no file leaves (the emitter gates this), so the JSON
    // element is the whole split value — no bundle marker rewrite, unlike a file
    // fork.
    private static List elementTriples(Map data, String field, String mapMode) {
        def v = data.get(field)
        if (v == null) return [['fork_none', -1, '']]

        if (v instanceof List && mapMode == 'array') {
            List l = (List) v
            if (l.isEmpty()) return [['fork_none', -1, '']]
            return (0..<l.size()).collect { int i ->
                ['fork_' + Integer.toString(i).padLeft(5, '0'), i, b64(l[i])]
            }
        }

        if (v instanceof Map && mapMode == 'map') {
            Map m = (Map) v
            if (m.isEmpty()) return [['fork_none', -1, '']]
            List keys = new ArrayList(m.keySet())
            // Order the keys by UTF-8 byte value, matching Go's sort.Strings
            // (bind.go sortedKeys) EXACTLY — the forkkeys sidecar the fi==0
            // instance writes uses that order, so the element index order here
            // must agree or a map fork would pair values with the wrong keys.
            // (Java's natural String order diverges only for supplementary-plane
            // code points, but matching Go byte order removes the risk entirely.)
            keys.sort { Object a, Object b -> compareUtf8((String) a, (String) b) }
            return (0..<keys.size()).collect { int i ->
                ['fork_' + Integer.toString(i).padLeft(5, '0'), i, b64(m[keys[i]])]
            }
        }

        // Wrong-kind (or an empty collection of the other kind): one sentinel so
        // a single instance fails loudly / yields the typed empty.
        return [['fork_none', -1, '']]
    }

    // b64 renders a value as base64-encoded JSON for shell-safe transport.
    private static String b64(Object v) {
        JsonOutput.toJson(v).getBytes('UTF-8').encodeBase64().toString()
    }

    // compareUtf8 orders two strings by unsigned UTF-8 byte value, matching Go's
    // lexical string comparison (sort.Strings) so a driver-side key sort agrees
    // with the Go-side forkkeys order byte-for-byte.
    private static int compareUtf8(String a, String b) {
        byte[] x = a.getBytes('UTF-8')
        byte[] y = b.getBytes('UTF-8')
        int n = Math.min(x.length, y.length)
        for (int i = 0; i < n; i++) {
            int d = (x[i] & 0xff) - (y[i] & 0xff)
            if (d != 0) return d
        }
        return x.length - y.length
    }
}
