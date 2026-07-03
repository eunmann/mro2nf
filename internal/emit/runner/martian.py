#!/usr/bin/env python3
"""Martian stage-code API for the mro2nf native runner (issue #79).

A lean port of vendor-martian/python/martian.py: same public surface and
quirks (make_path returns BYTES; Record.__getitem__ indexes the dict-keys
view as-is), but with no metadata-file protocol and no fd3/fd4 channels:

  - log_* write to stderr in mrp's log-line format
    ('%Y-%m-%d %H:%M:%S [level] msg'),
  - exit() raises StageAssertion, which run_stage.py maps to exit code 42
    (internal/shim/shim.go AssertExitCode),
  - alarm() appends to ./_warnings (cwd is the phase files dir) and stderr,
  - update_progress() best-effort writes ./_progress,
  - the resolved per-phase resources are installed by run_stage.py via
    _configure() and served by get_threads_allocation()/get_memory_allocation().

Stage code does `import martian`; run_stage.py's directory is sys.path[0]
(and is re-appended before the stage module import), so this sibling module
is what resolves.
"""

from __future__ import absolute_import, division, print_function

import datetime
import json
import math
import os
import resource
import subprocess
import sys
import time
from pathlib import PurePath

# Resolved per-phase state, set by run_stage.py via _configure() before the
# stage phase runs. Values mirror what the mre shim writes into _jobinfo
# (internal/shim/meta.go writeJobInfo).
_FILES_PATH = b""
_THREADS = 1
_MEM_GB = 1


class StageException(Exception):
    """Base exception type for stage code."""


class StageAssertion(SystemExit):
    """Raised by exit(): a non-retryable Martian assertion.

    Subclasses SystemExit so a stage's broad `except Exception` cannot
    swallow it, matching the vendor adapter where exit() ends in sys.exit().
    run_stage.py catches it and exits 42 (shim.AssertExitCode).
    """

    def __init__(self, message):
        super().__init__(42)
        self.message = message


def _configure(files_path, threads, mem_gb):
    """Install the per-phase state. Called by run_stage.py, not stage code."""
    global _FILES_PATH, _THREADS, _MEM_GB  # pylint: disable=global-statement
    _FILES_PATH = os.fsencode(files_path)
    _THREADS = threads
    _MEM_GB = mem_gb


class Record:
    """An object with a set of attributes generated from a dictioanry."""

    def __init__(self, f_dict):
        """Initializes the object from a dictionary."""
        self.slots = f_dict.keys()
        for field_name in self.slots:
            setattr(self, field_name, f_dict[field_name])

    def items(self):
        """Returns the a dictionary with the elements which were in the keys in
        dictionary used to initialize the record."""
        return {
            field_name: getattr(self, field_name) for field_name in self.slots
        }

    def __str__(self):
        """Formats the object as a string."""
        return str(self.items())

    def __iter__(self):
        """Iterate through the values of the object corresponding to keys in
        the dictionary used to initialize the object."""
        for field_name in self.slots:
            yield getattr(self, field_name)

    def __getitem__(self, index):
        """Get the value associated with the Nth key in the source
        dictionary."""
        # Vendor quirk kept as-is: self.slots is a dict-keys view, which is
        # not indexable, so this raises TypeError exactly like the vendor.
        return getattr(self, self.slots[index])

    def coerce_strings(self):
        """This exists only for backwards compatibility."""


def clear(outs):
    """Set all of the outs to None."""
    for field_name in outs.slots:
        setattr(outs, field_name, None)


def json_sanitize(data):
    """Converts NaN values into None values, and decode raw bytes."""
    retval = data
    if isinstance(data, float):
        # Handle exceptional floats.
        if math.isnan(data) or data == float("+Inf") or data == float("-Inf"):
            retval = None
    elif isinstance(data, dict):
        # Recurse on dictionaries.
        retval = {}
        for k in data.keys():
            retval[k] = json_sanitize(data[k])
    elif isinstance(data, str):
        # This branch is required to prevent the __iter__ branch from
        # processing strings.
        pass
    elif isinstance(data, bytes):
        retval = data.decode("utf-8", errors="ignore")
    elif isinstance(data, PurePath):
        retval = str(data)
    elif hasattr(data, "__iter__"):
        # Recurse on lists.
        retval = [json_sanitize(d) for d in data]
    return retval


def json_dumps_safe(data, indent=None):
    """Returns a formatted json string of the data, with NaN values converted
    to None."""
    return json.dumps(json_sanitize(data), indent=indent)


def get_mem_kb():
    """Get the current max rss memory for this process and completed child
    processes."""
    return max(
        resource.getrusage(resource.RUSAGE_SELF).ru_maxrss,
        resource.getrusage(resource.RUSAGE_CHILDREN).ru_maxrss,
    )


def convert_gb_to_kb(mem_gb):
    """Convert from gb to kb."""
    return mem_gb * 1024 * 1024


def padded_print(field_name, value):
    """Pad a string with leading spaces to be the same length as field_name."""
    offset = len(field_name) - len(str(value))
    if offset > 0:
        return (" " * offset) + str(value)
    return str(value)


def profile(func):
    """Passthrough decorator: the runner has no line profiler to register
    with (vendor martian.py appends to the StageWrapper's function list)."""
    return func


