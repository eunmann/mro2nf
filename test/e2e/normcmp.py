#!/usr/bin/env python3
"""Compare a Martian _outs against a transpiled pipeline_outs.json.

Objects compare with their key order and numbers by their source token. A
normalization that excuses a real difference applies only while the divergence
allowlist (divergences.json beside this script, or the fifth argument) has an
entry for its comparator rule, as in the Go differential (published_test.go):

- follow-mrp-published-symlinks: for an output mrp publishes as a symlink in
  outs/, mrp's _outs names the link target, mro2nf the published path, so the
  target in mrp's strings becomes the link's path.
- outs-json-strip-outs-prefix: mrp writes absolute paths into the pipestance
  outs/ dir, mro2nf writes the same paths relative to its results/ dir, so the
  outs-dir prefix is stripped from mrp's strings.

An issue-tracked entry applies its rule only on the fixtures it names; the Go
differential, not this script, fails it once it excuses nothing there.

Usage: normcmp.py <mrp_outs_json> <mrp_outs_dir> <nf_outs_json> <fixture> [<allowlist>]
"""
import json
import os
import sys

PUBLISHED_SYMLINKS = "follow-mrp-published-symlinks"
STRIP_OUTS_PREFIX = "outs-json-strip-outs-prefix"

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
    """Map the path mrp's _outs names for each symlink under outs to the link.

    mrp names the link's target, except for a stage's relative symlink that
    leads through further links: there outs/ links to the last intermediate
    link and mrp names that link's target (martian/core/post_process.go
    copyOutSymlink). Mirrors mrpPublishedLinks in published_test.go.
    """
    links = {}
    for dirpath, dirnames, filenames in os.walk(outs):
        for name in dirnames + filenames:
            p = os.path.join(dirpath, name)
            if not os.path.islink(p):
                continue
            target = read_link_abs(p)
            links[target] = p
            if os.path.islink(target):
                links[read_link_abs(target)] = p
    return links


def norm(v, rewrite):
    if isinstance(v, str):
        return rewrite(v)
    if isinstance(v, list):
        return [norm(x, rewrite) for x in v]
    if isinstance(v, tuple) and v[0] == OBJECT:
        return (OBJECT, [(k, norm(x, rewrite)) for k, x in v[1]])
    return v


def mrp_rewrite(rules, mrp_dir):
    links = published_links(mrp_dir) if PUBLISHED_SYMLINKS in rules else {}
    prefix = mrp_dir.rstrip("/") + "/" if STRIP_OUTS_PREFIX in rules else ""

    def rewrite(s):
        s = links.get(s, s)
        return s[len(prefix):] if prefix and s.startswith(prefix) else s

    return rewrite


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

    mrp = render(norm(load(mrp_json), mrp_rewrite(rules, mrp_dir)))
    nf = render(norm(load(nf_json), lambda s: s))

    if mrp == nf:
        print("  json: match")
        return 0

    print(f"FAIL[{name}]: _outs json mismatch")
    print(f"  mrp(normalized) = {mrp}")
    print(f"  nextflow        = {nf}")
    return 1


if __name__ == "__main__":
    sys.exit(main())
