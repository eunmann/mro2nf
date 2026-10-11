#!/usr/bin/env python3
"""Native stage runner for mro2nf-generated Nextflow processes (issue #79).

Replaces the `mre <phase>` + martian_shell.py hop for python stages: does the
bundle data plane in-process, imports the stage module, calls
split(args) / main(args, outs) / join(args, outs, chunk_defs, chunk_outs)
directly, and writes the outs bundle with byte-level parity to mre's output.

Exit codes: 42 for martian.exit() (internal/shim/shim.go AssertExitCode),
1 for any other failure, 0 on success.

Ports, section by section:
  - CLI flags:            cmd/mre/main.go (commonFlags + per-phase flag sets)
  - bundle read/write:    internal/shim/bundle.go (ReadBundle, WriteBundle,
                          MarkFiles, CopyTree, WriteChunkBundle)
  - type manifest + walk: internal/types/manifest.go, internal/types/types.go
  - scalar coercion:      cmd/mre/bundle.go coerceInputs +
                          internal/types/types.go CoerceScalars
  - output validation:    cmd/mre/bundle.go validateOuts +
                          internal/types/validate.go ValidateOutputs
  - resources/skeletons:  internal/shim/meta.go, internal/shim/shim.go
  - JSON serialization:   Go encoding/json (MarshalIndent: sorted map keys,
                          2-space indent, HTML escaping, float encoding)
  - stage import/records: vendor-martian/python/martian_shell.py
"""

import decimal
import json
import math
import os
import re
import resource as _resource_mod
import stat as _stat_mod
import sys
import threading
import traceback
from types import SimpleNamespace

import martian  # the sibling runner martian.py (sys.path[0] is this dir)

# Ports internal/shim/bundle.go marker constants and markerRel.
RESERVED_PREFIX = "@mre:"
FILE_MARKER = RESERVED_PREFIX + "file:"
DIR_MARKER = RESERVED_PREFIX + "dir:"
ESCAPE_PREFIX = RESERVED_PREFIX + "str:"
BUNDLE_DATA = "data.json"
BUNDLE_FILES = "f"
_MARKER_REL = re.compile(BUNDLE_FILES + r"/L[0-9]{4,}/([^/\x00]+)")

# Ports internal/shim/shim.go constants.
EXTRA_VMEM_GB = 3
THREADS_PER_JOB = 1
MEM_GB_PER_JOB = 1
CENTI_PER_CORE = 100
MB_PER_GB = 1024
ASSERT_EXIT_CODE = 42
BYTES_PER_GB = 1 << 30

INT64_MIN = -(2 ** 63)
INT64_MAX = 2 ** 63  # Go coerceNumber: f < math.MaxInt64 rounds to 2^63


class RunnerError(Exception):
    """A runner-level failure (bad flags, malformed input), reported like
    mre's `mre: <err>` line, without a stage traceback."""


# ---------------------------------------------------------------------------
# CLI flags — ports cmd/mre/main.go addCommon + runSplit/runMain/runJoin
# flag sets. Go flag style: single-dash multi-char, `-flag value` or
# `-flag=value`; parsing stops at the first non-flag argument.
# -shell/-mrjob/-lang are mre-only and deliberately NOT accepted here.
# ---------------------------------------------------------------------------

_COMMON_FLAGS = {
    "stagecode": str, "call": str, "mro": str, "work": str, "o": str,
    "special": str,
    "threads": float, "memgb": float, "vmemgb": float,
    "overridethreads": float, "overridememgb": float,
    "monitor": bool, "confine": bool, "attempt": int, "allocbytes": int,
    "types": str, "callable": str, "role": str,
}
_PHASE_FLAGS = {
    "split": {"args": str, "chunkdir": str, "joinres": str},
    "main": {"args": str, "chunk": str},
    "join": {"args": str, "chunksfile": str, "chunkdir": str, "joinres": str},
}
# Ports the non-empty string flag defaults (cmd/mre/main.go addCommon -work,
# runJoin -chunkdir); split's -chunkdir defaults to "" (no chunk bundles).
_STR_DEFAULTS = {("*", "work"): ".", ("join", "chunkdir"): "."}
# Ports the addProducer default role per phase (cmd/mre/main.go).
_ROLE_DEFAULT = {"split": "chunkin", "main": "mainout", "join": "out"}
# Go strconv.ParseBool's exact accepted spellings (what flag's bool parsing
# uses); anything else is a hard parse error.
_BOOL_TRUE = {"1", "t", "T", "TRUE", "true", "True"}
_BOOL_FALSE = {"0", "f", "F", "FALSE", "false", "False"}


def _flag_defaults(phase, spec):
    vals = {}
    for name, kind in spec.items():
        if kind is bool:
            vals[name] = False
        elif kind is float:
            vals[name] = 1.0 if name in ("threads", "memgb") else 0.0
        elif kind is int:
            vals[name] = 1 if name == "attempt" else 0
        else:
            vals[name] = _STR_DEFAULTS.get(
                (phase, name), _STR_DEFAULTS.get(("*", name), ""))
    vals["role"] = _ROLE_DEFAULT[phase]
    return vals


def parse_flags(phase, argv):
    """Parse a phase's flag tail exactly like Go's flag package would."""
    spec = dict(_COMMON_FLAGS)
    spec.update(_PHASE_FLAGS[phase])
    vals = _flag_defaults(phase, spec)
    i = 0
    while i < len(argv):
        tok = argv[i]
        if tok == "--" or tok == "-" or not tok.startswith("-"):
            break  # Go's flag package stops at the first non-flag argument
        name = tok[2:] if tok.startswith("--") else tok[1:]
        inline = None
        if "=" in name:
            name, inline = name.split("=", 1)
        if name not in spec:
            raise RunnerError("flag provided but not defined: -%s" % name)
        kind = spec[name]
        if kind is bool:
            if inline is None or inline in _BOOL_TRUE:
                vals[name] = True
            elif inline in _BOOL_FALSE:
                vals[name] = False
            else:
                raise RunnerError(
                    "invalid boolean value %r for -%s" % (inline, name)
                )
            i += 1
            continue
        if inline is None:
            i += 1
            if i >= len(argv):
                raise RunnerError("flag needs an argument: -%s" % name)
            inline = argv[i]
        if kind is float or kind is int:
            try:
                vals[name] = float(inline) if kind is float else int(inline, 0)
            except ValueError:
                raise RunnerError(
                    "invalid value %r for flag -%s" % (inline, name)
                ) from None
        else:
            vals[name] = inline
        i += 1
    return SimpleNamespace(**vals)


# ---------------------------------------------------------------------------
# Type manifest — ports internal/types/manifest.go.
# ---------------------------------------------------------------------------

class Manifest:
    """The types.json manifest: struct table + per-callable param sets."""

    def __init__(self, structs=None, callables=None, loaded=False):
        self.structs = structs or {}  # struct name -> field param list
        self.callables = callables or {}
        self.loaded = loaded  # mirrors producer.types != "" in cmd/mre/bundle.go

    @classmethod
    def load(cls, path):
        if not path:
            return cls()
        with open(path, encoding="utf-8") as src:
            data = json.load(src)
        structs = {
            name: (st.get("fields") or [])
            for name, st in (data.get("structs") or {}).items()
        }
        return cls(structs, data.get("callables") or {}, True)

    def params(self, callable_, role):
        """Ports Manifest.Params: unknown callable or role yields []."""
        c = self.callables.get(callable_)
        if not c:
            return []
        if role == "in":
            return c.get("in") or []
        if role == "out":
            return c.get("out") or []
        if role == "chunkin":
            return c.get("chunkIn") or []
        if role == "mainout":
            return (c.get("out") or []) + (c.get("chunkOut") or [])
        return []


def is_struct(structs, name):
    """Ports types.Table.IsStruct."""
    return name in structs