# On linux, provide a method to set PDEATHSIG on child processes.
if sys.platform.startswith("linux"):
    import ctypes
    import ctypes.util
    from signal import SIGKILL

    _LIBC = ctypes.CDLL(ctypes.util.find_library("c"))

    _PR_SET_PDEATHSIG = ctypes.c_int(1)  # <sys/prctl.h>

    def child_preexec_set_pdeathsig():
        """When used as the preexec_fn argument for subprocess.Popen etc,
        causes the subprocess to receive SIGKILL if the parent process
        terminates."""
        zero = ctypes.c_ulong(0)
        _LIBC.prctl(
            _PR_SET_PDEATHSIG, ctypes.c_ulong(SIGKILL), zero, zero, zero
        )

else:
    child_preexec_set_pdeathsig = None  # pylint: disable=invalid-name


def _to_string_type(message):
    """Ports martian_shell.py _Metadata._to_string_type."""
    if isinstance(message, bytes):
        return message.decode("utf-8", errors="ignore")
    if not isinstance(message, str):
        return str(message)
    return message


def _ensure_binary(string):
    """Encode unicode strings to bytes, leave byte strings alone."""
    if isinstance(string, str):
        return string.encode("utf-8")
    return string


def _timestamp_now():
    """Formats the current time per martian_shell.py _Metadata.make_timestamp."""
    return datetime.datetime.fromtimestamp(time.time()).strftime(
        "%Y-%m-%d %H:%M:%S"
    )


def _log(level, message):
    """Write one mrp-format log line to stderr (the runner has no _log fd)."""
    sys.stderr.write(
        "{} [{}] {}\n".format(_timestamp_now(), level, _to_string_type(message))
    )
    sys.stderr.flush()


# pylint: disable=invalid-name,too-many-arguments
def Popen(
    args,
    bufsize=0,
    executable=None,
    stdin=None,
    stdout=None,
    stderr=None,
    preexec_fn=child_preexec_set_pdeathsig,
    close_fds=False,
    shell=False,
    cwd=None,
    env=None,
    universal_newlines=False,
    startupinfo=None,
    creationflags=0,
):
    """Log opening of a subprocess."""
    _log("exec", " ".join(args))
    # pylint: disable=subprocess-popen-preexec-fn, consider-using-with
    return subprocess.Popen(
        args,
        bufsize=bufsize,
        executable=executable,
        stdin=stdin,
        stdout=stdout,
        stderr=stderr,
        preexec_fn=preexec_fn,
        close_fds=close_fds,
        shell=shell,
        cwd=cwd,
        env=env,
        universal_newlines=universal_newlines,
        startupinfo=startupinfo,
        creationflags=creationflags,
    )


def check_call(args, stdin=None, stdout=None, stderr=None, shell=False):
    """Log running a given subprocess."""
    _log("exec", " ".join(args))
    return subprocess.check_call(
        args,
        shell=shell,
        stdin=stdin,
        stdout=stdout,
        stderr=stderr,
        preexec_fn=child_preexec_set_pdeathsig,
    )


def make_path(filename):
    """Get the file path for a named file. Returns bytes (vendor quirk)."""
    if isinstance(filename, str):
        filename = filename.encode("utf-8")
    return os.path.join(_FILES_PATH, filename)


def get_invocation_args():
    """Get the args from the invocation."""
    return {}


def get_invocation_call():
    """Get the call information from the invocation."""
    return None


def get_martian_version():
    """Get the martian version (internal/shim/meta.go writeJobInfo)."""
    return "mro2nf"


def get_pipelines_version():
    """Get the pipelines version (internal/shim/meta.go writeJobInfo)."""
    return "mro2nf"


def get_threads_allocation():
    """Get the number of threads allocated to this job by the runtime."""
    return _THREADS


def get_memory_allocation():
    """Get the amount of memory in GB allocated to this job by the runtime."""
    return _MEM_GB


def get_pipestance_uuid():
    """Get the UUID of the top-level pipeline instance.

    Returns an empty string if the UUID is not available.
    """
    return os.getenv("MRO_UUID") or os.getenv("MRO_FORCE_UUID") or ""


def update_progress(message):
    """Best-effort progress update: write ./_progress (cwd is the phase files
    dir); there is no mrp to read it, but stages may call this freely."""
    try:
        with open("_progress", "wb") as dest:
            dest.write(_ensure_binary(message))
    except OSError:
        pass


def log_info(message):
    """Log a message."""
    _log("info", message)


def log_warn(message):
    """Log a warning."""
    _log("warn", message)


def log_time(message):
    """Log a timestamp for an action."""
    _log("time", message)


def log_json(label, obj):
    """Log an object in json format."""
    _log("json", json_dumps_safe({"label": label, "object": obj}))


def throw(message):
    """Raise a stage exception."""
    raise StageException(message)


def exit(message):  # pylint: disable=redefined-builtin
    """Fail the pipeline with an assertion (non-retryable; exit code 42)."""
    raise StageAssertion(message)


def alarm(message):
    """Add a message to the alarms: append ./_warnings, echo to stderr."""
    text = _to_string_type(message)
    try:
        with open("_warnings", "a", encoding="utf-8") as dest:
            dest.write(text + "\n")
    except OSError:
        pass
    sys.stderr.write(text + "\n")
