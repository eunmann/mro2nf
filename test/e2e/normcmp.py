#!/usr/bin/env python3
"""Compare a Martian _outs against a transpiled pipeline_outs.json.

Objects compare with their key order and numbers by their source token. A
normalization that excuses a real difference applies only while the divergence
allowlist (divergences.json beside this script, or the fifth argument) has an
entry for its comparator rule, as in the Go differential (published_test.go):

- follow-mrp-published-symlinks: for an output mrp publishes as a symlink in
  outs/, mrp's _outs names the link target, mro2nf the published path, so the
  target in mrp's strings becomes the link's path; a target several links
  share becomes, for each reference, the link its counterpart names.
- outs-json-strip-outs-prefix: mrp writes absolute paths into the pipestance
  outs/ dir, mro2nf writes the same paths relative to its results/ dir, so the
  outs-dir prefix is stripped from mrp's strings.
- placeholder-unpublished-paths: a file left unpublished is named where its
  task wrote it, by mrp in the pipestance outside outs/, by mro2nf in a
  Nextflow task work dir, so that path on either side becomes one
  placeholder.

An issue-tracked entry applies its rule only on the fixtures it names; the Go
differential, not this script, fails it once it excuses nothing there.

Usage: normcmp.py <mrp_outs_json> <mrp_outs_dir> <nf_outs_json> <fixture> [<allowlist>]
"""
import json
import os
import re
import sys

PUBLISHED_SYMLINKS = "follow-mrp-published-symlinks"
STRIP_OUTS_PREFIX = "outs-json-strip-outs-prefix"
UNPUBLISHED_PATHS = "placeholder-unpublished-paths"

UNPUBLISHED = "<unpublished>"

# An absolute path inside a Nextflow task work dir: one below the task's
# <2 hex>/<30 hex> hash dirs. Mirrors nfTaskPath in published_test.go.
NF_TASK_PATH = re.compile(r"^/(?:[^/]+/)*[0-9a-f]{2}/[0-9a-f]{30}/.")

OBJECT = "<object>"
NUMBER = "<number>"


def number(token):
    return (NUMBER, token)


def load(path):
    with open(path) as f:
        return json.load(
            f,
            object_pairs_hook=lambda pairs: (OBJECT, pairs),
            parse_float=number,
            parse_int=number,
            parse_constant=number,
        )


def excused_rules(path, fixture):
    with open(path) as f:
        doc = json.load(f)
    rules = set()
    for e in doc["divergences"]:
        matcher = e.get("comparator")
        if matcher and fixture in matcher.get("fixtures", [fixture]):
            rules.add(matcher["rule"])
    return rules


def read_link_abs(p):
    target = os.readlink(p)
    return target if os.path.isabs(target) else os.path.normpath(os.path.join(os.path.dirname(p), target))


def published_links(outs):
    """Map each path mrp's _outs names for a symlink under outs to the links.

    The links of a path are sorted. mrp names the link's target, except for a
    stage's relative symlink that leads through further links: there outs/
    links to the last intermediate link and mrp names that link's target
    (martian/core/post_process.go copyOutSymlink). A target under outs is the
    file mrp moved there for the first output that reached it, which mro2nf
    names too, so it is kept. Mirrors mrpPublishedLinks in published_test.go.
    """
    links = {}
    in_outs = outs.rstrip("/") + "/"
    for dirpath, dirnames, filenames in os.walk(outs):
        for name in dirnames + filenames:
            p = os.path.join(dirpath, name)
            if not os.path.islink(p):
                continue
            target = read_link_abs(p)
            if target.startswith(in_outs):
                continue
            links.setdefault(target, []).append(p)
            if os.path.islink(target):
                nxt = read_link_abs(target)
                if not nxt.startswith(in_outs):
                    links.setdefault(nxt, []).append(p)
    for paths in links.values():
        paths.sort()
    return links


def norm(v, rewrite, other=None):
    """Rewrite each string in v given its counterpart in other.

    The counterpart is the value at the same object key or array index, None
    where other has none.
    """
    if isinstance(v, str):
        return rewrite(v, other)
    if isinstance(v, list):
        o = other if isinstance(other, list) else []
        return [norm(x, rewrite, o[i] if i < len(o) else None) for i, x in enumerate(v)]
    if isinstance(v, tuple) and v[0] == OBJECT:
        o = dict(other[1]) if isinstance(other, tuple) and other[0] == OBJECT else {}
        return (OBJECT, [(k, norm(x, rewrite, o.get(k))) for k, x in v[1]])
    return v


def mrp_rewrite(rules, mrp_dir):
    """Rewrite an mrp _outs string given its pipeline_outs.json counterpart.

    mrp names a target once for each output that reaches it and links each
    output's own path to it, and the pipestance does not record which link is
    whose, so a target with several links becomes the one its counterpart
    names, each link for one reference only; a reference left without one
    keeps the target. Mirrors mrpOutsRewrite in published_test.go.
    """
    links = published_links(mrp_dir) if PUBLISHED_SYMLINKS in rules else {}
    in_outs = mrp_dir.rstrip("/") + "/"
    prefix = in_outs if STRIP_OUTS_PREFIX in rules else ""
    in_ps = os.path.dirname(mrp_dir.rstrip("/")) + "/" if UNPUBLISHED_PATHS in rules else ""
    claimed = set()

    def path(s):
        if in_ps and s.startswith(in_ps) and not s.startswith(in_outs):
            return UNPUBLISHED
        if prefix and s.startswith(prefix):
            return s[len(prefix):]
        return s

    def rewrite(s, nf):
        paths = links.get(s, [])
        if len(paths) == 1:
            return path(paths[0])
        for p in paths:
            if p not in claimed and path(p) == nf:
                claimed.add(p)
                return path(p)
        return path(s)

    return rewrite


def nf_rewrite(rules):
    if UNPUBLISHED_PATHS not in rules:
        return lambda s, _: s
    return lambda s, _: UNPUBLISHED if NF_TASK_PATH.match(s) else s


def render(v):
    if isinstance(v, tuple) and v[0] == NUMBER:
        return v[1]
    if isinstance(v, tuple):
        return "{" + ",".join(json.dumps(k, ensure_ascii=False) + ":" + render(x) for k, x in v[1]) + "}"
    if isinstance(v, list):
        return "[" + ",".join(render(x) for x in v) + "]"
    return json.dumps(v, ensure_ascii=False)


def main():
    mrp_json, mrp_dir, nf_json, name = sys.argv[1:5]
    allowlist = sys.argv[5] if len(sys.argv) > 5 else os.path.join(
        os.path.dirname(os.path.abspath(__file__)), "divergences.json")
    rules = excused_rules(allowlist, name)

    nf_outs = norm(load(nf_json), nf_rewrite(rules))
    mrp = render(norm(load(mrp_json), mrp_rewrite(rules, mrp_dir), nf_outs))
    nf = render(nf_outs)

    if mrp == nf:
        print("  json: match")
        return 0

    print(f"FAIL[{name}]: _outs json mismatch")
    print(f"  mrp(normalized) = {mrp}")
    print(f"  nextflow        = {nf}")
    return 1


if __name__ == "__main__":
    sys.exit(main())