def out_filename(p, structs):
    """Ports internal/types/types.go OutFilename."""
    if p.get("outName"):
        return p["outName"]
    base = p.get("baseType", "")
    if p.get("arrayDim", 0) > 0 or p.get("mapDim", 0) > 0 or is_struct(structs, base):
        return p["name"]
    if base in ("file", "path"):
        return p["name"]
    return p["name"] + "." + base


# ---------------------------------------------------------------------------
# Type-directed walk — ports internal/types/types.go Apply/walk/walkMap.
# ---------------------------------------------------------------------------

def apply_params(structs, params, vals, fn):
    """Ports types.Table.Apply: walk each matching param's value with fn."""
    out = dict(vals)
    for p in params:
        name = p["name"]
        if name not in vals:
            continue
        try:
            out[name] = _walk(
                structs, vals[name], p.get("baseType", ""),
                p.get("arrayDim", 0), p.get("mapDim", 0), p.get("isFile", False),
                fn,
            )
        except RunnerError as err:
            raise RunnerError("param %s: %s" % (name, err)) from None
    return out


def _walk(structs, v, base, array_dim, map_dim, is_file, fn):
    if v is None:
        return None
    if isinstance(v, list):
        if array_dim <= 0:
            return v
        return [
            _walk(structs, e, base, array_dim - 1, map_dim, is_file, fn)
            for e in v
        ]
    if isinstance(v, dict):
        if map_dim > 0:
            # One typed-map level carrying mapDim-1 inner array dims; sorted
            # keys keep the staged file-leaf ordinals deterministic (walkMap).
            return {
                k: _walk(structs, v[k], base, array_dim + map_dim - 1, 0,
                         is_file, fn)
                for k in sorted(v)
            }
        if base in structs:
            return apply_params(structs, structs[base], v, fn)
        return v
    if isinstance(v, str):
        if is_file and array_dim == 0 and map_dim == 0:
            return fn(v)
        return v
    return v


# ---------------------------------------------------------------------------
# Scalar coercion — ports types.Table.CoerceScalars/coerce/coerceNumber and
# cmd/mre/bundle.go coerceInputs.
# ---------------------------------------------------------------------------

def coerce_scalars(structs, params, vals):
    out = dict(vals)
    for p in params:
        name = p["name"]
        if name in vals:
            out[name] = _coerce(
                structs, vals[name], p.get("baseType", ""),
                p.get("arrayDim", 0), p.get("mapDim", 0),
            )
    return out


def _coerce(structs, v, base, array_dim, map_dim):
    if isinstance(v, bool):  # bool is an int in Python; json.Number never is
        return v
    if isinstance(v, list):
        if array_dim > 0:
            return [
                _coerce(structs, e, base, array_dim - 1, map_dim) for e in v
            ]
        return v
    if isinstance(v, dict):
        if map_dim > 0:
            return {
                k: _coerce(structs, e, base, array_dim + map_dim - 1, 0)
                for k, e in v.items()
            }
        if base in structs:
            return coerce_scalars(structs, structs[base], v)
        return v
    if isinstance(v, (int, float)):
        return _coerce_number(v, base)
    return v


def _coerce_number(n, base):
    """Ports coerceNumber: whole-number float -> int for int params, within
    int64 bounds (upper bound strict: float64(MaxInt64) rounds up to 2^63)."""
    if base != "int":
        return n
    if isinstance(n, int):
        return n
    if n == math.trunc(n) and INT64_MIN <= n < INT64_MAX:
        return int(n)
    return n


# ---------------------------------------------------------------------------
# Output validation — ports internal/types/validate.go ValidateOutputs (mrp's
# Fork.verifyOutput) and cmd/mre/bundle.go validateOuts. Values are the
# sanitized Python outs, so a JSON integer is a Python int (never a bool), and
# a float in an int slot fails exactly as its "2.0" text does on the adapter path.
# ---------------------------------------------------------------------------

_SCALAR_KINDS = {
    "int": "an integer", "float": "a floating point number", "string": "a string",
    "file": "a string", "path": "a string", "bool": "a boolean value", "map": "a map",
}


def validate_outputs(structs, params, outs, callable_):
    """Raise RunnerError when a stage's final outs fail mrp's validation."""
    problems = []
    for p in params:
        name = p["name"]
        if name not in outs:
            problems.append("Missing output value '%s'" % name)
            continue
        err = _validate(structs, outs[name], p.get("baseType", ""),
                        p.get("arrayDim", 0), p.get("mapDim", 0),
                        p.get("isFile", False))
        if err:
            problems.append("Expected %s output value '%s' %s"
                            % (p.get("type", ""), name, err))
    if problems:
        raise RunnerError("validate outputs of %s: invalid stage outputs:\n%s"
                          % (callable_, "\n".join(problems)))


def _validate(structs, v, base, array_dim, map_dim, is_file):
    if v is None:
        return None
    if array_dim > 0:
        if not isinstance(v, list):
            return _unparsable(v, "an array")
        for i, e in enumerate(v):
            err = _validate(structs, e, base, array_dim - 1, map_dim, is_file)
            if err:
                return "element %d: %s" % (i, err)
        return None
    if map_dim > 0:
        return _validate_map(structs, v, base, map_dim, is_file)
    if base in structs:
        if not isinstance(v, dict):
            return _unparsable(v, "a map")
        errs = []
        for f in structs[base]:
            if f["name"] not in v:
                errs.append("missing key: " + f["name"])
                continue
            err = _validate(structs, v[f["name"]], f.get("baseType", ""),
                            f.get("arrayDim", 0), f.get("mapDim", 0),
                            f.get("isFile", False))
            if err:
                errs.append("key %s: %s" % (f["name"], err))
        return "\n".join(errs) or None
    return _validate_scalar(v, base)


def _validate_map(structs, v, base, map_dim, is_file):
    if not isinstance(v, dict):
        return _unparsable(v, "a map")
    for k in sorted(v):
        if is_file:
            err = _legal_filename(k)
            if err:
                return "key %s: %s" % (k, err)
        err = _validate(structs, v[k], base, map_dim - 1, 0, is_file)
        if err:
            return "key %s: %s" % (k, err)
    return None


def _validate_scalar(v, base):
    if base not in _SCALAR_KINDS:
        return None  # a user filetype: mrp only alarms
    is_num = isinstance(v, (int, float)) and not isinstance(v, bool)
    ok = {
        "int": is_num and isinstance(v, int) and INT64_MIN <= v < INT64_MAX,
        "float": is_num,
        "string": isinstance(v, str),
        "file": isinstance(v, str),
        "path": isinstance(v, str),
        "bool": isinstance(v, bool),
        "map": isinstance(v, dict),
    }[base]
    return None if ok else _unparsable(v, _SCALAR_KINDS[base])


def _unparsable(v, what):
    shown = json.dumps(v)
    if len(shown) > 32:
        shown = shown[:32] + "..."
    return "value '%s' cannot be parsed as %s" % (shown, what)


def _legal_filename(name):
    """Ports Martian's IsLegalUnixFilename (syntax/compile_params.go)."""
    if len(name.encode("utf-8")) > 255:
        return "illegal filename: too long"
    if name == "":
        return "illegal filename: empty string"
    if name in (".", ".."):
        return "illegal filename: reserved name"
    if "/" in name:
        return "illegal filename: '/' is not allowed in filenames"
    if "\0" in name:
        return "illegal filename: null characters are not allowed in filenames"
    return None


def coerce_inputs(man, callable_, vals):
    """Ports producer.coerceInputs: no-op without a manifest or empty args."""
    if not man.loaded or not vals:
        return vals
    return coerce_scalars(man.structs, man.params(callable_, "in"), vals)


# ---------------------------------------------------------------------------
# Bundle READ — ports internal/shim/bundle.go ReadBundle/CutMarker and
# cmd/mre/bundle.go rawToMap. Values are parsed with plain json.load: every
# payload mre writes was serialized by Python (json_dumps_safe) or by this
# runner, so parse->repr round-trips reproduce the literals byte-for-byte
# (Python float repr is shortest and exact), and stage code sees the same
# int/float types it would get from martian_shell's json.load of _args.
# ---------------------------------------------------------------------------

