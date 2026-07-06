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

    // requireBool coerces a resolved disable flag to a boolean, failing loudly on
    // a null or non-boolean value the way mrp does ("disabled is bound to a null
    // value, which is not permitted", core/stage.go). Groovy's `(boolean)` cast
    // would silently map null to false, running a branch mrp would have failed —
    // a silent divergence.
    private static boolean requireBool(Object value, String what) {
        if (!(value instanceof Boolean)) {
            String kind = value == null ? 'null' : value.getClass().getSimpleName()
            throw new IllegalArgumentException(
                "disable flag ${what} resolved to ${kind} (${value}); a disabled binding must be a boolean")
        }
        return (boolean) value
    }

    // disabled reports a DISABLE gate's resolved flag: the `disabled` field of
    // its output bundle's data.json. Used by the run/skip branch of a
    // conditionally-disabled call.
    static boolean disabled(Path bundleDir) {
        requireBool(parseSidecar(bundleDir).get('disabled'), "'disabled'")
    }

    // disabledField reads a disable-gate boolean directly from a source bundle's
    // data.json (jsonFile) by top-level field name — the native alternative to a
    // DISABLE task when the gate's ref resolves to a single top-level field: the
    // pipeline input (self.<field>) or an upstream output (CALL.out.<field>) (#59).
    static boolean disabledField(Path jsonFile, String field) {
        requireBool(((Map) parseJson(jsonFile)).get(field), field)
    }

    // disabledDir is disabledField for a bundle DIRECTORY (its data.json) rather
    // than the data.json file — used by the keyed disable gate, whose per-fork
    // pipeline args arrive as a staged bundle dir (#59).
    static boolean disabledDir(Path bundleDir, String field) {
        requireBool(parseSidecar(bundleDir).get(field), field)
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
    // split source (#99): the split collection in `field` is sliced ONCE here on
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
        elementTriples(jsonFile.text, field, mapMode)
    }

    // forkElementsPa is forkElements for a QUEUE-pipeargs pipeline (#99): pa is a
    // ≤1-item queue that cannot broadcast into N element instances, so each
    // tuple carries the pipeargs bundle (paJson + paLeaves) alongside its
    // element — the fused instance stages `pipeargs` from the tuple instead of a
    // broadcast input. Upstream refs are barred in queue-pipeargs pipelines
    // (nativeScatterable), so the collection is always in pipeargs itself.
    static List forkElementsPa(Path paJson, Object paLeaves, String field, String mapMode) {
        elementTriples(paJson.text, field, mapMode).collect { Object t ->
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
        elementTriples(bundleDir.resolve('data.json').text, field, mapMode).collect { Object t ->
            List tuple = (List) t
            [outerKey, outerKey.toString() + '~' + (String) tuple[0], tuple[1], tuple[2], bundleDir]
        }
    }

    // elementTriples slices `field`'s collection into [key, index, elementB64]
    // per fork — the shared core of forkElements/forkElementsPa/
    // forkElementsKeyed. The collection is sliced ONCE here on the driver and
    // each fork gets ONLY its own pre-sliced element, so no instance re-parses
    // the whole collection (the O(N^2) the per-fork forkbind -index did across
    // N instances). Emits in the SAME order/index the full-fork write uses
    // (array order; map keys sorted by UTF-8 byte value, matching bind.go
    // sortedKeys), so the gather sees identical ordering. Each element is the
    // RAW substring of the source JSON, base64-encoded for shell-safe transport
    // (a string value with shell metacharacters cannot break the command). Raw
    // carriage is load-bearing (#124): a JsonSlurper -> JsonOutput round-trip
    // re-encodes numeric lexemes (1e5 -> 1E+5, -0.0 -> 0.0) and silently
    // OVERFLOWS integers past Long, while the -index path embeds the source
    // bytes verbatim via json.RawMessage — the two paths must produce
    // byte-identical _args (contract 1). An empty/null/wrong-kind source
    // yields the single index -1 sentinel (its instance validates via forkbind
    // -keysonly and feeds the gather the typed empty). Value-only: `field`
    // carries no file leaves (the emitter gates this), so the JSON element is
    // the whole split value — no bundle marker rewrite, unlike a file fork.
    private static List elementTriples(String json, String field, String mapMode) {
        String raw = rawTopField(json, field)
        if (raw == null || raw == 'null') return [['fork_none', -1, '']]

        if (raw.startsWith('[') && mapMode == 'array') {
            List<String> elems = rawArrayElements(raw)
            if (elems.isEmpty()) return [['fork_none', -1, '']]
            return (0..<elems.size()).collect { int i ->
                ['fork_' + Integer.toString(i).padLeft(5, '0'), i, b64(elems[i])]
            }
        }

        if (raw.startsWith('{') && mapMode == 'map') {
            List<List<String>> entries = rawObjectEntries(raw)
            if (entries.isEmpty()) return [['fork_none', -1, '']]
            // Order the keys by UTF-8 byte value, matching Go's sort.Strings
            // (bind.go sortedKeys) EXACTLY — the forkkeys sidecar the fi==0
            // instance writes uses that order, so the element index order here
            // must agree or a map fork would pair values with the wrong keys.
            // (Java's natural String order diverges only for supplementary-plane
            // code points, but matching Go byte order removes the risk entirely.)
            entries.sort { List<String> a, List<String> b -> compareUtf8(a[0], b[0]) }
            return (0..<entries.size()).collect { int i ->
                ['fork_' + Integer.toString(i).padLeft(5, '0'), i, b64(entries[i][1])]
            }
        }

        // Wrong-kind (or an empty collection of the other kind): one sentinel so
        // a single instance fails loudly / yields the typed empty.
        return [['fork_none', -1, '']]
    }

    // b64 base64-encodes a raw JSON substring for shell-safe transport.
    private static String b64(String rawJson) {
        rawJson.getBytes('UTF-8').encodeBase64().toString()
    }

    // rawTopField returns the raw substring of a top-level object field's
    // value, or null when the field is absent. Deliberately scans the WHOLE
    // object instead of stopping at the match: the single O(file) pass then
    // doubles as full-document validation, so a corrupt data.json fails loudly
    // even when the split field itself scans clean. First match wins on a
    // duplicate key (unreachable: data.json is written by Go's json.Marshal,
    // which never emits duplicate keys).
    private static String rawTopField(String json, String field) {
        for (List<String> e : rawObjectEntries(json)) {
            if (e[0] == field) return e[1]
        }
        return null
    }

    // rawObjectEntries scans a JSON object and returns its [decodedKey,
    // rawValueSubstring] entries in source order — one entry per key
    // occurrence, so a duplicate key yields duplicate entries (unreachable in
    // practice: the input is written by Go's json.Marshal, which never emits
    // duplicates). Keys are decoded (JSON string escapes resolved) so they
    // compare and sort exactly like the Go side's decoded keys — never match
    // or sort on the raw substring, where the escaped and literal forms of a
    // key (Go writes `&` as the six chars backslash-u-0-0-2-6) order
    // differently;
    // values stay raw. Malformed input throws — data.json is machine-written,
    // so that is a system bug that must fail loudly.
    private static List<List<String>> rawObjectEntries(String s) {
        List<List<String>> out = []
        int i = skipWs(s, 0)
        expect(s, i, '{' as char)
        i = skipWs(s, i + 1)
        if (charAt(s, i) == ('}' as char)) return out
        while (true) {
            int keyEnd = scanString(s, i)
            String key = decodeKey(s.substring(i, keyEnd))
            i = skipWs(s, keyEnd)
            expect(s, i, ':' as char)
            int vs = skipWs(s, i + 1)
            int ve = scanValue(s, vs)
            out.add([key, s.substring(vs, ve)])
            i = skipWs(s, ve)
            if (charAt(s, i) == (',' as char)) { i = skipWs(s, i + 1); continue }
            expect(s, i, '}' as char)
            return out
        }
    }

    // decodeKey resolves a raw JSON string token (quotes included) to its
    // decoded value. The common escape-free key is a plain substring; only a
    // key containing a backslash pays for a JsonSlurper parse. That parse
    // depends on JsonSlurper accepting a ROOT-LEVEL primitive (a bare JSON
    // string), which the Groovy 3+ parser Nextflow ships does.
    private static String decodeKey(String rawKey) {
        if (!rawKey.contains('\\')) return rawKey.substring(1, rawKey.length() - 1)
        (String) new JsonSlurper().parseText(rawKey)
    }

    // rawArrayElements scans a JSON array and returns each element's raw
    // substring in order.
    private static List<String> rawArrayElements(String s) {
        List<String> out = []
        int i = skipWs(s, 0)
        expect(s, i, '[' as char)
        i = skipWs(s, i + 1)
        if (charAt(s, i) == (']' as char)) return out
        while (true) {
            int ve = scanValue(s, i)
            out.add(s.substring(i, ve))
            i = skipWs(s, ve)
            if (charAt(s, i) == (',' as char)) { i = skipWs(s, i + 1); continue }
            expect(s, i, ']' as char)
            return out
        }
    }

    // scanValue returns the end index (exclusive) of the JSON value starting at
    // i: a string, a depth-tracked (string-aware) object/array, or a
    // number/true/false/null lexeme ending at the next delimiter.
    private static int scanValue(String s, int i) {
        char c = charAt(s, i)
        if (c == ('"' as char)) return scanString(s, i)
        if (c == ('{' as char) || c == ('[' as char)) {
            int depth = 0
            int j = i
            while (j < s.length()) {
                char d = s.charAt(j)
                if (d == ('"' as char)) { j = scanString(s, j); continue }
                if (d == ('{' as char) || d == ('[' as char)) depth++
                else if (d == ('}' as char) || d == (']' as char)) { depth--; if (depth == 0) return j + 1 }
                j++
            }
            throw malformed(s, i)
        }
        int j = i
        while (j < s.length()) {
            char d = s.charAt(j)
            if (d == (',' as char) || d == ('}' as char) || d == (']' as char) || isWs(d)) break
            j++
        }
        if (j == i) throw malformed(s, i)
        return j
    }

    // scanString returns the end index (exclusive) of the JSON string whose
    // opening quote is at i, honoring backslash escapes.
    private static int scanString(String s, int i) {
        expect(s, i, '"' as char)
        int j = i + 1
        while (j < s.length()) {
            char c = s.charAt(j)
            if (c == ('\\' as char)) { j += 2; continue }
            if (c == ('"' as char)) return j + 1
            j++
        }
        throw malformed(s, i)
    }

    private static int skipWs(String s, int i) {
        while (i < s.length() && isWs(s.charAt(i))) i++
        return i
    }

    private static boolean isWs(char c) {
        c == (' ' as char) || c == ('\t' as char) || c == ('\n' as char) || c == ('\r' as char)
    }

    private static char charAt(String s, int i) {
        if (i >= s.length()) throw malformed(s, i)
        s.charAt(i)
    }

    private static void expect(String s, int i, char want) {
        if (charAt(s, i) != want) throw malformed(s, i)
    }

    private static IllegalArgumentException malformed(String s, int i) {
        new IllegalArgumentException('malformed JSON at offset ' + i + ': ' + s.take(80))
    }

    // compareUtf8 orders two strings by unsigned UTF-8 byte value, matching Go's
    // lexical string comparison (sort.Strings) so a driver-side key sort agrees
    // with the Go-side forkkeys order byte-for-byte.
    static int compareUtf8(String a, String b) {
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
