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
        rs.write_bundle(bdir, payload, self.PARAMS, self.STRUCTS)
        with open(os.path.join(bdir, "data.json")) as fh:
            data = json.load(fh)
        # Ordinals follow param order, then sorted map keys (aa before zz); each
        # leaf keeps the source extension (.txt) so typed-file readers resolve it.
        self.assertEqual(data["arr"], ["@mre:file:f/L0000.txt", None])
        self.assertEqual(data["st"], {"f": "@mre:file:f/L0001.txt", "n": 7})
        self.assertEqual(data["m"], {"aa": "@mre:file:f/L0002.txt",
                                     "zz": "@mre:file:f/L0003.txt"})
        self.assertEqual(data["passthru"], src["a"])
        with open(os.path.join(bdir, "f", "L0002.txt")) as fh:
            self.assertEqual(fh.read(), "d")  # aa -> d.txt
        # read_bundle resolves markers back to absolute staged paths.
        resolved = rs.read_bundle(bdir)
        self.assertEqual(resolved["arr"][0],
                         os.path.join(os.path.abspath(bdir), "f", "L0000.txt"))

    def test_missing_and_empty_leaves_kept(self):
        tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, tmp, True)
        payload = {"arr": [os.path.join(tmp, "nope"), ""]}
        bdir = os.path.join(tmp, "bundle")
        rs.write_bundle(bdir, payload, self.PARAMS, self.STRUCTS)
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
        rs.write_bundle(bdir, {"arr": [d]}, self.PARAMS, self.STRUCTS)
        with open(os.path.join(bdir, "data.json")) as fh:
            data = json.load(fh)
        self.assertEqual(data["arr"], ["@mre:dir:f/L0000"])
        with open(os.path.join(bdir, "f", "L0000", "x")) as fh:
            self.assertEqual(fh.read(), "x")


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
        self.assertEqual(eff.vmem_gb, 7.0)  # <1GB recomputes
        eff = rs.resolve_resources(_res(), _res(mem_gb=4.0, vmem_gb=9.0))
        self.assertEqual(eff.vmem_gb, 9.0)

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
