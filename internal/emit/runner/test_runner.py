#!/usr/bin/env python3
"""Unit tests for the native stage runner (run_stage.py + martian.py).

Pure-stdlib unittest; discover with
    python3 -m unittest discover -s internal/emit/runner -t internal/emit/runner

Pins the review-batch fixes (SystemExit classification, zero-chunk splits,
scoped -monitor enforcement, Go-escaped RawMessage keys, strict bool flags)
plus the core pure functions ported from the Go shim.
"""

import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import martian  # noqa: E402
import run_stage as rs  # noqa: E402

RUNNER = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                      "run_stage.py")

# A stage module whose behavior is selected via the TMODE env var, so one
# module covers the whole SystemExit matrix.
STAGE_SRC = '''\
import os
import sys

import martian


def split(args):
    mode = os.environ.get("TMODE", "")
    if mode == "exit0":
        sys.exit(0)
    if mode == "none":
        return None
    if mode == "empty":
        return {}
    return {"chunks": [{"i": 1}]}


def main(args, outs):
    mode = os.environ.get("TMODE", "")
    if mode == "assert":
        martian.exit("boom")
    if mode == "exit0":
        outs.note = "mutated"
        sys.exit(0)
    if mode == "exit5":
        sys.exit(5)
    if mode == "exit42":
        sys.exit(42)
    outs.note = "ran"
'''

TYPES_JSON = {
    "structs": {},
    "callables": {
        "TSTAGE": {
            "in": [],
            "out": [
                {"name": "note", "type": "string", "baseType": "string",
                 "arrayDim": 0, "mapDim": 0, "isFile": False},
                {"name": "report", "type": "txt", "baseType": "txt",
                 "arrayDim": 0, "mapDim": 0, "isFile": True},
            ],
        }
    },
}


def _res(**kw):
    return rs.Resources(**kw)


class GoJSONTest(unittest.TestCase):
    """The Go-compatible encoder: MarshalIndent formatting rules."""

    def test_sorted_keys_indent_and_html_escaping(self):
        got = rs.go_json({"b": 1, "a": ["x<y", None, True], "c": {}})
        want = (
            '{\n'
            '  "a": [\n'
            '    "x\\u003cy",\n'
            '    null,\n'
            '    true\n'
            '  ],\n'
            '  "b": 1,\n'
            '  "c": {}\n'
            '}'
        )
        self.assertEqual(got, want)

    def test_float_literals(self):
        # Stage-produced floats keep Python repr (mre passes the Python-
        # written literal through as json.Number).
        self.assertEqual(rs.go_json(42.0), "42.0")
        self.assertEqual(rs.go_json(1.5), "1.5")
        # Go-produced floats (Resources) use encoding/json's format.
        self.assertEqual(rs.go_json(rs.GoFloat(42.0)), "42")
        self.assertEqual(rs.go_json(rs.GoFloat(1.5)), "1.5")
        self.assertEqual(rs.go_json(rs.GoFloat(0.0)), "0")
        self.assertEqual(rs.go_json(rs.GoFloat(1e21)), "1e+21")
        self.assertEqual(rs.go_json(rs.GoFloat(1e-7)), "1e-7")
        self.assertEqual(rs.go_json(rs.GoFloat(1e20)),
                         "100000000000000000000")

    def test_numpy_scalars_coerced(self):
        # A stage that returns numpy values (CellRanger's SUBSAMPLE_READS returns
        # a np.float64) must not leak numpy's repr into the bundle JSON — numpy>=2
        # makes repr(np.float64(x)) == 'np.float64(x)', which Nextflow can't parse.
        # .tolist() (detected via __module__, no numpy import) yields native
        # scalars/lists. Faked here since the unit env has no numpy.
        class _FakeF64(float):
            __module__ = "numpy"

            def tolist(self):
                return float(self)

        class _FakeArr:
            __module__ = "numpy.ndarray"

            def tolist(self):
                return [1, 2]

        self.assertEqual(rs.go_json(_FakeF64(0.5)), "0.5")
        self.assertEqual(rs.go_json({"x": _FakeF64(1.25)}),
                         '{\n  "x": 1.25\n}')
        self.assertEqual(rs.go_json(_FakeArr()), "[\n  1,\n  2\n]")

    def test_go_string_control_and_line_separators(self):
        self.assertEqual(rs.go_json("a\tb\x01 "),
                         '"a\\tb\\u0001\\u2028"')

    def test_pystrings_keys_go_escaped_values_py_escaped(self):
        # Fix 5: Go itself marshals the chunk args map keys (raw UTF-8 +
        # HTML escapes); only the VALUES are RawMessage passthrough
        # (ensure_ascii escapes, insertion order kept).
        defs = [(_res(mem_gb=2.0),
                 {"café": {"z": "é<", "a": 1}})]
        got = rs.go_json(rs.chunk_defs_doc(defs))
        want = (
            '[\n'
            '  {\n'
            '    "args": {\n'
            '      "café": {\n'
            '        "z": "\\u00e9\\u003c",\n'
            '        "a": 1\n'
            '      }\n'
            '    },\n'
            '    "resources": {\n'
            '      "threads": 0,\n'
            '      "mem_gb": 2,\n'
            '      "vmem_gb": 0\n'
            '    }\n'
            '  }\n'
            ']'
        )
        self.assertEqual(got, want)

    def test_resources_struct_order_and_omitempty_special(self):
        got = rs.go_json(rs.resources_struct(
            _res(threads=1.0, mem_gb=2.5, vmem_gb=5.0, special="gpu")))
        want = (
            '{\n'
            '  "threads": 1,\n'
            '  "mem_gb": 2.5,\n'
            '  "vmem_gb": 5,\n'
            '  "special": "gpu"\n'
            '}'
        )
        self.assertEqual(got, want)