def cut_marker(s):
    """Returns (rel, is_dir, ok) for a transport marker string; ok is False
    for anything but a well-formed marker (ports CutMarker)."""
    if s.startswith(DIR_MARKER):
        rel, is_dir = s[len(DIR_MARKER):], True
    elif s.startswith(FILE_MARKER):
        rel, is_dir = s[len(FILE_MARKER):], False
    else:
        return "", False, False
    m = _MARKER_REL.fullmatch(rel)
    if m is None or m.group(1) in (".", ".."):
        return "", False, False
    return rel, is_dir, True


def _map_strings(v, fn):
    """A copy of the decoded JSON tree v with fn applied to every string
    value; dict keys are left alone (ports mapStrings)."""
    if isinstance(v, str):
        return fn(v)
    if isinstance(v, list):
        return [_map_strings(e, fn) for e in v]
    if isinstance(v, dict):
        return {k: _map_strings(e, fn) for k, e in v.items()}
    return v


def _escape(s):
    """Ports escapeValue's leaf rule: a reserved-prefix string MarkFiles did
    not mark is escaped so it never reads back as a marker."""
    return ESCAPE_PREFIX + s if s.startswith(RESERVED_PREFIX) else s


def _resolve_leaf(s, bundle_abs):
    """Ports resolveLeaf: escaped value -> original, marker -> absolute
    staged path, any other reserved string -> error, else unchanged."""
    if s.startswith(ESCAPE_PREFIX):
        return s[len(ESCAPE_PREFIX):]
    rel, _, ok = cut_marker(s)
    if ok:
        return os.path.join(bundle_abs, rel)
    if s.startswith(RESERVED_PREFIX):
        raise RunnerError("malformed bundle marker: %s" % json.dumps(s))
    return s


def read_bundle(dir_):
    """Load a bundle payload, rewriting markers to absolute staged paths and
    restoring escaped values. An empty dir yields None (an absent optional
    input)."""
    if not dir_:
        return None
    path = os.path.join(dir_, BUNDLE_DATA)
    try:
        with open(path, encoding="utf-8") as src:
            v = json.load(src)
    except OSError as err:
        raise RunnerError("read bundle %s: %s" % (dir_, err)) from None
    except ValueError as err:
        raise RunnerError("parse bundle %s: %s" % (dir_, err)) from None
    bundle_abs = os.path.abspath(dir_)
    try:
        return _map_strings(v, lambda s: _resolve_leaf(s, bundle_abs))
    except RunnerError as err:
        raise RunnerError("resolve bundle %s: %s" % (dir_, err)) from None


def as_object(v, what):
    """Ports rawToMap: absent/null/"" payloads become {}, any other
    non-object payload is an error."""
    if v is None or v == "":
        return {}
    if not isinstance(v, dict):
        raise RunnerError("decode %s: expected a JSON object" % what)
    return v


# ---------------------------------------------------------------------------
# Resources — ports internal/shim/meta.go parseResources/resolveResources/
# injectResources/mergeArgs/withResources and internal/shim/shim.go Resources.
# ---------------------------------------------------------------------------

class Resources:
    """Per-phase resource allocation (shim.Resources)."""

    __slots__ = ("threads", "mem_gb", "vmem_gb", "special")

    def __init__(self, threads=0.0, mem_gb=0.0, vmem_gb=0.0, special=""):
        self.threads = threads
        self.mem_gb = mem_gb
        self.vmem_gb = vmem_gb
        self.special = special


def _as_float(v):
    """Decode a resource field of an object mre or the runner wrote:
    non-numeric (or out-of-range) values become 0."""
    if isinstance(v, bool) or not isinstance(v, (int, float)):
        return 0.0
    f = float(v)
    if math.isinf(f) or math.isnan(f):
        return 0.0
    return f


# The split-returned resource keys in the order mrp's updateFromLazyArgs (and
# meta.go parseResources) checks them, so an error names the key mrp's does.
_RESOURCE_KEYS = (("__threads", "threads"), ("__mem_gb", "mem_gb"),
                  ("__vmem_gb", "vmem_gb"), ("__special", "special"))


def _resource_value(key, val):
    """Decode one split-returned resource value as Go's json.Unmarshal into
    the field's type does: null is unset, and any other JSON type, or a number
    past float64's range, fails the split as it does under mrp."""
    val = _numpy_native(val)
    if key == "__special":
        if val is None or isinstance(val, str):
            return val or ""
        want = "a string"
    else:
        if val is None:
            return 0.0
        if isinstance(val, (int, float)) and not isinstance(val, bool):
            try:
                return float(val)
            except OverflowError:
                pass
        want = "a float64 number"
    raise RunnerError("invalid resource value: %s = %.80r: want %s"
                      % (key, val, want))


def split_resources(m):
    """Ports parseResources: split __-prefixed resource keys from data args."""
    res = Resources()
    args = dict(m)
    for key, attr in _RESOURCE_KEYS:
        if key in args:
            setattr(res, attr, _resource_value(key, args.pop(key)))
    return res, args


def _coalesce(primary, fallback):
    return primary if primary != 0 else fallback


def _ceil_mb(gb):
    return math.ceil(abs(gb) * MB_PER_GB)


def resolve_resources(chunk, res):
    """Ports resolveResources: chunk (or join) requests win; negative
    adaptive sentinels resolve to |x|; threads round up to whole centicores
    and memory and vmem to whole MiB, unset threads and memory take the job
    defaults, and vmem defaults to memory plus headroom and never falls below
    memory (mrp's local GetSystemReqs)."""
    eff = Resources(special=chunk.special or res.special)
    centi = math.ceil(abs(_coalesce(chunk.threads, res.threads)) * CENTI_PER_CORE)
    if centi == 0:
        centi = THREADS_PER_JOB * CENTI_PER_CORE
    eff.threads = centi / CENTI_PER_CORE
    mem_mb = _ceil_mb(_coalesce(chunk.mem_gb, res.mem_gb))
    if mem_mb == 0:
        mem_mb = MEM_GB_PER_JOB * MB_PER_GB
    vmem_mb = _ceil_mb(_coalesce(chunk.vmem_gb, res.vmem_gb))
    if vmem_mb == 0:
        vmem_mb = mem_mb + EXTRA_VMEM_GB * MB_PER_GB
    eff.mem_gb = mem_mb / MB_PER_GB
    eff.vmem_gb = max(vmem_mb, mem_mb) / MB_PER_GB
    return eff


def monitor_limits(eff, fl):
    """Ports shim.go monitorLimits: a retry enforces the larger allocation
    Nextflow escalated the task to."""
    alloc = fl.allocbytes / float(BYTES_PER_GB)
    if fl.attempt <= 1 or alloc <= eff.mem_gb:
        return eff
    return Resources(threads=eff.threads, mem_gb=alloc,
                     vmem_gb=max(eff.vmem_gb, alloc + EXTRA_VMEM_GB),
                     special=eff.special)


def _resource_num(v):
    """A resolved resource as the stage reads it back from mre's _args
    (meta.go resourceNum, mrp's ToLazyMap): three significant digits, no
    exponent from 999.5 up."""
    f = float(v)
    if -999.5 < f < 999.5:
        return json.loads("%.3g" % f)
    return json.loads("%.0f" % f)


def _js_num(v):
    """A resolved resource as the stage reads it from _jobinfo: mirrors shim
    writeJobInfo's json.Marshal of the float, so whole values become ints."""
    f = float(v)
    if f.is_integer() and abs(f) < 1e21:
        return int(f)
    return f