class FlagParserTest(unittest.TestCase):
    """cmd/mre/main.go flag-set port."""

    def test_space_and_equals_forms(self):
        fl = rs.parse_flags("main", [
            "-threads", "4", "-memgb=2.5", "-args", "ab", "-o", "outs",
        ])
        self.assertEqual(fl.threads, 4.0)
        self.assertEqual(fl.memgb, 2.5)
        self.assertEqual(fl.args, "ab")
        self.assertEqual(fl.o, "outs")
        self.assertEqual(fl.role, "mainout")  # per-phase default

    def test_bool_strictness(self):
        self.assertTrue(rs.parse_flags("main", ["-monitor"]).monitor)
        self.assertTrue(rs.parse_flags("main", ["-monitor=TRUE"]).monitor)
        self.assertFalse(rs.parse_flags("main", ["-monitor=0"]).monitor)
        for bad in ("yes", "tru", "x", "TrUe"):
            with self.assertRaises(rs.RunnerError):
                rs.parse_flags("main", ["-monitor=%s" % bad])

    def test_unknown_flag_and_missing_value(self):
        with self.assertRaises(rs.RunnerError):
            rs.parse_flags("main", ["-shell", "x"])  # mre-only flag
        with self.assertRaises(rs.RunnerError):
            rs.parse_flags("main", ["-threads"])
        with self.assertRaises(rs.RunnerError):
            rs.parse_flags("main", ["-threads", "abc"])

    def test_stops_at_first_non_flag(self):
        fl = rs.parse_flags("main", ["-threads", "2", "stray", "-memgb", "9"])
        self.assertEqual(fl.threads, 2.0)
        self.assertEqual(fl.memgb, 1.0)  # untouched default


class WalkAndMarkersTest(unittest.TestCase):
    """types.go walk + bundle.go markers on a nested struct/array/map."""

    STRUCTS = {"S": [
        {"name": "f", "type": "file", "baseType": "file",
         "arrayDim": 0, "mapDim": 0, "isFile": True},
        {"name": "n", "type": "int", "baseType": "int",
         "arrayDim": 0, "mapDim": 0, "isFile": False},
    ]}
    PARAMS = [
        {"name": "arr", "type": "file[]", "baseType": "file",
         "arrayDim": 1, "mapDim": 0, "isFile": True},
        {"name": "st", "type": "S", "baseType": "S",
         "arrayDim": 0, "mapDim": 0, "isFile": True},
        {"name": "m", "type": "map<file>", "baseType": "file",
         "arrayDim": 0, "mapDim": 1, "isFile": True},
    ]

    def test_write_and_read_bundle_round_trip(self):
        tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, tmp, True)
        src = {}
        for name in ("a", "b", "c", "d"):
            p = os.path.join(tmp, name + ".txt")
            with open(p, "w") as fh:
                fh.write(name)
            src[name] = p
        payload = {
            "arr": [src["a"], None],
            "st": {"f": src["b"], "n": 7},
            "m": {"zz": src["c"], "aa": src["d"]},
            "passthru": src["a"],  # no matching param: untouched
        }
        bdir = os.path.join(tmp, "bundle")
        rs.write_bundle(bdir, payload, self.PARAMS, self.STRUCTS, None)
        with open(os.path.join(bdir, "data.json")) as fh:
            data = json.load(fh)
        # Ordinals follow param order, then sorted map keys (aa before zz); each
        # leaf keeps its source basename (#284).
        self.assertEqual(data["arr"], ["@mre:file:f/L0000/a.txt", None])
        self.assertEqual(data["st"], {"f": "@mre:file:f/L0001/b.txt", "n": 7})
        self.assertEqual(data["m"], {"aa": "@mre:file:f/L0002/d.txt",
                                     "zz": "@mre:file:f/L0003/c.txt"})
        self.assertEqual(data["passthru"], src["a"])
        with open(os.path.join(bdir, "f", "L0002", "d.txt")) as fh:
            self.assertEqual(fh.read(), "d")
        # read_bundle resolves markers back to absolute staged paths.
        resolved = rs.read_bundle(bdir)
        self.assertEqual(resolved["arr"][0],
                         os.path.join(os.path.abspath(bdir), "f", "L0000", "a.txt"))

    def test_shared_basename_and_suffixed_dir(self):
        # Leaves sharing a basename stay apart in their ordinal dirs, and a
        # directory spelled with a trailing "/" or "/." keeps its own name.
        tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, tmp, True)
        leaves = []
        for sample in ("sampleA", "sampleB"):
            os.mkdir(os.path.join(tmp, sample))
            p = os.path.join(tmp, sample, "reads.txt")
            with open(p, "w") as fh:
                fh.write(sample)
            leaves.append(p)
        d = os.path.join(tmp, "outdir")
        os.mkdir(d)
        leaves += [d + "/", d + "/."]
        bdir = os.path.join(tmp, "bundle")
        rs.write_bundle(bdir, {"arr": leaves}, self.PARAMS, self.STRUCTS, None)
        with open(os.path.join(bdir, "data.json")) as fh:
            data = json.load(fh)
        self.assertEqual(data["arr"], [
            "@mre:file:f/L0000/reads.txt", "@mre:file:f/L0001/reads.txt",
            "@mre:dir:f/L0002/outdir", "@mre:dir:f/L0003/outdir"])
        with open(os.path.join(bdir, "f", "L0001", "reads.txt")) as fh:
            self.assertEqual(fh.read(), "sampleB")

    def test_marker_shaped_strings_round_trip(self):
        # #328: a string spelled like a transport marker comes back as the
        # same string wherever it sits, exactly as mrp passes it on; the rels
        # name the bundle's real leaf, so resolving one would hand a stage
        # another file's path in place of its string.
        tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, tmp, True)
        made = os.path.join(tmp, "made.txt")
        with open(made, "w") as fh:
            fh.write("made")
        params = [
            {"name": "made", "baseType": "txt", "arrayDim": 0, "mapDim": 0,
             "isFile": True},
            {"name": "fake", "baseType": "txt", "arrayDim": 0, "mapDim": 0,
             "isFile": True},
            {"name": "s", "baseType": "string", "arrayDim": 0, "mapDim": 0},
            {"name": "arr", "baseType": "string", "arrayDim": 1, "mapDim": 0},
            {"name": "m", "baseType": "map", "arrayDim": 0, "mapDim": 0},
        ]
        strs = ["@mre:file:f/L0000/made.txt", "@mre:dir:f/L0000/made.txt",
                "@mre:str:@mre:file:f/L0000/made.txt", "@mre:",
                "@mre:file:../elsewhere", "plain"]
        payload = {
            "made": made,
            "fake": "@mre:file:f/L0000/made.txt",
            "s": "@mre:file:f/L0000/made.txt",
            "arr": strs,
            "m": {"k": "@mre:dir:f/L0000/made.txt", "nested": strs},
            "extra": "@mre:file:f/L0000/made.txt",
        }
        bdir = os.path.join(tmp, "bundle")
        rs.write_bundle(bdir, payload, params, {}, None)
        want = dict(payload)
        want["made"] = os.path.join(os.path.abspath(bdir), "f", "L0000", "made.txt")
        self.assertEqual(rs.read_bundle(bdir), want)

    def test_read_bundle_rejects_malformed_markers(self):
        # #328: mre writes a marker only as f/L<ordinal>/<basename> inside
        # the bundle and escapes every other reserved string, so any other
        # reserved-prefix shape is a corrupt or forged bundle.
        bad = ["@mre:file:../elsewhere", "@mre:dir:/abs/path",
               "@mre:file:f/../../elsewhere", "@mre:file:f/L12/a.txt",
               "@mre:file:g/L0000/a.txt", "@mre:file:f/L0000",
               "@mre:file:f/L0000.txt", "@mre:file:f/L0000/",
               "@mre:file:f/L0000/.", "@mre:file:f/L0000/..",
               "@mre:file:f/L0000/../../x", "@mre:file:f/L0000/sub/a.txt",
               "@mre:file:f/L0000/a\x00b", "@mre:file:f/L0000a/b",
               "@mre:other:x", "@mre:"]
        for s in bad:
            with self.subTest(s=s):
                tmp = tempfile.mkdtemp()
                self.addCleanup(shutil.rmtree, tmp, True)
                with open(os.path.join(tmp, "data.json"), "w") as fh:
                    json.dump({"v": [s]}, fh)
                with self.assertRaises(rs.RunnerError):
                    rs.read_bundle(tmp)

    def test_missing_and_empty_leaves_kept(self):
        tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, tmp, True)
        payload = {"arr": [os.path.join(tmp, "nope"), ""]}
        bdir = os.path.join(tmp, "bundle")
        rs.write_bundle(bdir, payload, self.PARAMS, self.STRUCTS, None)
        with open(os.path.join(bdir, "data.json")) as fh:
            data = json.load(fh)
        self.assertEqual(data["arr"], [os.path.join(tmp, "nope"), ""])

    def test_dir_leaf_gets_dir_marker(self):
        tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, tmp, True)
        d = os.path.join(tmp, "adir")
        os.mkdir(d)
        with open(os.path.join(d, "x"), "w") as fh:
            fh.write("x")
        bdir = os.path.join(tmp, "bundle")
        rs.write_bundle(bdir, {"arr": [d]}, self.PARAMS, self.STRUCTS, None)
        with open(os.path.join(bdir, "data.json")) as fh:
            data = json.load(fh)
        self.assertEqual(data["arr"], ["@mre:dir:f/L0000/adir"])
        with open(os.path.join(bdir, "f", "L0000", "adir", "x")) as fh:
            self.assertEqual(fh.read(), "x")


class CopyTreeTest(unittest.TestCase):
    """copy_tree's symlink handling, matching copyTree's (bundle.go)."""

    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.tmp, True)

    def test_symlink_cycle_fails(self):
        # out/sub/loop -> out: recursing into loop re-enters an ancestor, which
        # would otherwise recurse until ENAMETOOLONG.
        root = os.path.join(self.tmp, "out")
        os.makedirs(os.path.join(root, "sub"))
        os.symlink(root, os.path.join(root, "sub", "loop"))
        with self.assertRaises(rs.RunnerError) as ctx:
            rs.copy_tree(root, os.path.join(self.tmp, "copy"), None)
        self.assertIn("directory symlink cycle", str(ctx.exception))

    def test_dangling_symlink_in_dir_skipped(self):
        src = os.path.join(self.tmp, "out")
        os.mkdir(src)
        with open(os.path.join(src, "real.txt"), "w") as fh:
            fh.write("keep")
        os.symlink(os.path.join(self.tmp, "gone"), os.path.join(src, "broken"))
        dst = os.path.join(self.tmp, "copy")
        rs.copy_tree(src, dst, None)
        with open(os.path.join(dst, "real.txt")) as fh:
            self.assertEqual(fh.read(), "keep")
        self.assertFalse(os.path.lexists(os.path.join(dst, "broken")))

    def test_looping_symlink_in_dir_fails(self):
        # ELOOP is not a dangling link: resolveSymlink fails it, so must we.
        src = os.path.join(self.tmp, "out")
        os.mkdir(src)
        os.symlink("a", os.path.join(src, "a"))
        with self.assertRaises(rs.RunnerError) as ctx:
            rs.copy_tree(src, os.path.join(self.tmp, "copy"), None)
        self.assertIn("resolve symlink", str(ctx.exception))

    def test_looping_symlink_leaf_fails(self):
        leaf = os.path.join(self.tmp, "loop.txt")
        os.symlink(leaf, leaf)
        params = [{"name": "out", "baseType": "txt", "arrayDim": 0,
                   "mapDim": 0, "isFile": True}]
        bundle = os.path.join(self.tmp, "bundle")
        with self.assertRaises(rs.RunnerError):
            rs.mark_files(bundle, {"out": leaf}, params, {}, None)