def inject_resources(merged, eff):
    """Ports injectResources: write the resolved __* keys into an args map."""
    merged["__mem_gb"] = _resource_num(eff.mem_gb)
    merged["__threads"] = _resource_num(eff.threads)
    merged["__vmem_gb"] = _resource_num(eff.vmem_gb)
    if eff.special:
        merged["__special"] = eff.special


def decode_chunk(v):
    """Ports cmd/mre/io.go decodeChunk: a chunk bundle's resolved payload."""
    if v is None:
        return Resources(), {}
    if not isinstance(v, dict):
        raise RunnerError("parse chunk: expected a JSON object")
    args = v.get("args")
    if args is None:
        args = {}
    if not isinstance(args, dict):
        raise RunnerError("parse chunk: args must be a JSON object")
    return resources_from_object(v.get("resources")), args


def resources_from_object(v):
    """Decode a {"threads":..,"mem_gb":..,"vmem_gb":..,"special":..} object."""
    r = Resources()
    if isinstance(v, dict):
        r.threads = _as_float(v.get("threads"))
        r.mem_gb = _as_float(v.get("mem_gb"))
        r.vmem_gb = _as_float(v.get("vmem_gb"))
        s = v.get("special")
        r.special = s if isinstance(s, str) else ""
    return r


# ---------------------------------------------------------------------------
# Skeleton outs — ports internal/shim/meta.go writeSkeletonOuts/skeletonOut.
# ---------------------------------------------------------------------------

def skeleton_outs(structs, out_params, files):
    return {p["name"]: _skeleton_out(p, files, structs) for p in out_params}


def _skeleton_out(p, files, structs):
    if p.get("arrayDim", 0) > 0:
        return []
    if p.get("mapDim", 0) > 0:
        return {}
    if is_struct(structs, p.get("baseType", "")):
        return None
    if p.get("isFile", False):
        return os.path.join(files, out_filename(p, structs))
    return None


# ---------------------------------------------------------------------------
# Go-compatible JSON serialization — ports Go encoding/json MarshalIndent as
# used by internal/shim/bundle.go writePayload and cmd/mre/io.go writeJSON:
# 2-space indent, alphabetically sorted object keys (Go map marshal), HTML
# escaping (<,>,& -> <...),  /  escaped, no trailing newline.
# ---------------------------------------------------------------------------

class GoStruct(dict):
    """A JSON object emitted in insertion order (a Go struct, e.g.
    shim.Resources / shim.ChunkDef field order) instead of sorted."""


class GoFloat(float):
    """A float emitted with Go's encoding/json float64 format (a value Go
    itself produced, e.g. a Resources field), not Python repr."""


class PyStrings:
    """Marks a subtree serialized as Go's json.RawMessage passthrough: string
    escaping and object order exactly as Python's json.dumps wrote them, plus
    compact()'s HTML escaping. This is chunks.json's per-chunk args
    (mre writeJSON of shim.ChunkDef.Args map[string]json.RawMessage: the raw
    _stage_defs bytes survive re-indentation, so nested key order and
    ensure_ascii escapes are Python's, while <,>,& get Go-escaped)."""

    def __init__(self, value):
        self.value = value


_GO_SHORT_ESCAPES = {'"': '\\"', "\\": "\\\\", "\n": "\\n", "\r": "\\r",
                     "\t": "\\t"}


def _go_string(s):
    """Go encoding/json string escaping with escapeHTML=true."""
    out = ['"']
    for ch in s:
        if ch in _GO_SHORT_ESCAPES:
            out.append(_GO_SHORT_ESCAPES[ch])
        elif ch in "<>&" or ch < " " or ch in "\u2028\u2029":
            out.append("\\u%04x" % ord(ch))
        else:
            out.append(ch)
    out.append('"')
    return "".join(out)


def _py_string(s):
    """Python json.dumps escaping (ensure_ascii) + Go compact's HTML
    escaping, for RawMessage-passthrough subtrees."""
    dumped = json.dumps(s)
    # Escaping the raw ASCII <,>,& in the dumped text is safe: they can only
    # occur inside the string literal, never as part of an escape sequence.
    return (dumped.replace("<", "\\u003c").replace(">", "\\u003e")
                  .replace("&", "\\u0026"))


def _go_float_literal(f):
    """Ports Go encoding/json floatEncoder for float64: shortest repr,
    'f' form for 1e-6 <= |v| < 1e21 (and 0), else 'e' form with the
    e-0X -> e-X exponent cleanup."""
    if math.isinf(f) or math.isnan(f):
        raise RunnerError("json: unsupported float value %r" % f)
    d = decimal.Decimal(repr(f))
    a = abs(f)
    if a != 0 and (a < 1e-6 or a >= 1e21):
        sign, digits, exp = d.as_tuple()
        mant = str(digits[0])
        if len(digits) > 1:
            mant += "." + "".join(str(x) for x in digits[1:])
        e = exp + len(digits) - 1
        es = ("e+%02d" % e) if e >= 0 else ("e-%d" % -e)
        return ("-" if sign else "") + mant + es
    s = format(d, "f")
    if "." in s:
        s = s.rstrip("0").rstrip(".")
    return s


def _json_key(k):
    """Coerce a non-string dict key the way Python json.dumps would (the
    payloads Go re-encodes always arrive with string keys already)."""
    if isinstance(k, str):
        return k
    if k is True:
        return "true"
    if k is False:
        return "false"
    if k is None:
        return "null"
    if isinstance(k, float):
        return repr(k)
    return str(k)


def go_json(v):
    """Serialize like Go json.MarshalIndent(v, "", "  ")."""
    return _enc(v, 0, False)


def _numpy_native(v):
    """Coerce a numpy scalar/array to its native Python equivalent, or return v
    unchanged. numpy scalars are not JSON-serializable as-is — numpy>=2.0 makes
    repr(np.float64(x)) == 'np.float64(x)' (invalid JSON), and np.int64 is not an
    int subclass at all — so a stage that returns numpy values (CellRanger's do)
    would emit a broken bundle. .tolist() maps a numpy scalar to a Python scalar
    and an ndarray to nested lists, matching martian_shell.py's JSON encoder.
    Detected via __module__ so the runner needs no numpy dependency of its own."""
    mod = type(v).__module__
    if (mod == "numpy" or mod.startswith("numpy.")) and hasattr(v, "tolist"):
        return v.tolist()
    return v


def _enc(v, level, raw):
    if isinstance(v, PyStrings):
        return _enc(v.value, level, True)
    v = _numpy_native(v)
    if v is None:
        return "null"
    if isinstance(v, bool):
        return "true" if v else "false"
    if isinstance(v, GoFloat):
        return _go_float_literal(float(v))
    if isinstance(v, float):
        return repr(v)  # Python json.dumps float format (see bundle READ note)
    if isinstance(v, int):
        return str(v)
    if isinstance(v, str):
        return _py_string(v) if raw else _go_string(v)
    if isinstance(v, dict):
        return _enc_dict(v, level, raw)
    if isinstance(v, (list, tuple)):
        return _enc_list(v, level, raw)
    raise RunnerError("json: cannot encode value of type %s" % type(v).__name__)


def _enc_dict(d, level, raw):
    if not d:
        return "{}"
    pairs = [(_json_key(k), val) for k, val in d.items()]
    if not raw and not isinstance(d, GoStruct):
        pairs.sort(key=lambda kv: kv[0])  # Go marshals map keys sorted
    pad = "  " * (level + 1)
    esc = _py_string if raw else _go_string
    parts = [
        pad + esc(k) + ": " + _enc(val, level + 1, raw) for k, val in pairs
    ]
    return "{\n" + ",\n".join(parts) + "\n" + "  " * level + "}"


def _enc_list(lst, level, raw):
    if not lst:
        return "[]"
    pad = "  " * (level + 1)
    parts = [pad + _enc(e, level + 1, raw) for e in lst]
    return "[\n" + ",\n".join(parts) + "\n" + "  " * level + "]"