class ConfinementTest(unittest.TestCase):
    """#332: copy_tree's confinement of a stage's outputs (ports
    internal/shim/confine.go)."""

    PARAMS = [{"name": "out", "baseType": "txt", "arrayDim": 0,
               "mapDim": 0, "isFile": True}]

    def setUp(self):
        root = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, root, True)
        self.work = os.path.join(root, "work")
        self.files = os.path.join(self.work, "main", "files")
        self.host = os.path.join(root, "host", "outside.txt")
        self.host_dir = os.path.join(root, "host", "refdir")
        upstream = os.path.join(root, "upstream")
        staged = os.path.join(self.work, "args", "f")
        up_dir = os.path.join(upstream, "L0001", "dir")
        for d in (self.files, staged, self.host_dir,
                  os.path.join(upstream, "L0000"), up_dir):
            os.makedirs(d)
        self._write(self.host, "host")
        self._write(os.path.join(self.host_dir, "x.txt"), "host dir")
        self._write(os.path.join(upstream, "L0000", "real.txt"), "upstream")
        self._write(os.path.join(up_dir, "y.txt"), "upstream dir")
        os.symlink(self.host, os.path.join(up_dir, "host"))
        os.symlink(os.path.join(upstream, "L0000"), os.path.join(staged, "L0000"))
        os.symlink(os.path.join(upstream, "L0001"), os.path.join(staged, "L0001"))
        self.staged_file = os.path.join(staged, "L0000", "real.txt")
        self.staged_dir = os.path.join(staged, "L0001", "dir")

    @staticmethod
    def _write(path, content):
        with open(path, "w") as fh:
            fh.write(content)

    def _mark(self, value, strict):
        bundle = tempfile.mkdtemp(dir=self.work)
        conf = rs.Confinement(self.work, "main", strict)
        marked = rs.mark_files(bundle, {"out": value}, self.PARAMS, {}, conf)
        rel, _, ok = rs.cut_marker(marked["out"])
        self.assertTrue(ok, marked["out"])
        return os.path.join(bundle, rel)

    def _out_of_tree_cases(self):
        """(value, rel below the leaf, host file it must hold) for each
        out-of-tree output shape."""
        own_dir = os.path.join(self.files, "own_dir")
        os.mkdir(own_dir)
        os.symlink(self.host, os.path.join(own_dir, "child"))
        os.symlink(self.host, os.path.join(self.files, "to_host"))
        os.symlink(self.host_dir, os.path.join(self.files, "to_host_dir"))
        return [(os.path.join(self.files, "to_host"), "", self.host),
                (os.path.join(self.files, "to_host_dir"), "x.txt",
                 os.path.join(self.host_dir, "x.txt")),
                (self.host, "", self.host),
                (own_dir, "child", self.host)]

    def test_strict_refuses_out_of_tree_targets(self):
        host_root = os.path.realpath(os.path.dirname(self.host))
        for value, _, _ in self._out_of_tree_cases():
            with self.subTest(value=value):
                with self.assertRaises(rs.RunnerError) as ctx:
                    self._mark(value, True)
                msg = str(ctx.exception)
                self.assertIn("outside the task work dir", msg)
                self.assertIn("param out", msg)
                self.assertIn(host_root, msg)

    def test_local_copies_out_of_tree_targets(self):
        for value, rel, host in self._out_of_tree_cases():
            with self.subTest(value=value):
                leaf = self._mark(value, False)
                staged = os.path.join(leaf, rel) if rel else leaf
                with open(staged) as fh, open(host) as want:
                    self.assertEqual(fh.read(), want.read())
                self.assertFalse(os.path.samefile(staged, host),
                                 "%s must be a copy, not a hard link" % staged)

    def test_admits_own_and_staged_files(self):
        own = os.path.join(self.files, "own.txt")
        self._write(own, "own")
        rel_fwd = os.path.join(self.files, "rel_fwd")
        os.symlink(os.path.join("..", "..", "args", "f", "L0000", "real.txt"), rel_fwd)
        cases = [(own, "", own), (self.staged_file, "", self.staged_file),
                 (rel_fwd, "", self.staged_file),
                 (self.staged_dir, "y.txt",
                  os.path.join(self.staged_dir, "y.txt"))]
        for strict in (True, False):
            for value, rel, src in cases:
                with self.subTest(value=value, strict=strict):
                    leaf = self._mark(value, strict)
                    staged = os.path.join(leaf, rel) if rel else leaf
                    self.assertTrue(os.path.samefile(staged, src),
                                    "%s must hard-link %s" % (staged, src))


class CoercionTest(unittest.TestCase):
    """types.go CoerceScalars port."""

    STRUCTS = {"S": [{"name": "k", "baseType": "int",
                      "arrayDim": 0, "mapDim": 0, "isFile": False}]}

    def _one(self, base, v, array_dim=0, map_dim=0):
        params = [{"name": "x", "baseType": base,
                   "arrayDim": array_dim, "mapDim": map_dim}]
        return rs.coerce_scalars(self.STRUCTS, params, {"x": v})["x"]

    def test_whole_float_to_int(self):
        got = self._one("int", 5.0)
        self.assertEqual(got, 5)
        self.assertIsInstance(got, int)

    def test_non_int_param_untouched(self):
        got = self._one("float", 5.0)
        self.assertIsInstance(got, float)

    def test_int64_bounds(self):
        # Upper bound strict: float64(MaxInt64) rounds up to 2^63.
        self.assertIsInstance(self._one("int", float(2 ** 63)), float)
        self.assertIsInstance(self._one("int", 1e20), float)  # > 2^63
        self.assertEqual(self._one("int", float(-(2 ** 63))), -(2 ** 63))
        self.assertIsInstance(self._one("int", 1e18), int)  # in range

    def test_bool_and_fraction_untouched(self):
        self.assertIs(self._one("int", True), True)
        self.assertEqual(self._one("int", 1.5), 1.5)

    def test_nested_array_map_struct(self):
        params = [
            {"name": "a", "baseType": "int", "arrayDim": 1, "mapDim": 0},
            {"name": "m", "baseType": "int", "arrayDim": 0, "mapDim": 1},
            {"name": "s", "baseType": "S", "arrayDim": 0, "mapDim": 0},
        ]
        vals = {"a": [1.0, 2.5], "m": {"k": 3.0}, "s": {"k": 4.0}}
        got = rs.coerce_scalars(self.STRUCTS, params, vals)
        self.assertEqual(got["a"], [1, 2.5])
        self.assertEqual(got["m"], {"k": 3})
        self.assertEqual(got["s"], {"k": 4})
        self.assertIsInstance(got["a"][0], int)


class ResourcesTest(unittest.TestCase):
    """meta.go resolveResources/parseResources port."""

    def test_negative_sentinels_resolve_to_magnitude(self):
        eff = rs.resolve_resources(_res(), _res(threads=-4.0, mem_gb=-8.0))
        self.assertEqual(eff.threads, 4.0)
        self.assertEqual(eff.mem_gb, 8.0)

    def test_vmem_floor_is_mem_plus_headroom(self):
        eff = rs.resolve_resources(_res(), _res(mem_gb=4.0))
        self.assertEqual(eff.vmem_gb, 7.0)
        eff = rs.resolve_resources(_res(), _res(mem_gb=4.0, vmem_gb=0.5))
        self.assertEqual(eff.vmem_gb, 4.0)  # below memory: raised to it
        eff = rs.resolve_resources(_res(), _res(mem_gb=4.0, vmem_gb=9.0))
        self.assertEqual(eff.vmem_gb, 9.0)

    def test_memory_rounds_up_to_whole_mib(self):
        eff = rs.resolve_resources(_res(), _res(mem_gb=0.30000001192092896))
        self.assertEqual(eff.mem_gb, 308 / 1024)
        self.assertEqual(rs._resource_num(eff.mem_gb), 0.301)
        self.assertEqual(rs._resource_num(eff.vmem_gb), 3.3)
        self.assertEqual(rs._resource_num(1e6), 1000000)
        self.assertEqual(rs.resolve_resources(_res(), _res()).mem_gb, 1.0)

    def test_static_special_unless_chunk_sets_one(self):
        fl = rs.parse_flags("main", ["-special", "gpu:2"])
        self.assertEqual(rs.resolve_resources(
            _res(), rs.cli_resources(fl)).special, "gpu:2")
        self.assertEqual(rs.resolve_resources(
            _res(special="chunk"), rs.cli_resources(fl)).special, "chunk")

    def test_threads_round_up_to_centicores(self):
        eff = rs.resolve_resources(_res(), _res(threads=0.3400000035762787))
        self.assertEqual(eff.threads, 0.35)
        self.assertEqual(rs.resolve_resources(_res(), _res()).threads, 1.0)
        fl = rs.parse_flags("main", ["-threads", "1", "-overridethreads", "0.5"])
        eff = rs.resolve_resources(
            rs.overridden(fl, _res(threads=4.0)), rs.cli_resources(fl))
        self.assertEqual(eff.threads, 0.5)

    def test_thread_envs_override_inherited(self):
        saved = {k: os.environ.get(k) for k in rs.THREAD_ENVS}
        try:
            os.environ["OMP_NUM_THREADS"] = "96"
            rs.apply_thread_envs(1.5)
            self.assertEqual({os.environ[k] for k in rs.THREAD_ENVS}, {"2"})
        finally:
            for k, v in saved.items():
                if v is None:
                    os.environ.pop(k, None)
                else:
                    os.environ[k] = v

    def test_override_beats_chunk_request(self):
        fl = rs.parse_flags("main", ["-memgb", "1.5", "-overridememgb", "3"])
        eff = rs.resolve_resources(
            rs.overridden(fl, _res(mem_gb=0.5)), rs.cli_resources(fl))
        self.assertEqual((eff.mem_gb, eff.vmem_gb), (3.0, 6.0))

    def test_override_beats_join_request(self):
        tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, tmp, True)
        path = os.path.join(tmp, "joinres.json")
        with open(path, "w", encoding="utf-8") as fh:
            json.dump({"threads": 2, "mem_gb": 0.5}, fh)
        fl = rs.parse_flags(
            "join", ["-joinres", path, "-overridememgb", "4"])
        req = rs.join_request(fl)
        self.assertEqual((req.threads, req.mem_gb), (2.0, 4.0))
        fl = rs.parse_flags("join", ["-joinres", path])
        self.assertEqual(rs.join_request(fl).mem_gb, 0.5)

    def test_retry_monitor_limits_follow_allocation(self):
        eff = rs.resolve_resources(_res(), _res(mem_gb=0.5))
        first = rs.parse_flags("main", ["-allocbytes", str(1 << 30)])
        self.assertEqual(rs.monitor_limits(eff, first).mem_gb, 0.5)
        retry = rs.parse_flags(
            "main", ["-attempt", "2", "-allocbytes", str(2 << 30)])
        lim = rs.monitor_limits(eff, retry)
        self.assertEqual((lim.mem_gb, lim.vmem_gb), (2.0, 5.0))

    def test_chunk_overrides_win(self):
        eff = rs.resolve_resources(
            _res(mem_gb=16.0, special="gpu"),
            _res(threads=2.0, mem_gb=4.0),
        )
        self.assertEqual(eff.mem_gb, 16.0)
        self.assertEqual(eff.threads, 2.0)
        self.assertEqual(eff.special, "gpu")

    def test_split_resources_separates_dunder_keys(self):
        res, args = rs.split_resources(
            {"__mem_gb": 2, "__threads": 3.0, "__special": "gpu",
             "__vmem_gb": "bad", "x": 1})
        self.assertEqual(res.mem_gb, 2.0)
        self.assertEqual(res.threads, 3.0)
        self.assertEqual(res.vmem_gb, 0.0)  # non-numeric -> 0 (asFloat)
        self.assertEqual(res.special, "gpu")
        self.assertEqual(args, {"x": 1})