def resources_struct(r):
    """shim.Resources in Go struct-field order with Go float formatting."""
    d = GoStruct()
    d["threads"] = GoFloat(r.threads)
    d["mem_gb"] = GoFloat(r.mem_gb)
    d["vmem_gb"] = GoFloat(r.vmem_gb)
    if r.special:  # json:"special,omitempty"
        d["special"] = r.special
    return d


# ---------------------------------------------------------------------------
# Bundle WRITE — ports internal/shim/bundle.go WriteBundle/MarkFiles/
# CopyTree/WriteChunkBundle.
# ---------------------------------------------------------------------------

def _leaf_name(src):
    """The basename the stage sees for the file leaf at src; ports leafName. A
    path ending in "/", "." or ".." names the directory it resolves to."""
    name = os.path.basename(os.path.abspath(src))
    if name == "":
        raise RunnerError("file leaf has no basename: %r" % src)
    return name


def mark_files(bundle_dir, payload, params, structs, confine):
    """Copy every file leaf into bundle_dir/f/L%04d/<basename> (walk order) and
    replace it with a transport marker; ports MarkFiles. Each leaf keeps the
    basename mrp passes, in its own ordinal dir so leaves never collide. Every
    other reserved-prefix string is escaped, a file leaf naming no file
    included. A Confinement (every runner write is a stage's) confines the
    leaves' files."""
    counter = [0]
    escaped = {k: _map_strings(v, _escape) for k, v in payload.items()}

    def copy_in(leaf):
        src = leaf[len(ESCAPE_PREFIX):] if leaf.startswith(ESCAPE_PREFIX) else leaf
        if src == "":
            return leaf
        try:
            st = os.stat(src)  # follows symlinks, like Go os.Stat
            src_is_dir = _stat_mod.S_ISDIR(st.st_mode)
        except FileNotFoundError:
            # A declared output the stage never wrote (or a dangling
            # symlink): keep the path string unchanged.
            return leaf
        except OSError:
            src_is_dir = False  # copy_tree will surface the error precisely
        rel = os.path.join(BUNDLE_FILES, "L%04d" % counter[0], _leaf_name(src))
        counter[0] += 1
        dst = os.path.join(bundle_dir, rel)
        os.makedirs(os.path.dirname(dst), exist_ok=True)
        copy_tree(src, dst, confine)
        return (DIR_MARKER if src_is_dir else FILE_MARKER) + rel

    try:
        return apply_params(structs, params, escaped, copy_in)
    except OSError as err:
        raise RunnerError("collect bundle files: %s" % err) from None


def copy_tree(src, dst, confine, copy_only=False, ancestors=None):
    """Ports CopyTree: resolve a symlinked source, recurse into directories,
    hard-link files when possible, fall back to a byte copy. A dangling
    symlink (ENOENT) is skipped; any other resolve failure, such as a link
    loop, is an error, as in resolveSymlink. A directory reached again on its
    own recursion path (a symlink back to an ancestor) is a cycle error. A
    non-None confine classifies the stage's own files; it is dropped below a
    staged input, and below a host path outside the task, whose tree is
    copied without hard links (copyTree's copyPolicy)."""
    if os.path.islink(src):
        try:
            os.stat(src)
        except FileNotFoundError:
            return
        except OSError as err:
            raise RunnerError("resolve symlink %s: %s" % (src, err)) from None
        resolved = os.path.realpath(src)
    else:
        resolved = src
    if confine is not None:
        kind = confine.admit(src)
        if kind == LEAF_STAGED:
            confine = None
        elif kind == LEAF_OUTSIDE:
            confine, copy_only = None, True
    src = resolved
    st = os.stat(src)
    if _stat_mod.S_ISDIR(st.st_mode):
        ancestors = set() if ancestors is None else ancestors
        dir_id = (st.st_dev, st.st_ino)
        if dir_id in ancestors:
            raise RunnerError("copy %s: directory symlink cycle" % src)
        ancestors.add(dir_id)
        try:
            os.makedirs(dst, mode=_stat_mod.S_IMODE(st.st_mode), exist_ok=True)
            for entry in sorted(os.listdir(src)):
                copy_tree(os.path.join(src, entry), os.path.join(dst, entry),
                          confine, copy_only, ancestors)
        finally:
            ancestors.discard(dir_id)
        return
    if not copy_only:
        try:
            os.link(src, dst)
            return
        except OSError:
            pass
    # Refuse to overwrite an existing destination (a truncating copy over a
    # hard-linked file would corrupt the shared inode).
    if os.path.lexists(dst):
        raise RunnerError("copy %s: destination already exists" % dst)
    _copy_file_contents(src, dst, _stat_mod.S_IMODE(st.st_mode))


MAX_LINK_HOPS = 40


def _path_forms(dir_):
    """dir_'s absolute path and, when it differs, its real path."""
    abs_ = os.path.abspath(dir_)
    real = os.path.realpath(abs_)
    return [abs_] if real == abs_ else [abs_, real]


def _within(p, root):
    return p == root or p.startswith(root.rstrip(os.sep) + os.sep)


# Ports confine.go's leafKind: the stage's own file in the work dir, a path
# reaching a Nextflow-staged input, or a host path outside the task.
LEAF_OWN, LEAF_STAGED, LEAF_OUTSIDE = "own", "staged", "outside"


class Confinement:
    """Ports internal/shim/confine.go Confinement: a stage's output leaf, and
    each symlink it passes through, either stays inside the task work dir,
    reaches a path in it outside the phase dir (a Nextflow-staged input, then
    followed wherever it points), or lands outside the task, which a strict
    confinement (-confine, isolated container targets) refuses and otherwise
    is copied, never hard-linked."""

    def __init__(self, work, phase, strict):
        self.work = _path_forms(work)
        self.phase = _path_forms(os.path.join(work, phase))
        self.strict = strict

    def _staged(self, p):
        for w in self.work:
            if p == w or not _within(p, w):
                continue
            return not any(_within(p, ph) or _within(ph, p) for ph in self.phase)
        return False

    def admit(self, src):
        """The LEAF_* kind of src, following its symlinks one hop at a time;
        a strict confinement raises RunnerError instead of LEAF_OUTSIDE."""
        p = os.path.abspath(src)
        for _ in range(MAX_LINK_HOPS):
            if self._staged(p):
                return LEAF_STAGED
            p = os.path.join(os.path.realpath(os.path.dirname(p)),
                             os.path.basename(p))
            if self._staged(p):
                return LEAF_STAGED
            try:
                st = os.lstat(p)
            except OSError as err:
                raise RunnerError("stat %s: %s" % (p, err)) from None
            if not _stat_mod.S_ISLNK(st.st_mode):
                if any(_within(p, w) for w in self.work):
                    return LEAF_OWN
                if not self.strict:
                    return LEAF_OUTSIDE
                raise RunnerError(
                    "stage output resolves outside the task work dir: "
                    "%s resolves to %s" % (src, p))
            target = os.readlink(p)
            if not os.path.isabs(target):
                target = os.path.join(os.path.dirname(p), target)
            p = os.path.normpath(target)
        raise RunnerError(
            "resolve %s: too many levels of symbolic links" % src)


def _copy_file_contents(src, dst, mode):
    fd = os.open(dst, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, mode)
    with os.fdopen(fd, "wb") as out, open(src, "rb") as inp:
        while True:
            block = inp.read(1 << 20)
            if not block:
                break
            out.write(block)


def write_payload(dir_, payload):
    """Ports writePayload: dir/data.json, Go MarshalIndent formatting."""
    os.makedirs(dir_, exist_ok=True)
    data = go_json(payload)
    with open(os.path.join(dir_, BUNDLE_DATA), "w", encoding="utf-8") as out:
        out.write(data)


def write_bundle(dir_, payload, params, structs, confine):
    """Ports WriteBundle: stage file leaves, then write the marked payload."""
    write_payload(dir_, mark_files(dir_, payload, params, structs, confine))