class SkeletonTest(unittest.TestCase):
    """meta.go writeSkeletonOuts / types.go OutFilename port."""

    STRUCTS = {"S": [{"name": "f", "baseType": "file",
                      "arrayDim": 0, "mapDim": 0, "isFile": True}]}

    def test_shapes(self):
        params = [
            {"name": "arr", "baseType": "txt", "arrayDim": 1, "mapDim": 0,
             "isFile": True},
            {"name": "m", "baseType": "txt", "arrayDim": 0, "mapDim": 1,
             "isFile": True},
            {"name": "st", "baseType": "S", "arrayDim": 0, "mapDim": 0,
             "isFile": True},
            {"name": "rep", "baseType": "txt", "arrayDim": 0, "mapDim": 0,
             "isFile": True},
            {"name": "p", "baseType": "path", "arrayDim": 0, "mapDim": 0,
             "isFile": True},
            {"name": "named", "baseType": "csv", "arrayDim": 0, "mapDim": 0,
             "isFile": True, "outName": "custom.bin"},
            {"name": "n", "baseType": "int", "arrayDim": 0, "mapDim": 0,
             "isFile": False},
        ]
        got = rs.skeleton_outs(self.STRUCTS, params, "/w/files")
        self.assertEqual(got["arr"], [])
        self.assertEqual(got["m"], {})
        self.assertIsNone(got["st"])
        self.assertEqual(got["rep"], "/w/files/rep.txt")
        self.assertEqual(got["p"], "/w/files/p")
        self.assertEqual(got["named"], "/w/files/custom.bin")
        self.assertIsNone(got["n"])


class ValidateOutputsTest(unittest.TestCase):
    """types/validate.go ValidateOutputs port (mrp output validation, #274)."""

    STRUCTS = {"S": [
        {"name": "a", "baseType": "int", "arrayDim": 0, "mapDim": 0, "isFile": False},
        {"name": "b", "baseType": "int", "arrayDim": 0, "mapDim": 0, "isFile": False},
    ]}

    @staticmethod
    def _param(name, base, array_dim=0, map_dim=0, is_file=False):
        return {"name": name, "type": base, "baseType": base,
                "arrayDim": array_dim, "mapDim": map_dim, "isFile": is_file}

    def test_valid(self):
        for param, outs in [
            (self._param("s", "S"), {"s": {"a": 1, "b": 2, "x": 3}}),
            (self._param("s", "S"), {"s": None}),
            (self._param("s", "S"), {"s": {"a": None, "b": 2}}),
            (self._param("f", "float"), {"f": 3}),
            (self._param("m", "int", map_dim=2), {"m": {"k": [1, 2]}}),
            (self._param("m", "int", map_dim=1), {"m": {"a/b": 1}}),
            (self._param("f", "txt", is_file=True), {"f": 5}),
        ]:
            rs.validate_outputs(self.STRUCTS, [param], outs, "UP")

    def test_invalid(self):
        for param, outs, want in [
            (self._param("s", "S"), {"s": {"a": 1}},
             "Expected S output value 's' missing key: b"),
            (self._param("s", "S"), {}, "Missing output value 's'"),
            (self._param("n", "int"), {"n": 2.0}, "output value 'n'"),
            (self._param("n", "int"), {"n": True}, "output value 'n'"),
            (self._param("n", "int"), {"n": 2 ** 63}, "output value 'n'"),
            (self._param("x", "string"), {"x": 3}, "output value 'x'"),
            (self._param("ss", "S", array_dim=1), {"ss": [{"a": 1, "b": 2}, {"a": 1}]},
             "element 1: missing key: b"),
            (self._param("m", "S", map_dim=1), {"m": {"k": {"b": 1}}},
             "key k: missing key: a"),
            (self._param("m", "file", map_dim=1, is_file=True), {"m": {"a/b": "x"}},
             "key a/b"),
            (self._param("m", "map"), {"m": [1]}, "output value 'm'"),
        ]:
            with self.assertRaises(rs.RunnerError) as cm:
                rs.validate_outputs(self.STRUCTS, [param], outs, "UP")
            self.assertIn(want, str(cm.exception))


class StageDefsTest(unittest.TestCase):
    """meta.go readStageDefs port + the falsy-return adapter-path parity."""

    def test_none_and_empty_fail_like_the_adapter_path(self):
        # End-to-end mre fails on a falsy split return (the vendor shell
        # serializes it as "" via `obj or ""`); the runner mirrors that.
        for result in (None, {}):
            with self.assertRaises(rs.RunnerError):
                rs.parse_stage_defs(result)

    def test_explicit_zero_chunk_split_is_valid(self):
        defs, join = rs.parse_stage_defs({"chunks": []})
        self.assertEqual(defs, [])
        self.assertEqual((join.threads, join.mem_gb, join.vmem_gb,
                          join.special), (0.0, 0.0, 0.0, ""))

    def test_non_object_rejected(self):
        for bad in ([], [1], "x", 3):
            if bad == []:  # falsy list: Go would fail on the shell's ""
                with self.assertRaises(rs.RunnerError):
                    rs.parse_stage_defs(bad)
                continue
            with self.assertRaises(rs.RunnerError):
                rs.parse_stage_defs(bad)

    def test_join_override_extracted(self):
        defs, join = rs.parse_stage_defs(
            {"chunks": [{"__mem_gb": 2, "x": 1}],
             "join": {"__mem_gb": 5, "__special": "gpu"}})
        self.assertEqual(len(defs), 1)
        self.assertEqual(defs[0][0].mem_gb, 2.0)
        self.assertEqual(defs[0][1], {"x": 1})
        self.assertEqual(join.mem_gb, 5.0)
        self.assertEqual(join.special, "gpu")