def write_text(path, text):
    """Ports cmd/mre/io.go writeRaw: empty path or "-" writes to stdout."""
    if path == "" or path == "-":
        sys.stdout.write(text)
        return
    with open(path, "w", encoding="utf-8") as out:
        out.write(text)


# ---------------------------------------------------------------------------
# Stage setup and phase execution — ports internal/shim/meta.go prepDirs,
# internal/shim/shim.go (cwd/TMPDIR/prlimit), and martian_shell.py's stage
# import and Record construction.
# ---------------------------------------------------------------------------

def prep_dirs(work, phase):
    """Ports prepDirs: <work>/<phase>/files (cwd) + <work>/<phase>/tmp
    (TMPDIR), both created up front; returns (meta, files) absolute."""
    meta = os.path.abspath(os.path.join(work, phase))
    files = os.path.join(meta, "files")
    os.makedirs(files, exist_ok=True)
    tmp = os.path.join(meta, "tmp")
    os.makedirs(tmp, exist_ok=True)
    os.environ["TMPDIR"] = tmp  # shim.go adapterEnv
    return meta, files


def apply_vmem_cap(monitor, vmem_gb):
    """Ports shim.go limitedCommand's prlimit --as, scoped like mre: only
    the stage phase call runs under the cap (mre caps just the adapter
    child, never its own data plane). Sets only the SOFT limit — the hard
    limit stays untouched so the cap can be restored after the phase —
    clamped to the hard limit and to int64. Best-effort, like mre when
    prlimit is missing. Returns the previous (soft, hard) pair, or None."""
    if not monitor or vmem_gb <= 0:
        return None
    try:
        prev = _resource_mod.getrlimit(_resource_mod.RLIMIT_AS)
        limit = min(int(vmem_gb * BYTES_PER_GB), 2 ** 63 - 1)
        hard = prev[1]
        if hard != _resource_mod.RLIM_INFINITY:
            limit = min(limit, hard)  # the soft limit may not exceed hard
        _resource_mod.setrlimit(_resource_mod.RLIMIT_AS, (limit, hard))
        return prev
    except (ValueError, OSError, OverflowError) as err:
        sys.stderr.write(
            "run_stage: -monitor requested but setrlimit failed (%s); "
            "running without a %g GB vmem cap\n" % (err, vmem_gb)
        )
        return None


def restore_vmem_cap(prev):
    """Undo apply_vmem_cap once the phase returns (best-effort)."""
    if prev is None:
        return
    try:
        _resource_mod.setrlimit(_resource_mod.RLIMIT_AS, prev)
    except (ValueError, OSError, OverflowError):
        pass


def _rss_from_statm(text, page_size):
    """Ports internal/shim/monitor.go procRSSPages: the resident page count
    is field 1 of /proc/<pid>/statm; malformed input reads as 0."""
    fields = text.split()
    if len(fields) <= 1:
        return 0
    try:
        pages = int(fields[1])
    except ValueError:
        return 0
    return pages * page_size


def _self_rss_bytes():
    """This process's current RSS in bytes, from /proc/self/statm; falls
    back to the peak (getrusage ru_maxrss, KB on Linux) where /proc is
    unavailable — best-effort, like monitor.go groupRSS returning 0."""
    try:
        with open("/proc/self/statm", encoding="ascii") as src:
            return _rss_from_statm(src.read(), _resource_mod.getpagesize())
    except OSError:
        return _resource_mod.getrusage(
            _resource_mod.RUSAGE_SELF
        ).ru_maxrss * 1024


def quota_message(used_bytes, limit_bytes):
    """Ports internal/shim/monitor.go memMonitor.message: Martian's
    canonical memory-quota text."""
    return "Stage exceeded its memory quota (using %.1f, allowed %gG)" % (
        used_bytes / float(BYTES_PER_GB),
        limit_bytes / float(BYTES_PER_GB),
    )


def start_rss_watchdog(monitor, mem_gb, stop):
    """Ports internal/shim/monitor.go memMonitor.watch for the in-process
    stage: a daemon thread samples RSS every second while the phase runs;
    on breach it prints the quota message and hard-exits 1 (retryable,
    mre's memViolation — never an assert). Deviations from mre: only this
    process is sampled (mre sums the whole process group, and folds the
    child's ru_maxrss in at exit), and sampling stops when the phase
    returns."""
    if not monitor or mem_gb <= 0:
        return
    limit = int(mem_gb * BYTES_PER_GB)

    def watch():
        while not stop.wait(1.0):
            rss = _self_rss_bytes()
            if rss > limit:
                sys.stderr.write(quota_message(rss, limit) + "\n")
                sys.stderr.flush()
                os._exit(1)

    threading.Thread(target=watch, daemon=True, name="rss-monitor").start()


class monitored_phase:
    """Scopes -monitor enforcement to the stage phase call only: the vmem
    soft cap and the RSS watchdog are active while split/main/join runs and
    are torn down before the runner's own data plane (outs encode,
    mark_files) resumes — mre likewise caps only the adapter subprocess."""

    def __init__(self, monitor, eff):
        self._monitor = monitor
        self._eff = eff
        self._stop = threading.Event()
        self._prev = None

    def __enter__(self):
        self._prev = apply_vmem_cap(self._monitor, self._eff.vmem_gb)
        start_rss_watchdog(self._monitor, self._eff.mem_gb, self._stop)
        return self

    def __exit__(self, *exc_info):
        self._stop.set()
        restore_vmem_cap(self._prev)
        return False


THREAD_ENVS = (
    "GOMAXPROCS", "MKL_NUM_THREADS", "NUMEXPR_MAX_THREADS",
    "NUMEXPR_NUM_THREADS", "OMP_NUM_THREADS", "OMP_THREAD_LIMIT",
    "POLARS_MAX_THREADS", "ROOT_MAX_THREADS", "TF_NUM_THREADS",
)


def apply_thread_envs(threads):
    """Ports shim.go adapterEnv's thread-limit variables (mrp's threadEnvs):
    each set to ceil(threads), overriding an inherited value. Set before the
    stage module is imported, since thread pools read them at load time."""
    for key in THREAD_ENVS:
        os.environ[key] = str(math.ceil(threads))


def import_stage(stagecode):
    """Ports martian_shell.py StageWrapper's stage-module import mechanics."""
    # Allow shells and stage code to import martian easily.
    sys.path.append(os.path.dirname(os.path.abspath(__file__)))
    # Load the stage code as a module.
    sys.path[0] = os.path.dirname(stagecode)
    return __import__(os.path.basename(stagecode))


def record(d):
    """A martian.Record with top-level keys sorted, matching the slots order
    a stage sees under mre (Go marshals every map it writes with sorted
    keys, so martian_shell's json.load yields sorted insertion order)."""
    return martian.Record(dict(sorted(d.items())))


def cli_resources(fl):
    """The phase request from -threads/-memgb/-vmemgb/-special, with any
    override."""
    return overridden(
        fl, Resources(threads=fl.threads, mem_gb=fl.memgb, vmem_gb=fl.vmemgb,
                      special=fl.special))


def overridden(fl, d):
    """Ports cmd/mre/main.go overridden (shim ApplyOverride): the phase's
    --overrides values beat a chunk's or join's own request."""
    return Resources(threads=_coalesce(fl.overridethreads, d.threads),
                     mem_gb=_coalesce(fl.overridememgb, d.mem_gb),
                     vmem_gb=d.vmem_gb, special=d.special)


def join_request(fl):
    """Ports cmd/mre/main.go joinRequest: the split-returned join request with
    the phase's --overrides values over it."""
    return overridden(fl, read_join_res(fl.joinres))


def abs_out(path):
    """Pin an output path to the invocation cwd: mre writes -o/-joinres/
    -chunkdir relative to its own cwd, but the runner chdirs into the phase
    files dir before the stage call, so relative outputs must be resolved
    first. ""/"-" stay as-is (stdout, cmd/mre/io.go writeRaw)."""
    if path in ("", "-"):
        return path
    return os.path.abspath(path)


def load_stage_args(fl, man):
    """Read + int-coerce the stage args bundle (runSplit/runMain/runJoin)."""
    args = as_object(read_bundle(fl.args), "args bundle")
    return coerce_inputs(man, fl.callable, args)


def setup_stage(fl, files, eff, invocation_args):
    """Configure martian (resources + the invocation call/args mre records
    in _jobinfo — internal/shim/meta.go writeJobInfo), enter the files dir,
    and import the stage module. The -monitor caps are NOT applied here;
    they are scoped to the phase call via monitored_phase."""
    martian._configure(
        files, _js_num(eff.threads), _js_num(eff.mem_gb),
        call=fl.call, invocation_args=invocation_args,
    )
    apply_thread_envs(eff.threads)
    os.chdir(files)
    try:
        return import_stage(fl.stagecode)
    except martian.StageAssertion:
        raise
    except SystemExit as exc:
        # An import-time sys.exit: the vendor shell's __init__ catches it,
        # writes the traceback to the error channel, and mre fails (exit 1)
        # regardless of the code.
        raise RunnerError(
            "import stage code: SystemExit: %s" % (exc.code,)
        ) from None


def stage_system_exit(exc, phase):
    """Classify a stage-raised sys.exit() (never martian.exit) like mre:
    martian_shell lets SystemExit propagate (its _main catches Exception
    only), so the adapter child exits with that status and nothing on the
    error channel; shim.go adapterIO.run treats a zero status as success and
    any nonzero status as a plain adapter failure, which cmd/mre/main.go
    exitCode maps to 1 — a stage sys.exit(42) is NOT an assert and never
    leaks its own code. Returns on zero; raises RunnerError on nonzero."""
    code = exc.code
    if code is None:
        code = 0
    if not isinstance(code, int):
        # CPython prints a non-int SystemExit code to stderr and exits 1.
        sys.stderr.write("%s\n" % (code,))
        code = 1
    if code != 0:
        raise RunnerError("adapter %s phase: exit status %d" % (phase, code))


def run_split(fl):
    """Ports cmd/mre/main.go runSplit + shim.RunSplit + meta.go readStageDefs:
    run split(args), then write chunks.json, joinres.json, and the per-chunk
    bundles."""
    _, files = prep_dirs(fl.work, "split")
    out_path, joinres, chunkdir = abs_out(fl.o), abs_out(fl.joinres), abs_out(fl.chunkdir)
    man = Manifest.load(fl.types)
    args = load_stage_args(fl, man)
    eff = resolve_resources(Resources(), cli_resources(fl))
    # The stage runs in files/ (setup_stage chdir's there), but the data plane
    # below must resolve relative out-path leaves against the task dir — the way
    # mre does (only the adapter child gets cmd.Dir=files; MarkFiles runs in the
    # task cwd). Capture it now and restore before write_chunk_bundles so the two
    # lanes stage the same leaves.
    task_cwd = os.getcwd()
    try:
        # setup_stage is inside the try so the finally's chdir also covers a
        # setup/import failure that leaves the cwd in files/.
        module = setup_stage(fl, files, eff, args)
        with monitored_phase(fl.monitor, monitor_limits(eff, fl)):
            raw_result = module.split(record(args))
    except martian.StageAssertion:
        raise
    except SystemExit as exc:
        stage_system_exit(exc, "split")
        # Exit 0 from split: the vendor shell dies before writing
        # _stage_defs, so mre fails to read it (shim.go readStageDefs).
        raise RunnerError(
            "read _stage_defs: split exited without returning chunk defs"
        ) from None
    finally:
        os.chdir(task_cwd)
    result = martian.json_sanitize(raw_result)
    defs, join_res = parse_stage_defs(result)
    write_text(out_path, go_json(chunk_defs_doc(defs)))
    if fl.joinres:
        write_text(joinres, go_json(resources_struct(join_res)))
    if fl.chunkdir:
        write_chunk_bundles(chunkdir, defs, man, fl.callable,
                            Confinement(fl.work, "split", fl.confine))


def parse_stage_defs(result):
    """Ports meta.go readStageDefs on the in-memory split return value."""
    if result is None or result == {}:
        # End-to-end adapter-path parity: the vendor shell's metadata.write
        # serializes a falsy split result as "" (its `obj or ""` quirk), which
        # meta.go readStageDefs cannot parse — mre exits 1. Although Go's
        # readStageDefs alone would accept null/{} as a 0-chunk split, the
        # runner mirrors the observable end-to-end behavior: a falsy split
        # return is a loud failure, not a silent empty split. A 0-chunk split
        # is still expressible as {'chunks': []}.
        raise RunnerError(
            "split returned %r; a 0-chunk split must be {'chunks': []} "
            "(the adapter path fails on a falsy split return)" % (result,)
        )
    if not isinstance(result, dict):
        raise RunnerError(
            "parse _stage_defs: split must return a JSON object of chunk defs"
        )
    chunks = result.get("chunks")
    if chunks is None:
        chunks = []
    if not isinstance(chunks, list):
        raise RunnerError("parse _stage_defs: chunks must be an array")
    defs = []
    for i, c in enumerate(chunks):
        if not isinstance(c, dict):
            raise RunnerError("parse _stage_defs: each chunk must be an object")
        try:
            defs.append(split_resources(c))
        except RunnerError as err:
            raise RunnerError("parse _stage_defs: chunk %d: %s" % (i, err)) from None
    return defs, _join_override(result.get("join"))


def _join_override(join):
    """Ports the join half of meta.go readStageDefs: the override carries only
    resource keys, and mrp fails the split on any other ("Invalid parameter in
    join definition")."""
    if join is None:
        return Resources()
    if not isinstance(join, dict):
        raise RunnerError("parse _stage_defs: join override must be an object")
    try:
        join_res, extra = split_resources(join)
    except RunnerError as err:
        raise RunnerError("parse _stage_defs: join override: %s" % err) from None
    if extra:
        raise RunnerError("unknown key in join override: %s"
                          % ", ".join(sorted(extra)))
    return join_res


def chunk_defs_doc(defs):
    """chunks.json as mre writeJSON([]shim.ChunkDef) renders it: Go itself
    marshals the Args map[string]json.RawMessage, so the map KEYS are sorted
    and Go-escaped (raw UTF-8 + HTML escapes) while each VALUE is RawMessage
    passthrough (Python json.dumps escaping and insertion order preserved);
    resources are in struct order with Go floats."""
    doc = []
    for res, cargs in defs:
        args_map = GoStruct()
        pairs = sorted(
            ((_json_key(k), v) for k, v in cargs.items()),
            key=lambda kv: kv[0],
        )
        for k, v in pairs:
            args_map[k] = PyStrings(v)
        cd = GoStruct()
        cd["args"] = args_map
        cd["resources"] = resources_struct(res)
        doc.append(cd)
    return doc


def write_chunk_bundles(dir_, defs, man, callable_, confine):
    """Ports cmd/mre/main.go writeChunkBundles + shim.WriteChunkBundle."""
    chunk_in = man.params(callable_, "chunkin")
    for i, (res, cargs) in enumerate(defs):
        bdir = os.path.join(dir_, "chunk_%05d" % i)
        marked = mark_files(bdir, dict(cargs), chunk_in, man.structs, confine)
        write_payload(bdir, {"args": marked, "resources": resources_struct(res)})