class JoinGatherTest(unittest.TestCase):
    """io.go readChunkData + gather.go port: the join lists its staged chunk
    bundles itself, in index order, against the chunks.json count (#323,
    #324)."""

    def _stage(self, names, chunks):
        tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, tmp)
        for name in names:
            os.mkdir(os.path.join(tmp, name))
        path = os.path.join(tmp, "chunks.json")
        with open(path, "w") as fh:
            json.dump(chunks, fh)
        return tmp, path

    def test_index_order_and_prefix_filter(self):
        names = ["chunk_%05d" % i for i in (11, 2, 0, 10, 1, 3, 4, 5, 6, 7,
                                            8, 9)]
        tmp, _ = self._stage(names + ["out_chunk_00000", "types.json"], [])
        got = rs.gather_indexed(tmp, rs.CHUNK_DEF_PREFIX, 12)
        self.assertEqual([os.path.basename(p) for p in got],
                         ["chunk_%05d" % i for i in range(12)])

    def test_order_past_padding_width(self):
        # Index 100000 is six digits: the index, not a lexical sort, orders.
        self.assertEqual(rs.split_index("out_chunk_100000"),
                         ("out_chunk_", 100000))
        with self.assertRaises(rs.RunnerError):
            rs.split_index("chunk_x")

    def test_flags_default_chunkdir_to_task_dir(self):
        self.assertEqual(rs.parse_flags("join", []).chunkdir, ".")
        self.assertEqual(rs.parse_flags("split", []).chunkdir, "")

    def test_zero_chunks(self):
        tmp, path = self._stage([], [])
        self.assertEqual(rs.read_chunk_data(path, tmp), ([], []))
        tmp, path = self._stage([], None)
        self.assertEqual(rs.read_chunk_data(path, os.path.join(tmp, "x")),
                         ([], []))

    def test_count_mismatch_fails(self):
        cases = [
            (["chunk_00000", "out_chunk_00000"], [{}, {}]),  # both short
            (["chunk_00000", "chunk_00001", "out_chunk_00000"], [{}, {}]),
            (["chunk_00000", "chunk_00002", "out_chunk_00000",
              "out_chunk_00001"], [{}, {}]),  # index gap
            ([], [{}]),  # nothing staged
        ]
        for names, chunks in cases:
            tmp, path = self._stage(names, chunks)
            with self.assertRaises(rs.RunnerError, msg=names):
                rs.read_chunk_data(path, tmp)
        with self.assertRaises(rs.RunnerError):
            rs.read_chunk_data("", ".")


class RSSMonitorTest(unittest.TestCase):
    """monitor.go sampler port (fix 3): statm parsing + quota message."""

    def test_rss_from_statm(self):
        self.assertEqual(rs._rss_from_statm("12345 678 90 1 0 2 0\n", 4096),
                         678 * 4096)
        self.assertEqual(rs._rss_from_statm("", 4096), 0)
        self.assertEqual(rs._rss_from_statm("12345", 4096), 0)
        self.assertEqual(rs._rss_from_statm("1 bogus", 4096), 0)

    def test_quota_message_matches_go_format(self):
        gb = rs.BYTES_PER_GB
        self.assertEqual(
            rs.quota_message(int(1.55 * gb), 1 * gb),
            "Stage exceeded its memory quota (using 1.5, allowed 1G)")
        self.assertEqual(
            rs.quota_message(3 * gb, int(2.5 * gb)),
            "Stage exceeded its memory quota (using 3.0, allowed 2.5G)")

    def test_self_rss_reads_positive(self):
        self.assertGreater(rs._self_rss_bytes(), 0)


class VmemCapTest(unittest.TestCase):
    """Fix 9: the vmem cap is soft-only and restorable (data plane uncapped)."""

    def test_apply_and_restore_soft_limit(self):
        resource = rs._resource_mod
        before = resource.getrlimit(resource.RLIMIT_AS)
        prev = rs.apply_vmem_cap(True, 1024.0)  # 1 TB: comfortably high
        try:
            if prev is not None:
                soft, hard = resource.getrlimit(resource.RLIMIT_AS)
                self.assertEqual(hard, before[1])  # hard untouched
                self.assertNotEqual(soft, resource.RLIM_INFINITY)
        finally:
            rs.restore_vmem_cap(prev)
        self.assertEqual(resource.getrlimit(resource.RLIMIT_AS), before)

    def test_disabled_is_none(self):
        self.assertIsNone(rs.apply_vmem_cap(False, 4.0))
        self.assertIsNone(rs.apply_vmem_cap(True, 0.0))
        rs.restore_vmem_cap(None)  # no-op

    def test_huge_value_does_not_raise(self):
        prev = rs.apply_vmem_cap(True, 1e30)  # clamped, best-effort
        rs.restore_vmem_cap(prev)