def run_outs_phase(fl, phase, files, eff, man, args, invoke):
    """The shared run_main/run_join tail: stage the skeleton outs, import the
    stage module, run invoke(module, outs) under -monitor scoping, validate a
    stage's final outs, and write the -o bundle. On a stage sys.exit(0) the
    vendor shell exits without
    rewriting _outs, so mre bundles the skeleton it staged before the run —
    never the possibly-mutated in-memory Record; a nonzero sys.exit raises
    via stage_system_exit."""
    out_path = abs_out(fl.o)
    out_params = man.params(fl.callable, fl.role)
    skeleton = skeleton_outs(man.structs, out_params, files)
    outs = record(skeleton)
    # The stage runs in files/ (setup_stage chdir's there); restore the task cwd
    # before the data plane so a relative out-path leaf resolves against the task
    # dir as mre does, not against files/ — otherwise the two lanes stage
    # different leaves for the same stage. See run_split for the full rationale.
    task_cwd = os.getcwd()
    exited = False
    try:
        # setup_stage is inside the try so the finally's chdir also covers a
        # setup/import failure that leaves the cwd in files/.
        module = setup_stage(fl, files, eff, args)
        with monitored_phase(fl.monitor, monitor_limits(eff, fl)):
            invoke(module, outs)
    except martian.StageAssertion:
        raise
    except SystemExit as exc:
        stage_system_exit(exc, phase)  # raises on a nonzero exit
        exited = True
    finally:
        os.chdir(task_cwd)
    confine = Confinement(fl.work, phase, fl.confine)
    if exited:
        payload = martian.json_sanitize(dict(skeleton))
    else:
        payload = martian.json_sanitize(outs.items())
    # A chunk main's outs are not validated: mrp's default enforcement level
    # skips them (cmd/mre/main.go runMain).
    if phase == "join" or not fl.chunk:
        validate_outputs(man.structs, man.params(fl.callable, "out"), payload,
                         fl.callable)
    write_bundle(out_path, payload, out_params, man.structs, confine)


def run_main(fl):
    """Ports cmd/mre/main.go runMain + shim.RunMain: merge stage and chunk
    args, inject resolved resources, run main(args, outs), write the bundle."""
    _, files = prep_dirs(fl.work, "main")
    man = Manifest.load(fl.types)
    args = load_stage_args(fl, man)
    chunk_res, chunk_args = decode_chunk(read_bundle(fl.chunk))
    eff = resolve_resources(overridden(fl, chunk_res), cli_resources(fl))
    merged = dict(args)
    merged.update(chunk_args)
    inject_resources(merged, eff)
    run_outs_phase(
        fl, "main", files, eff, man, args,
        lambda module, outs: module.main(record(merged), outs),
    )


def run_join(fl):
    """Ports cmd/mre/main.go runJoin + shim.RunJoin: args + resolved
    resources, chunk defs/outs Records, run join(...), write the bundle."""
    _, files = prep_dirs(fl.work, "join")
    man = Manifest.load(fl.types)
    args = load_stage_args(fl, man)
    chunk_defs, chunk_outs = read_chunk_data(fl.chunksfile, fl.chunkdir)
    eff = resolve_resources(join_request(fl), cli_resources(fl))
    merged = dict(args)
    inject_resources(merged, eff)
    run_outs_phase(
        fl, "join", files, eff, man, args,
        lambda module, outs: module.join(
            record(merged), outs, chunk_defs, chunk_outs),
    )


CHUNK_DEF_PREFIX = "chunk_"
CHUNK_OUT_PREFIX = "out_" + CHUNK_DEF_PREFIX


def read_chunk_data(chunks_file, chunk_dir):
    """Ports cmd/mre/io.go readChunkData + shim writeChunkData + the
    martian_shell join path: chunk_defs Records carry only each chunk's args
    (resources are stripped, as mre's _chunk_defs does); chunk_outs Records
    are each chunk's resolved outs payload. Both defs and outs are per-chunk
    bundle dirs listed from chunk_dir in chunk-index order, which must number
    exactly the chunks chunks.json lists, and are resolved via read_bundle, so
    a file-typed chunk-def leaf the split phase created is rewritten to a
    staged path the join worker can open (#217). Zero chunks is a 0-chunk
    split (the join still runs)."""
    count = read_chunk_count(chunks_file)
    def_recs = []
    for dir_ in gather_indexed(chunk_dir, CHUNK_DEF_PREFIX, count):
        _res, args = decode_chunk(read_bundle(dir_))
        def_recs.append(record(args))
    out_recs = []
    for dir_ in gather_indexed(chunk_dir, CHUNK_OUT_PREFIX, count):
        payload = as_object(read_bundle(dir_), "chunk outs bundle %s" % dir_)
        out_recs.append(record(payload))
    return def_recs, out_recs


def read_join_res(path):
    """Ports cmd/mre/io.go readJoinRes: the split's join resource override,
    none for an empty path."""
    if not path:
        return Resources()
    with open(path, encoding="utf-8") as fh:
        return resources_from_object(json.load(fh))


def read_chunk_count(path):
    """Ports cmd/mre/io.go readChunkCount: the split's chunk-def count."""
    if not path:
        raise RunnerError(
            "join requires -chunksfile <the split's chunks.json>")
    with open(path, encoding="utf-8") as fh:
        defs = json.load(fh)
    if defs is None:
        return 0
    if not isinstance(defs, list):
        raise RunnerError("parse chunk defs %s: not a JSON array" % path)
    return len(defs)


def gather_indexed(dir_, prefix, want):
    """Ports cmd/mre/gather.go gatherIndexed + orderByIndex: the paths of
    dir_'s prefix+<stem><index> entries in index order, which must be
    exactly indices 0..want-1 under one shared stem. A missing dir is zero
    entries (what Nextflow stages for an empty list)."""
    try:
        entries = os.listdir(dir_)
    except FileNotFoundError:
        entries = []
    names = sorted(n for n in entries if n.startswith(prefix))
    if len(names) != want:
        raise RunnerError(
            "gathered entry count differs from the expected count: "
            "%d %s* entries in %s, want %d" % (len(names), prefix, dir_, want))
    paths = [None] * want
    stem = None
    for name in names:
        s, idx = split_index(name)
        if stem is None:
            stem = s
        elif s != stem:
            raise RunnerError(
                "malformed gather entry: %r does not share the stem %r"
                % (name, stem))
        if idx >= want or paths[idx] is not None:
            raise RunnerError(
                "malformed gather entry: %r index %d is repeated or past "
                "the %d expected entries" % (name, idx, want))
        paths[idx] = os.path.join(dir_, name)
    return paths


def split_index(name):
    """Ports cmd/mre/gather.go splitIndex: (stem, trailing decimal index)."""
    cut = len(name)
    while cut > 0 and name[cut - 1] in "0123456789":
        cut -= 1
    if cut == len(name):
        raise RunnerError(
            "malformed gather entry: %r has no trailing index" % name)
    return name[:cut], int(name[cut:])


# ---------------------------------------------------------------------------
# Entry point — exit classification ports cmd/mre/main.go exitCode +
# internal/shim/shim.go stageFailure: martian.exit -> 42, anything else -> 1.
# ---------------------------------------------------------------------------

_PHASES = {"split": run_split, "main": run_main, "join": run_join}


def main(argv):
    if not argv:
        raise RunnerError("usage: run_stage.py <split|main|join> [flags]")
    phase = argv[0]
    runner = _PHASES.get(phase)
    if runner is None:
        raise RunnerError("unknown phase: %r" % phase)
    runner(parse_flags(phase, argv[1:]))


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except martian.StageAssertion as exc:
        # mrp's write_assert prefix; shim.go classifies it non-retryable.
        sys.stderr.write(
            "ASSERT:%s\n" % martian._to_string_type(exc.message)
        )
        sys.exit(ASSERT_EXIT_CODE)
    except RunnerError as exc:
        sys.stderr.write("run_stage: %s\n" % exc)
        sys.exit(1)
    except SystemExit:
        raise
    except (KeyboardInterrupt, GeneratorExit):
        # The mre adapter path dies by signal on interrupt; do not convert
        # one into a retryable stage failure — let Python's default apply.
        raise
    except BaseException:  # pylint: disable=broad-except
        traceback.print_exc()
        sys.exit(1)