class SystemExitMatrixTest(unittest.TestCase):
    """Fix 1: end-to-end exit classification via subprocess runs."""

    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp()
        stage_dir = os.path.join(cls.tmp, "stages", "tstage")
        os.makedirs(stage_dir)
        with open(os.path.join(stage_dir, "__init__.py"), "w") as fh:
            fh.write(STAGE_SRC)
        cls.stagecode = stage_dir
        cls.types = os.path.join(cls.tmp, "types.json")
        with open(cls.types, "w") as fh:
            json.dump(TYPES_JSON, fh)

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.tmp, True)

    def _run(self, phase, mode, extra):
        wd = tempfile.mkdtemp(dir=self.tmp)
        env = dict(os.environ, TMODE=mode)
        argv = [sys.executable, RUNNER, phase,
                "-stagecode", self.stagecode, "-call", "TOP",
                "-types", self.types, "-callable", "TSTAGE",
                "-threads", "1", "-memgb", "1", "-work", "w"] + extra
        proc = subprocess.run(
            argv, cwd=wd, env=env, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, timeout=60,
        )
        return proc, wd

    def test_assert_exits_42(self):
        proc, _ = self._run("main", "assert", ["-o", "outs"])
        self.assertEqual(proc.returncode, 42)
        self.assertIn(b"ASSERT:boom", proc.stderr)

    def test_main_sys_exit_zero_bundles_unmutated_skeleton(self):
        proc, wd = self._run("main", "exit0", ["-o", "outs"])
        self.assertEqual(proc.returncode, 0, proc.stderr)
        with open(os.path.join(wd, "outs", "data.json")) as fh:
            data = json.load(fh)
        # The Record was mutated (note="mutated") but mre bundles the
        # pre-staged skeleton: note null, report = skeleton path.
        self.assertIsNone(data["note"])
        self.assertTrue(data["report"].endswith("report.txt"))

    def test_main_sys_exit_nonzero_is_1_not_leaked(self):
        proc, _ = self._run("main", "exit5", ["-o", "outs"])
        self.assertEqual(proc.returncode, 1)

    def test_main_sys_exit_42_is_not_an_assert(self):
        proc, _ = self._run("main", "exit42", ["-o", "outs"])
        self.assertEqual(proc.returncode, 1)
        self.assertNotIn(b"ASSERT", proc.stderr)

    def test_split_sys_exit_zero_is_1(self):
        proc, _ = self._run("split", "exit0", ["-o", "chunks.json"])
        self.assertEqual(proc.returncode, 1)

    def test_split_falsy_return_fails_like_the_adapter_path(self):
        # None/{} split returns fail end-to-end under mre (the vendor shell's
        # `obj or ""` serialization quirk); the runner exits 1 the same way.
        for mode in ("none", "empty"):
            proc, _ = self._run(
                "split", mode, ["-o", "chunks.json", "-joinres",
                                "joinres.json"])
            self.assertEqual(proc.returncode, 1, proc.stderr)


class MartianModuleTest(unittest.TestCase):
    """martian.py surface: fixes 6 and 7 plus the vendor quirks."""

    def setUp(self):
        # Snapshot and restore module state mutated by _configure.
        self._saved = (martian._FILES_PATH, martian._THREADS,
                       martian._MEM_GB, martian._INVOCATION_CALL,
                       martian._INVOCATION_ARGS, martian._WARNINGS_PATH,
                       martian._PROGRESS_PATH)

    def tearDown(self):
        (martian._FILES_PATH, martian._THREADS, martian._MEM_GB,
         martian._INVOCATION_CALL, martian._INVOCATION_ARGS,
         martian._WARNINGS_PATH, martian._PROGRESS_PATH) = self._saved

    def test_invocation_wiring(self):
        martian._configure("/w/main/files", 2, 4, call="PIPE",
                           invocation_args={"x": 1})
        self.assertEqual(martian.get_invocation_call(), "PIPE")
        self.assertEqual(martian.get_invocation_args(), {"x": 1})
        self.assertEqual(martian.get_threads_allocation(), 2)
        self.assertEqual(martian.get_memory_allocation(), 4)

    def test_make_path_returns_bytes(self):
        martian._configure("/w/main/files", 1, 1)
        got = martian.make_path("out.txt")
        self.assertEqual(got, b"/w/main/files/out.txt")

    def test_alarm_and_progress_target_work_dir_absolute(self):
        tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, tmp, True)
        files = os.path.join(tmp, "main", "files")
        os.makedirs(files)
        martian._configure(files, 1, 1)
        cwd = os.getcwd()
        other = tempfile.mkdtemp(dir=tmp)
        os.chdir(other)  # a stage chdir must not scatter the sidecars
        try:
            martian.alarm("careful")
            martian.update_progress("50%")
        finally:
            os.chdir(cwd)
        meta = os.path.join(tmp, "main")
        with open(os.path.join(meta, "_warnings")) as fh:
            self.assertEqual(fh.read(), "careful\n")
        with open(os.path.join(meta, "_progress"), "rb") as fh:
            self.assertEqual(fh.read(), b"50%")
        self.assertFalse(os.path.exists(os.path.join(files, "_warnings")))

    def test_exit_raises_assertion_that_survives_except_exception(self):
        try:
            try:
                martian.exit("boom")
            except Exception:  # pylint: disable=broad-except
                self.fail("StageAssertion must not be caught as Exception")
        except martian.StageAssertion as exc:
            self.assertEqual(exc.message, "boom")
            self.assertEqual(exc.code, 42)

    def test_record_quirks(self):
        rec = martian.Record({"a": 1, "b": 2})
        self.assertEqual(rec.a, 1)
        self.assertEqual(rec.items(), {"a": 1, "b": 2})
        self.assertEqual(list(rec), [1, 2])
        with self.assertRaises(TypeError):
            rec[0]  # vendor quirk: dict-keys view is not indexable
        rec.coerce_strings()  # no-op

    def test_json_sanitize(self):
        got = martian.json_sanitize(
            {"nan": float("nan"), "b": b"x", "t": (1, 2)})
        self.assertEqual(got, {"nan": None, "b": "x", "t": [1, 2]})


if __name__ == "__main__":
    unittest.main()
